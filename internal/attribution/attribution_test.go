package attribution

import (
	"testing"
)

const testUserOptimism = "Optimism"

func TestNewService(t *testing.T) {
	svc := NewService(nil)
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	if svc.networkID != 1 {
		t.Errorf("expected default network ID 1, got %d", svc.networkID)
	}
	if svc.knownUsers == nil {
		t.Fatal("expected non-nil knownUsers map")
	}
}

func TestSetNetworkID(t *testing.T) {
	svc := NewService(nil)
	svc.SetNetworkID(11155111)
	if svc.networkID != 11155111 {
		t.Errorf("expected network ID 11155111, got %d", svc.networkID)
	}
}

func TestGetUserAttribution_KnownUser(t *testing.T) {
	svc := NewService(nil)
	svc.knownUsers["0xabcdef1234567890abcdef1234567890abcdef12"] = testUserOptimism

	result := svc.GetUserAttribution("0xABCDEF1234567890ABCDEF1234567890ABCDEF12")
	if result != testUserOptimism {
		t.Errorf("expected %q, got %q", testUserOptimism, result)
	}
}

func TestGetUserAttribution_UnknownUser(t *testing.T) {
	svc := NewService(nil)

	result := svc.GetUserAttribution("0x0000000000000000000000000000000000000000")
	if result != "" {
		t.Errorf("expected empty string for unknown user, got %q", result)
	}
}

func TestGetUserAttribution_CaseInsensitive(t *testing.T) {
	svc := NewService(nil)
	svc.knownUsers["0xaaa"] = "TestUser"

	tests := []struct {
		name    string
		address string
		want    string
	}{
		{"lowercase", "0xaaa", "TestUser"},
		{"uppercase", "0xAAA", "TestUser"},
		{"mixed case", "0xAaA", "TestUser"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := svc.GetUserAttribution(tt.address)
			if result != tt.want {
				t.Errorf("GetUserAttribution(%q) = %q, want %q", tt.address, result, tt.want)
			}
		})
	}
}

func TestGetUserAttribution_MultipleUsers(t *testing.T) {
	svc := NewService(nil)
	svc.knownUsers["0xaaa"] = testUserOptimism
	svc.knownUsers["0xbbb"] = "Arbitrum"
	svc.knownUsers["0xccc"] = "Base"

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
		result := svc.GetUserAttribution(tt.address)
		if result != tt.want {
			t.Errorf("GetUserAttribution(%q) = %q, want %q", tt.address, result, tt.want)
		}
	}
}
