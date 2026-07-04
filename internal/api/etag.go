package api

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
)

// etagResponseWriter buffers a response so ETagMiddleware can hash the body
// and answer If-None-Match revalidations with a body-less 304. Headers pass
// through to the underlying writer's header map; only status and body are
// deferred.
type etagResponseWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (w *etagResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *etagResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.buf.Write(b)
}

// ETagMiddleware adds a weak ETag to successful GET responses and serves 304
// Not Modified on a matching If-None-Match. The blob-flow dashboard polls the
// same URLs on block cadence, so between blocks browsers and the Cloudflare
// edge revalidate with a header-only exchange instead of re-downloading the
// full JSON payload. Responses are already assembled in memory (respondJSON
// buffers before writing), so the extra buffering here is one copy of an
// already-materialized body.
func ETagMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}

		ew := &etagResponseWriter{ResponseWriter: w}
		next.ServeHTTP(ew, r)

		status := ew.status
		if status == 0 {
			status = http.StatusOK
		}

		if status == http.StatusOK && ew.buf.Len() > 0 {
			etag := weakETag(ew.buf.Bytes())
			w.Header().Set("ETag", etag)
			if ifNoneMatchSatisfied(r.Header.Get("If-None-Match"), etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		w.WriteHeader(status)
		_, _ = w.Write(ew.buf.Bytes())
	})
}

// weakETag derives a weak validator from the uncompressed body. Weak because
// the same entity may be served under different content encodings, and because
// byte-identical JSON is the equivalence that matters for revalidation.
func weakETag(body []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(body)
	return fmt.Sprintf(`W/"%x-%x"`, len(body), h.Sum64())
}

// ifNoneMatchSatisfied reports whether the If-None-Match header matches etag,
// using weak comparison (encoding-insensitive) per RFC 9110 §8.8.3.2.
func ifNoneMatchSatisfied(ifNoneMatch, etag string) bool {
	ifNoneMatch = strings.TrimSpace(ifNoneMatch)
	if ifNoneMatch == "" {
		return false
	}
	if ifNoneMatch == "*" {
		return true
	}

	want := strings.TrimPrefix(etag, "W/")
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		if strings.TrimPrefix(candidate, "W/") == want {
			return true
		}
	}
	return false
}
