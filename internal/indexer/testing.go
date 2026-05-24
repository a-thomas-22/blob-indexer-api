package indexer

import (
	"context"
	"sync"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/attribution"
	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
)

// NewForTest creates an Indexer suitable for unit tests. It does not require
// an Ethereum client and is safe to use in handler-level tests that only call
// GetNetworkInfo, GetLastIndexedBlock, GetTopBlobUsers, and GetBlobCounts.
func NewForTest(database *db.DB, cfg *config.Config, network config.NetworkConfig, lastBlock uint64) *Indexer {
	ctx, cancel := context.WithCancel(context.Background())

	var attrSvc *attribution.Service
	if database != nil {
		attrSvc = attribution.NewService(database, network.ChainID)
	}

	return &Indexer{
		db:                   database,
		attribution:          attrSvc,
		config:               cfg,
		network:              network,
		lastIndexedBlock:     lastBlock,
		ctx:                  ctx,
		cancel:               cancel,
		failedBlocks:         make(map[uint64]int),
		failedBlockNextRetry: make(map[uint64]time.Time),
		mu:                   sync.Mutex{},
	}
}
