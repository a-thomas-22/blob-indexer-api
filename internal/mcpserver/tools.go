package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverInstructions primes the client model on the domain and on which tool
// answers which question. It is sent once during initialize.
const serverInstructions = `You are connected to a read-only Ethereum blob market indexer (EIP-4844 blob transactions).

Domain primer:
- Rollups post data to L1 in blobs. Each block has a target and a maximum blob count; the blob base fee rises when recent blocks carried more than target and falls when they carried less.
- "Utilization" is blobs per block relative to the maximum; "market pressure" summarizes how full recent blocks were and where the fee is heading.
- Fees are reported in wei (and gwei where noted). Blob users are rollup sender addresses, attributed to named entities (Base, Arbitrum, ...) where known.

Tool guide:
- Start with get_blob_market_overview for a one-call snapshot (current fee, pressure, mempool backlog, rolling stats, indexer freshness).
- Use list_networks to discover networks; pass network as a name (mainnet, sepolia) or chain ID. When only one network is indexed the parameter is optional.
- Use get_blob_pricing for the current fee and recent blocks, get_blob_market_chart for history, get_top_blob_users / get_entity for who is posting, get_blob_records for all-time extremes.
- Check get_indexer_status (last indexed block, lag) before trusting "current" values.
- Every successful result is JSON of the form {"data": ..., "meta": ...}; failures return an error message you can act on (for example an invalid range value).`

// Tool names, shared by the catalog, the overview, and the allowlist checks.
const (
	toolGetBlobMarketOverview    = "get_blob_market_overview"
	toolListNetworks             = "list_networks"
	toolGetIndexerStatus         = "get_indexer_status"
	toolGetBlobPricing           = "get_blob_pricing"
	toolGetMempoolPressure       = "get_mempool_pressure"
	toolGetBlobStats             = "get_blob_stats"
	toolGetRollingStats          = "get_rolling_stats"
	toolGetBlobMarketChart       = "get_blob_market_chart"
	toolGetAttributionUsageChart = "get_attribution_usage_chart"
	toolGetCostComparisonChart   = "get_cost_comparison_chart"
	toolGetBlobTipsChart         = "get_blob_tips_chart"
	toolGetTopBlobUsers          = "get_top_blob_users"
	toolGetEntity                = "get_entity"
	toolGetBlobRecords           = "get_blob_records"
	toolGetLatestBlobs           = "get_latest_blobs"
	toolGetMempoolBlobs          = "get_mempool_blobs"
	toolGetBlobReplacements      = "get_blob_replacements"
	toolGetBlock                 = "get_block"
	toolGetBlobByTxHash          = "get_blob_by_tx_hash"
	toolGetBlobByVersionedHash   = "get_blob_by_versioned_hash"
	toolSearch                   = "search"
)

// paramNetwork is the REST query parameter selecting the network.
const paramNetwork = "network"

// toolSpec describes one tool: its MCP metadata plus a registration function
// that binds a typed input to a backend path.
type toolSpec struct {
	name        string
	title       string
	description string
	register    func(s *Server, srv *mcp.Server, spec *toolSpec)
}

// registerTool returns a registration function for a tool whose input maps
// to a single backend GET. build may return an error for inputs that cannot
// form a request (e.g. an empty required hash); it is reported as a tool
// error.
func registerTool[In any](build func(in In) (path string, query url.Values, err error)) func(*Server, *mcp.Server, *toolSpec) {
	return func(s *Server, srv *mcp.Server, spec *toolSpec) {
		mcp.AddTool(srv, spec.tool(), func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			path, query, err := build(in)
			if err != nil {
				return errorResult(err), nil, nil
			}
			result, err := s.call(ctx, path, query)
			return result, nil, err
		})
	}
}

