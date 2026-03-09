package attribution

import (
	"context"
	"testing"

	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

const testUserOptimism = "Optimism"

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

func TestNormalizeAddress(t *testing.T) {
	got := normalizeAddress("0xABCDEF1234567890ABCDEF1234567890ABCDEF12")
	want := "0xabcdef1234567890abcdef1234567890abcdef12"
	if got != want {
		t.Fatalf("normalizeAddress() = %q, want %q", got, want)
	}
}

func TestNewService_WithNetworkID(t *testing.T) {
	s := NewService(nil, 42)
	if s.networkID != 42 {
		t.Fatalf("expected networkID=42, got %d", s.networkID)
	}
}

func TestSetNetworkID(t *testing.T) {
	s := NewService(nil)
	s.SetNetworkID(42)
	if s.networkID != 42 {
		t.Errorf("expected networkID=42, got %d", s.networkID)
	}
}

func TestGetUserAttribution_KnownUser(t *testing.T) {
	s := NewService(nil)
	s.knownUsers["0xabcdef1234567890abcdef1234567890abcdef12"] = testUserOptimism

	result := s.GetUserAttribution("0xABCDEF1234567890ABCDEF1234567890ABCDEF12")
	if result != testUserOptimism {
		t.Errorf("expected %q, got %q", testUserOptimism, result)
	}
}

func TestGetUserAttribution_Unknown(t *testing.T) {
	s := NewService(nil)
	result := s.GetUserAttribution("0x1234")
	if result != "" {
		t.Errorf("expected empty string for unknown address, got %q", result)
	}
}

func TestUpdateUserLastSeen_UnknownAddress(t *testing.T) {
	s := NewService(nil)
	// Unknown address should return nil without DB call.
	err := s.UpdateUserLastSeen(context.TODO(), "0xunknown")
	if err != nil {
		t.Errorf("expected nil error for unknown address, got %v", err)
	}
}

func TestGetUserAttribution_CaseInsensitive(t *testing.T) {
	s := NewService(nil)
	s.knownUsers["0xdeadbeef"] = "TestUser"

	for _, addr := range []string{"0xDeAdBeEf", "0xDEADBEEF", "0xdeadbeef"} {
		result := s.GetUserAttribution(addr)
		if result != "TestUser" {
			t.Errorf("GetUserAttribution(%q) = %q, want %q", addr, result, "TestUser")
		}
	}
}

func TestGetUserAttribution_MultipleUsers(t *testing.T) {
	s := NewService(nil)
	s.knownUsers["0xaaa"] = testUserOptimism
	s.knownUsers["0xbbb"] = "Arbitrum"
	s.knownUsers["0xccc"] = "Base"

	tests := []struct {
		address string
		want    string
	}{
		{"0xAAA", testUserOptimism},
		{"0xBBB", "Arbitrum"},
		{"0xCCC", "Base"},
		{"0xDDD", ""},
	}

	for _, tt := range tests {
		result := s.GetUserAttribution(tt.address)
		if result != tt.want {
			t.Errorf("GetUserAttribution(%q) = %q, want %q", tt.address, result, tt.want)
		}
	}
}
