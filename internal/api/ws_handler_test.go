package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func newTestWSAPI(t *testing.T) *API {
	t.Helper()
	hub := NewHub()
	go hub.Run()
	t.Cleanup(hub.Stop)

	return &API{
		hub: hub,
		networks: map[int]config.NetworkConfig{
			11155111: {Name: "sepolia", ChainID: 11155111, Enabled: true},
		},
		config:        &config.Config{},
		statsCache:    make(map[int]statsCacheEntry),
		topUsersCache: make(map[string]topUsersCacheEntry),
	}
}

func TestHandleWebSocket_Success(t *testing.T) {
	api := newTestWSAPI(t)

	r := chi.NewRouter()
	r.Get("/ws", api.HandleWebSocket)

	server := httptest.NewServer(r)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws?network=sepolia"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}

	// Send a subscribe message to verify the connection works.
	sub, _ := json.Marshal(WSSubscribeMessage{Subscribe: []WSEventType{EventNewBlock}})
	if err := conn.WriteMessage(websocket.TextMessage, sub); err != nil {
		t.Fatal(err)
	}

	// Broadcast an event and verify reception.
	time.Sleep(50 * time.Millisecond)
	api.hub.BroadcastEvent("sepolia", WSEvent{Type: EventNewBlock, Data: NewBlockData{BlockNumber: 1}})

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
		t.Errorf("got type %q, want %q", event.Type, EventNewBlock)
	}
}

func TestHandleWebSocket_InvalidNetwork(t *testing.T) {
	api := newTestWSAPI(t)

	r := chi.NewRouter()
	r.Get("/ws", api.HandleWebSocket)

	server := httptest.NewServer(r)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws?network=nonexistent"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected dial to fail for invalid network")
	}
	if resp != nil && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestHandleWebSocket_DefaultNetwork(t *testing.T) {
	api := newTestWSAPI(t)

	r := chi.NewRouter()
	r.Get("/ws", api.HandleWebSocket)

	server := httptest.NewServer(r)
	defer server.Close()

	// No ?network= param — should default to first enabled.
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	conn.Close()
}

func TestHandleWebSocket_NilHub(t *testing.T) {
	api := &API{
		hub: nil,
		networks: map[int]config.NetworkConfig{
			11155111: {Name: "sepolia", ChainID: 11155111, Enabled: true},
		},
		config:        &config.Config{},
		statsCache:    make(map[int]statsCacheEntry),
		topUsersCache: make(map[string]topUsersCacheEntry),
	}

	r := chi.NewRouter()
	r.Get("/ws", api.HandleWebSocket)

	server := httptest.NewServer(r)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws?network=sepolia"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected dial to fail when hub is nil")
	}
	if resp != nil && resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestHandleWebSocket_ViaFullRouter(t *testing.T) {
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080},
		Indexer: config.IndexerConfig{Version: "test"},
		Networks: []config.NetworkConfig{
			{Name: "sepolia", ChainID: 11155111, Enabled: true},
		},
		WebSocket: config.WebSocketConfig{
			PollInterval:          3 * time.Second,
			UsersThrottleInterval: 30 * time.Second,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := NewRouter(ctx, nil, cfg)
	server := httptest.NewServer(handler)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/ws?network=sepolia"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial through full router failed: %v", err)
	}
	defer conn.Close()
}
