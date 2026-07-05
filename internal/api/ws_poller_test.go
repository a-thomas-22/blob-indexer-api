package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func testNetworks() map[int]config.NetworkConfig {
	return map[int]config.NetworkConfig{
		11155111: {Name: "sepolia", ChainID: 11155111, Enabled: true},
	}
}

func sepoliaNetwork() config.NetworkConfig {
	return config.NetworkConfig{Name: "sepolia", ChainID: 11155111}
}

// registerTestClient registers a buffered client on the hub and waits for the
// registration to be processed.
func registerTestClient(t *testing.T, hub *Hub) *Client {
	t.Helper()
	client := &Client{
		hub:         hub,
		send:        make(chan []byte, 256),
		networkName: "sepolia",
	}
	hub.register <- client
	waitFor(t, func() bool { return hub.ClientCount() == 1 })
	return client
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// drainEvents empties the client's send buffer after a settle delay.
func drainEvents(client *Client, settle time.Duration) []WSEvent {
	time.Sleep(settle)
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

func countEvents(events []WSEvent, eventType WSEventType) int {
	n := 0
	for _, e := range events {
		if e.Type == eventType {
			n++
		}
	}
	return n
}

// pollerBlockDB is a mockDB preloaded with block_metrics-shaped responses:
// maxBlock answers the baseline query, blockNumbers the catch-up scan, and
// metricsFor/blobsFor the per-block broadcast queries.
type pollerBlockDB struct {
	mu           sync.Mutex
	maxBlock     uint64
	baselineErr  error
	blockNumbers []uint64
	scanErr      error
	metricsFor   map[uint64][]models.BlockMetrics
	metricsErr   error
	blobsFor     map[uint64][]models.Blob
	blobsErr     error
}

func (p *pollerBlockDB) mock() *mockDB {
	return &mockDB{
		getFn: func(_ context.Context, dest interface{}, query string, _ ...interface{}) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			switch query {
			case queryMaxBlockMetricsNumber:
				if p.baselineErr != nil {
					return p.baselineErr
				}
				*dest.(*uint64) = p.maxBlock
			case queryBlobStats:
				*dest.(*models.BlobStatsAggregate) = models.BlobStatsAggregate{TotalBlobs: 10}
			}
			return nil
		},
		selectFn: func(_ context.Context, dest interface{}, query string, args ...interface{}) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			switch query {
			case queryBlockMetricsNumbersSince:
				if p.scanErr != nil {
					return p.scanErr
				}
				*dest.(*[]uint64) = append([]uint64(nil), p.blockNumbers...)
			case queryBlockMetricsByNumber:
				if p.metricsErr != nil {
					return p.metricsErr
				}
				nums := args[1].(*pq.Int64Array)
				var out []models.BlockMetrics
				for _, n := range *nums {
					out = append(out, p.metricsFor[uint64(n)]...)
				}
				*dest.(*[]models.BlockMetrics) = out
			case queryBlobsByBlockNumber:
				if p.blobsErr != nil {
					return p.blobsErr
				}
				*dest.(*[]models.Blob) = append([]models.Blob(nil), p.blobsFor[args[1].(uint64)]...)
			case queryTopBlobUsersAllByCount:
				*dest.(*[]models.BlobUserStats) = []models.BlobUserStats{}
			}
			return nil
		},
	}
}

func (p *pollerBlockDB) set(fn func(*pollerBlockDB)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(p)
}

func metricsRow(blockNumber uint64, blobCount int) []models.BlockMetrics {
	return []models.BlockMetrics{{
		ChainID:          11155111,
		BlockNumber:      int64(blockNumber),
		BlockTimestamp:   time.Unix(1700000000, 0).UTC(),
		BlobCount:        blobCount,
		BlobGasUsed:      int64(blobCount) * 131072,
		BlobGasTarget:    786432,
		BlobGasLimit:     1179648,
		BlobBaseFee:      "1000",
		UtilizationRatio: "0.5",
		BlobParamsTarget: 6,
		BlobParamsMax:    9,
	}}
}

