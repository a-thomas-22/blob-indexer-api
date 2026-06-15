package api

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// Hub maintains the set of active WebSocket clients and broadcasts
// events to the ones whose network and subscription filters match.
//
// Admission control (the global and per-IP connection caps) is enforced by a
// dedicated mutex rather than the channel-serialized clients map, because
// over-cap upgrades must be rejected synchronously by the upgrade handler
// before any per-client goroutines or buffers are allocated.
type Hub struct {
	clients     map[*Client]struct{}
	broadcast   chan broadcastMessage
	register    chan *Client
	unregister  chan *Client
	done        chan struct{}
	stopOnce    sync.Once
	clientCount atomic.Int64

	admitMu sync.Mutex
	total   int            // reserved slots, guarded by admitMu
	perIP   map[string]int // reserved slots per IP, guarded by admitMu
}

// broadcastMessage carries a pre-marshaled event plus its metadata so
// the Hub can filter without re-parsing.
type broadcastMessage struct {
	eventType   WSEventType
	networkName string
	data        []byte // JSON-encoded WSEvent
}

// NewHub creates a new Hub.
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]struct{}),
		broadcast:  make(chan broadcastMessage, 256),
		register:   make(chan *Client, 16),
		unregister: make(chan *Client, 16),
		done:       make(chan struct{}),
		perIP:      make(map[string]int),
	}
}

// admit reserves a connection slot for ip, enforcing the global cap maxClients
// and the per-IP cap maxConnsPerIP. A limit that is zero or negative is treated
// as unlimited. It returns true if the slot was reserved; on failure no slot is
// held and the caller must reject the upgrade. Every successful admit must be
// paired with exactly one release, which the unregister path guarantees.
func (h *Hub) admit(ip string, maxClients, maxConnsPerIP int) bool {
	h.admitMu.Lock()
	defer h.admitMu.Unlock()

	if maxClients > 0 && h.total >= maxClients {
		return false
	}
	if maxConnsPerIP > 0 && ip != "" && h.perIP[ip] >= maxConnsPerIP {
		return false
	}

	h.total++
	if ip != "" {
		h.perIP[ip]++
	}
	return true
}

// release returns the connection slot previously reserved by admit for ip.
func (h *Hub) release(ip string) {
	h.admitMu.Lock()
	defer h.admitMu.Unlock()

	if h.total > 0 {
		h.total--
	}
	if ip == "" {
		return
	}
	if n := h.perIP[ip]; n <= 1 {
		delete(h.perIP, ip)
	} else {
		h.perIP[ip] = n - 1
	}
}

// removeClient deletes a client from the hub, closes its send channel, and
// releases its admission slot. It must only be called from the Run goroutine.
func (h *Hub) removeClient(client *Client) {
	if _, ok := h.clients[client]; !ok {
		return
	}
	delete(h.clients, client)
	close(client.send)
	h.clientCount.Store(int64(len(h.clients)))
	h.release(client.remoteIP)
}

// Run starts the hub's main select loop. It must be called in its own goroutine.
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = struct{}{}
			h.clientCount.Store(int64(len(h.clients)))
			logger.Debug("WebSocket client registered",
				zap.String("network", client.networkName),
				zap.Int("total_clients", len(h.clients)))

		case client := <-h.unregister:
			h.removeClient(client)
			logger.Debug("WebSocket client unregistered",
				zap.String("network", client.networkName),
				zap.Int("total_clients", len(h.clients)))

		case msg := <-h.broadcast:
			for client := range h.clients {
				if !client.wantsEvent(msg.eventType, msg.networkName) {
					continue
				}
				select {
				case client.send <- msg.data:
				default:
					// Slow client — drop it.
					h.removeClient(client)
					logger.Warn("Dropping slow WebSocket client",
						zap.String("network", client.networkName))
				}
			}

		case <-h.done:
			// Best-effort drain of queued admissions/removals so a client that
			// was admitted (slot reserved) and sent to register/unregister but
			// not yet processed has its slot released and goroutines unblocked,
			// rather than leaking. Registered clients fall through to the
			// teardown below; unregistered ones are released by removeClient.
			for draining := true; draining; {
				select {
				case client := <-h.register:
					h.clients[client] = struct{}{}
				case client := <-h.unregister:
					h.removeClient(client)
				default:
					draining = false
				}
			}
			for client := range h.clients {
				close(client.send)
				delete(h.clients, client)
				h.release(client.remoteIP)
			}
			h.clientCount.Store(0)
			return
		}
	}
}

// BroadcastEvent marshals an event once and enqueues it for fan-out.
func (h *Hub) BroadcastEvent(networkName string, event WSEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		logger.Error("Failed to marshal WebSocket event", zap.Error(err))
		return
	}

	select {
	case h.broadcast <- broadcastMessage{
		eventType:   event.Type,
		networkName: networkName,
		data:        data,
	}:
	default:
		logger.Warn("WebSocket broadcast channel full, dropping event",
			zap.String("type", string(event.Type)),
			zap.String("network", networkName))
	}
}

// Stop signals the hub to shut down. It is safe to call multiple times.
func (h *Hub) Stop() {
	h.stopOnce.Do(func() {
		close(h.done)
	})
}

// ClientCount returns the number of connected clients. Safe for concurrent use.
func (h *Hub) ClientCount() int {
	return int(h.clientCount.Load())
}
