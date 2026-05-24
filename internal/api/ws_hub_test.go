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