func blobRow(blockNumber uint64, txHash string) models.Blob {
	return models.Blob{
		ChainID:           11155111,
		BlockNumber:       int64(blockNumber),
		TxHash:            txHash,
		BaseFeePerBlobGas: "1000",
		TipPerBlobGas:     "100",
		TotalCostWei:      "0.001",
		Timestamp:         time.Unix(1700000000, 0).UTC(),
		Confirmed:         true,
	}
}

// newBlockNumbers decodes the block numbers of every new_block event.
func newBlockNumbers(t *testing.T, events []WSEvent) []int64 {
	t.Helper()
	var nums []int64
	for _, e := range events {
		if e.Type != EventNewBlock {
			continue
		}
		raw, err := json.Marshal(e.Data)
		if err != nil {
			t.Fatalf("re-marshal event data: %v", err)
		}
		var data NewBlockData
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatalf("unmarshal new_block data: %v", err)
		}
		nums = append(nums, data.BlockNumber)
	}
	return nums
}

func TestPoller_BaselineDoesNotBroadcastHistory(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{maxBlock: 100, blockNumbers: []uint64{99, 100}}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)

	poller.scanNetwork(context.Background(), sepoliaNetwork())

	events := drainEvents(client, 50*time.Millisecond)
	if len(events) != 0 {
		t.Fatalf("baseline scan must not broadcast, got %d events", len(events))
	}
	st := poller.chainState(11155111)
	if !st.baselined || st.head != 100 {
		t.Fatalf("expected baselined head=100, got baselined=%v head=%d", st.baselined, st.head)
	}
}

func TestPoller_BaselineErrorRetriesWithoutCorruption(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{baselineErr: fmt.Errorf("db down"), maxBlock: 100}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)

	poller.scanNetwork(context.Background(), sepoliaNetwork())
	if poller.chainState(11155111).baselined {
		t.Fatal("baseline must not be established on error")
	}

	// Error clears — next tick baselines silently, swallowing nothing later.
	blockDB.set(func(p *pollerBlockDB) { p.baselineErr = nil })
	poller.scanNetwork(context.Background(), sepoliaNetwork())

	st := poller.chainState(11155111)
	if !st.baselined || st.head != 100 {
		t.Fatalf("expected baselined head=100 after retry, got baselined=%v head=%d", st.baselined, st.head)
	}
	if events := drainEvents(client, 50*time.Millisecond); len(events) != 0 {
		t.Fatalf("baseline retry must not broadcast, got %d events", len(events))
	}
}

func TestPoller_ScanBroadcastsNewBlocks_IncludingZeroBlobBlocks(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		maxBlock: 100,
		metricsFor: map[uint64][]models.BlockMetrics{
			101: metricsRow(101, 2),
			102: metricsRow(102, 0), // zero-blob block must still broadcast
		},
		blobsFor: map[uint64][]models.Blob{
			101: {blobRow(101, "0xaaa"), blobRow(101, "0xbbb")},
		},
	}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)

	poller.scanNetwork(context.Background(), sepoliaNetwork()) // baseline at 100
	blockDB.set(func(p *pollerBlockDB) { p.blockNumbers = []uint64{101, 102} })
	poller.scanNetwork(context.Background(), sepoliaNetwork())

	events := drainEvents(client, 50*time.Millisecond)
	nums := newBlockNumbers(t, events)
	if len(nums) != 2 || nums[0] != 101 || nums[1] != 102 {
		t.Fatalf("expected new_block for 101 and 102, got %v", nums)
	}
	if countEvents(events, EventStatsUpdate) == 0 {
		t.Error("expected a stats_update after block broadcasts")
	}

	// Every new_block must carry pricing (block_metrics-derived).
	for _, e := range events {
		if e.Type != EventNewBlock {
			continue
		}
		raw, _ := json.Marshal(e.Data)
		var data NewBlockData
		if err := json.Unmarshal(raw, &data); err != nil || data.Pricing == nil {
			t.Fatalf("new_block without pricing: %s", raw)
		}
		if data.BlockNumber == 102 && len(data.Blobs) != 0 {
			t.Fatalf("zero-blob block carried %d blobs", len(data.Blobs))
		}
	}

	st := poller.chainState(11155111)
	if st.head != 102 {
		t.Fatalf("head should advance to 102, got %d", st.head)
	}
}

