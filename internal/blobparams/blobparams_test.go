package blobparams

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

func TestChainConfigForID_KnownChains(t *testing.T) {
	tests := []struct {
		name    string
		chainID int
		want    *params.ChainConfig
	}{
		{"mainnet", 1, params.MainnetChainConfig},
		{"sepolia", 11155111, params.SepoliaChainConfig},
		{"holesky", 17000, params.HoleskyChainConfig},
		{"hoodi", 560048, params.HoodiChainConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ChainConfigForID(tt.chainID)
			if cfg.ChainID.Int64() != tt.want.ChainID.Int64() {
				t.Errorf("ChainConfigForID(%d) chain ID = %d, want %d", tt.chainID, cfg.ChainID.Int64(), tt.want.ChainID.Int64())
			}
			if cfg.BlobScheduleConfig == nil {
				t.Errorf("ChainConfigForID(%d) has nil BlobScheduleConfig", tt.chainID)
			}
		})
	}
}

func TestChainConfigForID_UnknownChain(t *testing.T) {
	cfg := ChainConfigForID(999999)
	if cfg.ChainID.Int64() != 999999 {
		t.Errorf("expected chain ID 999999, got %d", cfg.ChainID.Int64())
	}
	if cfg.BlobScheduleConfig == nil {
		t.Error("synthetic config should have BlobScheduleConfig")
	}
	if cfg.CancunTime == nil || *cfg.CancunTime != 0 {
		t.Error("synthetic config should have CancunTime=0")
	}
}

func TestGetBlobParams_HoodiBPO(t *testing.T) {
	// Regression: Hoodi (560048) must use its real fork schedule, not a
	// Cancun-only synthetic fallback. Hoodi's BPO2 activates at 1762955544
	// with target 14 / max 21 / update fraction 11684671.
	cfg := ChainConfigForID(560048)
	if cfg.ChainID.Int64() != 560048 {
		t.Fatalf("Hoodi chain ID = %d, want 560048", cfg.ChainID.Int64())
	}
	bp := GetBlobParams(cfg, 1762955544+100)
	if bp.Target != 14 {
		t.Errorf("Hoodi BPO2 target = %d, want 14", bp.Target)
	}
	if bp.Max != 21 {
		t.Errorf("Hoodi BPO2 max = %d, want 21", bp.Max)
	}
	if bp.UpdateFraction != 11684671 {
		t.Errorf("Hoodi BPO2 update fraction = %d, want 11684671", bp.UpdateFraction)
	}
	if got := ForkName(cfg, 1762955544+100); got != "BPO2" {
		t.Errorf("Hoodi fork name = %q, want BPO2", got)
	}
}

func TestBuildChainConfig_EmptyLearnedReturnsBaseline(t *testing.T) {
	// With no learned entries, a known chain must be identical to its compiled config.
	cfg := BuildChainConfig(560048, nil)
	bp := GetBlobParams(cfg, 1762955544+100)
	if bp.Target != 14 || bp.Max != 21 {
		t.Errorf("Hoodi BPO2 = %d/%d, want 14/21", bp.Target, bp.Max)
	}
}

func TestBuildChainConfig_LearnedNewBPOExtendsKnownChain(t *testing.T) {
	// Simulate a not-yet-compiled BPO3 learned from the node for Hoodi.
	// Hoodi's compiled schedule ends at BPO2 (1762955544, 14/21). A learned
	// BPO3 at a later time with 21/32 must take effect after its activation
	// while earlier forks are preserved.
	bpo3Time := uint64(1763545368)
	learned := []ScheduleEntry{
		{ActivationTime: bpo3Time, Target: 21, Max: 32, UpdateFraction: 20609697},
	}
	cfg := BuildChainConfig(560048, learned)

	// Before BPO3: still BPO2.
	if bp := GetBlobParams(cfg, bpo3Time-100); bp.Target != 14 || bp.Max != 21 {
		t.Errorf("pre-BPO3 = %d/%d, want 14/21", bp.Target, bp.Max)
	}
	// After BPO3: learned params active.
	bp := GetBlobParams(cfg, bpo3Time+100)
	if bp.Target != 21 || bp.Max != 32 || bp.UpdateFraction != 20609697 {
		t.Errorf("BPO3 = %d/%d/%d, want 21/32/20609697", bp.Target, bp.Max, bp.UpdateFraction)
	}
	// EIP-7918/Osaka semantics preserved from the compiled config.
	if cfg.OsakaTime == nil || *cfg.OsakaTime != 1761677592 {
		t.Errorf("Osaka time = %v, want 1761677592", cfg.OsakaTime)
	}
}

