package attribution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

const testBlobListAddress = "0xabcdef1234567890abcdef1234567890abcdef12"

func TestGetUserAttributionForBlock_UsesBlobListClaims(t *testing.T) {
	svc := NewService(nil)
	toBlock := int64(199)
	svc.setClaims([]Claim{
		{
			ChainID:        1,
			Source:         blobListSource,
			Address:        testBlobListAddress,
			EntityID:       "old-base",
			Name:           "Old Base",
			Category:       "rollup",
			Role:           "batcher",
			Confidence:     "confirmed",
			Status:         "active",
			ValidFromBlock: 100,
			ValidToBlock:   &toBlock,
		},
		{
			ChainID:        1,
			Source:         blobListSource,
			Address:        testBlobListAddress,
			EntityID:       "base",
			Name:           "Base",
			Category:       "rollup",
			Role:           "batcher",
			Confidence:     "confirmed",
			Status:         "active",
			ValidFromBlock: 200,
		},
		{
			ChainID:        1,
			Source:         blobListSource,
			Address:        testBlobListAddress,
			EntityID:       "disputed",
			Name:           "Disputed",
			Category:       "rollup",
			Role:           "batcher",
			Confidence:     "confirmed",
			Status:         "disputed",
			ValidFromBlock: 200,
		},
	}, 250)

	if got := svc.GetUserAttributionForBlock(testBlobListAddress, 150); got != "Old Base" {
		t.Fatalf("expected historical attribution Old Base, got %q", got)
	}
	if got := svc.GetUserAttributionForBlock(testBlobListAddress, 250); got != "Base" {
		t.Fatalf("expected current historical attribution Base, got %q", got)
	}
	if got := svc.GetUserAttribution(testBlobListAddress); got != "Base" {
		t.Fatalf("expected current attribution Base, got %q", got)
	}
	if got := svc.GetUserAttributionForBlock(testBlobListAddress, 99); got != "" {
		t.Fatalf("expected no attribution before valid range, got %q", got)
	}
}

