// (c) 2019-2020, Ava Labs, Inc.
//
// This file is a derived work, based on the go-ethereum library whose original
// notices appear below.
//
// It is distributed under a license compatible with the licensing terms of the
// original code from which it is derived.
//
// Much love to the original authors for their work.
// **********
// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package eth implements the Ethereum protocol.
package eth

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ava-labs/avalanchego/utils/timer/mockable"
	"github.com/ava-labs/subnet-evm/accounts"
	"github.com/ava-labs/subnet-evm/consensus"
	"github.com/ava-labs/subnet-evm/consensus/dummy"
	"github.com/ava-labs/subnet-evm/core"
	"github.com/ava-labs/subnet-evm/core/bloombits"
	"github.com/ava-labs/subnet-evm/core/rawdb"
	"github.com/ava-labs/subnet-evm/core/types"
	"github.com/ava-labs/subnet-evm/core/vm"
	"github.com/ava-labs/subnet-evm/eth/ethconfig"
	"github.com/ava-labs/subnet-evm/eth/filters"
	"github.com/ava-labs/subnet-evm/eth/gasprice"
	"github.com/ava-labs/subnet-evm/eth/tracers"
	"github.com/ava-labs/subnet-evm/ethdb"
	"github.com/ava-labs/subnet-evm/internal/ethapi"
	"github.com/ava-labs/subnet-evm/miner"
	"github.com/ava-labs/subnet-evm/node"
	"github.com/ava-labs/subnet-evm/params"
	"github.com/ava-labs/subnet-evm/rpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
)

// Config contains the configuration options of the ETH protocol.
// Deprecated: use ethconfig.Config instead.
type Config = ethconfig.Config

var DefaultSettings Settings = Settings{MaxBlocksPerRequest: 2000}

type Settings struct {
	MaxBlocksPerRequest int64 // Maximum number of blocks to serve per getLogs request
}

// Ethereum implements the Ethereum full node service.
type Ethereum struct {
	config *Config

	// Handlers
	txPool     *core.TxPool
	blockchain *core.BlockChain

	// DB interfaces
	chainDb ethdb.Database // Block chain database

	eventMux       *event.TypeMux
	engine         consensus.Engine
	accountManager *accounts.Manager

	bloomRequests     chan chan *bloombits.Retrieval // Channel receiving bloom data retrieval requests
	bloomIndexer      *core.ChainIndexer             // Bloom indexer operating during block imports
	closeBloomHandler chan struct{}

	APIBackend *EthAPIBackend

	miner     *miner.Miner
	etherbase common.Address

	networkID     uint64
	netRPCService *ethapi.PublicNetAPI

	lock sync.RWMutex // Protects the variadic fields (e.g. gas price and etherbase)

	settings Settings // Settings for Ethereum API
}

// New creates a new Ethereum object (including the
// initialisation of the common Ethereum object)
func New(stack *node.Node, config *Config,
	chainDb ethdb.Database,
	settings Settings,
	lastAcceptedHash common.Hash,
	clock *mockable.Clock,
) (*Ethereum, error) {
	if chainDb == nil {
		return nil, errors.New("chainDb cannot be nil")
	}
	if !config.Pruning && config.TrieDirtyCache > 0 {
		// If snapshots are enabled, allocate 2/5 of the TrieDirtyCache memory cap to the snapshot cache
		if config.SnapshotCache > 0 {
			config.TrieCleanCache += config.TrieDirtyCache * 3 / 5
			config.SnapshotCache += config.TrieDirtyCache * 2 / 5
		} else {
			// If snapshots are disabled, the TrieDirtyCache will be written through to the clean cache
			// so move the cache allocation from the dirty cache to the clean cache
			config.TrieCleanCache += config.TrieDirtyCache
			config.TrieDirtyCache = 0
		}
	}
	log.Info("Allocated trie memory caches", "clean", common.StorageSize(config.TrieCleanCache)*1024*1024, "dirty", common.StorageSize(config.TrieDirtyCache)*1024*1024)

	chainConfig, genesisErr := core.SetupGenesisBlock(chainDb, config.Genesis)
	if genesisErr != nil {
		return nil, genesisErr
	}
	log.Info("Initialised chain configuration", "config", chainConfig)

	eth := &Ethereum{
		config:            config,
		chainDb:           chainDb,
		eventMux:          stack.EventMux(),
		accountManager:    stack.AccountManager(),
		engine:            dummy.NewEngine(),
		closeBloomHandler: make(chan struct{}),
		networkID:         config.NetworkId,
		etherbase:         config.Miner.Etherbase,
		bloomRequests:     make(chan chan *bloombits.Retrieval),
		bloomIndexer:      core.NewBloomIndexer(chainDb, params.BloomBitsBlocks, params.BloomConfirms),
		settings:          settings,
	}

	bcVersion := rawdb.ReadDatabaseVersion(chainDb)
	dbVer := "<nil>"
	if bcVersion != nil {
		dbVer = fmt.Sprintf("%d", *bcVersion)
	}
	log.Info("Initialising Ethereum protocol", "network", config.NetworkId, "dbversion", dbVer)

	if !config.SkipBcVersionCheck {
		if bcVersion != nil && *bcVersion > core.BlockChainVersion {
			return nil, fmt.Errorf("database version is v%d, Subnet EVM %s only supports v%d", *bcVersion, params.VersionWithMeta, core.BlockChainVersion)
		} else if bcVersion == nil || *bcVersion < core.BlockChainVersion {
			log.Warn("Upgrade blockchain database version", "from", dbVer, "to", core.BlockChainVersion)
			rawdb.WriteDatabaseVersion(chainDb, core.BlockChainVersion)
		}
	}
	var (
		vmConfig = vm.Config{
			EnablePreimageRecording: config.EnablePreimageRecording,
			AllowUnfinalizedQueries: config.AllowUnfinalizedQueries,
		}
		cacheConfig = &core.CacheConfig{
			TrieCleanLimit: config.TrieCleanCache,
			TrieDirtyLimit: config.TrieDirtyCache,
			Pruning:        config.Pruning,
			SnapshotLimit:  config.SnapshotCache,
			SnapshotAsync:  config.SnapshotAsync,
			SnapshotVerify: config.SnapshotVerify,
			Preimages:      config.Preimages,
		}
	)
	var err error
	eth.blockchain, err = core.NewBlockChain(chainDb, cacheConfig, chainConfig, eth.engine, vmConfig, lastAcceptedHash)
	if err != nil {
		return nil, err
	}
	eth.bloomIndexer.Start(eth.blockchain)

	// Original code (requires disk):
	// if config.TxPool.Journal != "" {
	// 	config.TxPool.Journal = stack.ResolvePath(config.TxPool.Journal)
	// }
	config.TxPool.Journal = ""
	eth.txPool = core.NewTxPool(config.TxPool, chainConfig, eth.blockchain)

	eth.miner = miner.New(eth, &config.Miner, chainConfig, eth.EventMux(), eth.engine, clock)

	eth.APIBackend = &EthAPIBackend{
		extRPCEnabled:       stack.Config().ExtRPCEnabled(),
		allowUnprotectedTxs: config.AllowUnprotectedTxs,
		eth:                 eth,
	}
	if config.AllowUnprotectedTxs {
		log.Info("Unprotected transactions allowed")
	}
	gpoParams := config.GPO
	eth.APIBackend.gpo = gasprice.NewOracle(eth.APIBackend, gpoParams)

	if err != nil {
		return nil, err
	}

	// Start the RPC service
	eth.netRPCService = ethapi.NewPublicNetAPI(eth.NetVersion())

	// Register the backend on the node
	stack.RegisterAPIs(eth.APIs())

	return eth, nil
}

