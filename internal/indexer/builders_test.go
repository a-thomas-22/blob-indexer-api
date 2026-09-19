package indexer

import (
	"bytes"
	"errors"
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/a-thomas-22/blob-indexer-api/internal/builders"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

func ptrString(s string) *string { return &s }
func ptrInt64(v int64) *int64    { return &v }

// blockTime is the reference slot start every candidate case is measured
// against; first-seen times are expressed as offsets from it.
var candidateBlockTime = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

func baseCandidateContext() candidateBlockContext {
	return candidateBlockContext{
		ChainID:        42,
		BlockNumber:    1000,
		BlockTimestamp: candidateBlockTime,
		MinAge:         6 * time.Second,
		BlobBaseFee:    big.NewInt(100),
		BaseFee:        big.NewInt(1000),
		// Room for exactly two blobs.
		RemainingBlobGas: 2 * 131072,
	}
}

// eligibleCandidate is a transaction that clears every test in the
// precedence chain; each case below spoils exactly one of them.
func eligibleCandidate() candidateTx {
	return candidateTx{
		TxHash:               "0xtx",
		FromAddress:          "0xsender",
		UserAttribution:      "rollup",
		Nonce:                ptrInt64(7),
		BlobCount:            1,
		MaxPriorityFeePerGas: ptrString("5"),
		MaxFeePerGas:         ptrString("2000"),
		MaxFeePerBlobGas:     ptrString("200"),
		FirstSeenAt:          candidateBlockTime.Add(-30 * time.Second),
	}
}

func TestClassifyCandidates(t *testing.T) {
	tests := []struct {
		name  string
		txs   []candidateTx
		want  []string
		tweak func(*candidateBlockContext)
	}{
		{
			name: "nothing wrong with it",
			txs:  []candidateTx{eligibleCandidate()},
			want: []string{models.CandidateEligible},
		},
		{
			name: "seen too late for the builder to have had it",
			txs: []candidateTx{func() candidateTx {
				tx := eligibleCandidate()
				tx.FirstSeenAt = candidateBlockTime.Add(-time.Second)
				return tx
			}()},
			want: []string{models.CandidateTooRecent},
		},
		{
			name: "exactly at the min age is still old enough",
			txs: []candidateTx{func() candidateTx {
				tx := eligibleCandidate()
				tx.FirstSeenAt = candidateBlockTime.Add(-6 * time.Second)
				return tx
			}()},
			want: []string{models.CandidateEligible},
		},
		{
			name: "higher nonce from the same sender sits behind a gap",
			txs: []candidateTx{
				func() candidateTx {
					tx := eligibleCandidate()
					tx.TxHash = "0xlow"
					tx.Nonce = ptrInt64(7)
					return tx
				}(),
				func() candidateTx {
					tx := eligibleCandidate()
					tx.TxHash = "0xhigh"
					tx.Nonce = ptrInt64(8)
					return tx
				}(),
			},
			want: []string{models.CandidateEligible, models.CandidateNonceGap},
		},
		{
			name: "a nonce we never recorded never triggers a gap",
			txs: []candidateTx{
				func() candidateTx {
					tx := eligibleCandidate()
					tx.TxHash = "0xlow"
					tx.Nonce = ptrInt64(7)
					return tx
				}(),
				func() candidateTx {
					tx := eligibleCandidate()
					tx.TxHash = "0xnonull"
					tx.Nonce = nil
					return tx
				}(),
			},
			want: []string{models.CandidateEligible, models.CandidateEligible},
		},
		{
			name: "another sender's lower nonce is irrelevant",
			txs: []candidateTx{
				func() candidateTx {
					tx := eligibleCandidate()
					tx.TxHash = "0xother"
					tx.FromAddress = "0xelse"
					tx.Nonce = ptrInt64(1)
					return tx
				}(),
				func() candidateTx {
					tx := eligibleCandidate()
					tx.Nonce = ptrInt64(9)
					return tx
				}(),
			},
			want: []string{models.CandidateEligible, models.CandidateEligible},
		},
		{
			name: "blob fee cap under the block's blob base fee",
			txs: []candidateTx{func() candidateTx {
				tx := eligibleCandidate()
				tx.MaxFeePerBlobGas = ptrString("99")
				return tx
			}()},
			want: []string{models.CandidatePricedOutBlobFee},
		},
		{
			name: "execution fee cap under the block's base fee",
			txs: []candidateTx{func() candidateTx {
				tx := eligibleCandidate()
				tx.MaxFeePerGas = ptrString("999")
				return tx
			}()},
			want: []string{models.CandidatePricedOutExecFee},
		},
		{
			name: "unobserved fee caps are not low bids",
			txs: []candidateTx{func() candidateTx {
				tx := eligibleCandidate()
				tx.MaxFeePerBlobGas = nil
				tx.MaxFeePerGas = nil
				return tx
			}()},
			want: []string{models.CandidateEligible},
		},
		{
			name: "unparsable fee caps are not low bids",
			txs: []candidateTx{func() candidateTx {
				tx := eligibleCandidate()
				tx.MaxFeePerBlobGas = ptrString("not-a-number")
				return tx
			}()},
			want: []string{models.CandidateEligible},
		},
		{
			name: "a fee cap equal to the base fee still pays",
			txs: []candidateTx{func() candidateTx {
				tx := eligibleCandidate()
				tx.MaxFeePerBlobGas = ptrString("100")
				tx.MaxFeePerGas = ptrString("1000")
				return tx
			}()},
			want: []string{models.CandidateEligible},
		},
		{
			name: "more blobs than the block had room left for",
			txs: []candidateTx{func() candidateTx {
				tx := eligibleCandidate()
				tx.BlobCount = 3
				return tx
			}()},
			want: []string{models.CandidateNoRoom},
		},
		{
			name: "exactly filling the remaining room is not no_room",
			txs: []candidateTx{func() candidateTx {
				tx := eligibleCandidate()
				tx.BlobCount = 2
				return tx
			}()},
			want: []string{models.CandidateEligible},
		},
		{
			name: "age wins over every other reason",
			txs: []candidateTx{func() candidateTx {
				tx := eligibleCandidate()
				tx.FirstSeenAt = candidateBlockTime
				tx.Nonce = ptrInt64(9)
				tx.MaxFeePerBlobGas = ptrString("1")
				tx.MaxFeePerGas = ptrString("1")
				tx.BlobCount = 9
				return tx
			}()},
			want: []string{models.CandidateTooRecent},
		},
		{
			name: "a nonce gap wins over being priced out",
			txs: []candidateTx{
				func() candidateTx {
					tx := eligibleCandidate()
					tx.TxHash = "0xlow"
					return tx
				}(),
				func() candidateTx {
					tx := eligibleCandidate()
					tx.TxHash = "0xhigh"
					tx.Nonce = ptrInt64(8)
					tx.MaxFeePerBlobGas = ptrString("1")
					return tx
				}(),
			},
			want: []string{models.CandidateEligible, models.CandidateNonceGap},
		},
		{
			name: "the blob fee wins over the execution fee",
			txs: []candidateTx{func() candidateTx {
				tx := eligibleCandidate()
				tx.MaxFeePerBlobGas = ptrString("1")
				tx.MaxFeePerGas = ptrString("1")
				return tx
			}()},
			want: []string{models.CandidatePricedOutBlobFee},
		},
		{
			name: "being priced out wins over having no room",
			txs: []candidateTx{func() candidateTx {
				tx := eligibleCandidate()
				tx.MaxFeePerGas = ptrString("1")
				tx.BlobCount = 9
				return tx
			}()},
			want: []string{models.CandidatePricedOutExecFee},
		},
		{
			name: "a block with no base fees prices nobody out",
			txs: []candidateTx{func() candidateTx {
				tx := eligibleCandidate()
				tx.MaxFeePerBlobGas = ptrString("1")
				tx.MaxFeePerGas = ptrString("1")
				return tx
			}()},
			tweak: func(c *candidateBlockContext) {
				c.BlobBaseFee = nil
				c.BaseFee = nil
			},
			want: []string{models.CandidateEligible},
		},
		{
			name: "a full block leaves room for nobody",
			txs:  []candidateTx{eligibleCandidate()},
			tweak: func(c *candidateBlockContext) {
				c.RemainingBlobGas = 0
			},
			want: []string{models.CandidateNoRoom},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			blockCtx := baseCandidateContext()
			if tc.tweak != nil {
				tc.tweak(&blockCtx)
			}
			got := classifyCandidates(tc.txs, blockCtx)
			if len(got) != len(tc.want) {
				t.Fatalf("classifyCandidates() returned %d rows, want %d", len(got), len(tc.want))
			}
			for idx := range got {
				if got[idx].Reason != tc.want[idx] {
					t.Errorf("row %d (%s): reason = %q, want %q", idx, got[idx].TxHash, got[idx].Reason, tc.want[idx])
				}
				if got[idx].ChainID != blockCtx.ChainID || got[idx].BlockNumber != blockCtx.BlockNumber {
					t.Errorf("row %d: block identity = (%d, %d), want (%d, %d)",
						idx, got[idx].ChainID, got[idx].BlockNumber, blockCtx.ChainID, blockCtx.BlockNumber)
				}
				if !got[idx].BlockTimestamp.Equal(blockCtx.BlockTimestamp) {
					t.Errorf("row %d: block timestamp = %s, want %s", idx, got[idx].BlockTimestamp, blockCtx.BlockTimestamp)
				}
			}
		})
	}
}

