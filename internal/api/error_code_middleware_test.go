package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeErrResp(t *testing.T, body []byte) Response {
	t.Helper()
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestRateLimit429_IncludesErrorCode(t *testing.T) {
	rl := NewRateLimiter(1, 1) // 1 rps, burst 1 -> second request is rejected
	h := RateLimitMiddlewareWithResolver(rl, newClientIPResolver(nil))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	var last *httptest.ResponseRecorder
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.RemoteAddr = "9.9.9.9:1234"
		last = httptest.NewRecorder()
		h.ServeHTTP(last, req)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on the second request, got %d", last.Code)
	}
	if got := decodeErrResp(t, last.Body.Bytes()).ErrorCode; got != errCodeRateLimited {
		t.Errorf("rate-limit error_code = %q, want %q", got, errCodeRateLimited)
	}
}

func TestDevAuth401_IncludesErrorCode(t *testing.T) {
	h := DevAPIKeyMiddleware("secret")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody) // missing X-API-Key
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if got := decodeErrResp(t, w.Body.Bytes()).ErrorCode; got != errCodeUnauthorized {
		t.Errorf("unauthorized error_code = %q, want %q", got, errCodeUnauthorized)
	}
}

func TestMaxBytesError_IncludesErrorCode(t *testing.T) {
	w := httptest.NewRecorder()
	RespondMaxBytesError(w)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
	if got := decodeErrResp(t, w.Body.Bytes()).ErrorCode; got == "" {
		t.Error("expected a non-empty error_code on the 413 response")
	}
}

func TestDevModeDisabled404_IncludesErrorCode(t *testing.T) {
	h := DevModeMiddleware(false)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	if got := decodeErrResp(t, w.Body.Bytes()).ErrorCode; got != errCodeNotFound {
		t.Errorf("not-found error_code = %q, want %q", got, errCodeNotFound)
	}
}

func TestContentTypeJSON415_IncludesErrorCode(t *testing.T) {
	h := ContentTypeJSON(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("data"))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", w.Code)
	}
	if got := decodeErrResp(t, w.Body.Bytes()).ErrorCode; got == "" {
		t.Error("expected a non-empty error_code on the 415 response")
	}
}
