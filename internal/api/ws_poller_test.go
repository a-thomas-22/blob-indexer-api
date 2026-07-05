package api

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func testNetworks() map[int]config.NetworkConfig {
	return map[int]config.NetworkConfig{
		11155111: {Name: "sepolia", ChainID: 11155111, Enabled: true},
	}
}

// collectBroadcasts drains the hub's broadcast channel for a duration and
// returns all events received by a test client on the "sepolia" network.
func collectBroadcasts(t *testing.T, hub *Hub, wait time.Duration) []WSEvent {
	t.Helper()
	client := &Client{
		hub:         hub,
		send:        make(chan []byte, 256),
		networkName: "sepolia",
	}
	hub.register <- client
	time.Sleep(20 * time.Millisecond)

	// Wait for events.
	time.Sleep(wait)

	var events []WSEvent
	for {
		select {
		case msg := <-client.send:
			var e WSEvent
			if err := json.Unmarshal(msg, &e); err == nil {
				events = append(events, e)
			}
		default:
			return events
		}
	}
}

func TestPoller_DetectsNewBlock(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	blockNumber := uint64(100)

	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if strings.Contains(query, "indexer_metadata") {
				v := dest.(*string)
				*v = "100"
			}
			if strings.Contains(query, "total_blobs") {
				s := dest.(*models.BlobStatsAggregate)
				*s = models.BlobStatsAggregate{TotalBlobs: 10}
			}
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch out := dest.(type) {
			case *[]models.Blob:
				if strings.Contains(query, "block_number >") {
					*out = []models.Blob{
						{
							ChainID:           11155111,
							BlockNumber:       int64(blockNumber),
							TxHash:            "0xabc",
							BaseFeePerBlobGas: "1000",
							TipPerBlobGas:     "100",
							TotalCostWei:      "0.001",
							Timestamp:         time.Now(),
							Confirmed:         true,
						},
					}
				} else if strings.Contains(query, "confirmed = false") {
					*out = []models.Blob{}
				}
			case *[]models.BlobUserStats:
				*out = []models.BlobUserStats{}
			}
			return nil
		},
	}

	poller := NewPoller(db, hub, testNetworks(), 50*time.Millisecond, 50*time.Millisecond)
	// Seed last seen block so the poller detects the "new" block.
	poller.lastSeenBlocks[11155111] = 99

	ctx, cancel := context.WithCancel(context.Background())
	go poller.Run(ctx)

	events := collectBroadcasts(t, hub, 200*time.Millisecond)
	cancel()

	foundNewBlock := false
	foundStats := false
	for _, e := range events {
		if e.Type == EventNewBlock {
			foundNewBlock = true
		}
		if e.Type == EventStatsUpdate {
			foundStats = true
		}
	}

	if !foundNewBlock {
		t.Error("expected new_block event")
	}
	if !foundStats {
		t.Error("expected stats_update event")
	}
}

func TestPoller_NoChange_NoBroadcast(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if strings.Contains(query, "indexer_metadata") {
				v := dest.(*string)
				*v = "100"
			}
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if query == queryPendingBlobTxHashes {
				hashes := dest.(*[]string)
				*hashes = []string{}
			}
			return nil
		},
	}

	poller := NewPoller(db, hub, testNetworks(), 50*time.Millisecond, time.Hour)
	poller.lastSeenBlocks[11155111] = 100 // same as DB returns

	ctx, cancel := context.WithCancel(context.Background())
	go poller.Run(ctx)

	events := collectBroadcasts(t, hub, 200*time.Millisecond)
	cancel()

	for _, e := range events {
		if e.Type == EventNewBlock {
			t.Error("should not broadcast new_block when block number unchanged")
		}
	}
}

