package api

import (
	"encoding/json"
	"testing"
	"time"

	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func TestWSEventType_Constants(t *testing.T) {
	tests := []struct {
		got  WSEventType
		want string
	}{
		{EventNewBlock, "new_block"},
		{EventMempoolUpdate, "mempool_update"},
		{EventStatsUpdate, "stats_update"},
		{EventUsersUpdate, "users_update"},
		{EventPing, "ping"},
	}
	for _, tt := range tests {
		if string(tt.got) != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}

func TestAllEventTypes_ContainsSubscribableEvents(t *testing.T) {
	expected := map[WSEventType]bool{
		EventNewBlock:      true,
		EventMempoolUpdate: true,
		EventStatsUpdate:   true,
		EventUsersUpdate:   true,
	}
	if len(AllEventTypes) != len(expected) {
		t.Fatalf("AllEventTypes has %d elements, want %d", len(AllEventTypes), len(expected))
	}
	for _, et := range AllEventTypes {
		if !expected[et] {
			t.Errorf("unexpected event type in AllEventTypes: %q", et)
		}
	}
}

func TestWSEvent_MarshalJSON(t *testing.T) {
	event := WSEvent{
		Type: EventPing,
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "ping" {
		t.Errorf("got type %q, want %q", m["type"], "ping")
	}
	if _, ok := m["data"]; ok {
		t.Error("expected no data field for ping event")
	}
}

func TestWSEvent_WithData_MarshalJSON(t *testing.T) {
	event := WSEvent{
		Type: EventNewBlock,
		Data: NewBlockData{
			BlockNumber: 12345,
			BlobCount:   3,
			Timestamp:   time.Date(2026, 3, 9, 14, 0, 0, 0, time.UTC),
			Blobs: []BlobResponse{
				{TxHash: "0xabc", BlockNumber: 12345, NetworkName: "sepolia"},
			},
			Pricing: &BlockPricingResponse{
				BlockNumber:        12345,
				BlobGasUsed:        393216,
				TargetBlobs:        3,
				MaxBlobs:           6,
				AvailableBlobs:     3,
				UtilizationPercent: 50,
				IsAboveTarget:      false,
			},
		},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "new_block" {
		t.Errorf("got type %q, want %q", m["type"], "new_block")
	}
	d, ok := m["data"].(map[string]interface{})
	if !ok {
		t.Fatal("expected data to be a map")
	}
	if d["block_number"].(float64) != 12345 {
		t.Errorf("got block_number %v, want 12345", d["block_number"])
	}
	if d["blob_count"].(float64) != 3 {
		t.Errorf("got blob_count %v, want 3", d["blob_count"])
	}
	pricing, ok := d["pricing"].(map[string]interface{})
	if !ok {
		t.Fatal("expected pricing to be a map")
	}
	if pricing["max_blobs"].(float64) != 6 {
		t.Errorf("got pricing.max_blobs %v, want 6", pricing["max_blobs"])
	}
	if pricing["blob_gas_used"].(float64) != 393216 {
		t.Errorf("got pricing.blob_gas_used %v, want 393216", pricing["blob_gas_used"])
	}
}

func TestMempoolUpdateData_MarshalJSON(t *testing.T) {
	event := WSEvent{
		Type: EventMempoolUpdate,
		Data: MempoolUpdateData{
			Action: "add",
			Blob:   BlobResponse{TxHash: "0xdef", Confirmed: false},
		},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	d := m["data"].(map[string]interface{})
	if d["action"] != "add" {
		t.Errorf("got action %q, want %q", d["action"], "add")
	}
}

func TestWSSubscribeMessage_UnmarshalJSON(t *testing.T) {
	raw := `{"subscribe":["new_block","stats_update"]}`
	var msg WSSubscribeMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Subscribe) != 2 {
		t.Fatalf("got %d subscriptions, want 2", len(msg.Subscribe))
	}
	if msg.Subscribe[0] != EventNewBlock {
		t.Errorf("got %q, want %q", msg.Subscribe[0], EventNewBlock)
	}
	if msg.Subscribe[1] != EventStatsUpdate {
		t.Errorf("got %q, want %q", msg.Subscribe[1], EventStatsUpdate)
	}
}

func TestWSSubscribeMessage_EmptySubscribe(t *testing.T) {
	raw := `{"subscribe":[]}`
	var msg WSSubscribeMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Subscribe) != 0 {
		t.Fatalf("got %d subscriptions, want 0", len(msg.Subscribe))
	}
}