func TestPoller_ScanSkipsAlreadyBroadcastBlocks(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		maxBlock:   100,
		metricsFor: map[uint64][]models.BlockMetrics{101: metricsRow(101, 1)},
		blobsFor:   map[uint64][]models.Blob{101: {blobRow(101, "0xaaa")}},
	}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)

	poller.scanNetwork(context.Background(), sepoliaNetwork())
	blockDB.set(func(p *pollerBlockDB) { p.blockNumbers = []uint64{101} })
	poller.scanNetwork(context.Background(), sepoliaNetwork())
	poller.scanNetwork(context.Background(), sepoliaNetwork()) // same scan result again

	events := drainEvents(client, 50*time.Millisecond)
	if got := countEvents(events, EventNewBlock); got != 1 {
		t.Fatalf("expected exactly one new_block, got %d", got)
	}
}

func TestPoller_ScanCatchesLateOutOfOrderCommit(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		maxBlock: 100,
		metricsFor: map[uint64][]models.BlockMetrics{
			101: metricsRow(101, 1),
			102: metricsRow(102, 1),
		},
		blobsFor: map[uint64][]models.Blob{
			101: {blobRow(101, "0xaaa")},
			102: {blobRow(102, "0xbbb")},
		},
	}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)

	poller.scanNetwork(context.Background(), sepoliaNetwork()) // baseline at 100

	// Block 102 commits first (out of order); 101 lands on a later scan —
	// behind the head, inside the trailing window.
	blockDB.set(func(p *pollerBlockDB) { p.blockNumbers = []uint64{102} })
	poller.scanNetwork(context.Background(), sepoliaNetwork())
	blockDB.set(func(p *pollerBlockDB) { p.blockNumbers = []uint64{101, 102} })
	poller.scanNetwork(context.Background(), sepoliaNetwork())

	events := drainEvents(client, 50*time.Millisecond)
	nums := newBlockNumbers(t, events)
	if len(nums) != 2 || nums[0] != 102 || nums[1] != 101 {
		t.Fatalf("expected 102 then late 101, got %v", nums)
	}
}

func TestPoller_NotificationBroadcastsBlock(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		metricsFor: map[uint64][]models.BlockMetrics{101: metricsRow(101, 1)},
		blobsFor:   map[uint64][]models.Blob{101: {blobRow(101, "0xaaa")}},
	}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)
	st := poller.chainState(11155111)
	st.baselined = true
	st.head = 100

	poller.handleNotification(context.Background(), `{"chain_id":11155111,"block_number":101}`)

	events := drainEvents(client, 50*time.Millisecond)
	nums := newBlockNumbers(t, events)
	if len(nums) != 1 || nums[0] != 101 {
		t.Fatalf("expected new_block 101 from notification, got %v", nums)
	}
	if st.head != 101 {
		t.Fatalf("head should advance to 101, got %d", st.head)
	}
}

func TestPoller_NotificationRebroadcastsReorgedBlock(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		metricsFor: map[uint64][]models.BlockMetrics{100: metricsRow(100, 1)},
		blobsFor:   map[uint64][]models.Blob{100: {blobRow(100, "0xreplaced")}},
	}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)
	st := poller.chainState(11155111)
	st.baselined = true
	st.head = 100
	st.seen[100] = struct{}{} // already broadcast pre-reorg

	// The replacement block's commit re-notifies; the corrected data must go
	// out even though the block number was already seen.
	poller.handleNotification(context.Background(), `{"chain_id":11155111,"block_number":100}`)

	events := drainEvents(client, 50*time.Millisecond)
	nums := newBlockNumbers(t, events)
	if len(nums) != 1 || nums[0] != 100 {
		t.Fatalf("expected re-broadcast of reorged block 100, got %v", nums)
	}
}