func TestPoller_UsersThrottle(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	callCount := 0

	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if strings.Contains(query, "indexer_metadata") {
				callCount++
				v := dest.(*string)
				// Increment block each call so poller detects new blocks.
				*v = "100"
				if callCount > 1 {
					*v = "101"
				}
				if callCount > 2 {
					*v = "102"
				}
			}
			if strings.Contains(query, "total_blobs") {
				s := dest.(*models.BlobStatsAggregate)
				*s = models.BlobStatsAggregate{TotalBlobs: 10}
			}
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch out := dest.(type) {
			case *[]models.Blob:
				if strings.Contains(query, "block_number >") {
					*out = []models.Blob{{
						ChainID:           11155111,
						BlockNumber:       101,
						TxHash:            "0xdef",
						BaseFeePerBlobGas: "1000",
						TipPerBlobGas:     "100",
						TotalCostWei:      "0.001",
						Timestamp:         time.Now(),
						Confirmed:         true,
					}}
				} else if strings.Contains(query, "confirmed = false") {
					*out = []models.Blob{}
				}
			case *[]models.BlobUserStats:
				*out = []models.BlobUserStats{}
			}
			return nil
		},
	}

	// Very long throttle — users_update should fire at most once.
	poller := NewPoller(db, hub, testNetworks(), 50*time.Millisecond, 10*time.Second)
	poller.lastSeenBlocks[11155111] = 99

	ctx, cancel := context.WithCancel(context.Background())
	go poller.Run(ctx)

	events := collectBroadcasts(t, hub, 300*time.Millisecond)
	cancel()

	usersCount := 0
	for _, e := range events {
		if e.Type == EventUsersUpdate {
			usersCount++
		}
	}
	if usersCount > 1 {
		t.Errorf("got %d users_update events, expected at most 1 due to throttle", usersCount)
	}
}

func TestPoller_ContextCancel(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}

	poller := NewPoller(db, hub, testNetworks(), 10*time.Millisecond, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		poller.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// OK — poller stopped.
	case <-time.After(2 * time.Second):
		t.Fatal("poller did not stop after context cancellation")
	}
}

func TestPoller_MempoolDiff_AddAndRemove(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	pollCycle := 0

	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if strings.Contains(query, "indexer_metadata") {
				v := dest.(*string)
				*v = "100"
			}
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch query {
			case queryPendingBlobTxHashes:
				hashes := dest.(*[]string)
				pollCycle++
				if pollCycle <= 1 {
					// First poll: one pending tx (baseline, no broadcast).
					*hashes = []string{"0x111"}
				} else {
					// Later polls: 0x111 gone (removed), 0x222 appeared (added).
					*hashes = []string{"0x222"}
				}
			case queryPendingBlobsByTxHashes:
				blobs := dest.(*[]models.Blob)
				*blobs = []models.Blob{
					{ChainID: 11155111, TxHash: "0x222", BaseFeePerBlobGas: "0", TipPerBlobGas: "0", TotalCostWei: "0"},
				}
			}
			return nil
		},
	}

	poller := NewPoller(db, hub, testNetworks(), 50*time.Millisecond, time.Hour)
	poller.lastSeenBlocks[11155111] = 100

	ctx, cancel := context.WithCancel(context.Background())
	go poller.Run(ctx)

	events := collectBroadcasts(t, hub, 250*time.Millisecond)
	cancel()

	addCount := 0
	removeCount := 0
	for _, e := range events {
		if e.Type == EventMempoolUpdate {
			data, _ := json.Marshal(e.Data)
			var m MempoolUpdateData
			if json.Unmarshal(data, &m) == nil {
				if m.Action == "add" {
					addCount++
				}
				if m.Action == "remove" {
					removeCount++
				}
			}
		}
	}

	if addCount == 0 {
		t.Error("expected at least one mempool add event")
	}
	if removeCount == 0 {
		t.Error("expected at least one mempool remove event")
	}
}

func TestPoller_QueryLastIndexedBlock_ErrorReturnsZero(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	poller := NewPoller(db, nil, testNetworks(), time.Second, time.Second)
	got := poller.queryLastIndexedBlock(context.Background(), 11155111)
	if got != 0 {
		t.Errorf("got %d, want 0 on error", got)
	}
}

