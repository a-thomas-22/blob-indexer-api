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

// wsDialStatus dials the test server's /ws endpoint (network=sepolia) with
// optional request headers and returns the handshake status code, closing any
// connection and response body for the caller. A status of 0 means no response
// was received.
func wsDialStatus(t *testing.T, serverURL string, header http.Header) int {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws?network=sepolia"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if conn != nil {
		conn.Close()
	}
	if resp == nil {
		return 0
	}
	defer resp.Body.Close()
	_ = err
	return resp.StatusCode
}

func TestWSCheckOrigin(t *testing.T) {
	allowList := config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://app.example.com"},
	}

	tests := []struct {
		name   string
		cfg    config.CORSConfig
		origin string
		want   bool
	}{
		{"allowed origin", allowList, "https://app.example.com", true},
		{"blocked origin", allowList, "https://evil.example.com", false},
		{"empty origin allowed", allowList, "", true},
		{"allow all origins", config.CORSConfig{Enabled: true, AllowAllOrigins: true}, "https://evil.example.com", true},
		{"cors disabled is permissive", config.CORSConfig{Enabled: false}, "https://evil.example.com", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := wsCheckOrigin(newCORSPolicy(tt.cfg))
			r := httptest.NewRequest(http.MethodGet, "/ws", http.NoBody)
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if got := check(r); got != tt.want {
				t.Errorf("wsCheckOrigin(%q) = %v, want %v", tt.origin, got, tt.want)
			}
		})
	}
}

func TestHandleWebSocket_OriginAllowed(t *testing.T) {
	api := newTestWSAPI(t)
	api.config.CORS = config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://app.example.com"},
	}
	// Production builds the origin policy once in newAPI; refresh it here since
	// the test mutates config after construction.
	api.wsOriginPolicy = newCORSPolicy(api.config.CORS)

	r := chi.NewRouter()
	r.Get("/ws", api.HandleWebSocket)
	server := httptest.NewServer(r)
	defer server.Close()

	header := http.Header{"Origin": []string{"https://app.example.com"}}
	if got := wsDialStatus(t, server.URL, header); got != http.StatusSwitchingProtocols {
		t.Errorf("got status %d, want %d", got, http.StatusSwitchingProtocols)
	}
}

func TestHandleWebSocket_OriginBlocked(t *testing.T) {
	api := newTestWSAPI(t)
	api.config.CORS = config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://app.example.com"},
	}
	// Production builds the origin policy once in newAPI; refresh it here since
	// the test mutates config after construction.
	api.wsOriginPolicy = newCORSPolicy(api.config.CORS)

	r := chi.NewRouter()
	r.Get("/ws", api.HandleWebSocket)
	server := httptest.NewServer(r)
	defer server.Close()

	header := http.Header{"Origin": []string{"https://evil.example.com"}}
	if got := wsDialStatus(t, server.URL, header); got != http.StatusForbidden {
		t.Errorf("got status %d, want %d", got, http.StatusForbidden)
	}

	// A blocked upgrade must not leak an admission slot.
	api.hub.admitMu.Lock()
	total := api.hub.total
	api.hub.admitMu.Unlock()
	if total != 0 {
		t.Errorf("blocked origin leaked an admission slot: total=%d", total)
	}
}

func TestHandleWebSocket_GlobalCapRejected(t *testing.T) {
	api := newTestWSAPI(t)
	api.config.WebSocket.MaxClients = 1

	r := chi.NewRouter()
	r.Get("/ws", api.HandleWebSocket)
	server := httptest.NewServer(r)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws?network=sepolia"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("first dial should succeed: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	defer conn.Close()

	// Second connection exceeds the global cap and must be rejected.
	if got := wsDialStatus(t, server.URL, nil); got != http.StatusTooManyRequests {
		t.Errorf("got status %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestHandleWebSocket_PerIPCapRejected(t *testing.T) {
	api := newTestWSAPI(t)
	api.config.WebSocket.MaxConnsPerIP = 1

	r := chi.NewRouter()
	r.Get("/ws", api.HandleWebSocket)
	server := httptest.NewServer(r)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws?network=sepolia"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("first dial should succeed: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	defer conn.Close()

	// Same loopback IP — second connection exceeds the per-IP cap.
	if got := wsDialStatus(t, server.URL, nil); got != http.StatusTooManyRequests {
		t.Errorf("got status %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestHandleWebSocket_CapReleasedOnDisconnect(t *testing.T) {
	api := newTestWSAPI(t)
	api.config.WebSocket.MaxConnsPerIP = 1

	r := chi.NewRouter()
	r.Get("/ws", api.HandleWebSocket)
	server := httptest.NewServer(r)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws?network=sepolia"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("first dial should succeed: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}

	// Close the first connection and wait for the hub to process unregister.
	conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		got = wsDialStatus(t, server.URL, nil)
		if got == http.StatusSwitchingProtocols {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got != http.StatusSwitchingProtocols {
		t.Fatalf("reconnect after disconnect should succeed, got status %d", got)
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
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}

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
	if resp != nil {
		if resp.Body != nil {
			resp.Body.Close()
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
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
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
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
	if resp != nil {
		if resp.Body != nil {
			resp.Body.Close()
		}
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
		}
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
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial through full router failed: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	conn.Close()
}
