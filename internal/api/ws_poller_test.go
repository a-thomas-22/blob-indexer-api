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
			if strings.Contains(query, "block_number >") {
				blobs := dest.(*[]models.Blob)
				*blobs = []models.Blob{
					{
						NetworkID:         11155111,
						BlockNumber:       int64(blockNumber),
						TxHash:            "0xabc",
						BaseFeePerBlobGas: "1000",
						TipPerBlobGas:     "100",
						TotalCostETH:      "0.001",
						Timestamp:         time.Now(),
						Confirmed:         true,
					},
				}
			}
			if strings.Contains(query, "confirmed = false") {
				blobs := dest.(*[]models.Blob)
				*blobs = []models.Blob{}
			}
			if strings.Contains(query, "GROUP BY") {
				users := dest.(*[]models.BlobUserStats)
				*users = []models.BlobUserStats{}
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
			if strings.Contains(query, "confirmed = false") {
				blobs := dest.(*[]models.Blob)
				*blobs = []models.Blob{}
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
			if strings.Contains(query, "block_number >") {
				blobs := dest.(*[]models.Blob)
				*blobs = []models.Blob{{
					NetworkID:         11155111,
					BlockNumber:       101,
					TxHash:            "0xdef",
					BaseFeePerBlobGas: "1000",
					TipPerBlobGas:     "100",
					TotalCostETH:      "0.001",
					Timestamp:         time.Now(),
					Confirmed:         true,
				}}
			}
			if strings.Contains(query, "confirmed = false") {
				blobs := dest.(*[]models.Blob)
				*blobs = []models.Blob{}
			}
			if strings.Contains(query, "GROUP BY") {
				users := dest.(*[]models.BlobUserStats)
				*users = []models.BlobUserStats{}
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
			if strings.Contains(query, "confirmed = false") {
				blobs := dest.(*[]models.Blob)
				pollCycle++
				switch {
				case pollCycle <= 1:
					// First poll: one pending tx (baseline, no broadcast).
					*blobs = []models.Blob{
						{NetworkID: 11155111, TxHash: "0x111", BaseFeePerBlobGas: "0", TipPerBlobGas: "0", TotalCostETH: "0"},
					}
				case pollCycle == 2:
					// Second poll: 0x111 gone (removed), 0x222 appeared (added).
					*blobs = []models.Blob{
						{NetworkID: 11155111, TxHash: "0x222", BaseFeePerBlobGas: "0", TipPerBlobGas: "0", TotalCostETH: "0"},
					}
				default:
					*blobs = []models.Blob{
						{NetworkID: 11155111, TxHash: "0x222", BaseFeePerBlobGas: "0", TipPerBlobGas: "0", TotalCostETH: "0"},
					}
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
			if strings.Contains(query, "GROUP BY") {
				users := dest.(*[]models.BlobUserStats)
				*users = []models.BlobUserStats{
					{
						Address:           "0xabc",
						Name:              "Test",
						Category:          "rollup",
						BlobCount:         5,
						TotalCostETH:      "0.01",
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

	if gotQuery != queryTopBlobUsersWithOptions {
		t.Fatal("expected poller to use enriched users query")
	}
	wantArgs := []interface{}{11155111, 10, 0, "all", "count"}
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