func TestPoller_NotificationIgnoresDeepHistoryUnknownChainAndGarbage(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		metricsFor: map[uint64][]models.BlockMetrics{500: metricsRow(500, 1)},
	}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)
	st := poller.chainState(11155111)
	st.baselined = true
	st.head = 1000

	// Deep-history rewrite (backfill), unknown chain, pre-baseline, garbage.
	poller.handleNotification(context.Background(), `{"chain_id":11155111,"block_number":500}`)
	poller.handleNotification(context.Background(), `{"chain_id":1,"block_number":2000}`)
	poller.handleNotification(context.Background(), `not-json`)

	if events := drainEvents(client, 50*time.Millisecond); len(events) != 0 {
		t.Fatalf("expected no broadcasts, got %d events", len(events))
	}
}

func TestPoller_NotificationBeforeBaselineIgnored(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		metricsFor: map[uint64][]models.BlockMetrics{101: metricsRow(101, 1)},
	}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)

	poller.handleNotification(context.Background(), `{"chain_id":11155111,"block_number":101}`)

	if events := drainEvents(client, 50*time.Millisecond); len(events) != 0 {
		t.Fatalf("pre-baseline notification must not broadcast, got %d events", len(events))
	}
}

func TestPoller_BroadcastBlock_QueryErrorRetriesViaScan(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		metricsErr: fmt.Errorf("transient"),
		metricsFor: map[uint64][]models.BlockMetrics{101: metricsRow(101, 1)},
		blobsFor:   map[uint64][]models.Blob{101: {blobRow(101, "0xaaa")}},
	}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)
	st := poller.chainState(11155111)
	st.baselined = true
	st.head = 100

	poller.handleNotification(context.Background(), `{"chain_id":11155111,"block_number":101}`)
	if _, seen := st.seen[101]; seen {
		t.Fatal("failed broadcast must unmark the block so the scan retries it")
	}

	// The error clears; the catch-up scan retries the block.
	blockDB.set(func(p *pollerBlockDB) {
		p.metricsErr = nil
		p.blockNumbers = []uint64{101}
	})
	poller.scanNetwork(context.Background(), sepoliaNetwork())

	events := drainEvents(client, 50*time.Millisecond)
	nums := newBlockNumbers(t, events)
	if len(nums) != 1 || nums[0] != 101 {
		t.Fatalf("expected retried broadcast of 101, got %v", nums)
	}
}

func TestPoller_BroadcastBlock_VanishedRowStaysSilent(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{} // no metrics rows at all
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)
	st := poller.chainState(11155111)
	st.baselined = true
	st.head = 100

	poller.handleNotification(context.Background(), `{"chain_id":11155111,"block_number":101}`)

	if events := drainEvents(client, 50*time.Millisecond); len(events) != 0 {
		t.Fatalf("vanished row must stay silent, got %d events", len(events))
	}
	if _, seen := st.seen[101]; !seen {
		t.Fatal("vanished row must stay marked seen — its replacement re-notifies")
	}
}

func TestPoller_ZeroClients_AdvancesStateAndInvalidatesCaches(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	// No clients registered.

	queried := false
	blockDB := &pollerBlockDB{
		metricsFor: map[uint64][]models.BlockMetrics{101: metricsRow(101, 1)},
	}
	db := blockDB.mock()
	baseSelect := db.selectFn
	db.selectFn = func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
		if query == queryBlockMetricsByNumber || query == queryBlobsByBlockNumber {
			queried = true
		}
		return baseSelect(ctx, dest, query, args...)
	}

	inv := &fakeInvalidator{}
	poller := NewPoller(db, hub, testNetworks(), time.Hour, time.Hour)
	poller.invalidator = inv
	st := poller.chainState(11155111)
	st.baselined = true
	st.head = 100

	poller.handleNotification(context.Background(), `{"chain_id":11155111,"block_number":101}`)

	if queried {
		t.Error("broadcast queries must be skipped with zero clients")
	}
	if len(inv.blockCalls) != 1 || inv.blockCalls[0] != 11155111 {
		t.Fatalf("caches must be invalidated even with zero clients, got %v", inv.blockCalls)
	}
	if st.head != 101 {
		t.Fatalf("state must advance with zero clients, got head %d", st.head)
	}
	if _, seen := st.seen[101]; !seen {
		t.Fatal("block must be marked seen with zero clients")
	}
}

