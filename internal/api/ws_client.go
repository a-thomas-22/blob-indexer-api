package api

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	// wsWriteWait is the time allowed to write a message to the peer.
	wsWriteWait = 10 * time.Second

	// wsPongWait is the time allowed to read the next pong message from the peer.
	wsPongWait = 60 * time.Second

	// wsPingPeriod is how often the server sends a ping (must be < wsPongWait).
	wsPingPeriod = 30 * time.Second

	// wsMaxMessageSize is the maximum size of an incoming message.
	wsMaxMessageSize = 4096

	// wsSendBufferSize is the capacity of the per-client send channel.
	wsSendBufferSize = 64
)

// Client represents a single WebSocket connection.
type Client struct {
	hub            *Hub
	conn           *websocket.Conn
	send           chan []byte
	networkChainID int
	networkName    string
	// remoteIP is the resolved client IP used for the per-IP connection cap.
	// It must match the value passed to Hub.admit so the slot is released on
	// disconnect.
	remoteIP string

	mu               sync.RWMutex
	subscribedEvents map[WSEventType]struct{} // nil = all events
}

// wantsEvent returns true if this client should receive the given event.
func (c *Client) wantsEvent(eventType WSEventType, networkName string) bool {
	if c.networkName != networkName {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.subscribedEvents == nil {
		return true
	}
	_, ok := c.subscribedEvents[eventType]
	return ok
}

// readPump reads messages from the WebSocket connection.
// It handles subscription control messages and closes on any error.
func (c *Client) readPump() {
	defer func() {
		select {
		case c.hub.unregister <- c:
		default:
		}
		c.conn.Close()
	}()
	c.conn.SetReadLimit(wsMaxMessageSize)
	if err := c.conn.SetReadDeadline(time.Now().Add(wsPongWait)); err != nil {
		return
	}
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseNormalClosure) {
				logger.Debug("WebSocket read error",
					zap.String("network", c.networkName),
					zap.Error(err))
			}
			return
		}

		var sub WSSubscribeMessage
		if err := json.Unmarshal(message, &sub); err != nil {
			continue // ignore unparseable messages
		}
		if len(sub.Subscribe) > 0 {
			events := make(map[WSEventType]struct{}, len(sub.Subscribe))
			for _, e := range sub.Subscribe {
				events[e] = struct{}{}
			}
			c.mu.Lock()
			c.subscribedEvents = events
			c.mu.Unlock()
			logger.Debug("WebSocket client updated subscription",
				zap.String("network", c.networkName),
				zap.Int("event_count", len(events)))
		}
	}
}

// writePump writes messages to the WebSocket connection.
// It sends queued events and periodic pings.
func (c *Client) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			if err := c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
				return
			}
			if !ok {
				// Hub closed the channel.
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		case <-ticker.C:
			if err := c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
				return
			}
			// Application-level ping.
			pingData, _ := json.Marshal(WSEvent{Type: EventPing})
			if err := c.conn.WriteMessage(websocket.TextMessage, pingData); err != nil {
				return
			}
			// WebSocket-level ping for connection liveness.
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