func TestClassifyCandidatesCopiesTheTransactionFields(t *testing.T) {
	tx := eligibleCandidate()
	got := classifyCandidates([]candidateTx{tx}, baseCandidateContext())
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
	row := got[0]
	if row.TxHash != tx.TxHash || row.FromAddress != tx.FromAddress || row.UserAttribution != tx.UserAttribution {
		t.Errorf("identity fields not copied: %+v", row)
	}
	if row.BlobCount != tx.BlobCount || row.Nonce == nil || *row.Nonce != *tx.Nonce {
		t.Errorf("count/nonce not copied: %+v", row)
	}
	if row.MaxPriorityFeePerGas == nil || *row.MaxPriorityFeePerGas != "5" ||
		row.MaxFeePerGas == nil || *row.MaxFeePerGas != "2000" ||
		row.MaxFeePerBlobGas == nil || *row.MaxFeePerBlobGas != "200" {
		t.Errorf("fee caps not copied: %+v", row)
	}
	if !row.FirstSeenAt.Equal(tx.FirstSeenAt) {
		t.Errorf("first seen = %s, want %s", row.FirstSeenAt, tx.FirstSeenAt)
	}
}

func TestClassifyCandidatesEmpty(t *testing.T) {
	if got := classifyCandidates(nil, baseCandidateContext()); got != nil {
		t.Fatalf("expected nil for an empty pool, got %+v", got)
	}
}

