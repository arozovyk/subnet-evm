// (c) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package evm

import (
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ava-labs/subnet-evm/params"

	"github.com/ava-labs/avalanchego/snow"
)

func TestBlockBuilderShutsDown(t *testing.T) {
	shutdownChan := make(chan struct{})
	wg := &sync.WaitGroup{}
	config := *params.TestChainConfig
	config.SubnetEVMTimestamp = big.NewInt(time.Now().Add(time.Hour).Unix())

	builder := &blockBuilder{
		ctx:          snow.DefaultContextTest(),
		chainConfig:  &config,
		shutdownChan: shutdownChan,
		shutdownWg:   wg,
	}

	builder.handleBlockBuilding()
	// Close [shutdownChan] and ensure that the wait group finishes in a reasonable
	// amount of time.
	close(shutdownChan)
	attemptAwait(t, wg, 5*time.Second)
}

func TestBlockBuilderSkipsTimerInitialization(t *testing.T) {
	shutdownChan := make(chan struct{})
	wg := &sync.WaitGroup{}
	builder := &blockBuilder{
		ctx:          snow.DefaultContextTest(),
		chainConfig:  params.TestChainConfig,
		shutdownChan: shutdownChan,
		shutdownWg:   wg,
	}

	builder.handleBlockBuilding()
	// The wait group should finish immediately since no goroutine
	// should be created when all prices should be set from the start
	attemptAwait(t, wg, time.Millisecond)
}

func TestBlockBuilderStopsTimer(t *testing.T) {
	shutdownChan := make(chan struct{})
	wg := &sync.WaitGroup{}
	config := *params.TestChainConfig
	config.SubnetEVMTimestamp = big.NewInt(time.Now().Add(1 * time.Second).Unix())

	builder := &blockBuilder{
		ctx:          snow.DefaultContextTest(),
		chainConfig:  &config,
		shutdownChan: shutdownChan,
		shutdownWg:   wg,
	}

	builder.handleBlockBuilding()

	if builder.buildBlockTimer == nil {
		t.Fatal("expected block timer to not be nil")
	}
	builder.buildBlockLock.Lock()
	builder.buildStatus = conditionalBuild
	builder.buildBlockLock.Unlock()

	// With Subnet EVM set slightly in the future, the builder should create a
	// goroutine to sleep until its time to update and mark the wait group as done when it has
	// completed the update.
	attemptAwait(t, wg, 5*time.Second)

	if builder.buildBlockTimer == nil {
		t.Fatal("expected block timer to be non-nil")
	}
	if builder.buildStatus != mayBuild {
		t.Fatalf("expected build status to be %d but got %d", dontBuild, builder.buildStatus)
	}
	if !builder.isSC {
		t.Fatal("expected isSC to be true")
	}
}
