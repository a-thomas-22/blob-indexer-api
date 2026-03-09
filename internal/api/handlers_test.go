package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{1048576, "1.00 MB"},
		{1073741824, "1.00 GB"},
	}

	for _, tt := range tests {
		result := formatBytes(tt.input)
		if result != tt.expected {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestGetMemoryUsage(t *testing.T) {
	result := getMemoryUsage()
	if !strings.HasSuffix(result, " MB") {
		t.Errorf("expected memory usage to end with ' MB', got %q", result)
	}
}

func TestRespondJSON(t *testing.T) {
	api := &API{}
	w := httptest.NewRecorder()

	data := Response{Success: true, Data: "hello"}
	api.respondJSON(w, http.StatusOK, data)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
}

func TestRespondError(t *testing.T) {
	api := &API{}
	w := httptest.NewRecorder()

	api.respondError(w, http.StatusBadRequest, "bad input")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp.Success {
		t.Error("expected Success=false")
	}
	if resp.Error != "bad input" {
		t.Errorf("expected error 'bad input', got %q", resp.Error)
	}
}

func TestRespondSuccess(t *testing.T) {
	api := &API{}
	w := httptest.NewRecorder()

	api.respondSuccess(w, "test-data")

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
	if resp.Data != "test-data" {
		t.Errorf("expected Data=%q, got %v", "test-data", resp.Data)
	}
}

func TestAPIError(t *testing.T) {
	err := NewAPIError("not found", http.StatusNotFound)
	if err.Error() != "not found" {
		t.Errorf("expected 'not found', got %q", err.Error())
	}
	if err.Status != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, err.Status)
	}
}

func TestMaxQueryLimit(t *testing.T) {
	if MaxQueryLimit != 100 {
		t.Errorf("expected MaxQueryLimit=100, got %d", MaxQueryLimit)
	}
}

func TestRespondJSON_EncodeError(t *testing.T) {
	api := &API{}
	w := httptest.NewRecorder()

	// math.Inf causes json.Encode to fail
	api.respondJSON(w, http.StatusOK, map[string]interface{}{
		"bad": math.Inf(1),
	})
	// Should not panic; the error handler writes an error response
}

func TestFormatBytes_LargeValues(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{1099511627776, "1.00 TB"},
	}
	for _, tt := range tests {
		result := formatBytes(tt.input)
		if result != tt.expected {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestErrNetworkNotFound(t *testing.T) {
	if ErrNetworkNotFound.Error() != "Network not found" {
		t.Errorf("unexpected error message: %s", ErrNetworkNotFound.Error())
	}
	if ErrNetworkNotFound.Status != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", ErrNetworkNotFound.Status)
	}
}

func TestErrNoNetworksAvailable(t *testing.T) {
	if ErrNoNetworksAvailable.Error() != "No networks available" {
		t.Errorf("unexpected error message: %s", ErrNoNetworksAvailable.Error())
	}
}
