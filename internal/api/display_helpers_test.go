package api

import (
	"testing"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

const (
	validDisplayTxHash  = "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	validDisplayAddress = "0x1111111111111111111111111111111111111111"
)

func TestFormatWeiAsGwei(t *testing.T) {
	tests := []struct {
		name string
		wei  string
		want string
	}{
		{name: "zero", wei: "0", want: "0"},
		{name: "one wei", wei: "1", want: "0.000000001"},
		{name: "one gwei", wei: "1000000000", want: "1"},
		{name: "decimal gwei", wei: "1234500000", want: "1.2345"},
		{name: "large exact", wei: "987654321000000000000", want: "987654321000"},
		{name: "trim spaces", wei: " 1500000000 ", want: "1.5"},
		{name: "invalid", wei: "1.5", want: ""},
		{name: "empty", wei: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatWeiAsGwei(tt.wei); got != tt.want {
				t.Fatalf("formatWeiAsGwei(%q) = %q, want %q", tt.wei, got, tt.want)
			}
		})
	}
}

func TestFormatWeiAsETH(t *testing.T) {
	tests := []struct {
		name string
		wei  string
		want string
	}{
		{name: "zero", wei: "0", want: "0"},
		{name: "one wei", wei: "1", want: "0.000000000000000001"},
		{name: "one eth", wei: "1000000000000000000", want: "1"},
		{name: "decimal eth", wei: "1234500000000000000", want: "1.2345"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatWeiAsETH(tt.wei); got != tt.want {
				t.Fatalf("formatWeiAsETH(%q) = %q, want %q", tt.wei, got, tt.want)
			}
		})
	}
}

func TestExplorerURLsForBlob(t *testing.T) {
	t.Run("mainnet confirmed", func(t *testing.T) {
		got := explorerURLsForBlob(1, validDisplayTxHash, validDisplayAddress, 123456, true)

		if got.Transaction != "https://etherscan.io/tx/"+validDisplayTxHash {
			t.Fatalf("Transaction = %q", got.Transaction)
		}
		if got.Address != "https://etherscan.io/address/"+validDisplayAddress {
			t.Fatalf("Address = %q", got.Address)
		}
		if got.Block != "https://etherscan.io/block/123456" {
			t.Fatalf("Block = %q", got.Block)
		}
	})

	t.Run("sepolia pending", func(t *testing.T) {
		got := explorerURLsForBlob(11155111, validDisplayTxHash, validDisplayAddress, 0, false)

		if got.Transaction != "https://sepolia.etherscan.io/tx/"+validDisplayTxHash {
			t.Fatalf("Transaction = %q", got.Transaction)
		}
		if got.Address != "https://sepolia.etherscan.io/address/"+validDisplayAddress {
			t.Fatalf("Address = %q", got.Address)
		}
		if got.Block != "" {
			t.Fatalf("Block = %q, want empty for pending blob", got.Block)
		}
	})

	t.Run("unknown network", func(t *testing.T) {
		got := explorerURLsForBlob(42, validDisplayTxHash, validDisplayAddress, 123456, true)

		if got != (explorerURLs{}) {
			t.Fatalf("got %#v, want empty URLs", got)
		}
	})

	t.Run("invalid identifiers", func(t *testing.T) {
		got := explorerURLsForBlob(1, "0xabc", "0x123", 123456, true)

		if got.Transaction != "" {
			t.Fatalf("Transaction = %q, want empty for invalid hash", got.Transaction)
		}
		if got.Address != "" {
			t.Fatalf("Address = %q, want empty for invalid address", got.Address)
		}
		if got.Block != "https://etherscan.io/block/123456" {
			t.Fatalf("Block = %q", got.Block)
		}
	})
}

func TestToBlobResponseDisplayHelpers(t *testing.T) {
	maxFee := "2000000000"
	blob := models.Blob{
		ChainID:           1,
		BlockNumber:       123456,
		BlobIndex:         2,
		TxHash:            validDisplayTxHash,
		FromAddress:       validDisplayAddress,
		BlobSizeBytes:     131072,
		BaseFeePerBlobGas: "1500000000",
		TipPerBlobGas:     "125000000",
		TotalCostWei:      "0.001",
		Timestamp:         time.Unix(1700000000, 0).UTC(),
		Confirmed:         true,
		MaxFeePerBlobGas:  &maxFee,
	}

	got := toBlobResponse(blob, "mainnet")

	if got.TransactionURL != "https://etherscan.io/tx/"+validDisplayTxHash {
		t.Fatalf("TransactionURL = %q", got.TransactionURL)
	}
	if got.FromAddressURL != "https://etherscan.io/address/"+validDisplayAddress {
		t.Fatalf("FromAddressURL = %q", got.FromAddressURL)
	}
	if got.BlockURL != "https://etherscan.io/block/123456" {
		t.Fatalf("BlockURL = %q", got.BlockURL)
	}
	if got.BaseFeePerBlobGasGwei != "1.5" {
		t.Fatalf("BaseFeePerBlobGasGwei = %q", got.BaseFeePerBlobGasGwei)
	}
	if got.TipPerBlobGasGwei != "0.125" {
		t.Fatalf("TipPerBlobGasGwei = %q", got.TipPerBlobGasGwei)
	}
	if got.MaxFeePerBlobGasGwei != "2" {
		t.Fatalf("MaxFeePerBlobGasGwei = %q", got.MaxFeePerBlobGasGwei)
	}
}
