package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

type aggregateContextTestKey struct{}

func TestAggregateWorkContextDropsCancellation(t *testing.T) {
	ctx := context.WithValue(context.Background(), aggregateContextTestKey{}, "request-value")
	ctx, cancel := context.WithCancel(ctx)
	cancel()

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody).WithContext(ctx)
	got := aggregateWorkContext(req)

	if err := got.Err(); err != nil {
		t.Fatalf("expected aggregate work context to ignore request cancellation, got %v", err)
	}
	if value := got.Value(aggregateContextTestKey{}); value != "request-value" {
		t.Fatalf("expected request context value to be preserved, got %v", value)
	}
}

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
	if resp.ErrorCode != errCodeInvalidRequest {
		t.Errorf("expected error_code 'invalid_request', got %q", resp.ErrorCode)
	}
}

func TestRespondError_MessageOverridesStatusCode(t *testing.T) {
	api := &API{}
	w := httptest.NewRecorder()

	// A 400 status whose message names a missing network should map to the
	// more specific network_not_found code rather than the generic 400 code.
	api.respondError(w, http.StatusBadRequest, "Network not found: foo")

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp.ErrorCode != errCodeNetworkNotFound {
		t.Errorf("expected error_code 'network_not_found', got %q", resp.ErrorCode)
	}
	if resp.Error != "Network not found: foo" {
		t.Errorf("human message should be unchanged, got %q", resp.Error)
	}
}

func TestRespondError_OmitsErrorCodeOnSuccess(t *testing.T) {
	// A success response (built directly, not via respondError) must never
	// carry an error_code, proving the omitempty tag holds.
	api := &API{}
	w := httptest.NewRecorder()
	api.respondJSON(w, http.StatusOK, Response{Success: true, Data: "ok"})

	if strings.Contains(w.Body.String(), "error_code") {
		t.Errorf("success response should omit error_code, got %q", w.Body.String())
	}
}

func TestErrorCodeFor(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		message string
		want    string
	}{
		{"bad request", http.StatusBadRequest, "invalid limit parameter", errCodeInvalidRequest},
		{"unauthorized", http.StatusUnauthorized, "missing api key", errCodeUnauthorized},
		{"forbidden", http.StatusForbidden, "dev mode disabled", errCodeForbidden},
		{"not found", http.StatusNotFound, "blob not found", errCodeNotFound},
		{"too many requests", http.StatusTooManyRequests, "slow down", errCodeRateLimited},
		{"service unavailable", http.StatusServiceUnavailable, "database busy", errCodeServiceUnavailable},
		{"internal error", http.StatusInternalServerError, "boom", errCodeInternal},
		{"unmapped status falls back", http.StatusTeapot, "short and stout", errCodeGeneric},
		{"network not found overrides 400", http.StatusBadRequest, "Network not found: mainnet", errCodeNetworkNotFound},
		{"network not found overrides 404", http.StatusNotFound, "network not found", errCodeNetworkNotFound},
		{"rate limit message overrides 500", http.StatusInternalServerError, "rate limit exceeded", errCodeRateLimited},
		{"case-insensitive message match", http.StatusBadRequest, "NETWORK NOT FOUND", errCodeNetworkNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorCodeFor(tt.status, tt.message); got != tt.want {
				t.Errorf("errorCodeFor(%d, %q) = %q, want %q", tt.status, tt.message, got, tt.want)
			}
		})
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
	if strings.Contains(w.Body.String(), "unsupported value") {
		t.Fatal("internal JSON encoding error should not be exposed in response body")
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500 on encode error, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "internal server error") {
		t.Errorf("expected safe error body, got %q", w.Body.String())
	}
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