func TestPoller_UsersThrottle(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		metricsFor: map[uint64][]models.BlockMetrics{
			101: metricsRow(101, 1),
			102: metricsRow(102, 1),
		},
		blobsFor: map[uint64][]models.Blob{
			101: {blobRow(101, "0xaaa")},
			102: {blobRow(102, "0xbbb")},
		},
	}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)
	st := poller.chainState(11155111)
	st.baselined = true
	st.head = 100

	poller.handleNotification(context.Background(), `{"chain_id":11155111,"block_number":101}`)
	poller.handleNotification(context.Background(), `{"chain_id":11155111,"block_number":102}`)

	events := drainEvents(client, 50*time.Millisecond)
	if got := countEvents(events, EventNewBlock); got != 2 {
		t.Fatalf("expected 2 new_block events, got %d", got)
	}
	if got := countEvents(events, EventStatsUpdate); got != 2 {
		t.Fatalf("stats_update should fire per block, got %d", got)
	}
	if got := countEvents(events, EventUsersUpdate); got != 1 {
		t.Fatalf("users_update should be throttled to 1, got %d", got)
	}
}

// fakeListener is a controllable blockListener for Run-loop tests.
type fakeListener struct {
	notifCh   chan *pq.Notification
	listenErr error
	mu        sync.Mutex
	closed    bool
	pings     int
}

func (f *fakeListener) Listen(string) error { return f.listenErr }
func (f *fakeListener) NotificationChannel() <-chan *pq.Notification {
	return f.notifCh
}
func (f *fakeListener) Ping() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pings++
	return nil
}
func (f *fakeListener) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func TestPoller_Run_NotificationDrivesBroadcast(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		maxBlock:   100,
		metricsFor: map[uint64][]models.BlockMetrics{101: metricsRow(101, 1)},
		blobsFor:   map[uint64][]models.Blob{101: {blobRow(101, "0xaaa")}},
	}

	listener := &fakeListener{notifCh: make(chan *pq.Notification, 1)}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), 20*time.Millisecond, time.Hour)
	poller.listenerFactory = func(func(pq.ListenerEventType)) blockListener { return listener }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		poller.Run(ctx)
		close(done)
	}()

	// Let the first tick establish the baseline, then notify.
	time.Sleep(60 * time.Millisecond)
	listener.notifCh <- &pq.Notification{Channel: blockNotifyChannel, Extra: `{"chain_id":11155111,"block_number":101}`}

	waitFor(t, func() bool {
		return countEvents(drainEvents(client, 10*time.Millisecond), EventNewBlock) == 1
	})

	cancel()
	<-done
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if !listener.closed {
		t.Error("listener must be closed on shutdown")
	}
}

func TestPoller_Run_ReconnectTriggersScan(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		maxBlock:   100,
		metricsFor: map[uint64][]models.BlockMetrics{101: metricsRow(101, 1)},
		blobsFor:   map[uint64][]models.Blob{101: {blobRow(101, "0xaaa")}},
	}

	listener := &fakeListener{notifCh: make(chan *pq.Notification)}
	var emitEvent func(pq.ListenerEventType)
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)
	poller.listenerFactory = func(onEvent func(pq.ListenerEventType)) blockListener {
		emitEvent = onEvent
		return listener
	}

	// Baseline synchronously before starting Run so the reconnect scan is the
	// only detection path in play (poll interval is 1h).
	poller.scanNetwork(context.Background(), sepoliaNetwork())
	blockDB.set(func(p *pollerBlockDB) { p.blockNumbers = []uint64{101} })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		poller.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	waitFor(t, func() bool { return emitEvent != nil })
	emitEvent(pq.ListenerEventReconnected)

	waitFor(t, func() bool {
		return countEvents(drainEvents(client, 10*time.Millisecond), EventNewBlock) == 1
	})
}

