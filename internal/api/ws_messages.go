package api

import "time"

// WSEventType represents the type of a WebSocket event.
type WSEventType string

const (
	// EventNewBlock is emitted when a new block with blobs is indexed.
	EventNewBlock WSEventType = "new_block"
	// EventMempoolUpdate is emitted when a blob tx enters or leaves the mempool.
	EventMempoolUpdate WSEventType = "mempool_update"
	// EventStatsUpdate is emitted when aggregate stats change.
	EventStatsUpdate WSEventType = "stats_update"
	// EventUsersUpdate is emitted when user rankings change.
	EventUsersUpdate WSEventType = "users_update"
	// EventPing is a heartbeat sent every 30s.
	EventPing WSEventType = "ping"
)

// AllEventTypes lists every subscribable event type.
var AllEventTypes = []WSEventType{
	EventNewBlock,
	EventMempoolUpdate,
	EventStatsUpdate,
	EventUsersUpdate,
}

// WSEvent is the envelope for all server-to-client WebSocket messages.
type WSEvent struct {
	Type WSEventType `json:"type"`
	Data interface{} `json:"data,omitempty"`
}

// NewBlockData is the payload for EventNewBlock.
type NewBlockData struct {
	BlockNumber int64          `json:"block_number"`
	BlobCount   int            `json:"blob_count"`
	Timestamp   time.Time      `json:"timestamp"`
	Blobs       []BlobResponse `json:"blobs"`
}

// MempoolUpdateData is the payload for EventMempoolUpdate.
type MempoolUpdateData struct {
	Action string       `json:"action"` // "add" or "remove"
	Blob   BlobResponse `json:"blob"`
}

// WSSubscribeMessage is an optional client-to-server subscription filter.
// If sent, only the listed event types will be delivered to the client.
type WSSubscribeMessage struct {
	Subscribe []WSEventType `json:"subscribe"`
}