func TestPoller_QueryLastIndexedBlock_InvalidValue(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			v := dest.(*string)
			*v = "not-a-number"
			return nil
		},
	}
	poller := NewPoller(db, nil, testNetworks(), time.Second, time.Second)
	got := poller.queryLastIndexedBlock(context.Background(), 11155111)
	if got != 0 {
		t.Errorf("got %d, want 0 on parse error", got)
	}
}

func TestPoller_BroadcastStatsUpdate_Error(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if strings.Contains(query, "total_blobs") {
				return fmt.Errorf("stats query error")
			}
			if strings.Contains(query, "indexer_metadata") {
				v := dest.(*string)
				*v = "100"
			}
			return nil
		},
	}

	network := config.NetworkConfig{Name: "sepolia", ChainID: 11155111}
	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	// Should not panic on error.
	poller.broadcastStatsUpdate(context.Background(), network)
}

func TestPoller_BroadcastUsersUpdate_Error(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("users query error")
		},
	}

	network := config.NetworkConfig{Name: "sepolia", ChainID: 11155111}
	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	// Should not panic on error.
	poller.broadcastUsersUpdate(context.Background(), network)
}

func TestPoller_BroadcastUsersUpdate_Success(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	var gotQuery string
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			gotQuery = query
			gotArgs = append([]interface{}{}, args...)
			if users, ok := dest.(*[]models.BlobUserStats); ok {
				*users = []models.BlobUserStats{
					{
						Address:           "0xabc",
						Name:              "Test",
						Category:          "rollup",
						BlobCount:         5,
						TotalCostWei:      "0.01",
						LastTimestamp:     time.Now(),
						BlobSharePercent:  25,
						SpendSharePercent: 40,
					},
				}
			}
			return nil
		},
	}

	client := &Client{hub: hub, send: make(chan []byte, 256), networkName: "sepolia"}
	hub.register <- client
	time.Sleep(20 * time.Millisecond)

	network := config.NetworkConfig{Name: "sepolia", ChainID: 11155111}
	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	poller.broadcastUsersUpdate(context.Background(), network)

	if gotQuery != queryTopBlobUsersAllByCount {
		t.Fatal("expected poller to use all-window users rollup query")
	}
	wantArgs := []interface{}{11155111, 10, 0, "all"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("got args %v, want %v", gotArgs, wantArgs)
	}

	select {
	case msg := <-client.send:
		var event WSEvent
		if err := json.Unmarshal(msg, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type != EventUsersUpdate {
			t.Errorf("got type %q, want %q", event.Type, EventUsersUpdate)
		}
		data, ok := event.Data.([]interface{})
		if !ok || len(data) != 1 {
			t.Fatalf("unexpected users_update data: %#v", event.Data)
		}
		user, ok := data[0].(map[string]interface{})
		if !ok {
			t.Fatalf("unexpected user payload: %#v", data[0])
		}
		if user["category"] != "rollup" || user["blob_share_percent"] != float64(25) || user["spend_share_percent"] != float64(40) {
			t.Fatalf("missing enriched user fields: %#v", user)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for users_update")
	}
}

func TestPoller_BroadcastStatsUpdate_Success(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if strings.Contains(query, "total_blobs") {
				s := dest.(*models.BlobStatsAggregate)
				*s = models.BlobStatsAggregate{TotalBlobs: 42, TotalConfirmedBlobs: 40}
			}
			if strings.Contains(query, "indexer_metadata") {
				v := dest.(*string)
				*v = "200"
			}
			return nil
		},
	}

	client := &Client{hub: hub, send: make(chan []byte, 256), networkName: "sepolia"}
	hub.register <- client
	time.Sleep(20 * time.Millisecond)

	network := config.NetworkConfig{Name: "sepolia", ChainID: 11155111}
	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	poller.broadcastStatsUpdate(context.Background(), network)

	select {
	case msg := <-client.send:
		var event WSEvent
		if err := json.Unmarshal(msg, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type != EventStatsUpdate {
			t.Errorf("got type %q, want %q", event.Type, EventStatsUpdate)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stats_update")
	}
}

func TestPoller_BroadcastNewBlocks_Error(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("query error")
		},
	}

	network := config.NetworkConfig{Name: "sepolia", ChainID: 11155111}
	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	// Should return false on error.
	if poller.broadcastNewBlocks(context.Background(), network, 99) {
		t.Error("expected broadcastNewBlocks to return false on error")
	}
}

