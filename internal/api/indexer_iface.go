package api

import (
	"context"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

// IndexerProvider abstracts the indexer methods required by the API layer.
// *indexer.Indexer satisfies this interface.
type IndexerProvider interface {
	GetNetworkInfo() config.NetworkConfig
	GetLastIndexedBlock() uint64
	GetCurrentBlock(ctx context.Context) (uint64, error)
	GetBlobCounts(ctx context.Context) (confirmedCount, pendingCount int, err error)
	GetTopBlobUsers(ctx context.Context, limit int) ([]models.BlobUserStats, error)
	Reindex(startBlock, endBlock uint64) error
}
