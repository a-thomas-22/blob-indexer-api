package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// apiBasePath is where the public REST routes live on the backend handler.
const apiBasePath = "/api/v1"

// maxErrorBodyBytes caps how much of a non-JSON backend response is echoed
// into a tool error.
const maxErrorBodyBytes = 512

// envelope mirrors the REST layer's standard response wrapper.
type envelope struct {
	Success   bool            `json:"success"`
	Data      json.RawMessage `json:"data,omitempty"`
	Meta      json.RawMessage `json:"meta,omitempty"`
	Error     string          `json:"error,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
}

// toolPayload is what successful tools return to the model: the REST data
// value plus the optional request metadata (e.g. the resolved /users range).
type toolPayload struct {
	Data json.RawMessage `json:"data"`
	Meta json.RawMessage `json:"meta,omitempty"`
}

// responseRecorder captures an in-process handler response. A minimal
// http.ResponseWriter keeps net/http/httptest (and its flag registration)
// out of the production binary.
type responseRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header)}
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(p)
}

// backendError is a REST-layer failure surfaced to the model as a tool
// error. It is returned as a value so callers can decide whether one failed
// section should fail the whole tool (single fetches) or be reported inline
// (the composite overview).
type backendError struct {
	Status  int
	Code    string
	Message string
}

func (e *backendError) Error() string {
	code := e.Code
	if code == "" {
		code = http.StatusText(e.Status)
	}
	return fmt.Sprintf("%s: %s (HTTP %d)", code, e.Message, e.Status)
}

// fetch dispatches GET apiBasePath+path?query to the backend in-process and
// decodes the standard envelope. Non-2xx statuses and success=false
// envelopes become a *backendError.
func (s *Server) fetch(ctx context.Context, path string, query url.Values) (*toolPayload, error) {
	target := apiBasePath + path
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build backend request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// Loopback requests carry no client address; a fixed loopback peer keeps
	// middleware that logs or keys on RemoteAddr well-defined.
	req.RemoteAddr = "127.0.0.1:0"

	rec := newResponseRecorder()
	s.backend.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}

	var env envelope
	if err := json.Unmarshal(rec.body.Bytes(), &env); err != nil {
		snippet := strings.TrimSpace(rec.body.String())
		if len(snippet) > maxErrorBodyBytes {
			snippet = snippet[:maxErrorBodyBytes] + "…"
		}
		return nil, &backendError{Status: rec.status, Code: "invalid_backend_response", Message: fmt.Sprintf("non-JSON response for %s: %s", target, snippet)}
	}
	if rec.status < 200 || rec.status >= 300 || !env.Success {
		msg := env.Error
		if msg == "" {
			msg = http.StatusText(rec.status)
		}
		return nil, &backendError{Status: rec.status, Code: env.ErrorCode, Message: msg}
	}
	return &toolPayload{Data: env.Data, Meta: env.Meta}, nil
}

// call runs fetch and renders the outcome as a CallToolResult. Backend errors
// are reported inside the result (IsError) rather than as protocol errors so
// the model can read the message (e.g. an invalid range) and self-correct.
func (s *Server) call(ctx context.Context, path string, query url.Values) (*mcp.CallToolResult, error) {
	payload, err := s.fetch(ctx, path, query)
	if err != nil {
		return errorResult(err), nil
	}
	return jsonResult(payload)
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	text, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode tool result: %w", err)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(text)}}}, nil
}

func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}