func TestCandidateAggregates(t *testing.T) {
	t.Run("summarizes only the eligible rows", func(t *testing.T) {
		candidates := []models.BlobInclusionCandidate{
			{Reason: models.CandidateEligible, BlobCount: 2, MaxPriorityFeePerGas: ptrString("10")},
			{Reason: models.CandidateEligible, BlobCount: 1, MaxPriorityFeePerGas: ptrString("40")},
			{Reason: models.CandidateEligible, BlobCount: 3, MaxPriorityFeePerGas: nil},
			{Reason: models.CandidateTooRecent, BlobCount: 6, MaxPriorityFeePerGas: ptrString("999")},
			{Reason: models.CandidateNoRoom, BlobCount: 6, MaxPriorityFeePerGas: ptrString("998")},
		}
		pending, txs, blobs, maxTip := candidateAggregates(candidates)
		if pending != 5 {
			t.Errorf("pending = %d, want 5", pending)
		}
		if txs != 3 {
			t.Errorf("eligible txs = %d, want 3", txs)
		}
		if blobs != 6 {
			t.Errorf("eligible blobs = %d, want 6", blobs)
		}
		if maxTip == nil || *maxTip != "40" {
			t.Errorf("max tip = %v, want 40", maxTip)
		}
	})

	t.Run("no eligible rows means no tip", func(t *testing.T) {
		pending, txs, blobs, maxTip := candidateAggregates([]models.BlobInclusionCandidate{
			{Reason: models.CandidateNonceGap, BlobCount: 1, MaxPriorityFeePerGas: ptrString("10")},
		})
		if pending != 1 || txs != 0 || blobs != 0 || maxTip != nil {
			t.Fatalf("got (%d, %d, %d, %v), want (1, 0, 0, nil)", pending, txs, blobs, maxTip)
		}
	})

	t.Run("an unparsable tip is skipped, not counted as zero", func(t *testing.T) {
		_, _, _, maxTip := candidateAggregates([]models.BlobInclusionCandidate{
			{Reason: models.CandidateEligible, MaxPriorityFeePerGas: ptrString("junk")},
		})
		if maxTip != nil {
			t.Fatalf("max tip = %v, want nil", maxTip)
		}
	})

	t.Run("an empty snapshot still reports zero, not nil", func(t *testing.T) {
		pending, txs, blobs, maxTip := candidateAggregates(nil)
		if pending != 0 || txs != 0 || blobs != 0 || maxTip != nil {
			t.Fatalf("got (%d, %d, %d, %v), want zeroes", pending, txs, blobs, maxTip)
		}
	})
}

// signedTxChainID is the test network's chain id, shared by every signed
// fixture so recovered senders compare equal to the fee recipient.
const signedTxChainID = 42

// signedTxTo builds a signed dynamic-fee transaction from key, so its sender
// is deterministic and can be compared with a block's fee recipient.
func signedTxTo(t *testing.T, to common.Address, value int64) (*types.Transaction, common.Address) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer := types.LatestSignerForChainID(big.NewInt(signedTxChainID))
	tx := types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
		ChainID:   big.NewInt(signedTxChainID),
		Nonce:     1,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(2),
		Gas:       21_000,
		To:        &to,
		Value:     big.NewInt(value),
	})
	return tx, crypto.PubkeyToAddress(key.PublicKey)
}