func TestRefreshBlobList_SyncsClaimsAndReattributesExistingBlobs(t *testing.T) {
	svc, mock := newMockService(t)
	artifact := fmt.Sprintf(`{
		"schema_version": 1,
		"submission_chain": "eip155-1",
		"addresses": {
			"%s": [{
				"entity_id": "base",
				"name": "Base",
				"category": "rollup",
				"role": "batcher",
				"confidence": "confirmed",
				"status": "active",
				"valid_from_block": 100,
				"valid_to_block": null,
				"chain_refs": [],
				"icon": null
			}]
		}
	}`, testBlobListAddress)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/eip155-1.json" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(artifact))
	}))
	t.Cleanup(server.Close)

	svc.ConfigureBlobList(BlobListConfig{
		Enabled:        true,
		BaseURL:        server.URL,
		RequestTimeout: time.Second,
	})

	rows := sqlmock.NewRows([]string{
		"chain_id", "source", "address", "entity_id", "name", "category", "role",
		"confidence", "status", "valid_from_block", "valid_to_block",
	}).AddRow(1, blobListSource, testBlobListAddress, "old-base", "Old Base", "rollup", "batcher", "confirmed", "active", 100, nil)

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT chain_id, source, address, entity_id, name, category, role, confidence, status, valid_from_block, valid_to_block").
		WithArgs(1, blobListSource).
		WillReturnRows(rows)
	mock.ExpectQuery("SELECT COALESCE\\(MAX\\(block_number\\), -1\\)").
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(123)))
	mock.ExpectExec("DELETE FROM blob_attribution_claims").
		WithArgs(1, blobListSource).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO blob_attribution_claims").
		WithArgs(1, blobListSource, testBlobListAddress, "base", "Base", "rollup", "batcher", "confirmed", "active", int64(100), nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO blob_users").
		WithArgs(1, testBlobListAddress, "Base", sqlmock.AnyArg(), "rollup").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("UPDATE blobs").
		WithArgs(1, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("UPDATE mempool_blobs").
		WithArgs(1, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE blobs").
		WithArgs("Base", 1, testBlobListAddress, int64(100)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("UPDATE mempool_blobs").
		WithArgs("Base", 1, testBlobListAddress).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := svc.RefreshBlobList(context.TODO()); err != nil {
		t.Fatalf("RefreshBlobList() error = %v", err)
	}

	if got := svc.GetUserAttributionForBlock(testBlobListAddress, 123); got != "Base" {
		t.Fatalf("expected synced attribution Base, got %q", got)
	}
	if got := svc.GetUserAttribution(testBlobListAddress); got != "Base" {
		t.Fatalf("expected current synced attribution Base, got %q", got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestBlobListConfig_WithDefaults(t *testing.T) {
	t.Run("fills missing defaults", func(t *testing.T) {
		got := BlobListConfig{}.withDefaults()
		if got.BaseURL != defaultBlobListBaseURL {
			t.Errorf("expected default base URL %q, got %q", defaultBlobListBaseURL, got.BaseURL)
		}
		if got.RequestTimeout != defaultRequestTimeout {
			t.Errorf("expected default timeout %v, got %v", defaultRequestTimeout, got.RequestTimeout)
		}
	})

	t.Run("preserves explicit values", func(t *testing.T) {
		cfg := BlobListConfig{
			Enabled:        true,
			BaseURL:        "https://example.invalid/base",
			RequestTimeout: 5 * time.Second,
		}
		got := cfg.withDefaults()
		if got.BaseURL != cfg.BaseURL {
			t.Errorf("expected base URL preserved, got %q", got.BaseURL)
		}
		if got.RequestTimeout != cfg.RequestTimeout {
			t.Errorf("expected timeout preserved, got %v", got.RequestTimeout)
		}
	})
}

func TestRefreshBlobList_DisabledIsNoop(t *testing.T) {
	svc := NewService(nil)
	if err := svc.RefreshBlobList(context.TODO()); err != nil {
		t.Fatalf("RefreshBlobList() on disabled service error = %v", err)
	}
}

func TestRefreshBlobList_PropagatesFetchError(t *testing.T) {
	svc := NewService(nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	svc.ConfigureBlobList(BlobListConfig{
		Enabled:        true,
		BaseURL:        server.URL,
		RequestTimeout: time.Second,
	})
	if err := svc.RefreshBlobList(context.TODO()); err == nil {
		t.Fatal("expected RefreshBlobList() to return error on non-200, non-404 status")
	}
}

func TestRefreshBlobList_NotFoundIsSkippedWithoutSync(t *testing.T) {
	// A chain the blob-list does not cover (404) must be a no-op: no DB access,
	// no error, and existing runtime claims left untouched.
	svc, mock := newMockService(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	// Seed an existing in-memory claim so we can prove the 404 path leaves it
	// intact rather than clobbering it via setClaims(nil).
	svc.setClaims([]Claim{{
		ChainID:        1,
		Source:         blobListSource,
		Address:        testBlobListAddress,
		EntityID:       "base",
		Name:           "Base",
		Category:       "rollup",
		Status:         claimStatusActive,
		Confidence:     claimConfidenceConfirmed,
		ValidFromBlock: 0,
	}}, -1)

	svc.ConfigureBlobList(BlobListConfig{
		Enabled:        true,
		BaseURL:        server.URL,
		RequestTimeout: time.Second,
	})

	if err := svc.RefreshBlobList(context.TODO()); err != nil {
		t.Fatalf("RefreshBlobList() on 404 error = %v, want nil", err)
	}
	// No DB expectations were registered, so any query would fail the mock.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	// Existing attribution must survive the skipped refresh.
	if got := svc.GetUserAttribution(testBlobListAddress); got != "Base" {
		t.Fatalf("expected existing attribution Base to survive 404, got %q", got)
	}
}

func TestFetchBlobListClaims_NotFoundReturnsSentinel(t *testing.T) {
	svc := NewService(nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	_, err := svc.fetchBlobListClaims(context.TODO(), BlobListConfig{
		BaseURL:        server.URL,
		RequestTimeout: time.Second,
	})
	if !errors.Is(err, errBlobListNotFound) {
		t.Fatalf("expected errBlobListNotFound, got %v", err)
	}
}

func TestFetchBlobListClaims_ErrorPaths(t *testing.T) {
	svc := NewService(nil)

	t.Run("invalid url", func(t *testing.T) {
		_, err := svc.fetchBlobListClaims(context.TODO(), BlobListConfig{
			BaseURL:        "http://%zz",
			RequestTimeout: time.Second,
		})
		if err == nil {
			t.Fatal("expected error building request")
		}
	})

	t.Run("transport error", func(t *testing.T) {
		_, err := svc.fetchBlobListClaims(context.TODO(), BlobListConfig{
			BaseURL:        "http://127.0.0.1:0",
			RequestTimeout: 100 * time.Millisecond,
		})
		if err == nil {
			t.Fatal("expected transport error")
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("{not-json"))
		}))
		t.Cleanup(server.Close)
		_, err := svc.fetchBlobListClaims(context.TODO(), BlobListConfig{
			BaseURL:        server.URL,
			RequestTimeout: time.Second,
		})
		if err == nil {
			t.Fatal("expected decode error")
		}
	})

	t.Run("wrong schema version", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"schema_version":2,"submission_chain":"eip155-1","addresses":{}}`))
		}))
		t.Cleanup(server.Close)
		_, err := svc.fetchBlobListClaims(context.TODO(), BlobListConfig{
			BaseURL:        server.URL,
			RequestTimeout: time.Second,
		})
		if err == nil {
			t.Fatal("expected schema version error")
		}
	})

	t.Run("wrong submission chain", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"schema_version":1,"submission_chain":"eip155-9999","addresses":{}}`))
		}))
		t.Cleanup(server.Close)
		_, err := svc.fetchBlobListClaims(context.TODO(), BlobListConfig{
			BaseURL:        server.URL,
			RequestTimeout: time.Second,
		})
		if err == nil {
			t.Fatal("expected submission chain mismatch error")
		}
	})

	t.Run("skips entries without name or entity id", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `{
				"schema_version": 1,
				"submission_chain": "eip155-1",
				"addresses": {
					"%s": [
						{"entity_id": "", "name": "Skip"},
						{"entity_id": "noname", "name": ""},
						{"entity_id": "ok", "name": "Keep", "valid_from_block": 0}
					]
				}
			}`, testBlobListAddress)
		}))
		t.Cleanup(server.Close)
		claims, err := svc.fetchBlobListClaims(context.TODO(), BlobListConfig{
			BaseURL:        server.URL,
			RequestTimeout: time.Second,
		})
		if err != nil {
			t.Fatalf("fetchBlobListClaims() error = %v", err)
		}
		if len(claims) != 1 || claims[0].EntityID != "ok" {
			t.Fatalf("expected single 'ok' claim, got %+v", claims)
		}
	})
}

