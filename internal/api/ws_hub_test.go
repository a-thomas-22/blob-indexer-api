package api

import (
	"encoding/json"
	"testing"
	"time"

	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func TestHub_RegisterAndUnregister(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	client := &Client{
		hub:         hub,
		send:        make(chan []byte, wsSendBufferSize),
		networkName: "sepolia",
	}

	hub.register <- client
	// Give the hub goroutine time to process.
	time.Sleep(20 * time.Millisecond)

	hub.unregister <- client
	time.Sleep(20 * time.Millisecond)

	// send channel should be closed after unregister.
	_, ok := <-client.send
	if ok {
		t.Error("expected client.send to be closed")
	}
}

func TestHub_BroadcastEvent_ReachesMatchingClient(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	client := &Client{
		hub:         hub,
		send:        make(chan []byte, wsSendBufferSize),
		networkName: "sepolia",
	}
	hub.register <- client
	time.Sleep(20 * time.Millisecond)

	hub.BroadcastEvent("sepolia", WSEvent{
		Type: EventStatsUpdate,
		Data: map[string]int{"total_blobs": 100},
	})

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
		t.Fatal("timed out waiting for broadcast")
	}
}

func TestHub_BroadcastEvent_SkipsWrongNetwork(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	client := &Client{
		hub:         hub,
		send:        make(chan []byte, wsSendBufferSize),
		networkName: "mainnet",
	}
	hub.register <- client
	time.Sleep(20 * time.Millisecond)

	hub.BroadcastEvent("sepolia", WSEvent{
		Type: EventNewBlock,
		Data: map[string]int{"block_number": 1},
	})

	// Give hub time to process.
	time.Sleep(50 * time.Millisecond)

	select {
	case <-client.send:
		t.Error("client on mainnet should not receive sepolia event")
	default:
		// Expected — no message.
	}
}

func TestHub_BroadcastEvent_RespectsSubscriptionFilter(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	client := &Client{
		hub:         hub,
		send:        make(chan []byte, wsSendBufferSize),
		networkName: "sepolia",
		subscribedEvents: map[WSEventType]struct{}{
			EventNewBlock: {},
		},
	}
	hub.register <- client
	time.Sleep(20 * time.Millisecond)

	// Should be filtered out.
	hub.BroadcastEvent("sepolia", WSEvent{Type: EventStatsUpdate})
	time.Sleep(50 * time.Millisecond)
	select {
	case <-client.send:
		t.Error("stats_update should be filtered for client subscribed only to new_block")
	default:
	}

	// Should be delivered.
	hub.BroadcastEvent("sepolia", WSEvent{Type: EventNewBlock})
	select {
	case <-client.send:
		// OK
	case <-time.After(time.Second):
		t.Fatal("new_block should be delivered")
	}
}

func TestHub_SlowClient_Dropped(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	// Client with tiny buffer so it fills up immediately.
	client := &Client{
		hub:         hub,
		send:        make(chan []byte, 1),
		networkName: "sepolia",
	}
	hub.register <- client
	time.Sleep(20 * time.Millisecond)

	// Fill the buffer.
	hub.BroadcastEvent("sepolia", WSEvent{Type: EventPing})
	time.Sleep(20 * time.Millisecond)

	// This should cause the slow client to be dropped.
	hub.BroadcastEvent("sepolia", WSEvent{Type: EventPing})
	time.Sleep(50 * time.Millisecond)

	// The send channel should be closed.
	// Drain the one buffered message first.
	<-client.send
	_, ok := <-client.send
	if ok {
		t.Error("expected client.send to be closed after slow drop")
	}
}

func TestHub_Stop_ClosesAllClients(t *testing.T) {
	hub := NewHub()
	go hub.Run()

	c1 := &Client{hub: hub, send: make(chan []byte, 8), networkName: "a"}
	c2 := &Client{hub: hub, send: make(chan []byte, 8), networkName: "b"}
	hub.register <- c1
	hub.register <- c2
	time.Sleep(20 * time.Millisecond)

	hub.Stop()
	time.Sleep(50 * time.Millisecond)

	if _, ok := <-c1.send; ok {
		t.Error("expected c1.send to be closed")
	}
	if _, ok := <-c2.send; ok {
		t.Error("expected c2.send to be closed")
	}
}

