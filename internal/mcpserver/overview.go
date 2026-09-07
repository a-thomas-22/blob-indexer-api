package mcpserver

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// overviewBlocks is how many recent blocks the overview's pricing section
// covers: enough to show the fee trend without flooding the context window.
const overviewBlocks = 20

// overviewSection is one backend fetch folded into the overview.
type overviewSection struct {
	key   string
	path  string
	query func(network string) url.Values
}

var overviewSections = []overviewSection{
	{key: "indexer_status", path: "/status", query: newQuery},
	{key: "pricing", path: "/blob/pricing", query: func(network string) url.Values {
		q := newQuery(network)
		setInt(q, "blocks", overviewBlocks)
		return q
	}},
	{key: "mempool_pressure", path: "/blob/mempool/pressure", query: newQuery},
	{key: "rolling_stats", path: "/stats/windows", query: newQuery},
}

// overviewResult is the composite payload. Sections are raw REST data values
// so their shapes match the single-purpose tools exactly.
type overviewResult struct {
	Network  string                     `json:"network,omitempty"`
	Sections map[string]json.RawMessage `json:"sections"`
	Errors   map[string]string          `json:"errors,omitempty"`
	Guide    string                     `json:"guide"`
}

const overviewGuide = "pricing.current_base_fee is the blob base fee now; pricing.market_pressure summarizes recent fullness and fee direction; mempool_pressure shows pending demand; rolling_stats compares fee and volume across windows; indexer_status.indexer_lag_blocks says how current this snapshot is."

// registerOverview adds the composite tool. Sections are fetched
// concurrently through the same cached REST paths; a failing section is
// reported under errors so a transient problem in one query does not hide
// the rest of the picture.
func registerOverview(s *Server, srv *mcp.Server, spec *toolSpec) {
	mcp.AddTool(srv, spec.tool(), func(ctx context.Context, _ *mcp.CallToolRequest, in networkInput) (*mcp.CallToolResult, any, error) {
		// Normalize once so the echoed header names the network the sections
		// actually queried: setString trims and omits blank selectors, so a
		// whitespace-only input resolves to the default network and must not
		// be echoed back as though it had selected one.
		network := strings.TrimSpace(in.Network)
		result := overviewResult{
			Network:  network,
			Sections: make(map[string]json.RawMessage, len(overviewSections)),
			Errors:   make(map[string]string),
			Guide:    overviewGuide,
		}

		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, section := range overviewSections {
			wg.Add(1)
			go func() {
				defer wg.Done()
				payload, err := s.fetch(ctx, section.path, section.query(network))
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					result.Errors[section.key] = err.Error()
					return
				}
				result.Sections[section.key] = payload.Data
			}()
		}
		wg.Wait()

		// Nothing succeeded: almost always a bad network selector, which every
		// section reports identically. Surface it as a tool error so the model
		// corrects the input instead of reading an empty snapshot.
		if len(result.Sections) == 0 {
			for _, msg := range result.Errors {
				return errorResult(&overviewError{msg: msg}), nil, nil
			}
		}
		if len(result.Errors) == 0 {
			result.Errors = nil
		}
		callResult, err := jsonResult(result)
		return callResult, nil, err
	})
}

type overviewError struct{ msg string }

func (e *overviewError) Error() string { return "every overview section failed: " + e.msg }
