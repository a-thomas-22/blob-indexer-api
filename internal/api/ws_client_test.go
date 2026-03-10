package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

// newTestWSServer creates an httptest.Server that upgrades to WebSocket and
// returns the connected client. The caller must close the server and connection.
func newTestWSServer(t *testing.T, hub *Hub) (*httptest.Server, *websocket.Conn) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade error: %v", err)
			return
		}

		client := &Client{
			hub:         hub,
			conn:        conn,
			send:        make(chan []byte, wsSendBufferSize),
			networkName: "sepolia",
		}

		hub.register <- client
		go client.writePump()
		go client.readPump()
	}))

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}

	return server, conn
}

func TestClient_ReceivesMessages(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	server, conn := newTestWSServer(t, hub)
	defer server.Close()
	defer conn.Close()

	// Allow registration to complete.
	time.Sleep(50 * time.Millisecond)

	hub.BroadcastEvent("sepolia", WSEvent{
		Type: EventStatsUpdate,
		Data: map[string]int{"total_blobs": 42},
	})

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}

	var event WSEvent
	if err := json.Unmarshal(msg, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != EventStatsUpdate {
		t.Errorf("got type %q, want %q", event.Type, EventStatsUpdate)
	}
}

func TestClient_SubscriptionFilter(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	server, conn := newTestWSServer(t, hub)
	defer server.Close()
	defer conn.Close()

	time.Sleep(50 * time.Millisecond)

	// Send subscription message — only want new_block.
	sub := WSSubscribeMessage{Subscribe: []WSEventType{EventNewBlock}}
	data, _ := json.Marshal(sub)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// Send stats_update — should be filtered.
	hub.BroadcastEvent("sepolia", WSEvent{Type: EventStatsUpdate})
	time.Sleep(50 * time.Millisecond)

	// Send new_block — should arrive.
	hub.BroadcastEvent("sepolia", WSEvent{Type: EventNewBlock, Data: NewBlockData{BlockNumber: 1}})

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}

	var event WSEvent
	if err := json.Unmarshal(msg, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != EventNewBlock {
		t.Errorf("got type %q, want %q (stats_update should have been filtered)", event.Type, EventNewBlock)
	}
}

func TestClient_ReceivesPing(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	// Use a custom server with a very short ping period for testing.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := &Client{
			hub:         hub,
			conn:        conn,
			send:        make(chan []byte, wsSendBufferSize),
			networkName: "test",
		}
		hub.register <- client
		// We just test that writePump sends the ping JSON.
		// Send a manual ping message via the send channel.
		pingData, _ := json.Marshal(WSEvent{Type: EventPing})
		client.send <- pingData
		go client.readPump()
		go client.writePump()
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}

	var event WSEvent
	if err := json.Unmarshal(msg, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != EventPing {
		t.Errorf("got type %q, want %q", event.Type, EventPing)
	}
}

func TestClient_CloseUnregisters(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	server, conn := newTestWSServer(t, hub)
	defer server.Close()

	time.Sleep(50 * time.Millisecond)

	// Close the client connection.
	conn.Close()
	time.Sleep(100 * time.Millisecond)

	// Broadcast should not panic even though client is gone.
	hub.BroadcastEvent("sepolia", WSEvent{Type: EventPing})
	time.Sleep(50 * time.Millisecond)
}

func TestClient_WritePump_ChannelClose(t *testing.T) {
	hub := NewHub()
	go hub.Run()

	var testClient *Client
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		testClient = &Client{
			hub:         hub,
			conn:        conn,
			send:        make(chan []byte, wsSendBufferSize),
			networkName: "sepolia",
		}
		hub.register <- testClient
		go testClient.writePump()
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	time.Sleep(50 * time.Millisecond)

	// Stop the hub — this closes all client send channels, triggering the !ok path.
	hub.Stop()
	time.Sleep(50 * time.Millisecond)

	// The server should send a close message when send channel closes.
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, _, readErr := conn.ReadMessage()
	if readErr == nil {
		t.Error("expected error after server closes connection")
	}
}

func TestClient_WantsEvent(t *testing.T) {
	tests := []struct {
		name       string
		client     *Client
		eventType  WSEventType
		network    string
		wantResult bool
	}{
		{
			name:       "all events, matching network",
			client:     &Client{networkName: "sepolia"},
			eventType:  EventNewBlock,
			network:    "sepolia",
			wantResult: true,
		},
		{
			name:       "all events, wrong network",
			client:     &Client{networkName: "mainnet"},
			eventType:  EventNewBlock,
			network:    "sepolia",
			wantResult: false,
		},
		{
			name: "subscribed, matching",
			client: &Client{
				networkName:      "sepolia",
				subscribedEvents: map[WSEventType]struct{}{EventNewBlock: {}},
			},
			eventType:  EventNewBlock,
			network:    "sepolia",
			wantResult: true,
		},
		{
			name: "subscribed, not matching event",
			client: &Client{
				networkName:      "sepolia",
				subscribedEvents: map[WSEventType]struct{}{EventStatsUpdate: {}},
			},
			eventType:  EventNewBlock,
			network:    "sepolia",
			wantResult: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.client.wantsEvent(tt.eventType, tt.network)
			if got != tt.wantResult {
				t.Errorf("wantsEvent() = %v, want %v", got, tt.wantResult)
			}
		})
	}
}