func TestBuildChainConfig_DoesNotMutateGlobal(t *testing.T) {
	// BuildChainConfig must never mutate go-ethereum's shared global config.
	before := GetBlobParams(params.HoodiChainConfig, 1762955544+100)
	_ = BuildChainConfig(560048, []ScheduleEntry{
		{ActivationTime: 1763545368, Target: 21, Max: 32, UpdateFraction: 20609697},
	})
	after := GetBlobParams(params.HoodiChainConfig, 1762955544+100)
	if before != after {
		t.Errorf("global Hoodi config mutated: before=%+v after=%+v", before, after)
	}
	if params.HoodiChainConfig.BPO3Time != nil {
		t.Error("global Hoodi config gained a BPO3 time")
	}
}

func TestBuildChainConfig_UnknownChainFromLearned(t *testing.T) {
	// An arbitrary chain go-ethereum does not ship: schedule built from learned
	// entries alone, with EIP-7918 active (Osaka assumed from genesis).
	learned := []ScheduleEntry{
		{ActivationTime: 1000, Target: 6, Max: 9, UpdateFraction: 5007716},
		{ActivationTime: 2000, Target: 14, Max: 21, UpdateFraction: 11684671},
	}
	cfg := BuildChainConfig(7777777, learned)

	if bp := GetBlobParams(cfg, 1500); bp.Target != 6 || bp.Max != 9 {
		t.Errorf("first boundary = %d/%d, want 6/9", bp.Target, bp.Max)
	}
	if bp := GetBlobParams(cfg, 2500); bp.Target != 14 || bp.Max != 21 {
		t.Errorf("second boundary = %d/%d, want 14/21", bp.Target, bp.Max)
	}
	if !cfg.IsOsaka(cfg.LondonBlock, 2500) {
		t.Error("unknown chain should be treated as Osaka-active (EIP-7918)")
	}
}

func TestBuildChainConfig_FillsAllSlotsAndTruncates(t *testing.T) {
	// Nine ascending boundaries exceed the eight representable fork slots; the
	// builder keeps the eight most recent and drops the oldest. Also exercises
	// the BPO3..BPO5 slots.
	learned := make([]ScheduleEntry, 0, 9)
	for i := 0; i < 9; i++ {
		learned = append(learned, ScheduleEntry{
			ActivationTime: uint64(1000 * (i + 1)),
			Target:         3 + i,
			Max:            6 + i,
			UpdateFraction: uint64(3338477 + i),
		})
	}
	cfg := BuildChainConfig(9001, learned)

	// Oldest boundary (t=1000) was dropped; the earliest retained boundary is
	// t=2000, so a query at t=2500 resolves the second-oldest learned entry.
	if bp := GetBlobParams(cfg, 2500); bp.Target != 4 || bp.Max != 7 {
		t.Errorf("earliest retained boundary = %d/%d, want 4/7", bp.Target, bp.Max)
	}
	// Latest boundary (t=9000, target 11 / max 14) sits in the BPO5 slot.
	if bp := GetBlobParams(cfg, 9500); bp.Target != 11 || bp.Max != 14 {
		t.Errorf("latest boundary = %d/%d, want 11/14", bp.Target, bp.Max)
	}
	if ForkName(cfg, 9500) != "BPO5" {
		t.Errorf("latest fork = %q, want BPO5", ForkName(cfg, 9500))
	}
}

func TestGetBlobParams_Cancun(t *testing.T) {
	cfg := params.MainnetChainConfig
	// Use a timestamp well within Cancun but before Prague
	// Cancun activated at 1710338135 (March 13, 2024)
	bp := GetBlobParams(cfg, 1710338135+100)
	if bp.Target != 3 {
		t.Errorf("Cancun target = %d, want 3", bp.Target)
	}
	if bp.Max != 6 {
		t.Errorf("Cancun max = %d, want 6", bp.Max)
	}
	if bp.UpdateFraction != 3338477 {
		t.Errorf("Cancun update fraction = %d, want 3338477", bp.UpdateFraction)
	}
	if bp.TargetGas != 3*131072 {
		t.Errorf("Cancun target gas = %d, want %d", bp.TargetGas, 3*131072)
	}
	if bp.MaxGas != 6*131072 {
		t.Errorf("Cancun max gas = %d, want %d", bp.MaxGas, 6*131072)
	}
}

func TestGetBlobParams_Prague(t *testing.T) {
	cfg := params.MainnetChainConfig
	if cfg.PragueTime == nil {
		t.Skip("Prague not configured for mainnet in this go-ethereum version")
	}
	bp := GetBlobParams(cfg, *cfg.PragueTime+100)
	if bp.Target != 6 {
		t.Errorf("Prague target = %d, want 6", bp.Target)
	}
	if bp.Max != 9 {
		t.Errorf("Prague max = %d, want 9", bp.Max)
	}
}

