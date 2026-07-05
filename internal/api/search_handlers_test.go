package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/lib/pq"

	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

const (
	testSearchTxHash        = "0x01aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testSearchVersionedHash = "0x01bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// searchTestDB builds a mockDB whose getFn/selectFn dispatch on the search
// query being executed. Nil handlers report sql.ErrNoRows (no match).
type searchTestDB struct {
	blockFn  func(dest *int64, args []interface{}) error
	txFn     func(dest *searchTxRow, args []interface{}) error
	blobFn   func(dest *searchBlobRow, args []interface{}) error
	senderFn func(dest *searchSenderRow, args []interface{}) error
	rollupFn func(dest *[]searchRollupRow, args []interface{}) error
}

func (s *searchTestDB) mock(t *testing.T) *mockDB {
	t.Helper()
	return &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch d := dest.(type) {
			case *int64:
				if !strings.Contains(query, "FROM block_metrics") {
					t.Fatalf("unexpected int64 query: %s", query)
				}
				if s.blockFn == nil {
					return sql.ErrNoRows
				}
				return s.blockFn(d, args)
			case *searchTxRow:
				if s.txFn == nil {
					return sql.ErrNoRows
				}
				return s.txFn(d, args)
			case *searchBlobRow:
				if s.blobFn == nil {
					return sql.ErrNoRows
				}
				return s.blobFn(d, args)
			case *searchSenderRow:
				if s.senderFn == nil {
					return sql.ErrNoRows
				}
				return s.senderFn(d, args)
			default:
				t.Fatalf("unexpected GetContext dest type %T", dest)
				return nil
			}
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			rollups, ok := dest.(*[]searchRollupRow)
			if !ok {
				t.Fatalf("unexpected SelectContext dest type %T", dest)
			}
			if s.rollupFn == nil {
				return nil
			}
			return s.rollupFn(rollups, args)
		},
	}
}

// doSearch runs the handler and decodes the data array generically so tests
// can assert both present fields and omitted keys.
func doSearch(t *testing.T, a *API, url string) (w *httptest.ResponseRecorder, data []map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, http.NoBody)
	w = httptest.NewRecorder()
	a.Search(w, req)

	if w.Code != http.StatusOK {
		return w, nil
	}
	var resp struct {
		Success bool                     `json:"success"`
		Data    []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
	return w, resp.Data
}

func TestSearch_MissingQ(t *testing.T) {
	a := newTestAPI()
	for _, url := range []string{"/", "/?q=", "/?q=%20%20"} {
		req := httptest.NewRequest(http.MethodGet, url, http.NoBody)
		w := httptest.NewRecorder()
		a.Search(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("url %q: expected 400, got %d", url, w.Code)
		}
	}
}

func TestSearch_BadNetwork(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?q=123&network=unknown", http.NoBody)
	w := httptest.NewRecorder()
	a.Search(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestSearch_BlockFound(t *testing.T) {
	db := &searchTestDB{
		blockFn: func(dest *int64, args []interface{}) error {
			if args[0] != 42 || args[1] != int64(19000000) {
				t.Errorf("unexpected block args: %v", args)
			}
			*dest = 19000000
			return nil
		},
	}
	a := newTestAPIWithDB(db.mock(t))

	// Comma group separators are accepted.
	w, data := doSearch(t, a, "/?q=19%2C000%2C000")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(data) != 1 {
		t.Fatalf("expected 1 match, got %d", len(data))
	}
	if data[0]["type"] != "block" || data[0]["block_number"] != float64(19000000) {
		t.Errorf("unexpected match: %v", data[0])
	}
	wantCache := fmt.Sprintf("public, max-age=%d, s-maxage=%d",
		int(searchCacheTTL.Seconds()), int(searchEdgeTTL.Seconds()))
	if got := w.Header().Get("Cache-Control"); got != wantCache {
		t.Errorf("Cache-Control = %q, want %q", got, wantCache)
	}
}

func TestSearch_BlockNotIndexed(t *testing.T) {
	a := newTestAPIWithDB((&searchTestDB{}).mock(t))
	w, data := doSearch(t, a, "/?q=999999999")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty matches, got %v", data)
	}
	// The contract is an empty array, not null (and not an omitted data key).
	if !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Errorf("expected empty data array in body, got %s", w.Body.String())
	}
}

