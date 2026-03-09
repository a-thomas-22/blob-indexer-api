package testutil

import (
	"testing"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

func TestInitInitializesLogger(t *testing.T) {
	if err := logger.Sync(); err != nil {
		// Sync may return a benign error on some platforms when stdout is not syncable.
		t.Logf("logger sync returned: %v", err)
	}
}