func (spec *toolSpec) tool() *mcp.Tool {
	openWorld := false
	return &mcp.Tool{
		Name:        spec.name,
		Title:       spec.title,
		Description: spec.description,
		Annotations: &mcp.ToolAnnotations{
			Title:          spec.title,
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  &openWorld,
		},
	}
}

// registerTools adds every tool the principal is allowed to call.
func (s *Server) registerTools(srv *mcp.Server, p *principal) {
	for i := range toolSpecs {
		spec := &toolSpecs[i]
		if !p.allows(spec.name) {
			continue
		}
		spec.register(s, srv, spec)
	}
}

// Shared input fragments. jsonschema tags become property descriptions;
// fields without omitempty are required.

type networkInput struct {
	Network string `json:"network,omitempty" jsonschema:"Network name (e.g. mainnet, sepolia) or chain ID. Optional when only one network is indexed; call list_networks to discover."`
}

type emptyInput struct{}

type pricingInput struct {
	networkInput
	Blocks int `json:"blocks,omitempty" jsonschema:"Number of recent blocks to include (default 20, max 512; out-of-range values are clamped)."`
}

type chartInput struct {
	networkInput
	Range       string `json:"range,omitempty" jsonschema:"Time range: 1h, 24h, 7d, or 30d (default 24h)."`
	Granularity string `json:"granularity,omitempty" jsonschema:"Bucket size: auto, block, minute, hour, or day (default auto picks a size that keeps the series small)."`
	Limit       int    `json:"limit,omitempty" jsonschema:"Maximum number of points for explicit granularities (default 300, max 1000)."`
}

type seriesChartInput struct {
	networkInput
	Range       string `json:"range,omitempty" jsonschema:"Time range: 1h, 24h, 7d, 30d, or all (default 24h)."`
	Granularity string `json:"granularity,omitempty" jsonschema:"Bucket size: auto, block, minute, hour, or day (default auto)."`
	Limit       int    `json:"limit,omitempty" jsonschema:"Number of top attributed series to keep before grouping the long tail into 'other' (default 5, max 25)."`
}

type usersInput struct {
	networkInput
	Limit            int    `json:"limit,omitempty" jsonschema:"Rows to return (default 10, max 100)."`
	Offset           int    `json:"offset,omitempty" jsonschema:"Rows to skip for pagination (default 0)."`
	Sort             string `json:"sort,omitempty" jsonschema:"Order by count (blobs posted) or spend (fees paid). Default count."`
	Range            string `json:"range,omitempty" jsonschema:"Aggregation window: 1h, 24h, 7d, 30d, or all (default all)."`
	Group            string `json:"group,omitempty" jsonschema:"address (one row per sender, default) or entity (collapse attributed addresses into one row per rollup)."`
	UnattributedOnly bool   `json:"unattributed_only,omitempty" jsonschema:"Only senders that are not attributed to a known entity (cannot be combined with group=entity)."`
}

type entityInput struct {
	networkInput
	Key   string `json:"key" jsonschema:"Entity key or display name as returned by get_top_blob_users (group=entity) or get_attribution_usage_chart."`
	Range string `json:"range,omitempty" jsonschema:"Aggregation window: 1h, 24h, 7d, 30d, or all (default all)."`
}

type recordsInput struct {
	networkInput
	Limit int `json:"limit,omitempty" jsonschema:"Entries per leaderboard (default 10, max 100)."`
}

type blobListInput struct {
	networkInput
	Limit  int    `json:"limit,omitempty" jsonschema:"Blobs to return (default 10, max 100)."`
	Offset int    `json:"offset,omitempty" jsonschema:"Blobs to skip for pagination (default 0, max 10000)."`
	From   string `json:"from,omitempty" jsonschema:"Filter by sender address (mutually exclusive with entity)."`
	Entity string `json:"entity,omitempty" jsonschema:"Filter by attributed entity key; returns blobs from any of the entity's addresses."`
}

