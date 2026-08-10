//go:build integration

package attribution

// End-to-end coverage for the blob_users reconcile in syncBlobListClaims
// against a real Postgres (TEST_DB_URL). The sqlmock unit tests never parse
// SQL, so this is the only check that the ownership marker predicate and the
// address-array reconcile actually delete the rows they are meant to — and
// only those rows.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/testdb"
)

const (
	integrationRollupAddress  = "0x1111111111111111111111111111111111111111"
	integrationManualAddress  = "0x2222222222222222222222222222222222222222"
	integrationRetiredAddress = "0x3333333333333333333333333333333333333333"
)

// TestRefreshBlobList_ExpiredThenDroppedClaimReleasesBlobUser walks the
// sequence that leaked a row: a claim is current at the tip, the chain passes
// its valid_to_block with the artifact unchanged, and the artifact later drops
// the claim. Diffing the previous projection against the current one missed
// the middle step (both are evaluated at the new tip, so an expired address is
// in neither), leaving /search and the /users name fallback serving a name the
// registry had stopped standing behind.
func TestRefreshBlobList_ExpiredThenDroppedClaimReleasesBlobUser(t *testing.T) {
	// This test resets its schema, so it runs on this package's dedicated
	// database rather than TEST_DB_URL itself — parallel test binaries from
	// other packages must never see the reset.
	url := testdb.URL(t, "attribution")
	sqlxDB, err := sqlx.Connect("postgres", url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sqlxDB.Close()

	for _, stmt := range []string{
		"DROP SCHEMA IF EXISTS public CASCADE",
		"CREATE SCHEMA public",
		"GRANT ALL ON SCHEMA public TO PUBLIC",
	} {
		if _, err := sqlxDB.Exec(stmt); err != nil {
			t.Fatalf("reset schema (%s): %v", stmt, err)
		}
	}
	if err := db.RunMigrations(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// Migration 000001 already seeds mainnet; this is a no-op there and the
	// row the foreign-chain assertions need otherwise.
	if _, err := sqlxDB.Exec(`
		INSERT INTO networks (chain_id, name, start_block) VALUES (1, 'mainnet', '0')
		ON CONFLICT (chain_id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed network: %v", err)
	}
	seedBlob(t, sqlxDB, 100, now)

	// The artifact the service fetches, swapped between refreshes. atomic.Value
	// because httptest serves it from the server's own goroutine.
	var artifact atomic.Value
	artifact.Store(blobListArtifactJSON(`{
		"entity_id": "base",
		"name": "Base",
		"category": "rollup",
		"role": "batcher",
		"confidence": "confirmed",
		"status": "active",
		"valid_from_block": 100,
		"valid_to_block": 150
	}`))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(artifact.Load().(string)))
	}))
	t.Cleanup(server.Close)

	svc := NewService(&db.DB{DB: sqlxDB}, 1)
	svc.ConfigureBlobList(BlobListConfig{
		Enabled:        true,
		BaseURL:        server.URL,
		RequestTimeout: 5 * time.Second,
	})

	// A hand-added user carries no ownership marker and must survive every
	// reconcile — the sync owns its own rows, not the whole table.
	if err := svc.AddKnownUser(ctx, integrationManualAddress, "Manual Entry", "hand-added", "infra"); err != nil {
		t.Fatalf("AddKnownUser(): %v", err)
	}

	// Stage 1: the claim is current at the tip (block 100), so its row exists.
	if err := svc.RefreshBlobList(ctx); err != nil {
		t.Fatalf("RefreshBlobList() with current claim: %v", err)
	}
	name, description, ok := blobUser(t, sqlxDB, integrationRollupAddress)
	if !ok || name != "Base" {
		t.Fatalf("expected a blob_users row named Base for the current claim, got %q (present=%v)", name, ok)
	}
	if !strings.HasPrefix(description, blobListUserDescriptionPrefix) {
		t.Fatalf("expected the synced row to carry the ownership marker %q, got description %q",
			blobListUserDescriptionPrefix, description)
	}

	// Stage 2: the chain passes valid_to_block=150 with the artifact unchanged.
	// blob_users is the current-only projection, so the row must go even though
	// the claim set did not change.
	seedBlob(t, sqlxDB, 200, now.Add(time.Hour))
	if err := svc.RefreshBlobList(ctx); err != nil {
		t.Fatalf("RefreshBlobList() after claim expiry: %v", err)
	}
	if name, _, ok := blobUser(t, sqlxDB, integrationRollupAddress); ok {
		t.Fatalf("expected the expired claim's blob_users row to be deleted, still found %q", name)
	}
	assertManualUserSurvives(t, sqlxDB)

	// Stage 3: the artifact drops the claim entirely. The row stays gone, and
	// so does the claim backing the /search union.
	artifact.Store(blobListArtifactJSON(""))
	if err := svc.RefreshBlobList(ctx); err != nil {
		t.Fatalf("RefreshBlobList() after claim drop: %v", err)
	}
	if name, _, ok := blobUser(t, sqlxDB, integrationRollupAddress); ok {
		t.Fatalf("expected no blob_users row after the claim was dropped, found %q", name)
	}
	var claimCount int
	if err := sqlxDB.Get(&claimCount, `
		SELECT COUNT(*) FROM blob_attribution_claims WHERE chain_id = 1 AND address = $1
	`, integrationRollupAddress); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if claimCount != 0 {
		t.Fatalf("expected the dropped claim to be gone, got %d rows", claimCount)
	}
	assertManualUserSurvives(t, sqlxDB)
}

// TestSyncBlobListClaims_KeepsCurrentAndForeignChainBlobUsers pins the other
// side of the reconcile: it must not reach rows that are still current, nor
// rows belonging to another chain.
func TestSyncBlobListClaims_KeepsCurrentAndForeignChainBlobUsers(t *testing.T) {
	url := testdb.URL(t, "attribution")
	sqlxDB, err := sqlx.Connect("postgres", url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sqlxDB.Close()

	for _, stmt := range []string{
		"DROP SCHEMA IF EXISTS public CASCADE",
		"CREATE SCHEMA public",
		"GRANT ALL ON SCHEMA public TO PUBLIC",
	} {
		if _, err := sqlxDB.Exec(stmt); err != nil {
			t.Fatalf("reset schema (%s): %v", stmt, err)
		}
	}
	if err := db.RunMigrations(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	if _, err := sqlxDB.Exec(`
		INSERT INTO networks (chain_id, name, start_block)
		VALUES (1, 'mainnet', '0'), (11155111, 'sepolia', '0')
		ON CONFLICT (chain_id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed networks: %v", err)
	}
	seedBlob(t, sqlxDB, 100, now)

	// A marker-carrying row on another chain: same address, different chain_id.
	if _, err := sqlxDB.Exec(`
		INSERT INTO blob_users (chain_id, address, name, description, category, first_seen, last_seen)
		VALUES (11155111, $1, 'Sepolia Base', $2, 'rollup', $3, $3)
	`, integrationRollupAddress, blobListUserDescriptionPrefix+" entity_id=base", now); err != nil {
		t.Fatalf("seed foreign-chain blob user: %v", err)
	}

	svc := NewService(&db.DB{DB: sqlxDB}, 1)
	toBlock := int64(150)
	claims := []Claim{
		{
			ChainID: 1, Source: blobListSource, Address: integrationRollupAddress,
			EntityID: "base", Name: "Base", Category: "rollup", Role: "batcher",
			Confidence: claimConfidenceConfirmed, Status: claimStatusActive, ValidFromBlock: 50,
		},
		{
			ChainID: 1, Source: blobListSource, Address: integrationRetiredAddress,
			EntityID: "retired", Name: "Retired", Category: "rollup", Role: "batcher",
			Confidence: claimConfidenceConfirmed, Status: claimStatusActive,
			ValidFromBlock: 0, ValidToBlock: &toBlock,
		},
	}

	// Both claims cover the tip (block 100), so the first sync writes a row for
	// each.
	if _, err := svc.syncBlobListClaims(ctx, claims); err != nil {
		t.Fatalf("syncBlobListClaims() seed: %v", err)
	}
	for _, address := range []string{integrationRollupAddress, integrationRetiredAddress} {
		if _, _, ok := blobUser(t, sqlxDB, address); !ok {
			t.Fatalf("expected a blob_users row for current claim %s", address)
		}
	}

	// Move the tip past the closed claim and re-sync the same artifact.
	seedBlob(t, sqlxDB, 200, now.Add(time.Hour))
	stats, err := svc.syncBlobListClaims(ctx, claims)
	if err != nil {
		t.Fatalf("syncBlobListClaims() after expiry: %v", err)
	}
	if stats.BlobUsersDeleted != 1 {
		t.Fatalf("expected exactly the expired row to be deleted, got %d deletions", stats.BlobUsersDeleted)
	}
	if name, _, ok := blobUser(t, sqlxDB, integrationRollupAddress); !ok || name != "Base" {
		t.Fatalf("expected the still-current row to survive, got %q (present=%v)", name, ok)
	}
	if _, _, ok := blobUser(t, sqlxDB, integrationRetiredAddress); ok {
		t.Fatalf("expected the expired row to be deleted")
	}

	var foreign int
	if err := sqlxDB.Get(&foreign, `
		SELECT COUNT(*) FROM blob_users WHERE chain_id = 11155111 AND address = $1
	`, integrationRollupAddress); err != nil {
		t.Fatalf("count foreign-chain rows: %v", err)
	}
	if foreign != 1 {
		t.Fatalf("expected the other chain's row to be untouched, got %d rows", foreign)
	}
}

func blobListArtifactJSON(claim string) string {
	if claim == "" {
		return `{"schema_version":1,"submission_chain":"eip155-1","addresses":{}}`
	}
	return fmt.Sprintf(`{
		"schema_version": 1,
		"submission_chain": "eip155-1",
		"addresses": {"%s": [%s]}
	}`, integrationRollupAddress, claim)
}

func seedBlob(t *testing.T, sqlxDB *sqlx.DB, blockNumber int64, ts time.Time) {
	t.Helper()
	if _, err := sqlxDB.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei, timestamp
		) VALUES (1, $1, 0, $2, $3, 'Base', 131072, 10, 2, 100, $4)
	`, blockNumber, fmt.Sprintf("0xtx%d", blockNumber), integrationRollupAddress, ts); err != nil {
		t.Fatalf("seed blob at block %d: %v", blockNumber, err)
	}
}

func blobUser(t *testing.T, sqlxDB *sqlx.DB, address string) (name, description string, found bool) {
	t.Helper()
	var row struct {
		Name        string         `db:"name"`
		Description sql.NullString `db:"description"`
	}
	err := sqlxDB.Get(&row, `
		SELECT name, description FROM blob_users WHERE chain_id = 1 AND address = $1
	`, address)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false
	}
	if err != nil {
		t.Fatalf("read blob_users row for %s: %v", address, err)
	}
	return row.Name, row.Description.String, true
}

func assertManualUserSurvives(t *testing.T, sqlxDB *sqlx.DB) {
	t.Helper()
	name, _, ok := blobUser(t, sqlxDB, integrationManualAddress)
	if !ok || name != "Manual Entry" {
		t.Fatalf("expected the hand-added blob_users row to survive the reconcile, got %q (present=%v)", name, ok)
	}
}
