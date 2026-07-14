package db

import (
	"strings"
	"testing"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/blobparams"
)

func TestBuildBlobScheduleUpsert(t *testing.T) {
	entries := []blobparams.ScheduleEntry{
		{ActivationTime: 1000, Target: 6, Max: 9, UpdateFraction: 5007716},
		{ActivationTime: 2000, Target: 14, Max: 21, UpdateFraction: 11684671},
	}
	observed := time.Unix(1762955600, 0)
	query, args, err := buildBlobScheduleUpsert(560048, entries, "eth_config", observed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Shared args $1/$2/$3 then 4 per entry.
	if len(args) != 3+len(entries)*4 {
		t.Fatalf("arg count = %d, want %d", len(args), 3+len(entries)*4)
	}
	if args[0] != 560048 || args[1] != observed || args[2] != "eth_config" {
		t.Errorf("shared args = %v, want [560048 %v eth_config]", args[:3], observed)
	}
	// First entry's per-row args begin at index 3.
	if args[3] != uint64(1000) || args[4] != 6 || args[5] != 9 || args[6] != uint64(5007716) {
		t.Errorf("first entry args = %v", args[3:7])
	}
	// Highest placeholder used is $ (3 + 2*4) = $11 for the last entry's update_fraction.
	if !strings.Contains(query, "$11") {
		t.Errorf("query missing expected final placeholder $11:\n%s", query)
	}
	if !strings.Contains(query, "ON CONFLICT (chain_id, activation_time) DO UPDATE") {
		t.Errorf("query missing upsert clause:\n%s", query)
	}
}

func TestBuildBlobScheduleUpsert_RejectsDuplicateActivationTime(t *testing.T) {
	_, _, err := buildBlobScheduleUpsert(1, []blobparams.ScheduleEntry{
		{ActivationTime: 100, Target: 3, Max: 6, UpdateFraction: 1},
		{ActivationTime: 100, Target: 6, Max: 9, UpdateFraction: 2},
	}, "eth_config", time.Unix(1, 0))
	if err == nil {
		t.Fatal("expected error for duplicate activation_time in batch")
	}
}
