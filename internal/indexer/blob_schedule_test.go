package indexer

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"

	"github.com/a-thomas-22/blob-indexer-api/internal/blobparams"
	"github.com/a-thomas-22/blob-indexer-api/internal/ethereum"
)

func TestScheduleEntriesFromEthConfig(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		if got := scheduleEntriesFromEthConfig("test", nil); got != nil {
			t.Errorf("want nil, got %+v", got)
		}
	})

	t.Run("dedupes and skips forks without a schedule", func(t *testing.T) {
		cfg := &ethereum.EthConfig{
			Current: &ethereum.EthConfigFork{ActivationTime: 100, BlobSchedule: &params.BlobConfig{Target: 6, Max: 9, UpdateFraction: 5007716}},
			Next:    &ethereum.EthConfigFork{ActivationTime: 200, BlobSchedule: &params.BlobConfig{Target: 14, Max: 21, UpdateFraction: 11684671}},
			// Last duplicates Current's boundary and must collapse to one entry.
			Last: &ethereum.EthConfigFork{ActivationTime: 100, BlobSchedule: &params.BlobConfig{Target: 6, Max: 9, UpdateFraction: 5007716}},
		}
		entries := scheduleEntriesFromEthConfig("test", cfg)
		if len(entries) != 2 {
			t.Fatalf("want 2 deduped entries, got %d: %+v", len(entries), entries)
		}
		byTime := map[uint64]blobparams.ScheduleEntry{}
		for _, e := range entries {
			byTime[e.ActivationTime] = e
		}
		if byTime[200].Target != 14 || byTime[200].Max != 21 {
			t.Errorf("boundary 200 = %+v, want target 14 / max 21", byTime[200])
		}
	})

	t.Run("skips fork missing blob schedule", func(t *testing.T) {
		cfg := &ethereum.EthConfig{
			Current: &ethereum.EthConfigFork{ActivationTime: 0, BlobSchedule: nil},
			Next:    &ethereum.EthConfigFork{ActivationTime: 300, BlobSchedule: &params.BlobConfig{Target: 3, Max: 6, UpdateFraction: 3338477}},
		}
		if got := scheduleEntriesFromEthConfig("test", cfg); len(got) != 1 || got[0].ActivationTime != 300 {
			t.Errorf("want single entry at 300, got %+v", got)
		}
	})

	t.Run("rejects invalid params from the node", func(t *testing.T) {
		cfg := &ethereum.EthConfig{
			// update_fraction 0 would divide-by-zero in CalcBlobFee.
			Current: &ethereum.EthConfigFork{ActivationTime: 100, BlobSchedule: &params.BlobConfig{Target: 6, Max: 9, UpdateFraction: 0}},
			// max < target is nonsensical.
			Next: &ethereum.EthConfigFork{ActivationTime: 200, BlobSchedule: &params.BlobConfig{Target: 9, Max: 6, UpdateFraction: 5007716}},
			// non-positive target.
			Last: &ethereum.EthConfigFork{ActivationTime: 300, BlobSchedule: &params.BlobConfig{Target: 0, Max: 6, UpdateFraction: 5007716}},
		}
		if got := scheduleEntriesFromEthConfig("test", cfg); len(got) != 0 {
			t.Errorf("want all invalid entries skipped, got %+v", got)
		}
	})
}

func TestGetBlobBaseFeeFromBlock_NoActiveConfigDoesNotPanic(t *testing.T) {
	idx := newTestIndexer()
	// A schedule whose only boundary starts at time 5000; a block at time 1000
	// has no active blob config, which would panic eip4844.CalcBlobFee.
	cfg := blobparams.BuildChainConfig(9999999, []blobparams.ScheduleEntry{
		{ActivationTime: 5000, Target: 6, Max: 9, UpdateFraction: 5007716},
	})
	excess := uint64(100000)
	block := types.NewBlockWithHeader(&types.Header{
		Number:        big.NewInt(1),
		Time:          1000,
		ExcessBlobGas: &excess,
	})
	fee := idx.getBlobBaseFeeFromBlock(cfg, block)
	if fee == nil || fee.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("expected minimum fee 1 when no config is active, got %v", fee)
	}
}

func TestRefreshBlobScheduleFromNode_NilClientAndDB(t *testing.T) {
	// newTestIndexer has no eth client and no DB, so the refresh must degrade to
	// the compiled baseline without panicking.
	idx := newTestIndexer()
	idx.refreshBlobScheduleFromNode(context.Background())

	cfg := idx.chainConfig.Load()
	if cfg == nil {
		t.Fatal("chain config not set after refresh")
	}
	// Chain 42 is unknown to go-ethereum, so the synthetic baseline resolves
	// Cancun params (target 3 / max 6) at any positive timestamp.
	bp := blobparams.GetBlobParams(cfg, 1000)
	if bp.Target != 3 || bp.Max != 6 {
		t.Errorf("baseline Cancun = %d/%d, want 3/6", bp.Target, bp.Max)
	}
}