// APIs return the collection of RPC services the ethereum package offers.
// NOTE, some of these services probably need to be moved to somewhere else.
func (s *Ethereum) APIs() []rpc.API {
	apis := ethapi.GetAPIs(s.APIBackend)

	// Append tracing APIs
	apis = append(apis, tracers.APIs(s.APIBackend)...)

	// Append all the local APIs and return
	return append(apis, []rpc.API{
		{
			Namespace: "eth",
			Version:   "1.0",
			Service:   NewPublicEthereumAPI(s),
			Public:    true,
		}, {
			Namespace: "eth",
			Version:   "1.0",
			Service:   filters.NewPublicFilterAPI(s.APIBackend, false, 5*time.Minute),
			Public:    true,
		}, {
			Namespace: "admin",
			Version:   "1.0",
			Service:   NewPrivateAdminAPI(s),
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPublicDebugAPI(s),
			Public:    true,
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPrivateDebugAPI(s),
		}, {
			Namespace: "net",
			Version:   "1.0",
			Service:   s.netRPCService,
			Public:    true,
		},
	}...)
}

func (s *Ethereum) Etherbase() (eb common.Address, err error) {
	s.lock.RLock()
	etherbase := s.etherbase
	s.lock.RUnlock()

	if etherbase != (common.Address{}) {
		return etherbase, nil
	}
	if wallets := s.AccountManager().Wallets(); len(wallets) > 0 {
		if accounts := wallets[0].Accounts(); len(accounts) > 0 {
			etherbase := accounts[0].Address

			s.lock.Lock()
			s.etherbase = etherbase
			s.lock.Unlock()

			log.Info("Etherbase automatically configured", "address", etherbase)
			return etherbase, nil
		}
	}
	return common.Address{}, fmt.Errorf("etherbase must be explicitly specified")
}

// SetEtherbase sets the mining reward address.
func (s *Ethereum) SetEtherbase(etherbase common.Address) {
	s.lock.Lock()
	s.etherbase = etherbase
	s.lock.Unlock()

	s.miner.SetEtherbase(etherbase)
}

func (s *Ethereum) Miner() *miner.Miner { return s.miner }

func (s *Ethereum) AccountManager() *accounts.Manager { return s.accountManager }
func (s *Ethereum) BlockChain() *core.BlockChain      { return s.blockchain }
func (s *Ethereum) TxPool() *core.TxPool              { return s.txPool }
func (s *Ethereum) EventMux() *event.TypeMux          { return s.eventMux }
func (s *Ethereum) Engine() consensus.Engine          { return s.engine }
func (s *Ethereum) ChainDb() ethdb.Database           { return s.chainDb }

func (s *Ethereum) NetVersion() uint64               { return s.networkID }
func (s *Ethereum) ArchiveMode() bool                { return !s.config.Pruning }
func (s *Ethereum) BloomIndexer() *core.ChainIndexer { return s.bloomIndexer }

// Start implements node.Lifecycle, starting all internal goroutines needed by the
// Ethereum protocol implementation.
func (s *Ethereum) Start() {
	// Start the bloom bits servicing goroutines
	s.startBloomHandlers(params.BloomBitsBlocks)
}

// Stop implements node.Lifecycle, terminating all internal goroutines used by the
// Ethereum protocol.
// FIXME remove error from type if this will never return an error
func (s *Ethereum) Stop() error {
	s.bloomIndexer.Close()
	close(s.closeBloomHandler)
	s.txPool.Stop()
	s.blockchain.Stop()
	s.engine.Close()
	s.chainDb.Close()
	s.eventMux.Stop()
	return nil
}

func (s *Ethereum) LastAcceptedBlock() *types.Block {
	return s.blockchain.LastAcceptedBlock()
}