func TestCalcBlobBaseFee(t *testing.T) {
	cfg := params.MainnetChainConfig
	excess := uint64(0)
	header := &types.Header{
		Time:          1710338135 + 100, // Cancun era
		ExcessBlobGas: &excess,
	}
	fee := CalcBlobBaseFee(cfg, header)
	// With 0 excess blob gas, the fee should be the minimum (1 wei)
	if fee.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("base fee with 0 excess = %s, want 1", fee.String())
	}

	// With significant excess blob gas, fee should be higher
	excess = 10000000 // 10M — enough to push fee above minimum
	fee = CalcBlobBaseFee(cfg, header)
	if fee.Cmp(big.NewInt(1)) <= 0 {
		t.Errorf("base fee with excess %d should be > 1, got %s", excess, fee.String())
	}
}

func TestForkName(t *testing.T) {
	cfg := params.MainnetChainConfig
	tests := []struct {
		name string
		time uint64
		want string
	}{
		{"pre-4844", 0, "Pre-4844"},
		{"Cancun", *cfg.CancunTime + 100, "Cancun"},
		{"Prague", *cfg.PragueTime + 100, "Prague"},
		{"Osaka", *cfg.OsakaTime + 100, "Osaka"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ForkName(cfg, tt.time)
			if got != tt.want {
				t.Errorf("ForkName(mainnet, %d) = %q, want %q", tt.time, got, tt.want)
			}
		})
	}
}

func TestForkName_BPO(t *testing.T) {
	cfg := params.MainnetChainConfig
	// BPO1Time and BPO2Time are set on mainnet
	london := cfg.LondonBlock

	// Find a time that is BPO1 but not BPO2
	bpo1Time := uint64(1765290071) // from mainnet config
	bpo2Time := uint64(1767747671)

	if cfg.IsBPO1(london, bpo1Time+100) {
		got := ForkName(cfg, bpo1Time+100)
		if cfg.IsBPO2(london, bpo1Time+100) {
			// Both active means BPO2 wins (higher priority)
		} else if got != "BPO1" {
			t.Errorf("ForkName at BPO1 time = %q, want BPO1", got)
		}
	}

	if cfg.IsBPO2(london, bpo2Time+100) {
		got := ForkName(cfg, bpo2Time+100)
		if got != "BPO2" {
			t.Errorf("ForkName at BPO2 time = %q, want BPO2", got)
		}
	}
}

func TestGetActiveBlobConfig_AllForks(t *testing.T) {
	cfg := params.MainnetChainConfig

	tests := []struct {
		name    string
		time    uint64
		wantNil bool
	}{
		{"pre-4844", 0, true},
		{"Cancun", *cfg.CancunTime + 100, false},
		{"Prague", *cfg.PragueTime + 100, false},
		{"Osaka", *cfg.OsakaTime + 100, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bc := getActiveBlobConfig(cfg, tt.time)
			if tt.wantNil && bc != nil {
				t.Errorf("expected nil blob config for %s", tt.name)
			}
			if !tt.wantNil && bc == nil {
				t.Errorf("expected non-nil blob config for %s", tt.name)
			}
		})
	}
}

func TestGetActiveBlobConfig_NilSchedule(t *testing.T) {
	cfg := &params.ChainConfig{
		ChainID:            big.NewInt(999),
		BlobScheduleConfig: nil,
	}
	bc := getActiveBlobConfig(cfg, 100)
	if bc != nil {
		t.Error("expected nil for nil BlobScheduleConfig")
	}
}

func TestGetActiveBlobConfig_BPO(t *testing.T) {
	cfg := params.MainnetChainConfig
	london := cfg.LondonBlock

	// BPO1Time=1765290071, BPO2Time=1767747671 on mainnet
	bpo1Time := uint64(1765290071)
	bpo2Time := uint64(1767747671)

	// Test BPO1 config
	if cfg.IsBPO1(london, bpo1Time+100) && !cfg.IsBPO2(london, bpo1Time+100) {
		bc := getActiveBlobConfig(cfg, bpo1Time+100)
		if bc == nil {
			t.Error("expected non-nil blob config at BPO1 time")
		}
	}

	// Test BPO2 config
	if cfg.IsBPO2(london, bpo2Time+100) {
		bc := getActiveBlobConfig(cfg, bpo2Time+100)
		if bc == nil {
			t.Error("expected non-nil blob config at BPO2 time")
		}
	}
}