func TestPoller_Run_ListenFailureFallsBackToScanOnly(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		maxBlock:   100,
		metricsFor: map[uint64][]models.BlockMetrics{101: metricsRow(101, 1)},
		blobsFor:   map[uint64][]models.Blob{101: {blobRow(101, "0xaaa")}},
	}

	listener := &fakeListener{notifCh: make(chan *pq.Notification), listenErr: fmt.Errorf("no LISTEN for you")}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), 20*time.Millisecond, time.Hour)
	poller.listenerFactory = func(func(pq.ListenerEventType)) blockListener { return listener }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		poller.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	time.Sleep(50 * time.Millisecond) // baseline tick
	blockDB.set(func(p *pollerBlockDB) { p.blockNumbers = []uint64{101} })

	waitFor(t, func() bool {
		return countEvents(drainEvents(client, 10*time.Millisecond), EventNewBlock) == 1
	})

	listener.mu.Lock()
	defer listener.mu.Unlock()
	if !listener.closed {
		t.Error("failed listener must be closed")
	}
}

func TestPoller_MempoolSkippedWithZeroClients(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	// No clients registered.

	mempoolQueried := false
	db := &mockDB{
		selectFn: func(_ context.Context, _ interface{}, query string, _ ...interface{}) error {
			if query == queryPendingBlobTxHashes {
				mempoolQueried = true
			}
			return nil
		},
	}
	poller := NewPoller(db, hub, testNetworks(), time.Hour, time.Hour)
	poller.pollMempoolAll(context.Background())

	if mempoolQueried {
		t.Fatal("mempool diffing must be skipped with zero clients")
	}
}

func TestPoller_MempoolDiff_AddAndRemove(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	pollCycle := 0
	db := &mockDB{
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

	poller := NewPoller(db, hub, testNetworks(), time.Hour, time.Hour)
	poller.pollMempool(context.Background(), sepoliaNetwork()) // baseline
	poller.pollMempool(context.Background(), sepoliaNetwork()) // diff

	events := drainEvents(client, 50*time.Millisecond)
	addCount := 0
	removeCount := 0
	for _, e := range events {
		if e.Type != EventMempoolUpdate {
			continue
		}
		data, _ := json.Marshal(e.Data)
		var m MempoolUpdateData
		if json.Unmarshal(data, &m) == nil {
			if m.Action == MempoolActionAdd {
				addCount++
			}
			if m.Action == MempoolActionRemove {
				removeCount++
			}
		}
	}

	if addCount != 1 {
		t.Errorf("expected one mempool add event, got %d", addCount)
	}
	if removeCount != 1 {
		t.Errorf("expected one mempool remove event, got %d", removeCount)
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
			return nil
		},
	}

	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	// Should not panic on error.
	poller.broadcastStatsUpdate(context.Background(), sepoliaNetwork())
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

	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	// Should not panic on error.
	poller.broadcastUsersUpdate(context.Background(), sepoliaNetwork())
}

func TestPoller_BroadcastUsersUpdate_Success(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if out, ok := dest.(*[]models.BlobUserStats); ok {
				*out = []models.BlobUserStats{
					{Address: "0xdead", Name: "Base", BlobCount: 5, TotalCostWei: "0"},
				}
			}
			return nil
		},
	}

	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	poller.broadcastUsersUpdate(context.Background(), sepoliaNetwork())

	events := drainEvents(client, 50*time.Millisecond)
	if countEvents(events, EventUsersUpdate) != 1 {
		t.Fatalf("expected one users_update, got %d", countEvents(events, EventUsersUpdate))
	}
}

func TestPoller_ScanError_NoBroadcastNoCorruption(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	client := registerTestClient(t, hub)

	blockDB := &pollerBlockDB{maxBlock: 100, scanErr: fmt.Errorf("scan failed")}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)

	poller.scanNetwork(context.Background(), sepoliaNetwork()) // baseline
	poller.scanNetwork(context.Background(), sepoliaNetwork()) // errors

	if events := drainEvents(client, 50*time.Millisecond); len(events) != 0 {
		t.Fatalf("scan error must not broadcast, got %d events", len(events))
	}
	st := poller.chainState(11155111)
	if st.head != 100 {
		t.Fatalf("scan error must not move the head, got %d", st.head)
	}
}