// TestHub_Stop_DrainsQueuedRegister verifies that a client which was admitted
// and enqueued on register but not yet processed when the hub shuts down still
// has its send channel closed and its admission slot released, rather than
// leaking. done is closed before Run starts so the drain path is exercised
// deterministically.
func TestHub_Stop_DrainsQueuedRegister(t *testing.T) {
	hub := NewHub()
	if !hub.admit("1.1.1.1", 0, 0) {
		t.Fatal("admit should succeed")
	}
	queued := &Client{hub: hub, send: make(chan []byte, 1), networkName: "a", remoteIP: "1.1.1.1"}
	hub.register <- queued // buffered, not yet processed by Run
	hub.Stop()             // close done before Run picks up the queued client

	finished := make(chan struct{})
	go func() { hub.Run(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after Stop")
	}

	if _, ok := <-queued.send; ok {
		t.Error("expected queued client's send channel to be closed")
	}
	hub.admitMu.Lock()
	total, perIP := hub.total, hub.perIP["1.1.1.1"]
	hub.admitMu.Unlock()
	if total != 0 || perIP != 0 {
		t.Errorf("expected admission slot released, got total=%d perIP=%d", total, perIP)
	}
}

func TestHub_ClientCount(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	// Initially zero.
	if got := hub.ClientCount(); got != 0 {
		t.Errorf("got %d, want 0", got)
	}

	c1 := &Client{hub: hub, send: make(chan []byte, 8), networkName: "a"}
	c2 := &Client{hub: hub, send: make(chan []byte, 8), networkName: "b"}
	hub.register <- c1
	hub.register <- c2
	time.Sleep(20 * time.Millisecond)

	if got := hub.ClientCount(); got != 2 {
		t.Errorf("got %d, want 2", got)
	}

	hub.unregister <- c1
	time.Sleep(20 * time.Millisecond)

	if got := hub.ClientCount(); got != 1 {
		t.Errorf("got %d, want 1", got)
	}
}

func TestHub_Admit_GlobalCap(t *testing.T) {
	hub := NewHub()

	if !hub.admit("1.1.1.1", 2, 0) {
		t.Fatal("first admit should succeed")
	}
	if !hub.admit("2.2.2.2", 2, 0) {
		t.Fatal("second admit should succeed")
	}
	if hub.admit("3.3.3.3", 2, 0) {
		t.Fatal("third admit should be rejected by the global cap")
	}

	// Releasing a slot frees global capacity for a new connection.
	hub.release("1.1.1.1")
	if !hub.admit("3.3.3.3", 2, 0) {
		t.Fatal("admit should succeed after a slot is released")
	}
}

func TestHub_Admit_PerIPCap(t *testing.T) {
	hub := NewHub()

	if !hub.admit("1.1.1.1", 0, 2) {
		t.Fatal("first connection from IP should be admitted")
	}
	if !hub.admit("1.1.1.1", 0, 2) {
		t.Fatal("second connection from IP should be admitted")
	}
	if hub.admit("1.1.1.1", 0, 2) {
		t.Fatal("third connection from same IP should be rejected")
	}

	// A different IP is unaffected by the first IP's count.
	if !hub.admit("2.2.2.2", 0, 2) {
		t.Fatal("connection from a different IP should be admitted")
	}

	// Releasing one slot for the capped IP frees capacity for it.
	hub.release("1.1.1.1")
	if !hub.admit("1.1.1.1", 0, 2) {
		t.Fatal("admit should succeed after the IP releases a slot")
	}
}

func TestHub_Admit_Unlimited(t *testing.T) {
	hub := NewHub()
	for i := 0; i < 100; i++ {
		if !hub.admit("1.1.1.1", 0, 0) {
			t.Fatalf("admit %d should succeed when caps are disabled", i)
		}
	}
}

func TestHub_Release_DoesNotUnderflow(t *testing.T) {
	hub := NewHub()
	// Releasing without a prior admit must not panic or go negative.
	hub.release("1.1.1.1")
	hub.release("")

	if !hub.admit("1.1.1.1", 1, 1) {
		t.Fatal("admit should succeed after spurious releases")
	}
	if hub.admit("1.1.1.1", 1, 1) {
		t.Fatal("global cap should still be enforced after spurious releases")
	}
}

func TestHub_Unregister_ReleasesSlot(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	// Reserve a slot the way the upgrade handler does.
	if !hub.admit("1.1.1.1", 1, 1) {
		t.Fatal("admit should succeed")
	}
	client := &Client{
		hub:         hub,
		send:        make(chan []byte, wsSendBufferSize),
		networkName: "sepolia",
		remoteIP:    "1.1.1.1",
	}
	hub.register <- client
	time.Sleep(20 * time.Millisecond)

	// The cap is full while the client is connected.
	if hub.admit("1.1.1.1", 1, 1) {
		hub.release("1.1.1.1")
		t.Fatal("cap should be full while the client is connected")
	}

	hub.unregister <- client
	time.Sleep(20 * time.Millisecond)

	// Disconnect must release the slot so a new connection is admitted.
	if !hub.admit("1.1.1.1", 1, 1) {
		t.Fatal("slot should be released after the client unregisters")
	}
}

func TestHub_SlowClientDrop_ReleasesSlot(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	if !hub.admit("1.1.1.1", 1, 1) {
		t.Fatal("admit should succeed")
	}
	client := &Client{
		hub:         hub,
		send:        make(chan []byte, 1),
		networkName: "sepolia",
		remoteIP:    "1.1.1.1",
	}
	hub.register <- client
	time.Sleep(20 * time.Millisecond)

	// Fill the buffer, then force a drop on the next broadcast.
	hub.BroadcastEvent("sepolia", WSEvent{Type: EventPing})
	time.Sleep(20 * time.Millisecond)
	hub.BroadcastEvent("sepolia", WSEvent{Type: EventPing})
	time.Sleep(50 * time.Millisecond)

	// Dropping the slow client must release its admission slot.
	if !hub.admit("1.1.1.1", 1, 1) {
		t.Fatal("slot should be released after the slow client is dropped")
	}
}

func TestHub_Stop_ReleasesSlots(t *testing.T) {
	hub := NewHub()
	go hub.Run()

	if !hub.admit("1.1.1.1", 0, 1) {
		t.Fatal("admit should succeed")
	}
	client := &Client{
		hub:         hub,
		send:        make(chan []byte, wsSendBufferSize),
		networkName: "sepolia",
		remoteIP:    "1.1.1.1",
	}
	hub.register <- client
	time.Sleep(20 * time.Millisecond)

	hub.Stop()
	time.Sleep(50 * time.Millisecond)

	// After shutdown the per-IP accounting is cleared.
	hub.admitMu.Lock()
	total := hub.total
	perIP := len(hub.perIP)
	hub.admitMu.Unlock()
	if total != 0 {
		t.Errorf("got total=%d, want 0 after Stop", total)
	}
	if perIP != 0 {
		t.Errorf("got %d per-IP entries, want 0 after Stop", perIP)
	}
}

func TestHub_BroadcastEvent_FullChannelDropsEvent(t *testing.T) {
	hub := NewHub()
	// Don't run the hub — fill the broadcast channel to test the default case.
	for i := 0; i < 256; i++ {
		hub.broadcast <- broadcastMessage{}
	}
	// This should hit the default (full channel) path and not block.
	hub.BroadcastEvent("sepolia", WSEvent{Type: EventPing})
}

func TestHub_BroadcastEvent_MultipleClients(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	c1 := &Client{hub: hub, send: make(chan []byte, wsSendBufferSize), networkName: "sepolia"}
	c2 := &Client{hub: hub, send: make(chan []byte, wsSendBufferSize), networkName: "sepolia"}
	hub.register <- c1
	hub.register <- c2
	time.Sleep(20 * time.Millisecond)

	hub.BroadcastEvent("sepolia", WSEvent{Type: EventNewBlock})

	for _, c := range []*Client{c1, c2} {
		select {
		case <-c.send:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for broadcast to reach client")
		}
	}
}

func TestHub_SendEventToClient_Delivered(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	client := &Client{
		hub:         hub,
		send:        make(chan []byte, 4),
		networkName: "sepolia",
	}
	hub.register <- client

	hub.SendEventToClient(client, WSEvent{Type: EventBlockSnapshot, Data: BlockSnapshotData{Blocks: []NewBlockData{{BlockNumber: 42}}}})

	select {
	case msg := <-client.send:
		var e WSEvent
		if err := json.Unmarshal(msg, &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if e.Type != EventBlockSnapshot {
			t.Fatalf("got type %q, want %q", e.Type, EventBlockSnapshot)
		}
	case <-time.After(time.Second):
		t.Fatal("direct message not delivered")
	}
}

func TestHub_SendEventToClient_UnregisteredIgnored(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	// Never registered — the hub must not touch its channel.
	client := &Client{
		hub:         hub,
		send:        make(chan []byte, 4),
		networkName: "sepolia",
	}

	hub.SendEventToClient(client, WSEvent{Type: EventBlockSnapshot})
	time.Sleep(50 * time.Millisecond)

	select {
	case <-client.send:
		t.Fatal("unregistered client must not receive direct messages")
	default:
	}
}

func TestHub_SendEventToClient_SlowClientDropped(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	client := &Client{
		hub:         hub,
		send:        make(chan []byte), // zero capacity: any send overflows
		networkName: "sepolia",
	}
	hub.register <- client
	for hub.ClientCount() != 1 {
		time.Sleep(time.Millisecond)
	}

	hub.SendEventToClient(client, WSEvent{Type: EventBlockSnapshot})

	deadline := time.Now().Add(time.Second)
	for hub.ClientCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("slow client was not dropped on direct send overflow")
		}
		time.Sleep(time.Millisecond)
	}
}
