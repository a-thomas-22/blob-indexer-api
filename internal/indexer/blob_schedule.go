package indexer

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/blobparams"
	"github.com/a-thomas-22/blob-indexer-api/internal/ethereum"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// blobSchedulePollInterval is how often the indexer re-reads the node's
// eth_config. The blob schedule only changes when a fork is scheduled or
// activates — rare events — and eth_config advertises the next fork ahead of
// time, so an infrequent poll is enough to stay current.
const blobSchedulePollInterval = 30 * time.Minute

// scheduleEntriesFromEthConfig flattens an eth_config response into schedule
// entries, deduplicated by activation time. Forks without a blob schedule
// (pre-4844) are skipped, as are entries whose parameters fail validation —
// the node is untrusted input and a bad value (notably update_fraction 0, which
// divides by zero in go-ethereum's fake-exponential and panics) must never
// reach the fork-math or the database.
func scheduleEntriesFromEthConfig(network string, cfg *ethereum.EthConfig) []blobparams.ScheduleEntry {
	if cfg == nil {
		return nil
	}
	byTime := make(map[uint64]blobparams.ScheduleEntry)
	for _, f := range []*ethereum.EthConfigFork{cfg.Current, cfg.Last, cfg.Next} {
		if f == nil || f.BlobSchedule == nil {
			continue
		}
		e := blobparams.ScheduleEntry{
			ActivationTime: f.ActivationTime,
			Target:         f.BlobSchedule.Target,
			Max:            f.BlobSchedule.Max,
			UpdateFraction: f.BlobSchedule.UpdateFraction,
		}
		if !validScheduleEntry(e) {
			logger.Warn("Skipping invalid blob schedule entry from eth_config",
				zap.String("network", network),
				zap.Uint64("activation_time", e.ActivationTime),
				zap.Int("target", e.Target),
				zap.Int("max", e.Max),
				zap.Uint64("update_fraction", e.UpdateFraction))
			continue
		}
		byTime[e.ActivationTime] = e
	}
	entries := make([]blobparams.ScheduleEntry, 0, len(byTime))
	for _, e := range byTime {
		entries = append(entries, e)
	}
	return entries
}

// validScheduleEntry rejects blob parameters that would corrupt the fork math:
// a non-positive target/max, max below target, or a zero update fraction (which
// panics go-ethereum's blob-fee fake-exponential with a zero denominator).
func validScheduleEntry(e blobparams.ScheduleEntry) bool {
	return e.Target > 0 && e.Max >= e.Target && e.UpdateFraction > 0
}

// refreshBlobScheduleFromNode reads the node's eth_config, persists any learned
// fork boundaries, then rebuilds the active chain config from the full persisted
// schedule. eth_config failures (e.g. a node that predates EIP-7910) are logged
// and tolerated: the rebuild still runs against whatever is already persisted,
// falling back to the compiled chain config when nothing has been learned.
func (i *Indexer) refreshBlobScheduleFromNode(ctx context.Context) {
	if i.ethClient != nil {
		if cfg, err := i.ethClient.FetchEthConfig(ctx); err != nil {
			logger.Debug("eth_config unavailable; using persisted/compiled blob schedule",
				zap.String("network", i.network.Name),
				zap.Error(err))
		} else if entries := scheduleEntriesFromEthConfig(i.network.Name, cfg); len(entries) > 0 && i.db != nil {
			// Reconcile rather than plain-upsert: a future fork the node has
			// rescheduled or canceled must not linger as a stale boundary that
			// still activates at its old timestamp. Past/current boundaries are
			// preserved (they already happened); only not-yet-active learned
			// boundaries absent from this poll are dropped.
			now := time.Now()
			if err := i.db.ReconcileFutureBlobSchedule(ctx, i.network.ChainID, entries, "eth_config", now); err != nil {
				logger.Error("Failed to persist learned blob schedule",
					zap.String("network", i.network.Name),
					zap.Error(err))
			}
		}
	}
	i.rebuildChainConfigFromDB(ctx)
}

// rebuildChainConfigFromDB loads the persisted schedule and atomically swaps in
// a chain config that reflects it. On error it leaves the current config in
// place (already seeded with the compiled baseline at construction).
func (i *Indexer) rebuildChainConfigFromDB(ctx context.Context) {
	var learned []blobparams.ScheduleEntry
	if i.db != nil {
		var err error
		if learned, err = i.db.GetBlobSchedule(ctx, i.network.ChainID); err != nil {
			logger.Error("Failed to load blob schedule; keeping current chain config",
				zap.String("network", i.network.Name),
				zap.Error(err))
			return
		}
	}
	cfg := blobparams.BuildChainConfig(i.network.ChainID, learned)
	prev := i.chainConfig.Swap(cfg)
	if prev == nil || blobparams.ForkName(prev, uint64(time.Now().Unix())) != blobparams.ForkName(cfg, uint64(time.Now().Unix())) {
		logger.Info("Active blob schedule updated",
			zap.String("network", i.network.Name),
			zap.Int("learned_boundaries", len(learned)),
			zap.String("current_fork", blobparams.ForkName(cfg, uint64(time.Now().Unix()))))
	}
}

// runBlobSchedulePoller periodically re-reads the node's eth_config so newly
// scheduled forks are picked up without a restart.
func (i *Indexer) runBlobSchedulePoller() {
	ticker := time.NewTicker(blobSchedulePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-i.ctx.Done():
			return
		case <-ticker.C:
			i.refreshBlobScheduleFromNode(i.ctx)
		}
	}
}
