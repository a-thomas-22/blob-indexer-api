package blobparams

import (
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
		{"Cancun", 1710338135 + 100, "Cancun"},
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