func TestPoller_BroadcastNewBlocks_EmptyResult(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			blobs := dest.(*[]models.Blob)
			*blobs = []models.Blob{}
			return nil
		},
	}

	client := &Client{hub: hub, send: make(chan []byte, 256), networkName: "sepolia"}
	hub.register <- client
	time.Sleep(20 * time.Millisecond)

	network := config.NetworkConfig{Name: "sepolia", ChainID: 11155111}
	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	if !poller.broadcastNewBlocks(context.Background(), network, 99) {
		t.Error("expected broadcastNewBlocks to return true for empty result")
	}

	time.Sleep(50 * time.Millisecond)
	select {
	case <-client.send:
		t.Error("should not broadcast when no blobs found")
	default:
	}
}

func TestPoller_BroadcastNewBlocks_IncludesPricing(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	blockTime := time.Date(2026, 3, 9, 14, 0, 0, 0, time.UTC)
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch {
			case strings.Contains(query, "FROM blobs"):
				blobs := dest.(*[]models.Blob)
				blobGasUsed := int64(131072)
				*blobs = []models.Blob{
					{
						ChainID:           11155111,
						BlockNumber:       100,
						BlobIndex:         0,
						TxHash:            "0xabc",
						BaseFeePerBlobGas: "4200000",
						TipPerBlobGas:     "100",
						TotalCostWei:      "550502400000",
						Timestamp:         blockTime,
						Confirmed:         true,
						BlobGasUsed:       &blobGasUsed,
					},
					{
						ChainID:           11155111,
						BlockNumber:       100,
						BlobIndex:         1,
						TxHash:            "0xdef",
						BaseFeePerBlobGas: "4200000",
						TipPerBlobGas:     "100",
						TotalCostWei:      "550502400000",
						Timestamp:         blockTime,
						Confirmed:         true,
						BlobGasUsed:       &blobGasUsed,
					},
				}
			case strings.Contains(query, "FROM block_metrics"):
				metrics := dest.(*[]models.BlockMetrics)
				*metrics = []models.BlockMetrics{
					{
						ChainID:          11155111,
						BlockNumber:      100,
						BlockTimestamp:   blockTime,
						BlobCount:        2,
						BlobGasUsed:      262144,
						BlobGasTarget:    393216,
						BlobGasLimit:     786432,
						ExcessBlobGas:    1234,
						BlobBaseFee:      "4200000",
						UtilizationRatio: "0.666667",
						BlobParamsTarget: 3,
						BlobParamsMax:    6,
						UpdateFraction:   3338477,
					},
				}
			}
			return nil
		},
	}

	client := &Client{hub: hub, send: make(chan []byte, 256), networkName: "sepolia"}
	hub.register <- client
	time.Sleep(20 * time.Millisecond)

	network := config.NetworkConfig{Name: "sepolia", ChainID: 11155111}
	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	if !poller.broadcastNewBlocks(context.Background(), network, 99) {
		t.Fatal("expected broadcastNewBlocks to succeed")
	}

	select {
	case msg := <-client.send:
		var event struct {
			Type WSEventType  `json:"type"`
			Data NewBlockData `json:"data"`
		}
		if err := json.Unmarshal(msg, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type != EventNewBlock {
			t.Fatalf("got type %q, want %q", event.Type, EventNewBlock)
		}
		if event.Data.Pricing == nil {
			t.Fatal("expected pricing payload")
		}
		pricing := event.Data.Pricing
		if pricing.BlobGasUsed != 262144 {
			t.Errorf("pricing.blob_gas_used = %d, want 262144", pricing.BlobGasUsed)
		}
		if pricing.TargetBlobs != 3 || pricing.MaxBlobs != 6 || pricing.AvailableBlobs != 4 {
			t.Errorf("unexpected capacity fields: target=%d max=%d available=%d",
				pricing.TargetBlobs, pricing.MaxBlobs, pricing.AvailableBlobs)
		}
		if pricing.UtilizationPercent != 33.33 {
			t.Errorf("pricing.utilization_percent = %v, want 33.33", pricing.UtilizationPercent)
		}
		if pricing.IsFull {
			t.Error("pricing.is_full = true, want false")
		}
		if pricing.IsAboveTarget {
			t.Error("pricing.is_above_target = true, want false")
		}
		if pricing.BlobBaseFeeGwei != "0.0042" {
			t.Errorf("pricing.blob_base_fee_gwei = %q, want 0.0042", pricing.BlobBaseFeeGwei)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for new_block")
	}
}

func TestPoller_PollMempool_Error(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("mempool query error")
		},
	}

	network := config.NetworkConfig{Name: "sepolia", ChainID: 11155111}
	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	// Should not panic on error.
	poller.pollMempool(context.Background(), network)
}

