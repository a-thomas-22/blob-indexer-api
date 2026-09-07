package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const explainPromptName = "explain_blob_market"

// registerPrompts adds the guided "explain the market" prompt. Prompts are
// not permission-scoped: they only tell the model which tools to call, and
// the tool allowlist still governs what it can actually fetch.
func (s *Server) registerPrompts(srv *mcp.Server) {
	srv.AddPrompt(&mcp.Prompt{
		Name:        explainPromptName,
		Title:       "Explain the blob market",
		Description: "Walk through the current state of a network's blob market and explain it in plain language: fee level and direction, how full blocks are, pending demand, who is posting, and how fresh the data is.",
		Arguments: []*mcp.PromptArgument{
			{Name: paramNetwork, Description: "Network name or chain ID (optional when only one network is indexed).", Required: false},
			{Name: "audience", Description: "Who the explanation is for, e.g. 'newcomer' or 'rollup operator' (default: someone new to blobs).", Required: false},
		},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		network := strings.TrimSpace(req.Params.Arguments[paramNetwork])
		audience := strings.TrimSpace(req.Params.Arguments["audience"])
		if audience == "" {
			audience = "someone new to Ethereum blobs"
		}
		networkClause := "the default network"
		if network != "" {
			networkClause = fmt.Sprintf("network %q", network)
		}
		text := fmt.Sprintf(`Explain what is happening in the Ethereum blob market on %s for %s.

Steps:
1. Call get_blob_market_overview (pass the network if given). Note indexer_status lag; if the indexer is far behind, say so up front.
2. Describe the current blob base fee (convert wei to gwei), whether recent blocks were above or below the blob target, and the predicted next fee direction. Use pricing.market_pressure as the headline.
3. Describe pending demand from mempool_pressure: how many blobs are waiting and whether bids exceed the current fee.
4. Compare rolling_stats windows (5m, 1h, 24h, 7d) to say whether conditions are heating up, cooling down, or steady.
5. Call get_top_blob_users with range=24h and group=entity to name who is using blob space today.
6. If anything looks unusual (fee spike, full-block streak), call get_blob_records to put it in historical context.

Keep it concise, avoid jargon where possible, and quote the numbers you relied on.`, networkClause, audience)
		return &mcp.GetPromptResult{
			Description: "Guided explanation of the current blob market",
			Messages: []*mcp.PromptMessage{{
				Role:    "user",
				Content: &mcp.TextContent{Text: text},
			}},
		}, nil
	})
}