type replacementsInput struct {
	networkInput
	TxHash string `json:"tx_hash,omitempty" jsonschema:"Only events where this transaction hash was replaced or was the replacement."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Events to return (default 25, max 100)."`
	Offset int    `json:"offset,omitempty" jsonschema:"Events to skip for pagination (default 0)."`
}

type blockInput struct {
	networkInput
	Number uint64 `json:"number" jsonschema:"Block number."`
}

type txHashInput struct {
	networkInput
	TxHash string `json:"tx_hash" jsonschema:"Transaction hash (0x-prefixed, 32 bytes)."`
}

type versionedHashInput struct {
	networkInput
	VersionedHash string `json:"versioned_hash" jsonschema:"EIP-4844 versioned blob hash (0x01-prefixed, 32 bytes)."`
}

type searchInput struct {
	networkInput
	Query string `json:"query" jsonschema:"Block height, 0x-prefixed hash, sender address, or rollup-name prefix."`
}

type rollingStatsInput struct {
	networkInput
	Windows []string `json:"windows,omitempty" jsonschema:"Rolling windows using m/h/d units, e.g. [\"5m\",\"1h\",\"24h\",\"7d\"] (the default; max 8 windows, max 30d each)."`
}

// query helpers: zero values are omitted so the REST defaults apply.

func newQuery(network string) url.Values {
	q := url.Values{}
	setString(q, paramNetwork, network)
	return q
}

func setString(q url.Values, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		q.Set(key, value)
	}
}

func setInt(q url.Values, key string, value int) {
	if value != 0 {
		q.Set(key, strconv.Itoa(value))
	}
}

func requirePathValue(label, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	return url.PathEscape(value), nil
}

var errEntityKeyRequired = errors.New("key is required")