func TestPoller_NilDB_ReturnsImmediately(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	poller := NewPoller(nil, hub, testNetworks(), 10*time.Millisecond, time.Hour)

	done := make(chan struct{})
	go func() {
		poller.Run(context.Background())
		close(done)
	}()

	select {
	case <-done:
		// OK — returned immediately because db is nil.
	case <-time.After(time.Second):
		t.Fatal("poller with nil db should return immediately")
	}
}

func TestNewPoller_DefaultIntervals(t *testing.T) {
	hub := NewHub()
	networks := testNetworks()

	p := NewPoller(nil, hub, networks, 0, 0)
	if p.pollInterval != defaultPollInterval {
		t.Errorf("got pollInterval %v, want %v", p.pollInterval, defaultPollInterval)
	}
	if p.usersThrottle != defaultUsersThrottle {
		t.Errorf("got usersThrottle %v, want %v", p.usersThrottle, defaultUsersThrottle)
	}
}

func TestPoller_SkipsWhenNoClientsConnected(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	var queries int64
	db := &mockDB{
		getFn: func(_ context.Context, _ interface{}, _ string, _ ...interface{}) error {
			atomic.AddInt64(&queries, 1)
			return nil
		},
		selectFn: func(_ context.Context, _ interface{}, _ string, _ ...interface{}) error {
			atomic.AddInt64(&queries, 1)
			return nil
		},
	}

	// Short interval so ticks fire promptly; hub has no clients registered.
	poller := NewPoller(db, hub, testNetworks(), 10*time.Millisecond, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		poller.Run(ctx)
		close(done)
	}()

	// Let several ticks fire while ClientCount == 0.
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done

	if got := atomic.LoadInt64(&queries); got != 0 {
		t.Fatalf("expected zero DB queries while no clients connected, got %d", got)
	}
}

// TestPoller_PollMempool_AddedFetchErrorKeepsBaseline verifies that when the
// full-row fetch for newly seen hashes fails, the previous baseline is kept so
// the additions are re-broadcast on the next tick instead of being lost.
func TestPoller_PollMempool_AddedFetchErrorKeepsBaseline(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch query {
			case queryPendingBlobTxHashes:
				hashes := dest.(*[]string)
				*hashes = []string{"0xaaa"}
				return nil
			case queryPendingBlobsByTxHashes:
				return fmt.Errorf("fetch error")
			}
			return nil
		},
	}

	network := config.NetworkConfig{Name: "sepolia", ChainID: 11155111}
	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	poller.lastPendingTxs[11155111] = map[string]struct{}{} // established, empty baseline

	poller.pollMempool(context.Background(), network)

	if _, ok := poller.lastPendingTxs[11155111]["0xaaa"]; ok {
		t.Fatal("baseline must not advance when the added-row fetch fails")
	}
}

type fakeInvalidator struct {
	blockCalls   []int
	mempoolCalls []int
}

