package ethereum

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/rpc"
)

type wsChainService struct{}

func (wsChainService) ChainId() string { return "0x2a" } //nolint:revive // JSON-RPC method name

// newWebsocketTestClient serves the eth namespace over WebSocket, the
// transport production nodes are dialed with.
func newWebsocketTestClient(t *testing.T, opts ...ClientOption) *Client {
	t.Helper()
	server := rpc.NewServer()
	if err := server.RegisterName("eth", wsChainService{}); err != nil {
		t.Fatalf("register service: %v", err)
	}
	httpServer := httptest.NewServer(server.WebsocketHandler([]string{"*"}))
	t.Cleanup(httpServer.Close)

	client, err := NewClient("ws"+strings.TrimPrefix(httpServer.URL, "http"), opts...)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(client.Close)
	if !client.IsWebsocket() {
		t.Fatal("expected a websocket client")
	}
	return client
}

func TestWebsocketClient_HonorsProactiveRateLimit(t *testing.T) {
	client := newWebsocketTestClient(t, WithRateLimit(RateLimitConfig{RequestsPerSecond: 4}))
	ctx := context.Background()

	// Burst of 4 admits the first four calls at once; the next four each
	// wait a quarter second for a token.
	start := time.Now()
	for i := 0; i < 8; i++ {
		if _, err := client.GetChainID(ctx); err != nil {
			t.Fatalf("GetChainID %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("8 calls at 4/s finished in %s; the websocket client is not rate limited", elapsed)
	}
}

func TestWebsocketClient_RateLimitWaitHonorsContext(t *testing.T) {
	client := newWebsocketTestClient(t, WithRateLimit(RateLimitConfig{RequestsPerSecond: 0.5}))

	// The single burst token goes to the first call; the second would wait
	// two seconds, but its context is canceled first.
	if _, err := client.GetChainID(context.Background()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.GetChainID(ctx)
	if err == nil {
		t.Fatal("expected the throttled call to fail once its context expired")
	}
	if !strings.Contains(err.Error(), "rate limit wait") {
		t.Fatalf("expected a rate limit wait error, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("throttled call did not return promptly on context expiry")
	}
}

func TestWebsocketClient_UnlimitedWithoutRateLimit(t *testing.T) {
	client := newWebsocketTestClient(t, WithRateLimit(RateLimitConfig{RequestsPerSecond: 0}))
	if client.limiter != nil {
		t.Fatal("expected no limiter when the rate is zero")
	}
	plain := newWebsocketTestClient(t)
	if plain.limiter != nil {
		t.Fatal("expected no limiter without the option")
	}
	if err := plain.throttle(context.Background()); err != nil {
		t.Fatalf("throttle without limiter: %v", err)
	}
}
