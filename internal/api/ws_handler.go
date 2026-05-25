package api

import (
	"net/http"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// wsUpgrader configures the WebSocket upgrade.
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// WebSocket origin checks are handled separately from HTTP CORS.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// HandleWebSocket upgrades an HTTP connection to WebSocket and registers the
// client with the hub. The network is selected via the ?network= query param,
// same as all other API endpoints.
func (a *API) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	if a.hub == nil {
		a.respondError(w, http.StatusServiceUnavailable, "WebSocket not available")
		return
	}

	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("WebSocket upgrade failed",
			zap.String("network", network.Name),
			zap.Error(err))
		return
	}

	client := &Client{
		hub:            a.hub,
		conn:           conn,
		send:           make(chan []byte, wsSendBufferSize),
		networkChainID: network.ChainID,
		networkName:    network.Name,
	}

	select {
	case a.hub.register <- client:
		go client.writePump()
		go client.readPump()
	case <-a.hub.done:
		logger.Error("WebSocket registration failed: hub is shutting down",
			zap.String("network", network.Name))
		_ = conn.Close()
	}
}
