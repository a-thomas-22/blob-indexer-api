package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMaxBytesMiddleware_AllowsSmallBody(t *testing.T) {
	handler := MaxBytesMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the full body to trigger MaxBytesReader validation
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("unexpected error reading body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))

	body := strings.NewReader(`{"key": "value"}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rr.Code)
	}
}

func TestMaxBytesMiddleware_RejectsOversizedBody(t *testing.T) {
	handler := MaxBytesMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			// The handler detects the MaxBytesError and returns 413
			if isMaxBytesError(err) {
				RespondMaxBytesError(w)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	// Create a body that exceeds the 1MB limit
	oversized := bytes.Repeat([]byte("x"), MaxRequestBodySize+1)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(oversized))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected status 413, got %d", rr.Code)
	}

	var resp Response
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Success {
		t.Error("expected Success to be false")
	}
	if !strings.Contains(resp.Error, "too large") {
		t.Errorf("expected error message to mention 'too large', got: %s", resp.Error)
	}
}

func TestMaxBytesMiddleware_ExactLimitAllowed(t *testing.T) {
	handler := MaxBytesMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			if isMaxBytesError(err) {
				RespondMaxBytesError(w)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	// Body exactly at the limit should be allowed
	exactSize := bytes.Repeat([]byte("x"), MaxRequestBodySize)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(exactSize))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200 for body at exact limit, got %d", rr.Code)
	}
}

func TestIsMaxBytesError(t *testing.T) {
	// http.MaxBytesError is only produced by MaxBytesReader.
	// We can trigger it by wrapping a body and trying to read past the limit.
	limited := http.MaxBytesReader(nil, io.NopCloser(strings.NewReader("hello world")), 1)
	_, err := io.ReadAll(limited)
	if err == nil {
		t.Fatal("expected an error from MaxBytesReader")
	}
	if !isMaxBytesError(err) {
		t.Errorf("expected isMaxBytesError to return true, got false for error: %v", err)
	}
}

func TestRespondMaxBytesError(t *testing.T) {
	rr := httptest.NewRecorder()
	RespondMaxBytesError(rr)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected status 413, got %d", rr.Code)
	}

	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", ct)
	}

	var resp Response
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Success {
		t.Error("expected Success to be false")
	}
	if !strings.Contains(resp.Error, "1MB") {
		t.Errorf("expected error message to mention limit, got: %s", resp.Error)
	}
}