func TestProposerPayment(t *testing.T) {
	proposer := common.HexToAddress("0x00000000000000000000000000000000deadbeef")

	t.Run("last tx from the fee recipient is the payment", func(t *testing.T) {
		payment, builder := signedTxTo(t, proposer, 1_234_000)
		other, _ := signedTxTo(t, proposer, 99)

		wei, to := proposerPayment(types.Transactions{other, payment}, builder.Hex(),
			func(tx *types.Transaction) (string, error) {
				if tx == payment {
					return builder.Hex(), nil
				}
				return "0xsomeone-else", nil
			})
		if wei == nil || *wei != "1234000" {
			t.Fatalf("wei = %v, want 1234000", wei)
		}
		if to == nil || !strings.EqualFold(*to, proposer.Hex()) {
			t.Fatalf("to = %v, want %s", to, proposer.Hex())
		}
	})

	t.Run("checksum casing does not matter", func(t *testing.T) {
		payment, builder := signedTxTo(t, proposer, 5)
		wei, _ := proposerPayment(types.Transactions{payment}, strings.ToLower(builder.Hex()),
			func(*types.Transaction) (string, error) { return strings.ToUpper(builder.Hex()), nil })
		if wei == nil || *wei != "5" {
			t.Fatalf("wei = %v, want 5", wei)
		}
	})

	t.Run("a locally built block has no payment tx", func(t *testing.T) {
		tx, _ := signedTxTo(t, proposer, 7)
		wei, to := proposerPayment(types.Transactions{tx}, "0xbuilder",
			func(*types.Transaction) (string, error) { return "0xsomeone-else", nil })
		if wei != nil || to != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", wei, to)
		}
	})

	t.Run("an unrecoverable sender records nothing", func(t *testing.T) {
		tx, builder := signedTxTo(t, proposer, 7)
		wei, to := proposerPayment(types.Transactions{tx}, builder.Hex(),
			func(*types.Transaction) (string, error) { return "", errors.New("bad signature") })
		if wei != nil || to != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", wei, to)
		}
	})

	t.Run("an empty block has no payment", func(t *testing.T) {
		wei, to := proposerPayment(nil, "0xbuilder",
			func(*types.Transaction) (string, error) { return "0xbuilder", nil })
		if wei != nil || to != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", wei, to)
		}
	})

	t.Run("without a sender resolver there is nothing to compare", func(t *testing.T) {
		tx, builder := signedTxTo(t, proposer, 7)
		if wei, to := proposerPayment(types.Transactions{tx}, builder.Hex(), nil); wei != nil || to != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", wei, to)
		}
	})

	t.Run("a contract creation records the value with no recipient", func(t *testing.T) {
		key, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		signer := types.LatestSignerForChainID(big.NewInt(42))
		tx := types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
			ChainID:   big.NewInt(42),
			Nonce:     1,
			GasTipCap: big.NewInt(1),
			GasFeeCap: big.NewInt(2),
			Gas:       100_000,
			Value:     big.NewInt(42),
		})
		builder := crypto.PubkeyToAddress(key.PublicKey)

		wei, to := proposerPayment(types.Transactions{tx}, builder.Hex(),
			func(*types.Transaction) (string, error) { return builder.Hex(), nil })
		if wei == nil || *wei != "42" {
			t.Fatalf("wei = %v, want 42", wei)
		}
		if to != nil {
			t.Fatalf("to = %v, want nil", to)
		}
	})

	t.Run("an empty fee recipient matches nobody", func(t *testing.T) {
		tx, _ := signedTxTo(t, proposer, 7)
		wei, _ := proposerPayment(types.Transactions{tx}, "",
			func(*types.Transaction) (string, error) { return "", nil })
		if wei != nil {
			t.Fatalf("wei = %v, want nil", wei)
		}
	})
}

func TestBlockBuilderRow(t *testing.T) {
	idx := newTestIndexer()
	coinbase := common.HexToAddress("0x1111111111111111111111111111111111111111")
	timestamp := candidateBlockTime

	blobTx := newSignedBlobTx(t, 42, 1)
	header := &types.Header{
		Number:   big.NewInt(77),
		Coinbase: coinbase,
		Extra:    []byte("beaverbuild.org"),
	}
	block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: []*types.Transaction{blobTx}})

	row := idx.blockBuilderRow(block, timestamp)
	if row.ChainID != idx.network.ChainID || row.BlockNumber != 77 {
		t.Fatalf("identity = (%d, %d), want (%d, 77)", row.ChainID, row.BlockNumber, idx.network.ChainID)
	}
	if !strings.EqualFold(row.FeeRecipient, coinbase.Hex()) {
		t.Errorf("fee recipient = %q, want %q", row.FeeRecipient, coinbase.Hex())
	}
	if row.ExtraData != "0x"+common.Bytes2Hex([]byte("beaverbuild.org")) {
		t.Errorf("extra data = %q", row.ExtraData)
	}
	if row.BuilderKey != "beaverbuild" || row.BuilderName != "beaverbuild" {
		t.Errorf("labels = (%q, %q), want beaverbuild", row.BuilderKey, row.BuilderName)
	}
	if row.TxCount != 1 {
		t.Errorf("tx count = %d, want 1", row.TxCount)
	}
	if row.CandidateSnapshot || row.PendingCandidateTxs != nil || row.EligibleSkippedMaxTip != nil {
		t.Errorf("snapshot fields must be left to insertBlockData: %+v", row)
	}
	// The one transaction is not from the coinbase, so no payment.
	if row.ProposerPaymentWei != nil {
		t.Errorf("proposer payment = %v, want nil", row.ProposerPaymentWei)
	}
	if !row.BlockTimestamp.Equal(timestamp) {
		t.Errorf("timestamp = %s, want %s", row.BlockTimestamp, timestamp)
	}
}

