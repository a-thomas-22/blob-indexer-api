package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// okHandler is a simple handler that writes a 200 OK response.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success":true}`))
})

func TestContentTypeJSON_PostWithValidContentType(t *testing.T) {
	handler := ContentTypeJSON(okHandler)

	body := strings.NewReader(`{"key":"value"}`)
	req := httptest.NewRequest(http.MethodPost, "/test", body)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rr.Code)
	}
}

func TestContentTypeJSON_PostWithCharsetParam(t *testing.T) {
	handler := ContentTypeJSON(okHandler)

	body := strings.NewReader(`{"key":"value"}`)
	req := httptest.NewRequest(http.MethodPost, "/test", body)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rr.Code)
	}
}

func TestContentTypeJSON_PostWithWrongContentType(t *testing.T) {
	handler := ContentTypeJSON(okHandler)

	body := strings.NewReader(`key=value`)
	req := httptest.NewRequest(http.MethodPost, "/test", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected status 415, got %d", rr.Code)
	}

	var resp Response
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Success {
		t.Error("expected success to be false")
	}
	if resp.Error != "Content-Type must be application/json" {
		t.Errorf("unexpected error message: %s", resp.Error)
	}
}

func TestContentTypeJSON_PostWithTextPlainContentType(t *testing.T) {
	handler := ContentTypeJSON(okHandler)

	body := strings.NewReader(`hello`)
	req := httptest.NewRequest(http.MethodPost, "/test", body)
	req.Header.Set("Content-Type", "text/plain")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected status 415, got %d", rr.Code)
	}
}

func TestContentTypeJSON_PostWithNoContentType(t *testing.T) {
	// Requests without a Content-Type header should be allowed through
	// (some endpoints may not require a body).
	handler := ContentTypeJSON(okHandler)

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	// Explicitly ensure no Content-Type header
	req.Header.Del("Content-Type")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200 for empty body/no content-type, got %d", rr.Code)
	}
}

func TestContentTypeJSON_PostWithEmptyBody(t *testing.T) {
	// Requests with Content-Length 0 should be allowed through even with
	// a wrong Content-Type, since there is effectively no body.
	handler := ContentTypeJSON(okHandler)

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(""))
	req.Header.Set("Content-Type", "text/plain")
	req.ContentLength = 0

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200 for empty body, got %d", rr.Code)
	}
}

func TestContentTypeJSON_GetRequestIgnored(t *testing.T) {
	handler := ContentTypeJSON(okHandler)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	// Even if someone sets a weird Content-Type on GET, it should be ignored.
	req.Header.Set("Content-Type", "text/plain")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200 for GET request, got %d", rr.Code)
	}
}

func TestContentTypeJSON_DeleteRequestIgnored(t *testing.T) {
	handler := ContentTypeJSON(okHandler)

	req := httptest.NewRequest(http.MethodDelete, "/test", nil)
	req.Header.Set("Content-Type", "text/plain")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200 for DELETE request, got %d", rr.Code)
	}
}

func TestContentTypeJSON_PutWithWrongContentType(t *testing.T) {
	handler := ContentTypeJSON(okHandler)

	body := strings.NewReader(`key=value`)
	req := httptest.NewRequest(http.MethodPut, "/test", body)
	req.Header.Set("Content-Type", "application/xml")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected status 415 for PUT with wrong content-type, got %d", rr.Code)
	}
}

func TestContentTypeJSON_PatchWithWrongContentType(t *testing.T) {
	handler := ContentTypeJSON(okHandler)

	body := strings.NewReader(`key=value`)
	req := httptest.NewRequest(http.MethodPatch, "/test", body)
	req.Header.Set("Content-Type", "multipart/form-data")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected status 415 for PATCH with wrong content-type, got %d", rr.Code)
	}
}

func TestContentTypeJSON_PutWithValidContentType(t *testing.T) {
	handler := ContentTypeJSON(okHandler)

	body := strings.NewReader(`{"key":"value"}`)
	req := httptest.NewRequest(http.MethodPut, "/test", body)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200 for PUT with valid content-type, got %d", rr.Code)
	}
}

func TestContentTypeJSON_PatchWithValidContentType(t *testing.T) {
	handler := ContentTypeJSON(okHandler)

	body := strings.NewReader(`{"key":"value"}`)
	req := httptest.NewRequest(http.MethodPatch, "/test", body)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200 for PATCH with valid content-type, got %d", rr.Code)
	}
}

func TestContentTypeJSON_OptionsRequestIgnored(t *testing.T) {
	handler := ContentTypeJSON(okHandler)

	req := httptest.NewRequest(http.MethodOptions, "/test", nil)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200 for OPTIONS request, got %d", rr.Code)
	}
}

func TestContentTypeJSON_InvalidMediaTypeFormat(t *testing.T) {
	handler := ContentTypeJSON(okHandler)

	body := strings.NewReader(`data`)
	req := httptest.NewRequest(http.MethodPost, "/test", body)
	req.Header.Set("Content-Type", "invalid-content-type;;;")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected status 415 for malformed content-type, got %d", rr.Code)
	}
}

func TestContentTypeJSON_ResponseContentType(t *testing.T) {
	handler := ContentTypeJSON(okHandler)

	body := strings.NewReader(`data`)
	req := httptest.NewRequest(http.MethodPost, "/test", body)
	req.Header.Set("Content-Type", "text/plain")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected response Content-Type 'application/json', got '%s'", ct)
	}
}
