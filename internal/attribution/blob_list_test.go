package attribution

import (
	"context"
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
			NetworkID:      1,
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
			NetworkID:      1,
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
			NetworkID:      1,
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
	})

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
		"network_id", "source", "address", "entity_id", "name", "category", "role",
		"confidence", "status", "valid_from_block", "valid_to_block",
	}).AddRow(1, blobListSource, testBlobListAddress, "old-base", "Old Base", "rollup", "batcher", "confirmed", "active", 100, nil)

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT network_id, source, address, entity_id, name, category, role, confidence, status, valid_from_block, valid_to_block").
		WithArgs(1, blobListSource).
		WillReturnRows(rows)
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
	mock.ExpectExec("UPDATE blobs").
		WithArgs("Base", 1, testBlobListAddress, int64(100)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("UPDATE blobs").
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