func TestPoller_SeenSetPrunedToTrailingWindow(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()
	registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		maxBlock:   100,
		metricsFor: map[uint64][]models.BlockMetrics{},
		blobsFor:   map[uint64][]models.Blob{},
	}
	for n := uint64(101); n <= 150; n++ {
		blockDB.metricsFor[n] = metricsRow(n, 0)
	}
	poller := NewPoller(blockDB.mock(), hub, testNetworks(), time.Hour, time.Hour)

	poller.scanNetwork(context.Background(), sepoliaNetwork()) // baseline
	nums := make([]uint64, 0, 50)
	for n := uint64(101); n <= 150; n++ {
		nums = append(nums, n)
	}
	blockDB.set(func(p *pollerBlockDB) { p.blockNumbers = nums })
	poller.scanNetwork(context.Background(), sepoliaNetwork())

	st := poller.chainState(11155111)
	if st.head != 150 {
		t.Fatalf("expected head 150, got %d", st.head)
	}
	for blockNumber := range st.seen {
		if blockNumber+trailingScanWindow < st.head {
			t.Fatalf("seen entry %d escaped pruning (head %d)", blockNumber, st.head)
		}
	}
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

	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	poller.lastPendingTxs[11155111] = map[string]struct{}{} // established, empty baseline

	poller.pollMempool(context.Background(), sepoliaNetwork())

	if _, ok := poller.lastPendingTxs[11155111]["0xaaa"]; ok {
		t.Fatal("baseline must not advance when the added-row fetch fails")
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

	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	// Should not panic and must not establish a baseline.
	poller.pollMempool(context.Background(), sepoliaNetwork())
	if poller.lastPendingTxs[11155111] != nil {
		t.Fatal("baseline must not be established on query error")
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
	registerTestClient(t, hub)

	blockDB := &pollerBlockDB{
		maxBlock:   100,
		metricsFor: map[uint64][]models.BlockMetrics{101: metricsRow(101, 1)},
		blobsFor:   map[uint64][]models.Blob{101: {blobRow(101, "0xaaa")}},
	}
	db := blockDB.mock()
	baseSelect := db.selectFn
	pendingHashes := []string{}
	db.selectFn = func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
		if query == queryPendingBlobTxHashes {
			*dest.(*[]string) = append([]string(nil), pendingHashes...)
			return nil
		}
		if query == queryPendingBlobsByTxHashes {
			*dest.(*[]models.Blob) = []models.Blob{
				{ChainID: 11155111, TxHash: "0xnew", BaseFeePerBlobGas: "0", TipPerBlobGas: "0", TotalCostWei: "0"},
			}
			return nil
		}
		return baseSelect(ctx, dest, query, args...)
	}

	inv := &fakeInvalidator{}
	network := sepoliaNetwork()
	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Second)
	poller.invalidator = inv
	st := poller.chainState(11155111)
	st.baselined = true
	st.head = 100
	poller.lastPendingTxs[11155111] = map[string]struct{}{} // established baseline → 0xnew is an add

	blockDB.set(func(p *pollerBlockDB) { p.blockNumbers = []uint64{101} })
	pendingHashes = []string{"0xnew"}
	poller.scanNetwork(context.Background(), network)
	poller.pollMempool(context.Background(), network)

	if len(inv.blockCalls) != 1 || inv.blockCalls[0] != 11155111 {
		t.Fatalf("expected one block invalidation for 11155111, got %v", inv.blockCalls)
	}
	if len(inv.mempoolCalls) != 1 || inv.mempoolCalls[0] != 11155111 {
		t.Fatalf("expected one mempool invalidation for 11155111, got %v", inv.mempoolCalls)
	}

	// No block advance and no mempool diff → no further invalidations.
	poller.scanNetwork(context.Background(), network)
	poller.pollMempool(context.Background(), network)
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
	client := registerTestClient(t, hub)

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

	poller := NewPoller(db, hub, testNetworks(), time.Second, time.Hour)
	poller.lastPendingTxs[11155111] = map[string]struct{}{} // baseline established

	poller.pollMempool(context.Background(), sepoliaNetwork())

	events := drainEvents(client, 50*time.Millisecond)
	adds := 0
	for _, e := range events {
		if e.Type != EventMempoolUpdate {
			continue
		}
		data, ok := e.Data.(map[string]interface{})
		if ok && data["action"] == string(MempoolActionAdd) {
			adds++
		}
	}
	if adds != 1 {
		t.Fatalf("expected exactly 1 add event for a multi-blob tx, got %d", adds)
	}
}
