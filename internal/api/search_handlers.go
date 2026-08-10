package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/lib/pq"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// Search match type discriminators returned in SearchMatchResponse.Type. Part
// of the API contract: the blob-flow search modal branches on them.
const (
	searchTypeBlock       = "block"
	searchTypeTransaction = "transaction"
	searchTypeBlob        = "blob"
	searchTypeAddress     = "address"
	searchTypeRollup      = "rollup"
)

// maxSearchQueryLength bounds the accepted search input. The longest
// resolvable shapes are 66-char 0x-hashes; anything longer can only be an
// unmatchable rollup-name prefix, so it short-circuits to no matches without
// touching the database.
const maxSearchQueryLength = 128

// maxSearchRollupMatches caps rollup-name prefix matches: the search modal
// shows a handful of suggestions, and short prefixes ("a") would otherwise
// return the whole known-rollup list.
const maxSearchRollupMatches = 10

// searchCacheTTL / searchEdgeTTL keep /search responses briefly cacheable so
// the edge can coalesce identical debounced queries, while a block search for
// a just-indexed height turns positive within roughly one slot.
const (
	searchCacheTTL = 5 * time.Second
	searchEdgeTTL  = 5 * time.Second
)

// SearchMatchResponse is one typed match resolved from a /search query. Type
// discriminates which of the remaining fields are populated.
type SearchMatchResponse struct {
	Type string `json:"type" enums:"block,transaction,blob,address,rollup" example:"transaction"`
	// BlockNumber is set on block, transaction, and blob matches; it is
	// omitted for pending (mempool) transactions and blobs not yet included
	// in a block.
	BlockNumber *int64 `json:"block_number,omitempty" example:"19000000"`
	// TxHash is set on transaction and blob matches.
	TxHash string `json:"tx_hash,omitempty"`
	// VersionedHash is set on blob matches.
	VersionedHash string `json:"versioned_hash,omitempty"`
	// Address is set on address matches, in EIP-55 checksummed form.
	Address string `json:"address,omitempty"`
	// UserAttribution is set on address matches when the sender is a known
	// rollup.
	UserAttribution string `json:"user_attribution,omitempty"`
	// Name and Addresses are set on rollup matches.
	Name      string   `json:"name,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
}

type searchTxRow struct {
	TxHash      string `db:"tx_hash"`
	BlockNumber int64  `db:"block_number"`
}

type searchBlobRow struct {
	VersionedHash string `db:"versioned_hash"`
	TxHash        string `db:"tx_hash"`
	BlockNumber   int64  `db:"block_number"`
}

type searchSenderRow struct {
	FromAddress     string `db:"from_address"`
	UserAttribution string `db:"user_attribution"`
}

type searchRollupRow struct {
	Name      string         `db:"name"`
	Addresses pq.StringArray `db:"addresses"`
}

// Search godoc
// @Summary Resolve a search query into typed matches
// @Description Resolves a free-form search query against indexed data. Decimal integers (comma group separators allowed) match indexed block heights. 0x-prefixed 64-hex values are tried as a blob transaction hash and as an EIP-4844 blob versioned hash — both matches are returned when both resolve. 0x-prefixed 40-hex values match addresses seen as blob senders. Anything else is a case-insensitive prefix match against known rollup attribution names; a rollup match's addresses array carries every address the attribution registry ties to that entity — retired senders whose historical blobs still carry the attribution as well as active ones — so any sender /users attributes to the entity is included (registry addresses with no indexed blobs yet may also appear). Returns an empty array (not 404) when nothing matches. block_number is omitted on matches still pending in the mempool. Versioned hashes are recorded from the versioned-hash migration onward, so older blobs resolve by transaction hash only.
// @Tags search
// @Accept json
// @Produce json
// @Param q query string true "Search query: block height, 0x-hash, address, or rollup-name prefix"
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Success 200 {object} Response{data=[]SearchMatchResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
// @Router /search [get]
func (a *API) Search(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		a.respondError(w, http.StatusBadRequest, "Missing q parameter")
		return
	}

	logger.Debug("Searching",
		zap.String("network", network.Name),
		zap.Int("query_length", len(query)))

	queryCtx, cancel := context.WithTimeout(r.Context(), aggregateQueryTimeout)
	defer cancel()

	matches, err := a.resolveSearchMatches(queryCtx, network, query)
	if err != nil {
		logger.Error("Failed to search",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondAggregateError(w, err, "Failed to search")
		return
	}

	setCacheControl(w, searchCacheTTL, searchEdgeTTL)
	a.respondSuccess(w, matches)
}

// resolveSearchMatches classifies the query by shape and runs the indexed
// lookups for that shape only. Unmatchable input yields an empty slice, never
// an error: for a type-ahead endpoint, "no results" is the normal outcome of
// most keystrokes.
func (a *API) resolveSearchMatches(ctx context.Context, network config.NetworkConfig, query string) ([]SearchMatchResponse, error) {
	matches := make([]SearchMatchResponse, 0, 2)
	if len(query) > maxSearchQueryLength {
		return matches, nil
	}

	if blockNumber, ok := parseSearchBlockNumber(query); ok {
		return a.searchBlock(ctx, network, blockNumber, matches)
	}

	if strings.HasPrefix(query, "0x") || strings.HasPrefix(query, "0X") {
		if common.IsHexHash(query) {
			return a.searchHash(ctx, network, strings.ToLower(query), matches)
		}
		if common.IsHexAddress(query) {
			return a.searchSender(ctx, network, common.HexToAddress(query).Hex(), matches)
		}
		// A 0x prefix that is neither a hash nor an address can only be a
		// truncated or malformed identifier; rollup names never start with 0x.
		return matches, nil
	}

	return a.searchRollups(ctx, network, query, matches)
}

func (a *API) searchBlock(ctx context.Context, network config.NetworkConfig, blockNumber int64, matches []SearchMatchResponse) ([]SearchMatchResponse, error) {
	var indexed int64
	err := a.db.GetContext(ctx, &indexed, querySearchBlockByNumber, network.ChainID, blockNumber)
	if errors.Is(err, sql.ErrNoRows) {
		return matches, nil
	}
	if err != nil {
		return nil, err
	}
	return append(matches, SearchMatchResponse{
		Type:        searchTypeBlock,
		BlockNumber: &indexed,
	}), nil
}

// searchHash tries a 64-hex query as a transaction hash first, then as a blob
// versioned hash, returning both matches when both resolve. Versioned hashes
// always start 0x01, but a transaction hash can too, so the two probes are
// independent index lookups rather than prefix-gated.
func (a *API) searchHash(ctx context.Context, network config.NetworkConfig, hash string, matches []SearchMatchResponse) ([]SearchMatchResponse, error) {
	var tx searchTxRow
	err := a.db.GetContext(ctx, &tx, querySearchTxByHash, network.ChainID, hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		matches = append(matches, SearchMatchResponse{
			Type:        searchTypeTransaction,
			TxHash:      tx.TxHash,
			BlockNumber: searchBlockNumberPtr(tx.BlockNumber),
		})
	}

	var blob searchBlobRow
	err = a.db.GetContext(ctx, &blob, querySearchBlobByVersionedHash, network.ChainID, hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		matches = append(matches, SearchMatchResponse{
			Type:          searchTypeBlob,
			VersionedHash: blob.VersionedHash,
			TxHash:        blob.TxHash,
			BlockNumber:   searchBlockNumberPtr(blob.BlockNumber),
		})
	}

	return matches, nil
}

func (a *API) searchSender(ctx context.Context, network config.NetworkConfig, address string, matches []SearchMatchResponse) ([]SearchMatchResponse, error) {
	var sender searchSenderRow
	err := a.db.GetContext(ctx, &sender, querySearchSenderByAddress, network.ChainID, address)
	if errors.Is(err, sql.ErrNoRows) {
		return matches, nil
	}
	if err != nil {
		return nil, err
	}
	return append(matches, SearchMatchResponse{
		Type:            searchTypeAddress,
		Address:         sender.FromAddress,
		UserAttribution: sender.UserAttribution,
	}), nil
}

func (a *API) searchRollups(ctx context.Context, network config.NetworkConfig, query string, matches []SearchMatchResponse) ([]SearchMatchResponse, error) {
	pattern := escapeLikePattern(strings.ToLower(query)) + "%"

	var rollups []searchRollupRow
	if err := a.db.SelectContext(ctx, &rollups, querySearchRollupsByName, network.ChainID, pattern, maxSearchRollupMatches); err != nil {
		return nil, err
	}
	for _, rollup := range rollups {
		matches = append(matches, SearchMatchResponse{
			Type:      searchTypeRollup,
			Name:      rollup.Name,
			Addresses: rollup.Addresses,
		})
	}
	return matches, nil
}

// parseSearchBlockNumber interprets the query as a decimal block height.
// Comma group separators are accepted anywhere ("19,000,000"); values beyond
// int64 cannot be block heights and report false rather than erroring, so
// they fall through to the (unmatchable) free-text arm.
func parseSearchBlockNumber(query string) (int64, bool) {
	digits := strings.ReplaceAll(query, ",", "")
	if digits == "" {
		return 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	blockNumber, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return blockNumber, true
}

// searchBlockNumberPtr converts the internal pending block-number sentinel to
// the wire representation: nil (omitted) for pending rows, mirroring
// BlobResponse.BlockNumber.
func searchBlockNumberPtr(blockNumber int64) *int64 {
	if blockNumber < 0 {
		return nil
	}
	return &blockNumber
}

// escapeLikePattern escapes LIKE metacharacters in user input so it matches
// literally inside a LIKE pattern (the queries declare ESCAPE '\').
func escapeLikePattern(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(s)
}