func TestBlockBuilderRowEmptyExtraDataFallsBackToTheAddress(t *testing.T) {
	idx := newTestIndexer()
	coinbase := common.HexToAddress("0x2222222222222222222222222222222222222222")
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(5), Coinbase: coinbase})

	row := idx.blockBuilderRow(block, candidateBlockTime)
	if row.ExtraData != "0x" {
		t.Errorf("extra data = %q, want 0x", row.ExtraData)
	}
	if row.BuilderKey != "addr:"+strings.ToLower(coinbase.Hex()) {
		t.Errorf("builder key = %q", row.BuilderKey)
	}
	if row.TxCount != 0 {
		t.Errorf("tx count = %d, want 0", row.TxCount)
	}
}

func TestCandidateSnapshotDue(t *testing.T) {
	idx := newTestIndexer()
	idx.candidateSnapshotMaxLag = time.Minute

	now := time.Now().UTC()
	if !idx.candidateSnapshotDue(now.Add(-5 * time.Second)) {
		t.Error("a block from five seconds ago must be snapshotted")
	}
	if idx.candidateSnapshotDue(now.Add(-10 * time.Minute)) {
		t.Error("a ten-minute-old block must not be snapshotted")
	}
	if idx.candidateSnapshotDue(now.Add(10 * time.Minute)) {
		t.Error("a block from the future must not be snapshotted")
	}

	idx.candidateSnapshotMaxLag = -1
	if idx.candidateSnapshotDue(now) {
		t.Error("a negative max lag must disable snapshots")
	}
}

func TestDurationOrDefault(t *testing.T) {
	if got := durationOrDefault(0, time.Minute); got != time.Minute {
		t.Errorf("unset = %s, want 1m", got)
	}
	if got := durationOrDefault(time.Second, time.Minute); got != time.Second {
		t.Errorf("set = %s, want 1s", got)
	}
	if got := durationOrDefault(-time.Second, time.Minute); got != -time.Second {
		t.Errorf("negative = %s, want -1s (disabled, not defaulted)", got)
	}
}

func TestExtraDataHexRoundTrip(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, []byte("Titan (titanbuilder.xyz)"), {0x00, 0xff, 0x10}} {
		encoded := hexEncodeExtraData(raw)
		if !strings.HasPrefix(encoded, "0x") {
			t.Fatalf("encoded %q has no 0x prefix", encoded)
		}
		decoded := decodeExtraData(encoded)
		if !bytes.Equal(decoded, raw) && (len(decoded) != 0 || len(raw) != 0) {
			t.Fatalf("round trip of %q gave %q", raw, decoded)
		}
	}
	if got := decodeExtraData("not hex"); got != nil {
		t.Errorf("decodeExtraData(%q) = %v, want nil", "not hex", got)
	}
	if got := decodeExtraData("0xzz"); got != nil {
		t.Errorf("decodeExtraData(%q) = %v, want nil", "0xzz", got)
	}
}

func TestParseWei(t *testing.T) {
	if got := parseWei("125"); got == nil || got.Int64() != 125 {
		t.Errorf("parseWei(125) = %v", got)
	}
	if got := parseWei(""); got != nil {
		t.Errorf("parseWei(\"\") = %v, want nil", got)
	}
	if got := parseWei("1.5"); got != nil {
		t.Errorf("parseWei(1.5) = %v, want nil", got)
	}
}

func TestStringOrEmpty(t *testing.T) {
	if got := stringOrEmpty(nil); got != "" {
		t.Errorf("stringOrEmpty(nil) = %q", got)
	}
	if got := stringOrEmpty(ptrString("9")); got != "9" {
		t.Errorf("stringOrEmpty(9) = %q", got)
	}
}

