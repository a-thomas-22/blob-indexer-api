package blobparams

import (
	"math/big"

	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// BlobParams holds the active blob parameters for a specific block timestamp.
type BlobParams struct {
	Target         int
	Max            int
	UpdateFraction uint64
	TargetGas      uint64
	MaxGas         uint64
}

// ChainConfigForID returns the go-ethereum ChainConfig for known chain IDs.
// For unknown chains, returns a synthetic config with Cancun enabled and default blob schedule.
func ChainConfigForID(chainID int) *params.ChainConfig {
	switch chainID {
	case 1:
		return params.MainnetChainConfig
	case 11155111:
		return params.SepoliaChainConfig
	case 17000:
		return params.HoleskyChainConfig
	default:
		return syntheticChainConfig(chainID)
	}
}

// syntheticChainConfig creates a minimal ChainConfig for unknown chains
// with Cancun always active and the default blob schedule.
func syntheticChainConfig(chainID int) *params.ChainConfig {
	zero := uint64(0)
	return &params.ChainConfig{
		ChainID:            big.NewInt(int64(chainID)),
		LondonBlock:        big.NewInt(0),
		CancunTime:         &zero,
		BlobScheduleConfig: params.DefaultBlobSchedule,
	}
}

// GetBlobParams returns the active blob parameters for a chain config at a given block timestamp.
func GetBlobParams(cfg *params.ChainConfig, time uint64) BlobParams {
	target := eip4844.TargetBlobsPerBlock(cfg, time)
	max := eip4844.MaxBlobsPerBlock(cfg, time)

	var updateFraction uint64
	if bc := getActiveBlobConfig(cfg, time); bc != nil {
		updateFraction = bc.UpdateFraction
	}

	return BlobParams{
		Target:         target,
		Max:            max,
		UpdateFraction: updateFraction,
		TargetGas:      uint64(target) * params.BlobTxBlobGasPerBlob,
		MaxGas:         uint64(max) * params.BlobTxBlobGasPerBlob,
	}
}

// CalcBlobBaseFee computes the blob base fee from a block header using
// go-ethereum's fork-aware EIP-4844 formula.
func CalcBlobBaseFee(cfg *params.ChainConfig, header *types.Header) *big.Int {
	return eip4844.CalcBlobFee(cfg, header)
}

// PredictNextBlobBaseFee estimates the next block's blob base fee given
// the current header. It simulates the excess blob gas update with no new blobs.
func PredictNextBlobBaseFee(cfg *params.ChainConfig, header *types.Header) *big.Int {
	nextExcess := eip4844.CalcExcessBlobGas(cfg, header, header.Time+12)
	// Build a synthetic header with the predicted excess
	synth := &types.Header{
		Time:          header.Time + 12,
		ExcessBlobGas: &nextExcess,
	}
	return eip4844.CalcBlobFee(cfg, synth)
}

// ForkName returns a human-readable name for the active blob fork stage.
func ForkName(cfg *params.ChainConfig, time uint64) string {
	london := cfg.LondonBlock
	switch {
	case cfg.IsBPO5(london, time):
		return "BPO5"
	case cfg.IsBPO4(london, time):
		return "BPO4"
	case cfg.IsBPO3(london, time):
		return "BPO3"
	case cfg.IsBPO2(london, time):
		return "BPO2"
	case cfg.IsBPO1(london, time):
		return "BPO1"
	case cfg.IsOsaka(london, time):
		return "Osaka"
	case cfg.IsPrague(london, time):
		return "Prague"
	case cfg.IsCancun(london, time):
		return "Cancun"
	default:
		return "Pre-4844"
	}
}

// getActiveBlobConfig returns the params.BlobConfig for the active fork at the given timestamp.
// This mirrors the private latestBlobConfig logic in go-ethereum's eip4844 package
// to access UpdateFraction which isn't exposed via public API.
func getActiveBlobConfig(cfg *params.ChainConfig, time uint64) *params.BlobConfig {
	s := cfg.BlobScheduleConfig
	if s == nil {
		return nil
	}
	london := cfg.LondonBlock
	switch {
	case cfg.IsBPO5(london, time) && s.BPO5 != nil:
		return s.BPO5
	case cfg.IsBPO4(london, time) && s.BPO4 != nil:
		return s.BPO4
	case cfg.IsBPO3(london, time) && s.BPO3 != nil:
		return s.BPO3
	case cfg.IsBPO2(london, time) && s.BPO2 != nil:
		return s.BPO2
	case cfg.IsBPO1(london, time) && s.BPO1 != nil:
		return s.BPO1
	case cfg.IsOsaka(london, time) && s.Osaka != nil:
		return s.Osaka
	case cfg.IsPrague(london, time) && s.Prague != nil:
		return s.Prague
	case cfg.IsCancun(london, time) && s.Cancun != nil:
		return s.Cancun
	default:
		return nil
	}
}
