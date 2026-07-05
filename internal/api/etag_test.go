package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func etagTestHandler(status int, body string) http.Handler {
	return ETagMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
}

func TestETagMiddleware_SetsWeakETagOn200(t *testing.T) {
	h := etagTestHandler(http.StatusOK, `{"success":true}`)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", http.NoBody))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	etag := w.Header().Get("ETag")
	if etag == "" || etag[:2] != "W/" {
		t.Fatalf("expected weak ETag, got %q", etag)
	}
	if w.Body.String() != `{"success":true}` {
		t.Fatalf("body mangled: %q", w.Body.String())
	}

	// Same body → same ETag.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if w2.Header().Get("ETag") != etag {
		t.Fatalf("ETag not stable: %q vs %q", w2.Header().Get("ETag"), etag)
	}
}

func TestETagMiddleware_304OnIfNoneMatch(t *testing.T) {
	h := etagTestHandler(http.StatusOK, `{"success":true}`)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	etag := w.Header().Get("ETag")

	for _, inm := range []string{etag, "*", `"bogus", ` + etag} {
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Header.Set("If-None-Match", inm)
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, req)

		if w2.Code != http.StatusNotModified {
			t.Fatalf("If-None-Match %q: expected 304, got %d", inm, w2.Code)
		}
		if w2.Body.Len() != 0 {
			t.Fatalf("If-None-Match %q: 304 must have no body, got %q", inm, w2.Body.String())
		}
		if w2.Header().Get("ETag") != etag {
			t.Fatalf("If-None-Match %q: 304 should carry the ETag", inm)
		}
	}

	// Non-matching validator gets the full response.
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set("If-None-Match", `W/"deadbeef"`)
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, req)
	if w3.Code != http.StatusOK || w3.Body.Len() == 0 {
		t.Fatalf("expected full 200 for non-matching validator, got %d", w3.Code)
	}
}

func TestETagMiddleware_SkipsErrorsAndNonGET(t *testing.T) {
	w := httptest.NewRecorder()
	etagTestHandler(http.StatusNotFound, `{"success":false}`).ServeHTTP(
		w, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 passthrough, got %d", w.Code)
	}
	if w.Header().Get("ETag") != "" {
		t.Fatal("errors must not carry an ETag")
	}
	if w.Body.String() != `{"success":false}` {
		t.Fatalf("error body mangled: %q", w.Body.String())
	}

	w2 := httptest.NewRecorder()
	etagTestHandler(http.StatusOK, `{}`).ServeHTTP(
		w2, httptest.NewRequest(http.MethodPost, "/", http.NoBody))
	if w2.Header().Get("ETag") != "" {
		t.Fatal("non-GET must not carry an ETag")
	}
}

func TestETagMiddleware_NoBodyNoETag(t *testing.T) {
	w := httptest.NewRecorder()
	etagTestHandler(http.StatusOK, "").ServeHTTP(
		w, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if w.Header().Get("ETag") != "" {
		t.Fatal("empty body must not carry an ETag")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestIfNoneMatchSatisfied(t *testing.T) {
	etag := `W/"10-abc"`
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"*", true},
		{`W/"10-abc"`, true},
		{`"10-abc"`, true}, // weak comparison ignores the W/ prefix
		{`"other", W/"10-abc"`, true},
		{`"other"`, false},
	}
	for _, tc := range cases {
		if got := ifNoneMatchSatisfied(tc.header, etag); got != tc.want {
			t.Errorf("ifNoneMatchSatisfied(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}
