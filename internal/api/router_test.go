package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a-thomas-22/blob-indexer-api/internal/indexer"
)

func TestAPIError(t *testing.T) {
	err := NewAPIError("test error", http.StatusNotFound)

	if err.Error() != "test error" {
		t.Errorf("expected error message 'test error', got %q", err.Error())
	}

	if err.Status != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, err.Status)
	}
}

func TestNewAPIError(t *testing.T) {
	tests := []struct {
		name    string
		message string
		status  int
	}{
		{"not found", "Not found", http.StatusNotFound},
		{"bad request", "Bad request", http.StatusBadRequest},
		{"internal error", "Internal error", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewAPIError(tt.message, tt.status)
			if err.Message != tt.message {
				t.Errorf("expected message %q, got %q", tt.message, err.Message)
			}
			if err.Status != tt.status {
				t.Errorf("expected status %d, got %d", tt.status, err.Status)
			}
			if err.Error() != tt.message {
				t.Errorf("Error() = %q, want %q", err.Error(), tt.message)
			}
		})
	}
}

func TestErrNetworkNotFound(t *testing.T) {
	if ErrNetworkNotFound.Error() != "Network not found" {
		t.Errorf("expected 'Network not found', got %q", ErrNetworkNotFound.Error())
	}
	if ErrNetworkNotFound.Status != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, ErrNetworkNotFound.Status)
	}
}

func TestErrNoNetworksAvailable(t *testing.T) {
	if ErrNoNetworksAvailable.Error() != "No networks available" {
		t.Errorf("expected 'No networks available', got %q", ErrNoNetworksAvailable.Error())
	}
	if ErrNoNetworksAvailable.Status != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d", http.StatusInternalServerError, ErrNoNetworksAvailable.Status)
	}
}

func TestGetNetworkFromRequest_NoIndexers(t *testing.T) {
	api := &API{
		indexers: make(map[int]*indexer.Indexer),
	}

	req := httptest.NewRequest(http.MethodGet, "/?network=mainnet", nil)
	_, err := api.getNetworkFromRequest(req)
	if err == nil {
		t.Error("expected error when no indexers available")
	}
}

func TestGetNetworkFromRequest_NoNetworkParam(t *testing.T) {
	api := &API{
		indexers: make(map[int]*indexer.Indexer),
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := api.getNetworkFromRequest(req)
	if err == nil {
		t.Error("expected error when no indexers available and no network param")
	}
}

func TestGetNetworkFromRequest_NetworkNotFound(t *testing.T) {
	api := &API{
		indexers: make(map[int]*indexer.Indexer),
	}

	req := httptest.NewRequest(http.MethodGet, "/?network=nonexistent", nil)
	_, err := api.getNetworkFromRequest(req)
	if err == nil {
		t.Error("expected error for nonexistent network")
	}
}