// insertBlockData must carry the pending row's first-seen timestamp onto the
// confirmed rows it writes: the pending row is deleted in the same statement,
// so this is the only chance to record when the node first saw the tx.
func TestInsertBlockDataPropagatesFirstSeenAt(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	firstSeen := time.Date(2026, 7, 1, 11, 59, 48, 0, time.UTC)
	blob := newBlobFixture()
	blob.BlockNumber = 500
	blob.Confirmed = true
	txIndex := 3
	blob.TxIndex = &txIndex
	indexedBlock := models.IndexedBlock{ChainID: blob.ChainID, BlockNumber: 500, BlockHash: "0xhash", ParentHash: "0xparent"}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number = $2 AND blob_index >= $3")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("DELETE FROM mempool_blobs WHERE").
		WillReturnRows(sqlmock.NewRows([]string{"tx_hash", "timestamp"}).AddRow(blob.TxHash, firstSeen))
	mock.ExpectExec("DELETE FROM mempool_blobs m").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO blobs").
		WithArgs(blob.ChainID, blob.BlockNumber, blob.BlobIndex, blob.TxHash, blob.FromAddress, blob.UserAttribution,
			blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostWei,
			blob.Timestamp, blob.MaxFeePerBlobGas, blob.BlobGasUsed, blob.VersionedHash, blob.Slot,
			blob.MaxPriorityFeePerGas, blob.MaxFeePerGas, blob.PriorityFeePerGas,
			firstSeen, txIndex).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexed_blocks").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := idx.insertBlockData([]models.Blob{blob}, indexedBlock, nil, nil, 0); err != nil {
		t.Fatalf("insertBlockData() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
	if blob.FirstSeenAt != nil {
		t.Error("the caller's slice element must not be mutated by value")
	}
}

// A block whose pending rows are gone (a reprocess) writes NULL, and the
// upsert's COALESCE is what keeps the stored value; the statement itself must
// still be issued with a nil parameter rather than skipping the column.
func TestInsertBlockDataWithoutPendingRowsWritesNullFirstSeen(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	blob := newBlobFixture()
	blob.BlockNumber = 501
	indexedBlock := models.IndexedBlock{ChainID: blob.ChainID, BlockNumber: 501}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("DELETE FROM mempool_blobs WHERE").
		WillReturnRows(sqlmock.NewRows([]string{"tx_hash", "timestamp"}))
	mock.ExpectExec("DELETE FROM mempool_blobs m").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO blobs").
		WithArgs(blob.ChainID, blob.BlockNumber, blob.BlobIndex, blob.TxHash, blob.FromAddress, blob.UserAttribution,
			blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostWei,
			blob.Timestamp, blob.MaxFeePerBlobGas, blob.BlobGasUsed, blob.VersionedHash, blob.Slot,
			blob.MaxPriorityFeePerGas, blob.MaxFeePerGas, blob.PriorityFeePerGas,
			nil, nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexed_blocks").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := idx.insertBlockData([]models.Blob{blob}, indexedBlock, nil, nil, 0); err != nil {
		t.Fatalf("insertBlockData() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestInsertBlockDataPromotionReadErrors(t *testing.T) {
	t.Run("scan failure aborts the block", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		blob := newBlobFixture()

		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE")).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery("DELETE FROM mempool_blobs WHERE").
			WillReturnRows(sqlmock.NewRows([]string{"tx_hash", "timestamp"}).AddRow("0xabc", "not-a-time"))
		mock.ExpectRollback()

		err := idx.insertBlockData([]models.Blob{blob}, models.IndexedBlock{ChainID: blob.ChainID}, nil, nil, 0)
		if err == nil || !strings.Contains(err.Error(), "failed to read promoted pending blobs") {
			t.Fatalf("expected a promoted-read error, got %v", err)
		}
	})

	t.Run("row iteration failure aborts the block", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		blob := newBlobFixture()

		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE")).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery("DELETE FROM mempool_blobs WHERE").
			WillReturnRows(sqlmock.NewRows([]string{"tx_hash", "timestamp"}).
				AddRow("0xabc", candidateBlockTime).
				RowError(0, errors.New("connection lost")))
		mock.ExpectRollback()

		err := idx.insertBlockData([]models.Blob{blob}, models.IndexedBlock{ChainID: blob.ChainID}, nil, nil, 0)
		if err == nil || !strings.Contains(err.Error(), "failed to read promoted pending blobs") {
			t.Fatalf("expected a promoted-read error, got %v", err)
		}
	})
}

// The candidate snapshot must be skipped for a block that is no longer live,
// and the builder row must then say so.
func TestInsertBlockDataSkipsSnapshotForOldBlocks(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.candidateSnapshotMaxLag = time.Minute

	metrics := &models.BlockMetrics{
		ChainID:        idx.network.ChainID,
		BlockNumber:    900,
		BlockTimestamp: time.Now().UTC().Add(-2 * time.Hour),
		BlobBaseFee:    "100",
		BaseFeeWei:     "1000",
		BlobGasLimit:   6 * 131072,
	}
	builder := &models.BlockBuilder{
		ChainID: idx.network.ChainID, BlockNumber: 900, BlockTimestamp: metrics.BlockTimestamp,
		FeeRecipient: "0xabc", ExtraData: "0x", BuilderKey: "addr:0xabc", BuilderName: "0xabc",
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO block_metrics").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO block_builders").
		WithArgs(builder.ChainID, builder.BlockNumber, builder.BlockTimestamp, builder.FeeRecipient, builder.ExtraData,
			builder.BuilderKey, builder.BuilderName, 0, nil, nil,
			false, nil, nil, nil, nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexed_blocks").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err := idx.insertBlockData(nil, models.IndexedBlock{ChainID: idx.network.ChainID, BlockNumber: 900}, metrics, builder, 0)
	if err != nil {
		t.Fatalf("insertBlockData() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
	if builder.CandidateSnapshot {
		t.Error("the caller's builder row must not be mutated")
	}
}

func TestInsertBlockDataBuilderErrors(t *testing.T) {
	newFixture := func(t *testing.T) (*Indexer, sqlmock.Sqlmock, *models.BlockMetrics, *models.BlockBuilder) {
		t.Helper()
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.candidateSnapshotMaxLag = time.Minute
		idx.candidateMinAge = 6 * time.Second

		metrics := &models.BlockMetrics{
			ChainID:        idx.network.ChainID,
			BlockNumber:    901,
			BlockTimestamp: time.Now().UTC(),
			BlobBaseFee:    "100",
			BaseFeeWei:     "1000",
			BlobGasLimit:   6 * 131072,
		}
		builder := &models.BlockBuilder{
			ChainID: idx.network.ChainID, BlockNumber: 901, BlockTimestamp: metrics.BlockTimestamp,
			FeeRecipient: "0xabc", ExtraData: "0x", BuilderKey: "addr:0xabc", BuilderName: "0xabc",
		}
		return idx, mock, metrics, builder
	}

	t.Run("candidate select failure aborts the block", func(t *testing.T) {
		idx, mock, metrics, builder := newFixture(t)
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE")).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO block_metrics").
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectQuery("FROM mempool_blobs").
			WillReturnError(errors.New("pool read failed"))
		mock.ExpectRollback()

		err := idx.insertBlockData(nil, models.IndexedBlock{ChainID: idx.network.ChainID, BlockNumber: 901}, metrics, builder, 0)
		if err == nil || !strings.Contains(err.Error(), "failed to read pending blob candidates") {
			t.Fatalf("expected a candidate read error, got %v", err)
		}
	})

	t.Run("candidate insert failure aborts the block", func(t *testing.T) {
		idx, mock, metrics, builder := newFixture(t)
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE")).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO block_metrics").
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectQuery("FROM mempool_blobs").
			WillReturnRows(sqlmock.NewRows([]string{
				"tx_hash", "from_address", "user_attribution", "nonce", "blob_count",
				"max_priority_fee_per_gas", "max_fee_per_gas", "max_fee_per_blob_gas", "first_seen_at",
			}).AddRow("0xpending", "0xsender", "", int64(4), 1, "5", "2000", "200",
				metrics.BlockTimestamp.Add(-time.Minute)))
		mock.ExpectExec("INSERT INTO blob_inclusion_candidates").
			WillReturnError(errors.New("candidate insert failed"))
		mock.ExpectRollback()

		err := idx.insertBlockData(nil, models.IndexedBlock{ChainID: idx.network.ChainID, BlockNumber: 901}, metrics, builder, 0)
		if err == nil || !strings.Contains(err.Error(), "failed to insert blob inclusion candidates") {
			t.Fatalf("expected a candidate insert error, got %v", err)
		}
	})

	t.Run("builder upsert failure aborts the block", func(t *testing.T) {
		idx, mock, metrics, builder := newFixture(t)
		idx.candidateSnapshotMaxLag = -1 // no snapshot, straight to the upsert
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE")).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO block_metrics").
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec("INSERT INTO block_builders").
			WillReturnError(errors.New("builder upsert failed"))
		mock.ExpectRollback()

		err := idx.insertBlockData(nil, models.IndexedBlock{ChainID: idx.network.ChainID, BlockNumber: 901}, metrics, builder, 0)
		if err == nil || !strings.Contains(err.Error(), "failed to insert block builder") {
			t.Fatalf("expected a builder upsert error, got %v", err)
		}
	})
}

// A live block classifies the leftover pool and summarizes it onto the
// builder row.
func TestInsertBlockDataTakesCandidateSnapshot(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.candidateSnapshotMaxLag = time.Minute
	idx.candidateMinAge = 6 * time.Second

	blockTime := time.Now().UTC()
	metrics := &models.BlockMetrics{
		ChainID:        idx.network.ChainID,
		BlockNumber:    902,
		BlockTimestamp: blockTime,
		BlobBaseFee:    "100",
		BaseFeeWei:     "1000",
		BlobGasLimit:   6 * 131072,
		BlobGasUsed:    131072,
	}
	builder := &models.BlockBuilder{
		ChainID: idx.network.ChainID, BlockNumber: 902, BlockTimestamp: blockTime,
		FeeRecipient: "0xabc", ExtraData: "0x", BuilderKey: "titan", BuilderName: "Titan",
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO block_metrics").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("FROM mempool_blobs").
		WillReturnRows(sqlmock.NewRows([]string{
			"tx_hash", "from_address", "user_attribution", "nonce", "blob_count",
			"max_priority_fee_per_gas", "max_fee_per_gas", "max_fee_per_blob_gas", "first_seen_at",
		}).
			AddRow("0xeligible", "0xsender", "rollup", int64(4), 2, "9", "2000", "200", blockTime.Add(-time.Minute)).
			AddRow("0xfresh", "0xother", "", int64(1), 1, "50", "2000", "200", blockTime))
	mock.ExpectExec("INSERT INTO blob_inclusion_candidates").
		WithArgs(
			idx.network.ChainID, int64(902), blockTime, "0xeligible", "0xsender", "rollup", int64(4), 2,
			"9", "2000", "200", blockTime.Add(-time.Minute), models.CandidateEligible,
			idx.network.ChainID, int64(902), blockTime, "0xfresh", "0xother", "", int64(1), 1,
			"50", "2000", "200", blockTime, models.CandidateTooRecent).
		WillReturnResult(sqlmock.NewResult(1, 2))
	mock.ExpectExec("INSERT INTO block_builders").
		WithArgs(builder.ChainID, builder.BlockNumber, blockTime, builder.FeeRecipient, builder.ExtraData,
			builder.BuilderKey, builder.BuilderName, 0, nil, nil,
			true, 2, 1, 2, "9").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexed_blocks").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err := idx.insertBlockData(nil, models.IndexedBlock{ChainID: idx.network.ChainID, BlockNumber: 902}, metrics, builder, 0)
	if err != nil {
		t.Fatalf("insertBlockData() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestInsertCandidatesChunks(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	candidates := make([]models.BlobInclusionCandidate, candidateInsertChunk+5)
	for i := range candidates {
		candidates[i] = models.BlobInclusionCandidate{
			ChainID: idx.network.ChainID, BlockNumber: 1, BlockTimestamp: candidateBlockTime,
			TxHash: "0x" + strings.Repeat("a", i%3+1), FromAddress: "0xs", BlobCount: 1,
			FirstSeenAt: candidateBlockTime, Reason: models.CandidateEligible,
		}
	}

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO blob_inclusion_candidates").WillReturnResult(sqlmock.NewResult(1, int64(candidateInsertChunk)))
	mock.ExpectExec("INSERT INTO blob_inclusion_candidates").WillReturnResult(sqlmock.NewResult(1, 5))
	mock.ExpectCommit()

	tx, err := idx.db.BeginTxx(idx.ctx, nil)
	if err != nil {
		t.Fatalf("BeginTxx: %v", err)
	}
	if err := idx.insertCandidates(tx, candidates); err != nil {
		t.Fatalf("insertCandidates() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestPruneStaleCandidates(t *testing.T) {
	t.Run("prunes and logs", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.candidateRetention = time.Hour

		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blob_inclusion_candidates WHERE chain_id = $1 AND block_timestamp < $2")).
			WithArgs(idx.network.ChainID, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 3))

		idx.pruneStaleCandidates(idx.ctx)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations not met: %v", err)
		}
	})

	t.Run("a failure is logged, not fatal", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.candidateRetention = time.Hour

		mock.ExpectExec("DELETE FROM blob_inclusion_candidates").
			WillReturnError(errors.New("prune failed"))

		idx.pruneStaleCandidates(idx.ctx)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations not met: %v", err)
		}
	})

	t.Run("a non-positive retention disables the prune", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.candidateRetention = 0

		idx.pruneStaleCandidates(idx.ctx)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("the prune must not touch the database: %v", err)
		}
	})
}

