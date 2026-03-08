package attribution

import (
	"testing"

	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func TestNewService(t *testing.T) {
	s := NewService(nil)
	if s == nil {
		t.Fatal("expected non-nil service")
	}
	if s.networkID != 1 {
		t.Errorf("expected default networkID=1, got %d", s.networkID)
	}
	if s.knownUsers == nil {
		t.Error("expected knownUsers map to be initialized")
	}
}

func TestSetNetworkID(t *testing.T) {
	s := NewService(nil)
	s.SetNetworkID(42)
	if s.networkID != 42 {
		t.Errorf("expected networkID=42, got %d", s.networkID)
	}
}

func TestGetUserAttribution_Known(t *testing.T) {
	s := NewService(nil)
	s.knownUsers["0xabcdef"] = "Optimism"

	result := s.GetUserAttribution("0xABCDEF")
	if result != "Optimism" {
		t.Errorf("expected 'Optimism', got %q", result)
	}
}

func TestGetUserAttribution_Unknown(t *testing.T) {
	s := NewService(nil)
	result := s.GetUserAttribution("0x1234")
	if result != "" {
		t.Errorf("expected empty string for unknown address, got %q", result)
	}
}

func TestGetUserAttribution_CaseInsensitive(t *testing.T) {
	s := NewService(nil)
	s.knownUsers["0xdeadbeef"] = "TestUser"

	tests := []string{
		"0xDeAdBeEf",
		"0xDEADBEEF",
		"0xdeadbeef",
	}
	for _, addr := range tests {
		result := s.GetUserAttribution(addr)
		if result != "TestUser" {
			t.Errorf("GetUserAttribution(%q) = %q, want 'TestUser'", addr, result)
		}
	}
}
