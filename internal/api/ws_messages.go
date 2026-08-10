package api

import "time"

// WSEventType represents the type of a WebSocket event.
type WSEventType string

const (
	// EventNewBlock is emitted when a new block is indexed — including blocks
	// with zero blob transactions, which still carry pricing data.
	EventNewBlock WSEventType = "new_block"
	// EventBlockSnapshot is sent once to every newly connected client and
	// carries the most recent blocks, so reconnecting clients recover blocks
	// broadcast while they were away without refetching.
	EventBlockSnapshot WSEventType = "block_snapshot"
	// EventMempoolUpdate is emitted when a blob tx enters or leaves the mempool.
	EventMempoolUpdate WSEventType = "mempool_update"
	// EventStatsUpdate is emitted when aggregate stats change.
	EventStatsUpdate WSEventType = "stats_update"
	// EventUsersUpdate is emitted when user rankings change.
	EventUsersUpdate WSEventType = "users_update"
	// EventPing is a heartbeat sent every 30s.
	EventPing WSEventType = "ping"

	// MempoolActionAdd indicates a pending blob transaction was observed.
	MempoolActionAdd = "add"
	// MempoolActionRemove indicates a pending blob transaction left the mempool.
	MempoolActionRemove = "remove"
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
	// Range identifies the aggregation window a users_update payload covers
	// (currently always "all"), so clients viewing a different REST range can
	// drop mismatched pushes instead of overwriting their table with them. It
	// lives on the envelope because the users_update data payload is a bare
	// array, which can't grow a field without breaking its shape. Empty for
	// event types that have no aggregation window.
	Range string `json:"range,omitempty"`
	// Group identifies the row grouping a users_update payload uses, matching
	// the REST `group` values on GET /users: absent for per-address rows,
	// "entity" for entity-grouped rows. Both variants are broadcast on every
	// users update, so clients must filter on this field — a table in one
	// mode has to drop pushes for the other or the two payloads overwrite
	// each other. Envelope-level for the same reason as Range.
	Group string      `json:"group,omitempty"`
	Data  interface{} `json:"data,omitempty"`
}

// NewBlockData is the payload for EventNewBlock.
type NewBlockData struct {
	BlockNumber int64                 `json:"block_number"`
	BlobCount   int                   `json:"blob_count"`
	Timestamp   time.Time             `json:"timestamp"`
	Blobs       []BlobResponse        `json:"blobs"`
	Pricing     *BlockPricingResponse `json:"pricing,omitempty"`
}

// BlockSnapshotData is the payload for EventBlockSnapshot. Blocks are ordered
// newest first.
type BlockSnapshotData struct {
	Blocks []NewBlockData `json:"blocks"`
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