func (f *fakeInvalidator) invalidateBlockCaches(chainID int) {
	f.blockCalls = append(f.blockCalls, chainID)
}
func (f *fakeInvalidator) invalidateMempoolCaches(chainID int) {
	f.mempoolCalls = append(f.mempoolCalls, chainID)
}

// TestPoller_InvalidatesCachesOnNewBlockAndMempoolChange verifies the poller
// drops origin response caches when it detects a new block or a mempool diff,
// before broadcasting.
func TestPoller_InvalidatesCachesOnNewBlockAndMempoolChange(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if strings.Contains(query, "indexer_metadata") {
				v := dest.(*string)
				*v = "101"
			}
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if query == queryPendingBlobTxHashes {
				hashes := dest.(*[]string)
				*hashes = []string{"0xnew"}
			}
			return nil
		},
	}

	inv := &fakeInvalidator{}
	network := config.NetworkConfig{Name: "sepolia", ChainID: 11155111}
	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	poller.invalidator = inv
	poller.lastSeenBlocks[11155111] = 100                   // DB reports 101 → new block
	poller.lastPendingTxs[11155111] = map[string]struct{}{} // established baseline → 0xnew is an add

	poller.pollNetwork(context.Background(), network)

	if len(inv.blockCalls) != 1 || inv.blockCalls[0] != 11155111 {
		t.Fatalf("expected one block invalidation for 11155111, got %v", inv.blockCalls)
	}
	if len(inv.mempoolCalls) != 1 || inv.mempoolCalls[0] != 11155111 {
		t.Fatalf("expected one mempool invalidation for 11155111, got %v", inv.mempoolCalls)
	}

	// No block advance and no mempool diff → no further invalidations.
	poller.pollNetwork(context.Background(), network)
	if len(inv.blockCalls) != 1 || len(inv.mempoolCalls) != 1 {
		t.Fatalf("expected no additional invalidations, got %v / %v", inv.blockCalls, inv.mempoolCalls)
	}
}

// TestPoller_MempoolDiff_MultiBlobTxSingleAdd verifies a pending transaction
// carrying several blobs (one mempool row per blob) broadcasts exactly one
// add event, mirroring the hash-keyed removals.
func TestPoller_MempoolDiff_MultiBlobTxSingleAdd(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch query {
			case queryPendingBlobTxHashes:
				hashes := dest.(*[]string)
				*hashes = []string{"0xmulti"}
			case queryPendingBlobsByTxHashes:
				blobs := dest.(*[]models.Blob)
				*blobs = []models.Blob{
					{ChainID: 11155111, TxHash: "0xmulti", BlobIndex: 0, BaseFeePerBlobGas: "0", TipPerBlobGas: "0", TotalCostWei: "0"},
					{ChainID: 11155111, TxHash: "0xmulti", BlobIndex: 1, BaseFeePerBlobGas: "0", TipPerBlobGas: "0", TotalCostWei: "0"},
					{ChainID: 11155111, TxHash: "0xmulti", BlobIndex: 2, BaseFeePerBlobGas: "0", TipPerBlobGas: "0", TotalCostWei: "0"},
				}
			}
			return nil
		},
	}

	network := config.NetworkConfig{Name: "sepolia", ChainID: 11155111}
	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Hour)
	poller.lastPendingTxs[11155111] = map[string]struct{}{} // baseline established

	client := &Client{
		hub:         hub,
		send:        make(chan []byte, 256),
		networkName: "sepolia",
	}
	hub.register <- client
	time.Sleep(20 * time.Millisecond)

	poller.pollMempool(context.Background(), network)
	time.Sleep(100 * time.Millisecond)

	adds := 0
	for {
		select {
		case msg := <-client.send:
			var e WSEvent
			if err := json.Unmarshal(msg, &e); err != nil {
				continue
			}
			if e.Type != EventMempoolUpdate {
				continue
			}
			data, ok := e.Data.(map[string]interface{})
			if ok && data["action"] == string(MempoolActionAdd) {
				adds++
			}
		default:
			if adds != 1 {
				t.Fatalf("expected exactly 1 add event for a multi-blob tx, got %d", adds)
			}
			return
		}
	}
}
