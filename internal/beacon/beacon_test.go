package beacon

import "testing"

func TestResolveClock_KnownChains(t *testing.T) {
	tests := []struct {
		name        string
		chainID     int
		wantGenesis uint64
	}{
		{"mainnet", 1, 1606824023},
		{"goerli", 5, 1616508000},
		{"sepolia", 11155111, 1655733600},
		{"holesky", 17000, 1695902400},
		{"hoodi", 560048, 1742213400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock, ok := ResolveClock(tt.chainID, 0, 0)
			if !ok {
				t.Fatalf("expected a clock for chain %d", tt.chainID)
			}
			if clock.GenesisTime != tt.wantGenesis {
				t.Errorf("genesis time = %d, want %d", clock.GenesisTime, tt.wantGenesis)
			}
			if clock.SecondsPerSlot != DefaultSecondsPerSlot {
				t.Errorf("seconds per slot = %d, want %d", clock.SecondsPerSlot, DefaultSecondsPerSlot)
			}
		})
	}
}

func TestResolveClock_UnknownChain(t *testing.T) {
	if _, ok := ResolveClock(999999, 0, 0); ok {
		t.Fatal("expected no clock for an unknown chain without configuration")
	}
}

func TestResolveClock_ConfigOverrides(t *testing.T) {
	// An unknown chain becomes derivable once configured.
	clock, ok := ResolveClock(999999, 1700000000, 5)
	if !ok {
		t.Fatal("expected a clock for a configured unknown chain")
	}
	if clock.GenesisTime != 1700000000 || clock.SecondsPerSlot != 5 {
		t.Errorf("clock = %+v, want genesis 1700000000 / 5s slots", clock)
	}

	// Explicit configuration wins over the compiled constant.
	clock, ok = ResolveClock(1, 42, 0)
	if !ok || clock.GenesisTime != 42 {
		t.Errorf("configured genesis should override compiled constant, got %+v ok=%v", clock, ok)
	}
}

func TestSlotAt(t *testing.T) {
	mainnet, _ := ResolveClock(1, 0, 0)

	// The merge block: slot 4700013 at timestamp 1663224179.
	slot, ok := mainnet.SlotAt(1663224179)
	if !ok || slot != 4700013 {
		t.Errorf("merge block slot = %d ok=%v, want 4700013", slot, ok)
	}

	// Genesis itself is slot 0.
	slot, ok = mainnet.SlotAt(mainnet.GenesisTime)
	if !ok || slot != 0 {
		t.Errorf("genesis slot = %d ok=%v, want 0", slot, ok)
	}

	// Pre-genesis timestamps are underivable.
	if _, ok := mainnet.SlotAt(mainnet.GenesisTime - 1); ok {
		t.Error("expected no slot before genesis")
	}

	// A zero-valued clock never derives (division guard).
	if _, ok := (Clock{}).SlotAt(123); ok {
		t.Error("expected no slot from a zero clock")
	}
}