func TestRunBuilderRelabel(t *testing.T) {
	metadataQuery := regexp.QuoteMeta("SELECT value FROM indexer_metadata")

	t.Run("relabels rows when the registry changed", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery(metadataQuery).
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("stale-version"))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT fee_recipient, extra_data FROM block_builders")).
			WithArgs(idx.network.ChainID).
			WillReturnRows(sqlmock.NewRows([]string{"fee_recipient", "extra_data"}).
				AddRow("0xabc", "0x"+common.Bytes2Hex([]byte("beaverbuild.org"))))
		mock.ExpectExec(regexp.QuoteMeta("UPDATE block_builders")).
			WithArgs(idx.network.ChainID, "0xabc", "0x"+common.Bytes2Hex([]byte("beaverbuild.org")), "beaverbuild", "beaverbuild").
			WillReturnResult(sqlmock.NewResult(0, 4))
		mock.ExpectExec("INSERT INTO indexer_metadata").
			WillReturnResult(sqlmock.NewResult(0, 1))

		idx.runBuilderRelabel()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations not met: %v", err)
		}
	})

	t.Run("does nothing when the version already matches", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery(metadataQuery).
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(builders.RegistryVersion()))

		idx.runBuilderRelabel()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations not met: %v", err)
		}
	})

	t.Run("a metadata read failure skips the pass", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery(metadataQuery).WillReturnError(errors.New("metadata unavailable"))

		idx.runBuilderRelabel()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations not met: %v", err)
		}
	})

	t.Run("a relabel failure leaves the version unrecorded", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery(metadataQuery).
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("stale-version"))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT fee_recipient, extra_data FROM block_builders")).
			WillReturnError(errors.New("scan failed"))

		idx.runBuilderRelabel()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations not met: %v", err)
		}
	})

	t.Run("a version write failure only repeats the pass next start", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery(metadataQuery).
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("stale-version"))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT fee_recipient, extra_data FROM block_builders")).
			WillReturnRows(sqlmock.NewRows([]string{"fee_recipient", "extra_data"}))
		mock.ExpectExec("INSERT INTO indexer_metadata").
			WillReturnError(errors.New("metadata write failed"))

		idx.runBuilderRelabel()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations not met: %v", err)
		}
	})
}