func TestSearch_HashResolvesBothTxAndBlob(t *testing.T) {
	db := &searchTestDB{
		txFn: func(dest *searchTxRow, args []interface{}) error {
			// Input is normalized to lowercase before the lookup.
			if args[0] != 42 || args[1] != testSearchTxHash {
				t.Errorf("unexpected tx args: %v", args)
			}
			dest.TxHash = testSearchTxHash
			dest.BlockNumber = 123
			return nil
		},
		blobFn: func(dest *searchBlobRow, args []interface{}) error {
			if args[0] != 42 || args[1] != testSearchTxHash {
				t.Errorf("unexpected blob args: %v", args)
			}
			dest.VersionedHash = testSearchTxHash
			dest.TxHash = "0xcc"
			dest.BlockNumber = -1 // still pending
			return nil
		},
	}
	a := newTestAPIWithDB(db.mock(t))

	// Uppercase hex input must be normalized to the lowercase storage form.
	w, data := doSearch(t, a, "/?q=0x"+strings.ToUpper(testSearchTxHash[2:]))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(data) != 2 {
		t.Fatalf("expected 2 matches, got %d: %v", len(data), data)
	}

	tx := data[0]
	if tx["type"] != "transaction" || tx["tx_hash"] != testSearchTxHash || tx["block_number"] != float64(123) {
		t.Errorf("unexpected transaction match: %v", tx)
	}

	blob := data[1]
	if blob["type"] != "blob" || blob["versioned_hash"] != testSearchTxHash || blob["tx_hash"] != "0xcc" {
		t.Errorf("unexpected blob match: %v", blob)
	}
	// Pending blob: block_number must be omitted, not null or -1.
	if _, present := blob["block_number"]; present {
		t.Errorf("expected block_number omitted for pending blob, got %v", blob["block_number"])
	}
}

func TestSearch_HashBlobOnly(t *testing.T) {
	db := &searchTestDB{
		blobFn: func(dest *searchBlobRow, args []interface{}) error {
			dest.VersionedHash = testSearchVersionedHash
			dest.TxHash = testSearchTxHash
			dest.BlockNumber = 456
			return nil
		},
	}
	a := newTestAPIWithDB(db.mock(t))

	_, data := doSearch(t, a, "/?q="+testSearchVersionedHash)
	if len(data) != 1 {
		t.Fatalf("expected 1 match, got %d: %v", len(data), data)
	}
	blob := data[0]
	if blob["type"] != "blob" || blob["versioned_hash"] != testSearchVersionedHash ||
		blob["tx_hash"] != testSearchTxHash || blob["block_number"] != float64(456) {
		t.Errorf("unexpected blob match: %v", blob)
	}
}

func TestSearch_HashNoMatches(t *testing.T) {
	a := newTestAPIWithDB((&searchTestDB{}).mock(t))
	_, data := doSearch(t, a, "/?q="+testSearchTxHash)
	if len(data) != 0 {
		t.Fatalf("expected empty matches, got %v", data)
	}
}

func TestSearch_AddressFound(t *testing.T) {
	// blobs.from_address is stored in EIP-55 checksummed form.
	checksummed := common.HexToAddress(validTestAddress).Hex()
	db := &searchTestDB{
		senderFn: func(dest *searchSenderRow, args []interface{}) error {
			// Lookup must use the checksummed storage form regardless of the
			// input casing.
			if args[0] != 42 || args[1] != checksummed {
				t.Errorf("unexpected sender args: %v", args)
			}
			dest.FromAddress = checksummed
			dest.UserAttribution = "base"
			return nil
		},
	}
	a := newTestAPIWithDB(db.mock(t))

	_, data := doSearch(t, a, "/?q="+strings.ToUpper(validTestAddress[2:]))
	if len(data) != 0 {
		t.Fatalf("expected no matches without 0x prefix, got %v", data)
	}

	_, data = doSearch(t, a, "/?q="+validTestAddress)
	if len(data) != 1 {
		t.Fatalf("expected 1 match, got %d: %v", len(data), data)
	}
	match := data[0]
	if match["type"] != "address" || match["address"] != checksummed || match["user_attribution"] != "base" {
		t.Errorf("unexpected address match: %v", match)
	}
}

func TestSearch_AddressNotASender(t *testing.T) {
	a := newTestAPIWithDB((&searchTestDB{}).mock(t))
	_, data := doSearch(t, a, "/?q="+validTestAddress)
	if len(data) != 0 {
		t.Fatalf("expected empty matches, got %v", data)
	}
}

