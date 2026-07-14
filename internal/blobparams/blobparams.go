package blobparams

import (
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// BytesPerBlob is the size of a single EIP-4844 blob in bytes: a blob holds
// 4096 field elements of 32 bytes each (4096 * 32 = 131072). This is the
// canonical Go-side source for the blob byte size.
//
// Note that 131072 is also EIP-4844's GAS_PER_BLOB
// (go-ethereum params.BlobTxBlobGasPerBlob): the per-blob byte size and the
// per-blob gas amount share the same numeric value by protocol definition. The
// blob-gas SQL in the migrations and the api package multiplies blob counts by
// this same literal, so the drift-guard test treats 131072 as a single shared
// protocol constant.
const BytesPerBlob = 4096 * 32

const (
	forkBPO5    = "BPO5"
	forkBPO4    = "BPO4"
	forkBPO3    = "BPO3"
	forkBPO2    = "BPO2"
	forkBPO1    = "BPO1"
	forkOsaka   = "Osaka"
	forkPrague  = "Prague"
	forkCancun  = "Cancun"
	forkPre4844 = "Pre-4844"
)

// BlobParams holds the active blob parameters for a specific block timestamp.
type BlobParams struct {
	Target         int
	Max            int
	UpdateFraction uint64
	TargetGas      uint64
	MaxGas         uint64
}

// ChainConfigForID returns the go-ethereum ChainConfig for chains that
// go-ethereum ships a full fork schedule for (including all BPO forks). For
// unknown chains it returns a synthetic Cancun-only config; that fallback
// cannot represent any post-Cancun or BPO fork, so it is a last resort — the
// learned eth_config schedule (see BuildChainConfig) is what actually keeps
// arbitrary networks correct.
func ChainConfigForID(chainID int) *params.ChainConfig {
	switch chainID {
	case 1:
		return params.MainnetChainConfig
	case 11155111:
		return params.SepoliaChainConfig
	case 17000:
		return params.HoleskyChainConfig
	case 560048:
		return params.HoodiChainConfig
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

// ScheduleEntry is one learned blob-parameter boundary: the parameters that
// become active at ActivationTime and stay active until the next boundary.
// These are learned from the connected node via eth_config (EIP-7910), so a new
// BPO fork — or an entirely new network — is picked up without a code change.
type ScheduleEntry struct {
	ActivationTime uint64
	Target         int
	Max            int
	UpdateFraction uint64
}

// maxBlobForkSlots is the number of blob-bearing fork slots go-ethereum's
// ChainConfig can represent: Cancun, Prague, Osaka, and BPO1..BPO5.
const maxBlobForkSlots = 8

type blobBoundary struct {
	time uint64
	cfg  *params.BlobConfig
}

// BuildChainConfig returns a ChainConfig whose blob schedule reflects the
// learned eth_config entries layered on top of the compiled baseline for the
// chain. The result feeds go-ethereum's fork-aware eip4844 functions unchanged,
// so every downstream computation (target/max, blob base fee, EIP-7918 excess
// gas) stays correct without reimplementing consensus math.
//
// For chains go-ethereum ships (mainnet, Hoodi, ...) the compiled schedule is
// the baseline and learned entries override or extend it — e.g. a newly
// scheduled BPO advertised via eth_config.next is picked up before any
// go-ethereum bump. For unknown chains the baseline is Cancun-only, so the
// schedule is built from the learned entries alone.
//
// Limitation: eth_config carries no fork identity, so for an unknown chain
// Osaka (which gates EIP-7918) is inferred positionally as the third distinct
// boundary. With fewer than three boundaries Osaka is not set and EIP-7918 is
// off; with three or more pre-Osaka boundaries it activates early. This only
// affects excess-gas / next-fee *prediction* — it does not change the blob
// schedule selected for a block or the stored blob base fee — and it never
// applies to compiled chains, whose real Osaka time is preserved.
func BuildChainConfig(chainID int, learned []ScheduleEntry) *params.ChainConfig {
	base := ChainConfigForID(chainID)
	if len(learned) == 0 {
		return base
	}

	// Merge base boundaries with learned entries, keyed by activation time so a
	// learned entry overrides the compiled config at the same fork time and adds
	// genuinely new boundaries.
	merged := make(map[uint64]*params.BlobConfig)
	for _, b := range baseBoundaries(base) {
		merged[b.time] = b.cfg
	}
	for _, e := range learned {
		merged[e.ActivationTime] = &params.BlobConfig{
			Target:         e.Target,
			Max:            e.Max,
			UpdateFraction: e.UpdateFraction,
		}
	}

	times := make([]uint64, 0, len(merged))
	for t := range merged {
		times = append(times, t)
	}
	sortUint64(times)
	// A chain with more distinct blob-param changes than go-ethereum can encode
	// keeps its most recent boundaries (the ones we actually index against).
	// Unreachable for any real chain (<=8 boundaries), but log rather than drop
	// silently so a schedule this large is visible. Blocks before the earliest
	// retained boundary resolve no config; getBlobBaseFeeFromBlock guards the
	// resulting CalcBlobFee panic.
	if len(times) > maxBlobForkSlots {
		logger.Warn("Blob schedule exceeds representable fork slots; dropping oldest boundaries",
			zap.Int("chain_id", chainID),
			zap.Int("total_boundaries", len(times)),
			zap.Int("kept", maxBlobForkSlots))
		times = times[len(times)-maxBlobForkSlots:]
	}

	out := *base // shallow copy; every pointer field we touch is reallocated below
	if out.LondonBlock == nil {
		out.LondonBlock = big.NewInt(0)
	}
	sched := &params.BlobScheduleConfig{}
	out.BlobScheduleConfig = sched
	clearBlobForkTimes(&out)

	// Assign boundaries to fork slots in ascending time order. go-ethereum
	// resolves the active fork as the highest-priority slot whose time is <= the
	// block time; slot priority ascends with assignment order, so this yields
	// "the latest boundary at or before the block". The third slot is Osaka, so
	// OsakaTime — which also gates EIP-7918 — lands on the third boundary. Real
	// chains follow Cancun→Prague→Osaka→BPO order, so this matches the true
	// Osaka activation; an arbitrary chain's third distinct blob-param change is
	// treated as the EIP-7918 boundary.
	for i, t := range times {
		setBlobSlot(&out, sched, i, t, merged[t])
	}
	return &out
}

// baseBoundaries extracts the (activation time, blob config) pairs the compiled
// config already defines, in no particular order.
func baseBoundaries(cfg *params.ChainConfig) []blobBoundary {
	s := cfg.BlobScheduleConfig
	if s == nil {
		return nil
	}
	pairs := []struct {
		t  *uint64
		bc *params.BlobConfig
	}{
		{cfg.CancunTime, s.Cancun},
		{cfg.PragueTime, s.Prague},
		{cfg.OsakaTime, s.Osaka},
		{cfg.BPO1Time, s.BPO1},
		{cfg.BPO2Time, s.BPO2},
		{cfg.BPO3Time, s.BPO3},
		{cfg.BPO4Time, s.BPO4},
		{cfg.BPO5Time, s.BPO5},
	}
	out := make([]blobBoundary, 0, len(pairs))
	for _, p := range pairs {
		if p.t != nil && p.bc != nil {
			out = append(out, blobBoundary{time: *p.t, cfg: p.bc})
		}
	}
	return out
}

// clearBlobForkTimes nils the blob-bearing fork time fields so a shallow copy of
// a compiled config does not leak stale boundaries into the rebuilt schedule.
func clearBlobForkTimes(cfg *params.ChainConfig) {
	cfg.CancunTime = nil
	cfg.PragueTime = nil
	cfg.OsakaTime = nil
	cfg.BPO1Time = nil
	cfg.BPO2Time = nil
	cfg.BPO3Time = nil
	cfg.BPO4Time = nil
	cfg.BPO5Time = nil
}

// setBlobSlot writes one boundary into the given fork slot (0=Cancun .. 7=BPO5).
func setBlobSlot(cfg *params.ChainConfig, sched *params.BlobScheduleConfig, slot int, time uint64, bc *params.BlobConfig) {
	t := time
	switch slot {
	case 0:
		cfg.CancunTime, sched.Cancun = &t, bc
	case 1:
		cfg.PragueTime, sched.Prague = &t, bc
	case 2:
		cfg.OsakaTime, sched.Osaka = &t, bc
	case 3:
		cfg.BPO1Time, sched.BPO1 = &t, bc
	case 4:
		cfg.BPO2Time, sched.BPO2 = &t, bc
	case 5:
		cfg.BPO3Time, sched.BPO3 = &t, bc
	case 6:
		cfg.BPO4Time, sched.BPO4 = &t, bc
	case 7:
		cfg.BPO5Time, sched.BPO5 = &t, bc
	}
}

func sortUint64(s []uint64) {
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
}

// GetBlobParams returns the active blob parameters for a chain config at a given block timestamp.
func GetBlobParams(cfg *params.ChainConfig, time uint64) BlobParams {
	target := eip4844.TargetBlobsPerBlock(cfg, time)
	maxBlobs := eip4844.MaxBlobsPerBlock(cfg, time)

	var updateFraction uint64
	if bc := getActiveBlobConfig(cfg, time); bc != nil {
		updateFraction = bc.UpdateFraction
	}

	return BlobParams{
		Target:         target,
		Max:            maxBlobs,
		UpdateFraction: updateFraction,
		TargetGas:      uint64(target) * params.BlobTxBlobGasPerBlob,
		MaxGas:         uint64(maxBlobs) * params.BlobTxBlobGasPerBlob,
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
		return forkBPO5
	case cfg.IsBPO4(london, time):
		return forkBPO4
	case cfg.IsBPO3(london, time):
		return forkBPO3
	case cfg.IsBPO2(london, time):
		return forkBPO2
	case cfg.IsBPO1(london, time):
		return forkBPO1
	case cfg.IsOsaka(london, time):
		return forkOsaka
	case cfg.IsPrague(london, time):
		return forkPrague
	case cfg.IsCancun(london, time):
		return forkCancun
	default:
		return forkPre4844
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
