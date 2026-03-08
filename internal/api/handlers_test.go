package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		name     string
		bytes    int64
		expected string
	}{
		{"zero bytes", 0, "0 B"},
		{"small bytes", 500, "500 B"},
		{"kilobytes", 1024, "1.00 KB"},
		{"megabytes", 1024 * 1024, "1.00 MB"},
		{"gigabytes", 1024 * 1024 * 1024, "1.00 GB"},
		{"terabytes", 1024 * 1024 * 1024 * 1024, "1.00 TB"},
		{"1.5 KB", 1536, "1.50 KB"},
		{"2.5 MB", 2621440, "2.50 MB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatBytes(tt.bytes)
			if result != tt.expected {
				t.Errorf("formatBytes(%d) = %s, want %s", tt.bytes, result, tt.expected)
			}
		})
	}
}

func TestGetMemoryUsage(t *testing.T) {
	result := getMemoryUsage()
	if result == "" {
		t.Error("expected non-empty memory usage string")
	}
	// Should end with " MB"
	if len(result) < 3 || result[len(result)-3:] != " MB" {
		t.Errorf("expected memory usage to end with ' MB', got %q", result)
	}
}

func TestRespondJSON(t *testing.T) {
	api := &API{}
	rr := httptest.NewRecorder()

	data := Response{
		Success: true,
		Data:    "test data",
	}

	api.respondJSON(rr, http.StatusOK, data)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rr.Code)
	}

	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", ct)
	}

	var resp Response
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Error("expected success to be true")
	}
}

func TestRespondError(t *testing.T) {
	api := &API{}
	rr := httptest.NewRecorder()

	api.respondError(rr, http.StatusBadRequest, "bad input")

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rr.Code)
	}

	var resp Response
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Success {
		t.Error("expected success to be false")
	}
	if resp.Error != "bad input" {
		t.Errorf("expected error 'bad input', got %q", resp.Error)
	}
}

func TestRespondJSON_DifferentStatusCodes(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"OK", http.StatusOK},
		{"Created", http.StatusCreated},
		{"NotFound", http.StatusNotFound},
		{"InternalServerError", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &API{}
			rr := httptest.NewRecorder()
			api.respondJSON(rr, tt.status, Response{Success: true})
			if rr.Code != tt.status {
				t.Errorf("expected status %d, got %d", tt.status, rr.Code)
			}
		})
	}
}

func TestMaxQueryLimit(t *testing.T) {
	if MaxQueryLimit != 100 {
		t.Errorf("expected MaxQueryLimit to be 100, got %d", MaxQueryLimit)
	}
}
