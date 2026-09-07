// Package mcpserver exposes the blob indexer's public REST surface as a
// permissioned Model Context Protocol (MCP) server, so LLM clients such as
// Claude can ask what is happening in the blob market.
//
// Every tool call is dispatched in-process to the API's own public routes
// (the "loopback" backend), so tools inherit the REST layer's caching,
// singleflight, validation, and response shapes instead of duplicating query
// logic. Access is fail-closed: requests must carry one of the configured API
// keys, and each key may be restricted to an allowlist of tools.
package mcpserver

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// ServerName is the MCP implementation name advertised during initialize.
const ServerName = "blob-indexer"

// Server serves a permissioned MCP endpoint over an in-process HTTP backend.
type Server struct {
	cfg        config.MCPConfig
	backend    http.Handler
	principals []*principal
	servers    map[string]*mcp.Server
	knownTools map[string]struct{}
	metrics    *metrics
	now        func() time.Time
}

// unknownToolLabel replaces client-supplied tool names that are not in the
// catalog before they reach logs or metric labels, so a caller cannot mint
// unbounded label cardinality by inventing names.
const unknownToolLabel = "unknown"

// principal is one authenticated client identity: a named key plus the tools
// it may call and its rate limiter.
type principal struct {
	name    string
	digest  [sha256.Size]byte   // of the secret; fixed-size so comparison time is independent of length
	allowed map[string]struct{} // nil grants every tool
	// limiter is per process. Every API replica holds its own bucket, so the
	// effective cap for a key is rate_limit_rps × replicas; see the config docs.
	limiter *rate.Limiter
}

func (p *principal) allows(tool string) bool {
	if p.allowed == nil {
		return true
	}
	_, ok := p.allowed[tool]
	return ok
}

// ValidateConfig checks the parts of the MCP config that only this package
// can judge: every tool named in a key's allowlist must exist. Structural
// checks (key presence, length, uniqueness) live in the config package.
func ValidateConfig(cfg config.MCPConfig) error {
	if !cfg.Enabled {
		return nil
	}
	known := make(map[string]struct{}, len(toolSpecs))
	for _, spec := range toolSpecs {
		known[spec.name] = struct{}{}
	}
	for _, key := range cfg.Keys {
		for _, tool := range key.Tools {
			if _, ok := known[tool]; !ok {
				return fmt.Errorf("mcp key %q allows unknown tool %q (known tools: %s)", key.Name, tool, strings.Join(ToolNames(), ", "))
			}
		}
	}
	return nil
}

// ToolNames returns the sorted catalog of tool names this server can serve.
func ToolNames() []string {
	names := make([]string, 0, len(toolSpecs))
	for _, spec := range toolSpecs {
		names = append(names, spec.name)
	}
	sort.Strings(names)
	return names
}

// New builds the MCP server. backend must route the API's public
// /api/v1/* endpoints; version is the API build version advertised to
// clients. It returns an error when a key allowlist names an unknown tool,
// so a typo in the permission model cannot silently grant or drop access.
func New(cfg config.MCPConfig, backend http.Handler, version string) (*Server, error) {
	if backend == nil {
		return nil, fmt.Errorf("mcpserver: backend handler is required")
	}
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if version == "" {
		version = "dev"
	}
	// The config package validates these for the API binary; re-check here so
	// programmatic callers cannot cross-wire principals (servers are keyed by
	// name, so a duplicate name would route one secret to another's tool set).
	names := make(map[string]struct{}, len(cfg.Keys))
	secrets := make(map[string]struct{}, len(cfg.Keys))
	for _, key := range cfg.Keys {
		if key.Name == "" || key.Key == "" {
			return nil, fmt.Errorf("mcpserver: key entries need a name and a secret")
		}
		if _, dup := names[key.Name]; dup {
			return nil, fmt.Errorf("mcpserver: duplicate key name %q", key.Name)
		}
		if _, dup := secrets[key.Key]; dup {
			return nil, fmt.Errorf("mcpserver: key %q reuses another key's secret", key.Name)
		}
		names[key.Name] = struct{}{}
		secrets[key.Key] = struct{}{}
	}

	s := &Server{
		cfg:        cfg,
		backend:    backend,
		servers:    make(map[string]*mcp.Server, len(cfg.Keys)),
		knownTools: make(map[string]struct{}, len(toolSpecs)),
		metrics:    newMetrics(),
		now:        time.Now,
	}
	for _, spec := range toolSpecs {
		s.knownTools[spec.name] = struct{}{}
	}

	// Tool handlers are shared; each principal gets its own mcp.Server that
	// registers only the tools it is allowed to see, so tools/list and
	// tools/call agree with the allowlist without per-call filtering.
	schemaCache := mcp.NewSchemaCache()
	for _, key := range cfg.Keys {
		p := &principal{
			name:    key.Name,
			digest:  sha256.Sum256([]byte(key.Key)),
			limiter: rate.NewLimiter(rate.Limit(cfg.RateLimitRPS), cfg.RateLimitBurst),
		}
		if len(key.Tools) > 0 {
			p.allowed = make(map[string]struct{}, len(key.Tools))
			for _, tool := range key.Tools {
				p.allowed[tool] = struct{}{}
			}
		}
		s.principals = append(s.principals, p)

		srv := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: version}, &mcp.ServerOptions{
			Instructions: serverInstructions,
			SchemaCache:  schemaCache,
			// Tool sets are fixed per principal, so never advertise
			// listChanged; stateless transport could not deliver it anyway.
			Capabilities: &mcp.ServerCapabilities{
				Tools:   &mcp.ToolCapabilities{},
				Prompts: &mcp.PromptCapabilities{},
			},
		})
		srv.AddReceivingMiddleware(s.observe(p))
		s.registerTools(srv, p)
		s.registerPrompts(srv)
		s.servers[p.name] = srv
	}

	return s, nil
}