func TestToBlobResponse_DerivesBlobCostFields(t *testing.T) {
	maxFeePerBlobGas := "5"
	blobGasUsed := int64(10)

	response := toBlobResponse(models.Blob{
		ChainID:           1,
		BaseFeePerBlobGas: "2",
		TotalCostWei:      "legacy-value",
		MaxFeePerBlobGas:  &maxFeePerBlobGas,
		BlobGasUsed:       &blobGasUsed,
	}, config.NetworkConfig{Name: "mainnet", ChainID: 1})

	if response.TotalCostWei != "legacy-value" {
		t.Fatalf("expected legacy total_cost_wei to be preserved, got %q", response.TotalCostWei)
	}
	if response.TotalCostWei != "legacy-value" {
		t.Fatalf("expected total_cost_wei=legacy-value, got %q", response.TotalCostWei)
	}
	if response.RealizedCostWei == nil || *response.RealizedCostWei != "20" {
		t.Fatalf("expected realized_cost_wei=20, got %v", response.RealizedCostWei)
	}
	if response.MaxCostWei == nil || *response.MaxCostWei != "50" {
		t.Fatalf("expected max_cost_wei=50, got %v", response.MaxCostWei)
	}
	if response.HeadroomWei == nil || *response.HeadroomWei != "30" {
		t.Fatalf("expected fee_cap_headroom_wei=30, got %v", response.HeadroomWei)
	}
	if response.HeadroomPercent == nil || *response.HeadroomPercent != "60.000000" {
		t.Fatalf("expected fee_cap_headroom_percent=60.000000, got %v", response.HeadroomPercent)
	}
}

func TestToBlobResponse_OmitsUnavailableBlobCostFields(t *testing.T) {
	maxFeePerBlobGas := "5"
	response := toBlobResponse(models.Blob{
		BaseFeePerBlobGas: "2",
		MaxFeePerBlobGas:  &maxFeePerBlobGas,
	}, config.NetworkConfig{Name: "mainnet", ChainID: 1})

	if response.RealizedCostWei != nil {
		t.Fatalf("expected realized_cost_wei to be omitted without blob_gas_used, got %q", *response.RealizedCostWei)
	}
	if response.MaxCostWei != nil {
		t.Fatalf("expected max_cost_wei to be omitted without blob_gas_used, got %q", *response.MaxCostWei)
	}
	if response.HeadroomWei != nil {
		t.Fatalf("expected fee_cap_headroom_wei to be omitted without blob_gas_used, got %q", *response.HeadroomWei)
	}
	if response.HeadroomPercent != nil {
		t.Fatalf("expected fee_cap_headroom_percent to be omitted without blob_gas_used, got %q", *response.HeadroomPercent)
	}

	blobGasUsed := int64(10)
	response = toBlobResponse(models.Blob{
		BaseFeePerBlobGas: "2",
		BlobGasUsed:       &blobGasUsed,
	}, config.NetworkConfig{Name: "mainnet", ChainID: 1})
	if response.RealizedCostWei == nil || *response.RealizedCostWei != "20" {
		t.Fatalf("expected realized_cost_wei=20 when base fee and blob gas are available, got %v", response.RealizedCostWei)
	}
	if response.MaxCostWei != nil || response.HeadroomWei != nil || response.HeadroomPercent != nil {
		t.Fatal("expected fee-cap fields to be omitted without max_fee_per_blob_gas")
	}
}

func TestIsDBTimeout(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline", context.DeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), true},
		{"statement timeout", &pq.Error{Code: "57014"}, true},
		{"other pq error", &pq.Error{Code: "23505"}, false},
		{"generic", errors.New("boom"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDBTimeout(tc.err); got != tc.want {
				t.Fatalf("isDBTimeout(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRespondAggregateError_TimeoutMapsTo503(t *testing.T) {
	a := newTestAPI()

	w := httptest.NewRecorder()
	a.respondAggregateError(w, context.DeadlineExceeded, "overloaded")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") != "5" {
		t.Fatalf("Retry-After = %q, want \"5\"", w.Header().Get("Retry-After"))
	}

	w = httptest.NewRecorder()
	a.respondAggregateError(w, errors.New("boom"), "failed")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") != "" {
		t.Fatalf("unexpected Retry-After on 500: %q", w.Header().Get("Retry-After"))
	}
}

func TestSetCacheControl(t *testing.T) {
	w := httptest.NewRecorder()
	setCacheControl(w, 15*time.Second, 30*time.Second)
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=15, s-maxage=30" {
		t.Fatalf("Cache-Control = %q", got)
	}
}