func TestMatchesBlock(t *testing.T) {
	to := int64(200)
	cases := []struct {
		name  string
		claim Claim
		block int64
		want  bool
	}{
		{"disputed always false", Claim{Status: claimStatusDisputed, ValidFromBlock: 0}, 50, false},
		{"current with open range", Claim{Status: claimStatusActive, ValidFromBlock: 0}, -1, true},
		{"current with future open range", Claim{Status: claimStatusActive, ValidFromBlock: 100}, -1, false},
		{"current with closed range", Claim{Status: claimStatusActive, ValidFromBlock: 0, ValidToBlock: &to}, -1, false},
		{"before valid", Claim{Status: claimStatusActive, ValidFromBlock: 100}, 50, false},
		{"after valid", Claim{Status: claimStatusActive, ValidFromBlock: 0, ValidToBlock: &to}, 250, false},
		{"within range", Claim{Status: claimStatusActive, ValidFromBlock: 0, ValidToBlock: &to}, 150, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.claim.matchesBlock(tc.block); got != tc.want {
				t.Fatalf("matchesBlock(%d) = %v, want %v", tc.block, got, tc.want)
			}
		})
	}
}

func TestClaimSortLessAndScore(t *testing.T) {
	to := int64(100)

	t.Run("score ordering", func(t *testing.T) {
		active := Claim{Status: claimStatusActive, Confidence: claimConfidenceConfirmed}
		probable := Claim{Status: claimStatusActive, Confidence: claimConfidenceProbable}
		possible := Claim{Status: claimStatusActive, Confidence: claimConfidencePossible}
		disputed := Claim{Status: claimStatusDisputed, Confidence: claimConfidenceConfirmed}
		if !claimSortLess(active, probable) {
			t.Errorf("expected confirmed to sort before probable")
		}
		if !claimSortLess(probable, possible) {
			t.Errorf("expected probable to sort before possible")
		}
		if !claimSortLess(active, disputed) {
			t.Errorf("expected active to sort before disputed")
		}
		if claimScore(Claim{Status: "unknown", Confidence: "unknown"}) != 0 {
			t.Errorf("expected zero score for unrecognized status/confidence")
		}
	})

	t.Run("valid_from_block tiebreak", func(t *testing.T) {
		newer := Claim{Status: claimStatusActive, ValidFromBlock: 200}
		older := Claim{Status: claimStatusActive, ValidFromBlock: 100}
		if !claimSortLess(newer, older) {
			t.Errorf("expected newer ValidFromBlock to sort first")
		}
	})

	t.Run("open range beats closed", func(t *testing.T) {
		open := Claim{Status: claimStatusActive, ValidFromBlock: 100}
		closed := Claim{Status: claimStatusActive, ValidFromBlock: 100, ValidToBlock: &to}
		if !claimSortLess(open, closed) {
			t.Errorf("expected open range to sort first")
		}
		if claimSortLess(closed, open) {
			t.Errorf("expected closed range NOT to sort before open")
		}
	})

	t.Run("name tiebreak", func(t *testing.T) {
		a := Claim{Status: claimStatusActive, ValidFromBlock: 100, Name: "Alpha"}
		b := Claim{Status: claimStatusActive, ValidFromBlock: 100, Name: "Beta"}
		if !claimSortLess(a, b) {
			t.Errorf("expected Alpha to sort before Beta")
		}
	})
}