// Handler returns the HTTP handler to mount at the configured path. It
// authenticates the request, then serves the streamable-HTTP MCP transport
// for the matching principal. The transport is stateless so any replica
// behind a load balancer can serve any request without session affinity.
func (s *Server) Handler() http.Handler {
	transport := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		p, ok := principalFromContext(r.Context())
		if !ok {
			// Unreachable: the auth middleware rejects unauthenticated
			// requests before the transport runs. Returning nil makes the
			// SDK answer 400 rather than serving an unscoped server.
			return nil
		}
		return s.servers[p.name]
	}, &mcp.StreamableHTTPOptions{
		Stateless: true,
		// The API sits behind a reverse proxy/tunnel, so the Host header is
		// the public hostname even though the pod listens on 0.0.0.0; the
		// SDK's localhost heuristic would otherwise reject those requests.
		DisableLocalhostProtection: true,
	})
	return s.authenticate(transport)
}

// Collector exposes per-principal tool-call metrics for the API's Prometheus
// registry.
func (s *Server) Collector() prometheus.Collector {
	return s.metrics
}

// Path is the mount path from config.
func (s *Server) Path() string {
	return s.cfg.Path
}

// observe is a receiving middleware that rate-limits and audit-logs tool
// calls for one principal. Rate-limit rejections are returned as tool
// errors (not protocol errors) so the calling model sees them and backs off.
func (s *Server) observe(p *principal) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			tool := s.toolLabel(req)

			if !p.limiter.Allow() {
				s.metrics.record(p.name, tool, outcomeRateLimited)
				logger.Warn("MCP tool call rate limited",
					zap.String("principal", p.name),
					zap.String("tool", tool))
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
						"rate limit exceeded for this API key (%.0f calls/s, burst %d); wait a few seconds before retrying",
						s.cfg.RateLimitRPS, s.cfg.RateLimitBurst)}},
				}, nil
			}

			start := s.now()
			result, err := next(ctx, method, req)
			outcome := outcomeOK
			switch {
			case err != nil:
				outcome = outcomeProtocolError
			case toolResultIsError(result):
				outcome = outcomeToolError
			}
			s.metrics.record(p.name, tool, outcome)
			logger.Info("MCP tool call",
				zap.String("principal", p.name),
				zap.String("tool", tool),
				zap.String("outcome", outcome),
				zap.Duration("duration", s.now().Sub(start)))
			return result, err
		}
	}
}

// toolLabel returns the requested tool's name if it is in the catalog and a
// constant otherwise. The name is client input, so it must be bounded before
// it becomes a metric label or log field.
func (s *Server) toolLabel(req mcp.Request) string {
	params, _ := req.GetParams().(*mcp.CallToolParamsRaw)
	if params == nil {
		return unknownToolLabel
	}
	if _, ok := s.knownTools[params.Name]; ok {
		return params.Name
	}
	return unknownToolLabel
}

func toolResultIsError(result mcp.Result) bool {
	callResult, ok := result.(*mcp.CallToolResult)
	return ok && callResult.IsError
}