func TestSearch_RollupPrefix(t *testing.T) {
	db := &searchTestDB{
		rollupFn: func(dest *[]searchRollupRow, args []interface{}) error {
			if args[0] != 42 || args[1] != "base%" || args[2] != maxSearchRollupMatches {
				t.Errorf("unexpected rollup args: %v", args)
			}
			*dest = []searchRollupRow{
				{Name: "Base", Addresses: pq.StringArray{"0xaa", "0xbb"}},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db.mock(t))

	// Case-insensitive: uppercase input must produce a lowercase pattern.
	_, data := doSearch(t, a, "/?q=BAse")
	if len(data) != 1 {
		t.Fatalf("expected 1 match, got %d: %v", len(data), data)
	}
	match := data[0]
	if match["type"] != "rollup" || match["name"] != "Base" {
		t.Errorf("unexpected rollup match: %v", match)
	}
	addresses, ok := match["addresses"].([]interface{})
	if !ok || len(addresses) != 2 || addresses[0] != "0xaa" || addresses[1] != "0xbb" {
		t.Errorf("unexpected rollup addresses: %v", match["addresses"])
	}
}

func TestSearch_RollupPatternEscaped(t *testing.T) {
	var gotPattern string
	db := &searchTestDB{
		rollupFn: func(dest *[]searchRollupRow, args []interface{}) error {
			gotPattern, _ = args[1].(string)
			return nil
		},
	}
	a := newTestAPIWithDB(db.mock(t))

	_, data := doSearch(t, a, "/?q="+"a%25b_c%5Cd") // a%b_c\d
	if len(data) != 0 {
		t.Fatalf("expected empty matches, got %v", data)
	}
	if gotPattern != `a\%b\_c\\d%` {
		t.Errorf("pattern = %q, want %q", gotPattern, `a\%b\_c\\d%`)
	}
}

func TestSearch_MalformedHexNoDBCall(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			t.Error("unexpected GetContext call")
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			t.Error("unexpected SelectContext call")
			return nil
		},
	}
	a := newTestAPIWithDB(db)

	// Truncated hash, odd-length hex, and over-long input all short-circuit.
	for _, q := range []string{"0x123abc", "0X" + strings.Repeat("a", 63), "0x" + strings.Repeat("a", 65), strings.Repeat("z", maxSearchQueryLength+1)} {
		_, data := doSearch(t, a, "/?q="+q)
		if len(data) != 0 {
			t.Errorf("q=%q: expected empty matches, got %v", q, data)
		}
	}
}

func TestSearch_DBError(t *testing.T) {
	db := &searchTestDB{
		blockFn: func(dest *int64, args []interface{}) error {
			return fmt.Errorf("db down")
		},
	}
	a := newTestAPIWithDB(db.mock(t))
	req := httptest.NewRequest(http.MethodGet, "/?q=100", http.NoBody)
	w := httptest.NewRecorder()
	a.Search(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestSearch_DBTimeout(t *testing.T) {
	db := &searchTestDB{
		txFn: func(dest *searchTxRow, args []interface{}) error {
			return context.DeadlineExceeded
		},
	}
	a := newTestAPIWithDB(db.mock(t))
	req := httptest.NewRequest(http.MethodGet, "/?q="+testSearchTxHash, http.NoBody)
	w := httptest.NewRecorder()
	a.Search(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want %q", got, "5")
	}
}

func TestSearch_BlobLookupError(t *testing.T) {
	db := &searchTestDB{
		txFn: func(dest *searchTxRow, args []interface{}) error {
			dest.TxHash = testSearchTxHash
			dest.BlockNumber = 1
			return nil
		},
		blobFn: func(dest *searchBlobRow, args []interface{}) error {
			return fmt.Errorf("db down")
		},
	}
	a := newTestAPIWithDB(db.mock(t))
	req := httptest.NewRequest(http.MethodGet, "/?q="+testSearchTxHash, http.NoBody)
	w := httptest.NewRecorder()
	a.Search(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestSearch_RollupLookupError(t *testing.T) {
	db := &searchTestDB{
		rollupFn: func(dest *[]searchRollupRow, args []interface{}) error {
			return fmt.Errorf("db down")
		},
	}
	a := newTestAPIWithDB(db.mock(t))
	req := httptest.NewRequest(http.MethodGet, "/?q=base", http.NoBody)
	w := httptest.NewRecorder()
	a.Search(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestParseSearchBlockNumber(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true},
		{"19000000", 19000000, true},
		{"19,000,000", 19000000, true},
		{"1,2,3", 123, true}, // lenient comma placement
		{",", 0, false},
		{"", 0, false},
		{"-5", 0, false},
		{"12.5", 0, false},
		{"12abc", 0, false},
		{"9223372036854775807", 9223372036854775807, true},
		{"9223372036854775808", 0, false}, // int64 overflow
	}
	for _, tt := range tests {
		got, ok := parseSearchBlockNumber(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseSearchBlockNumber(%q) = (%d, %v), want (%d, %v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestEscapeLikePattern(t *testing.T) {
	tests := []struct{ in, want string }{
		{"base", "base"},
		{"100%", `100\%`},
		{"a_b", `a\_b`},
		{`a\b`, `a\\b`},
		{`%_\`, `\%\_\\`},
	}
	for _, tt := range tests {
		if got := escapeLikePattern(tt.in); got != tt.want {
			t.Errorf("escapeLikePattern(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