// toolSpecs is the full tool catalog. Order is the order clients see.
var toolSpecs = []toolSpec{
	{
		name:        toolGetBlobMarketOverview,
		title:       "Blob market overview",
		description: "One-call snapshot of a network's blob market: current blob base fee and predicted next fee, market pressure and utilization of recent blocks, pending mempool backlog, rolling fee/volume statistics over several windows, and indexer freshness. Start here to understand what is going on right now. Sections that fail are reported under errors instead of failing the whole call.",
		register:    registerOverview,
	},
	{
		name:        toolListNetworks,
		title:       "List networks",
		description: "List the networks this indexer serves, with chain IDs and per-network freshness (last indexed block, chain head, lag).",
		register: registerTool(func(emptyInput) (string, url.Values, error) {
			return "/networks", url.Values{}, nil
		}),
	},
	{
		name:        toolGetIndexerStatus,
		title:       "Indexer status",
		description: "Indexer health for one network: last indexed block, chain head, lag in blocks, indexed-block coverage, and backfill progress. Use it to judge how current the other tools' data is.",
		register: registerTool(func(in networkInput) (string, url.Values, error) {
			return "/status", newQuery(in.Network), nil
		}),
	},
	{
		name:        toolGetBlobPricing,
		title:       "Blob pricing",
		description: "Current blob base fee, excess blob gas, utilization, the predicted next-block fee, the active blob schedule (target/max blobs), a market-pressure summary, and per-block metrics for the most recent blocks.",
		register: registerTool(func(in pricingInput) (string, url.Values, error) {
			q := newQuery(in.Network)
			setInt(q, "blocks", in.Blocks)
			return "/blob/pricing", q, nil
		}),
	},
	{
		name:        toolGetMempoolPressure,
		title:       "Mempool pressure",
		description: "Pending (unconfirmed) blob transactions waiting in the mempool: counts, pending blob count versus block capacity, and the distribution of max fees bidders are offering. High backlog with fees above the current base fee signals upward fee pressure.",
		register: registerTool(func(in networkInput) (string, url.Values, error) {
			return "/blob/mempool/pressure", newQuery(in.Network), nil
		}),
	},
	{
		name:        toolGetBlobStats,
		title:       "Historical stats",
		description: "All-time totals for a network: confirmed and pending blob counts and average base fee, tip, and total cost per blob.",
		register: registerTool(func(in networkInput) (string, url.Values, error) {
			return "/stats", newQuery(in.Network), nil
		}),
	},
	{
		name:        toolGetRollingStats,
		title:       "Rolling window stats",
		description: "Blob market statistics over rolling windows ending now (average, median, and p95 blob base fee; blob and block counts; fill rate). Compare windows such as 5m versus 24h to see whether conditions are heating up or cooling down.",
		register: registerTool(func(in rollingStatsInput) (string, url.Values, error) {
			q := newQuery(in.Network)
			setString(q, "windows", strings.Join(in.Windows, ","))
			return "/stats/windows", q, nil
		}),
	},
	{
		name:        toolGetBlobMarketChart,
		title:       "Blob market chart",
		description: "Time series of blob base fee and blob volume per bucket over a range (up to 30d). Use it to describe trends, spikes, and quiet periods.",
		register: registerTool(func(in chartInput) (string, url.Values, error) {
			return "/charts/blob-market", chartQuery(in), nil
		}),
	},
	{
		name:        toolGetAttributionUsageChart,
		title:       "Attribution usage chart",
		description: "Time series of blob volume grouped by attributed entity (rollup), with the long tail grouped into 'other', plus each entity's share of the range. Answers who is using blob space over time.",
		register: registerTool(func(in seriesChartInput) (string, url.Values, error) {
			return "/charts/attribution-usage", seriesChartQuery(in), nil
		}),
	},
	{
		name:        toolGetCostComparisonChart,
		title:       "Cost comparison chart",
		description: "Time series comparing what posting data as blobs cost against the equivalent calldata cost, showing how much rollups saved (or would have saved) by using blobs.",
		register: registerTool(func(in chartInput) (string, url.Values, error) {
			return "/charts/cost-comparison", chartQuery(in), nil
		}),
	},
	{
		name:        toolGetBlobTipsChart,
		title:       "Blob tips chart",
		description: "Time series of execution-layer priority fees (tips) paid by blob transactions, grouped by attributed entity. range=all is not supported.",
		register: registerTool(func(in seriesChartInput) (string, url.Values, error) {
			return "/charts/blob-tips", seriesChartQuery(in), nil
		}),
	},
	{
		name:        toolGetTopBlobUsers,
		title:       "Top blob users",
		description: "Leaderboard of blob senders by blob count or fee spend over a window, optionally grouped into attributed entities (rollups) or restricted to unattributed addresses.",
		register: registerTool(func(in usersInput) (string, url.Values, error) {
			q := newQuery(in.Network)
			setInt(q, "limit", in.Limit)
			setInt(q, "offset", in.Offset)
			setString(q, "sort", in.Sort)
			setString(q, "range", in.Range)
			setString(q, "group", in.Group)
			if in.UnattributedOnly {
				return "/users/unattributed", q, nil
			}
			return "/users", q, nil
		}),
	},
	{
		name:        toolGetEntity,
		title:       "Entity detail",
		description: "Detail for one attributed entity (rollup): aggregate blob count and spend over a window plus a per-address breakdown of the addresses it posts from.",
		register: registerTool(func(in entityInput) (string, url.Values, error) {
			key, err := requirePathValue("key", in.Key)
			if err != nil {
				return "", nil, errEntityKeyRequired
			}
			q := newQuery(in.Network)
			setString(q, "range", in.Range)
			return "/entities/" + key, q, nil
		}),
	},
	{
		name:        toolGetBlobRecords,
		title:       "Records and streaks",
		description: "Historical leaderboards: longest streaks of full and above-target blocks (including the streak in progress), highest blob base fee peaks, and busiest hours.",
		register: registerTool(func(in recordsInput) (string, url.Values, error) {
			q := newQuery(in.Network)
			setInt(q, "limit", in.Limit)
			return "/records", q, nil
		}),
	},
	{
		name:        toolGetLatestBlobs,
		title:       "Latest blobs",
		description: "Most recently confirmed blob transactions, newest first, optionally filtered to one sender address or one attributed entity.",
		register: registerTool(func(in blobListInput) (string, url.Values, error) {
			return "/blob/latest", blobListQuery(in), nil
		}),
	},
	{
		name:        toolGetMempoolBlobs,
		title:       "Pending blobs",
		description: "Blob transactions currently pending in the mempool, optionally filtered to one sender address or one attributed entity.",
		register: registerTool(func(in blobListInput) (string, url.Values, error) {
			return "/blob/mempool", blobListQuery(in), nil
		}),
	},
	{
		name:        toolGetBlobReplacements,
		title:       "Fee-bump replacements",
		description: "Recent fee-bump replacement events, where a pending blob transaction was replaced by a higher-fee version. Frequent replacements indicate senders competing for scarce blob space.",
		register: registerTool(func(in replacementsInput) (string, url.Values, error) {
			q := newQuery(in.Network)
			setString(q, "tx_hash", in.TxHash)
			setInt(q, "limit", in.Limit)
			setInt(q, "offset", in.Offset)
			return "/blob/replacements", q, nil
		}),
	},
	{
		name:        toolGetBlock,
		title:       "Block detail",
		description: "One indexed block with its blob metrics (blob count, base fee, excess blob gas) and the blob transactions it contains.",
		register: registerTool(func(in blockInput) (string, url.Values, error) {
			return "/block/" + strconv.FormatUint(in.Number, 10), newQuery(in.Network), nil
		}),
	},
	{
		name:        toolGetBlobByTxHash,
		title:       "Blob by transaction hash",
		description: "Look up one blob transaction by its transaction hash, including sender, attribution, fees, and versioned blob hashes.",
		register: registerTool(func(in txHashInput) (string, url.Values, error) {
			hash, err := requirePathValue("tx_hash", in.TxHash)
			if err != nil {
				return "", nil, err
			}
			return "/blob/" + hash, newQuery(in.Network), nil
		}),
	},
	{
		name:        toolGetBlobByVersionedHash,
		title:       "Blob by versioned hash",
		description: "Look up the blob transaction that carried a given EIP-4844 versioned blob hash.",
		register: registerTool(func(in versionedHashInput) (string, url.Values, error) {
			hash, err := requirePathValue("versioned_hash", in.VersionedHash)
			if err != nil {
				return "", nil, err
			}
			return "/blob/by-hash/" + hash, newQuery(in.Network), nil
		}),
	},
	{
		name:        toolSearch,
		title:       "Search",
		description: "Typed search across blocks, transaction hashes, versioned hashes, sender addresses, and rollup names. Use it when you have an identifier but do not know what it is.",
		register: registerTool(func(in searchInput) (string, url.Values, error) {
			query := strings.TrimSpace(in.Query)
			if query == "" {
				return "", nil, errors.New("query is required")
			}
			q := newQuery(in.Network)
			q.Set("q", query)
			return "/search", q, nil
		}),
	},
}

func chartQuery(in chartInput) url.Values {
	q := newQuery(in.Network)
	setString(q, "range", in.Range)
	setString(q, "granularity", in.Granularity)
	setInt(q, "limit", in.Limit)
	return q
}

func seriesChartQuery(in seriesChartInput) url.Values {
	q := newQuery(in.Network)
	setString(q, "range", in.Range)
	setString(q, "granularity", in.Granularity)
	setInt(q, "limit", in.Limit)
	return q
}

func blobListQuery(in blobListInput) url.Values {
	q := newQuery(in.Network)
	setInt(q, "limit", in.Limit)
	setInt(q, "offset", in.Offset)
	setString(q, "from", in.From)
	setString(q, "entity", in.Entity)
	return q
}