func TestForkName_AllBranches(t *testing.T) {
	// Build a synthetic config with all forks enabled at known times
	zero := uint64(0)
	t100 := uint64(100)
	t200 := uint64(200)
	t300 := uint64(300)
	t400 := uint64(400)
	t500 := uint64(500)
	t600 := uint64(600)
	t700 := uint64(700)
	t800 := uint64(800)
	bc := &params.BlobConfig{Target: 3, Max: 6, UpdateFraction: 3338477}
	cfg := &params.ChainConfig{
		ChainID:     big.NewInt(999),
		LondonBlock: big.NewInt(0),
		CancunTime:  &zero,
		PragueTime:  &t100,
		OsakaTime:   &t200,
		BPO1Time:    &t300,
		BPO2Time:    &t400,
		BPO3Time:    &t500,
		BPO4Time:    &t600,
		BPO5Time:    &t700,
		BlobScheduleConfig: &params.BlobScheduleConfig{
			Cancun: bc,
			Prague: bc,
			Osaka:  bc,
			BPO1:   bc,
			BPO2:   bc,
			BPO3:   bc,
			BPO4:   bc,
			BPO5:   bc,
		},
	}

	tests := []struct {
		time uint64
		want string
	}{
		{t800, "BPO5"},
		{t700, "BPO5"},
		{t600, "BPO4"},
		{t500, "BPO3"},
		{t400, "BPO2"},
		{t300, "BPO1"},
		{t200, "Osaka"},
		{t100, "Prague"},
		{zero + 1, "Cancun"},
	}
	for _, tt := range tests {
		name := tt.want
		t.Run(name, func(t *testing.T) {
			got := ForkName(cfg, tt.time)
			if got != tt.want {
				t.Errorf("ForkName at time %d = %q, want %q", tt.time, got, tt.want)
			}
		})
	}
}

func TestGetActiveBlobConfig_AllBranches(t *testing.T) {
	zero := uint64(0)
	t100 := uint64(100)
	t200 := uint64(200)
	t300 := uint64(300)
	t400 := uint64(400)
	t500 := uint64(500)
	t600 := uint64(600)
	t700 := uint64(700)
	t800 := uint64(800)
	bc := &params.BlobConfig{Target: 3, Max: 6, UpdateFraction: 3338477}
	cfg := &params.ChainConfig{
		ChainID:     big.NewInt(999),
		LondonBlock: big.NewInt(0),
		CancunTime:  &zero,
		PragueTime:  &t100,
		OsakaTime:   &t200,
		BPO1Time:    &t300,
		BPO2Time:    &t400,
		BPO3Time:    &t500,
		BPO4Time:    &t600,
		BPO5Time:    &t700,
		BlobScheduleConfig: &params.BlobScheduleConfig{
			Cancun: bc,
			Prague: bc,
			Osaka:  bc,
			BPO1:   bc,
			BPO2:   bc,
			BPO3:   bc,
			BPO4:   bc,
			BPO5:   bc,
		},
	}

	tests := []struct {
		time    uint64
		wantNil bool
	}{
		{t800, false},     // BPO5
		{t600, false},     // BPO4
		{t500, false},     // BPO3
		{t400, false},     // BPO2
		{t300, false},     // BPO1
		{t200, false},     // Osaka
		{t100, false},     // Prague
		{zero + 1, false}, // Cancun
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("time_%d", tt.time), func(t *testing.T) {
			bc := getActiveBlobConfig(cfg, tt.time)
			if tt.wantNil && bc != nil {
				t.Error("expected nil")
			}
			if !tt.wantNil && bc == nil {
				t.Error("expected non-nil")
			}
		})
	}
}

func TestGetBlobParams_SyntheticChain(t *testing.T) {
	cfg := ChainConfigForID(12345)
	bp := GetBlobParams(cfg, 100)
	// Synthetic config should have Cancun active (CancunTime=0)
	if bp.Target <= 0 {
		t.Errorf("expected positive target, got %d", bp.Target)
	}
	if bp.Max <= 0 {
		t.Errorf("expected positive max, got %d", bp.Max)
	}
	if bp.UpdateFraction == 0 {
		t.Error("expected non-zero update fraction for synthetic config")
	}
}

func TestPredictNextBlobBaseFee(t *testing.T) {
	cfg := params.MainnetChainConfig
	excess := uint64(100000)
	blobGasUsed := uint64(393216) // 3 blobs worth
	header := &types.Header{
		Time:          1710338135 + 100,
		ExcessBlobGas: &excess,
		BlobGasUsed:   &blobGasUsed,
		BaseFee:       big.NewInt(30000000000), // 30 Gwei
	}
	nextFee := PredictNextBlobBaseFee(cfg, header)
	if nextFee == nil || nextFee.Sign() <= 0 {
		t.Error("predicted next fee should be positive")
	}
}
