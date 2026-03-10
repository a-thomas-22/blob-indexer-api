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
type Hub struct {
	clients     map[*Client]struct{}
	broadcast   chan broadcastMessage
	register    chan *Client
	unregister  chan *Client
	done        chan struct{}
	stopOnce    sync.Once
	clientCount atomic.Int64
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
	}
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
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
				h.clientCount.Store(int64(len(h.clients)))
				logger.Debug("WebSocket client unregistered",
					zap.String("network", client.networkName),
					zap.Int("total_clients", len(h.clients)))
			}

		case msg := <-h.broadcast:
			for client := range h.clients {
				if !client.wantsEvent(msg.eventType, msg.networkName) {
					continue
				}
				select {
				case client.send <- msg.data:
				default:
					// Slow client — drop it.
					delete(h.clients, client)
					close(client.send)
					h.clientCount.Store(int64(len(h.clients)))
					logger.Warn("Dropping slow WebSocket client",
						zap.String("network", client.networkName))
				}
			}

		case <-h.done:
			for client := range h.clients {
				close(client.send)
				delete(h.clients, client)
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