func TestSortClaimsForApplication(t *testing.T) {
	to := int64(150)
	claims := []Claim{
		{Name: "high", Status: claimStatusActive, Confidence: claimConfidenceConfirmed, ValidFromBlock: 200},
		{Name: "low", Status: claimStatusActive, Confidence: claimConfidencePossible, ValidFromBlock: 50, ValidToBlock: &to},
		{Name: "mid", Status: claimStatusActive, Confidence: claimConfidenceProbable, ValidFromBlock: 100},
	}
	sortClaimsForApplication(claims)
	if claims[0].Name != "low" || claims[1].Name != "mid" || claims[2].Name != "high" {
		t.Fatalf("unexpected application order: %+v", claims)
	}

	tied := []Claim{
		{Name: "later", Status: claimStatusActive, Confidence: claimConfidenceConfirmed, ValidFromBlock: 200},
		{Name: "earlier", Status: claimStatusActive, Confidence: claimConfidenceConfirmed, ValidFromBlock: 100},
	}
	sortClaimsForApplication(tied)
	if tied[0].Name != "earlier" {
		t.Fatalf("expected earlier ValidFromBlock first when scores tie, got %+v", tied)
	}
}

func TestUpdateBlobsForClaim(t *testing.T) {
	ctx := context.TODO()

	t.Run("open range update", func(t *testing.T) {
		svc, mock := newMockService(t)
		mock.ExpectExec("UPDATE blobs").
			WithArgs("Base", 1, testBlobListAddress, int64(100)).
			WillReturnResult(sqlmock.NewResult(0, 3))
		claim := Claim{Address: testBlobListAddress, Name: "Base", ValidFromBlock: 100}
		res, err := updateBlobsForClaim(ctx, svc.db, 1, claim)
		if err != nil {
			t.Fatalf("updateBlobsForClaim() error = %v", err)
		}
		if rowsAffected(res) != 3 {
			t.Fatalf("expected 3 rows affected, got %d", rowsAffected(res))
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("closed range update", func(t *testing.T) {
		svc, mock := newMockService(t)
		to := int64(200)
		mock.ExpectExec("UPDATE blobs").
			WithArgs("Old Base", 1, testBlobListAddress, int64(100), to).
			WillReturnResult(sqlmock.NewResult(0, 2))
		claim := Claim{Address: testBlobListAddress, Name: "Old Base", ValidFromBlock: 100, ValidToBlock: &to}
		res, err := updateBlobsForClaim(ctx, svc.db, 1, claim)
		if err != nil {
			t.Fatalf("updateBlobsForClaim() error = %v", err)
		}
		if rowsAffected(res) != 2 {
			t.Fatalf("expected 2 rows affected, got %d", rowsAffected(res))
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("open range error", func(t *testing.T) {
		svc, mock := newMockService(t)
		mock.ExpectExec("UPDATE blobs").
			WithArgs("Base", 1, testBlobListAddress, int64(100)).
			WillReturnError(assertiveError("boom"))
		claim := Claim{Address: testBlobListAddress, Name: "Base", ValidFromBlock: 100}
		if _, err := updateBlobsForClaim(ctx, svc.db, 1, claim); err == nil {
			t.Fatal("expected open range update error")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("closed range error", func(t *testing.T) {
		svc, mock := newMockService(t)
		to := int64(200)
		mock.ExpectExec("UPDATE blobs").
			WithArgs("Old Base", 1, testBlobListAddress, int64(100), to).
			WillReturnError(assertiveError("boom"))
		claim := Claim{Address: testBlobListAddress, Name: "Old Base", ValidFromBlock: 100, ValidToBlock: &to}
		if _, err := updateBlobsForClaim(ctx, svc.db, 1, claim); err == nil {
			t.Fatal("expected closed range update error")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

type errResult struct{}

func (errResult) LastInsertId() (int64, error) { return 0, errors.New("not supported") }
func (errResult) RowsAffected() (int64, error) { return 0, errors.New("rows affected error") }

func TestRowsAffected(t *testing.T) {
	if got := rowsAffected(nil); got != 0 {
		t.Errorf("expected 0 for nil result, got %d", got)
	}
	if got := rowsAffected(errResult{}); got != 0 {
		t.Errorf("expected 0 when RowsAffected errors, got %d", got)
	}
	if got := rowsAffected(sqlmock.NewResult(0, 5)); got != 5 {
		t.Errorf("expected 5 rows, got %d", got)
	}
}

func TestStartBlobListRefresh(t *testing.T) {
	t.Run("no-op when interval is zero", func(t *testing.T) {
		svc := NewService(nil)
		svc.startBlobListRefresh(context.TODO())
		if svc.refreshing {
			t.Fatal("expected no background goroutine when interval is zero")
		}
	})

	t.Run("starts goroutine that exits on ctx cancel", func(t *testing.T) {
		svc, _ := newMockService(t)
		svc.ConfigureBlobList(BlobListConfig{
			Enabled:         true,
			BaseURL:         "http://127.0.0.1:0",
			RefreshInterval: 10 * time.Millisecond,
			RequestTimeout:  10 * time.Millisecond,
		})
		ctx, cancel := context.WithCancel(context.Background())
		svc.startBlobListRefresh(ctx)

		// Already-running guard returns early without spawning another.
		svc.startBlobListRefresh(ctx)

		// Let the ticker fire at least once so the error branch is exercised.
		time.Sleep(50 * time.Millisecond)
		cancel()
		// Give the goroutine time to observe the cancellation.
		time.Sleep(20 * time.Millisecond)
	})
}

func TestSyncBlobListClaims_BeginError(t *testing.T) {
	svc, mock := newMockService(t)
	mock.ExpectBegin().WillReturnError(assertiveError("begin failed"))
	if _, err := svc.syncBlobListClaims(context.TODO(), nil); err == nil {
		t.Fatal("expected begin error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSyncBlobListClaims_SelectError(t *testing.T) {
	svc, mock := newMockService(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT chain_id, source, address").
		WithArgs(1, blobListSource).
		WillReturnError(assertiveError("select failed"))
	mock.ExpectRollback()
	if _, err := svc.syncBlobListClaims(context.TODO(), nil); err == nil {
		t.Fatal("expected select error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestClaimDescription(t *testing.T) {
	full := Claim{
		Source:     blobListSource,
		EntityID:   "base",
		Role:       "batcher",
		Confidence: "confirmed",
		Status:     "active",
	}.description()
	for _, want := range []string{"source=blob-list", "entity_id=base", "role=batcher", "confidence=confirmed", "status=active"} {
		if !contains(full, want) {
			t.Errorf("expected description to contain %q, got %q", want, full)
		}
	}

	minimal := Claim{Source: blobListSource, EntityID: "x"}.description()
	if contains(minimal, "role=") || contains(minimal, "confidence=") || contains(minimal, "status=") {
		t.Errorf("expected minimal description to omit empty fields, got %q", minimal)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

var _ sql.Result = errResult{}
