package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/beacon"
	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

// versionedHashPtr returns a pointer to the given versioned hash for the
// nullable models.Blob field.
func versionedHashPtr(hash string) *string {
	return &hash
}

func weiPtr(wei uint64) *string {
	s := new(big.Int).SetUint64(wei).String()
	return &s
}

// Known rollups and their addresses
var knownRollups = []struct {
	Address     string
	Name        string
	Description string
	Category    string
}{
	{
		Address:     "0x1111111111111111111111111111111111111111",
		Name:        "Arbitrum",
		Description: "Arbitrum is a Layer 2 cryptocurrency platform that makes smart contracts scalable, fast, and private.",
		Category:    "rollup",
	},
	{
		Address:     "0x2222222222222222222222222222222222222222",
		Name:        "Optimism",
		Description: "Optimism is a Layer 2 scaling solution for Ethereum that utilizes optimistic rollups.",
		Category:    "rollup",
	},
	{
		Address:     "0x3333333333333333333333333333333333333333",
		Name:        "Base",
		Description: "Base is a secure, low-cost, builder-friendly Ethereum L2 built to bring the next billion users onchain.",
		Category:    "rollup",
	},
	{
		Address:     "0x4444444444444444444444444444444444444444",
		Name:        "zkSync",
		Description: "zkSync is a ZK rollup that preserves the security of the underlying blockchain.",
		Category:    "rollup",
	},
}

func main() {
	// Load configuration
	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:postgres@localhost:5432/blobindexer?sslmode=disable"
	}

	dbCfg := config.DatabaseConfig{
		URL:             dbURL,
		MaxOpenConns:    25,
		MaxIdleConns:    10,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: 1 * time.Minute,
	}

	// Create context
	ctx := context.Background()

	// Connect to database
	database, err := db.Connect(ctx, dbCfg)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer database.DB.Close()

	// Generate test data
	if err := generateTestData(ctx, database); err != nil {
		log.Fatalf("Failed to generate test data: %v", err)
	}

	log.Println("Test data generated successfully")
}

func generateTestData(ctx context.Context, database *db.DB) error {
	// Add known rollups
	if err := addKnownRollups(ctx, database); err != nil {
		return fmt.Errorf("failed to add known rollups: %w", err)
	}

	// Add test blobs
	if err := addTestBlobs(ctx, database); err != nil {
		return fmt.Errorf("failed to add test blobs: %w", err)
	}

	return nil
}

// seedChainID is the network the generated test data is attributed to
// (mainnet, which the baseline migration seeds into the networks table).
const seedChainID = 1

func addKnownRollups(ctx context.Context, database *db.DB) error {
	log.Println("Adding known rollups...")

	// Add each known rollup
	for _, rollup := range knownRollups {
		now := time.Now().UTC()
		query := `
			INSERT INTO blob_users (chain_id, address, name, description, category, first_seen, last_seen)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (chain_id, address) DO UPDATE SET
				name = $3,
				description = $4,
				category = $5,
				last_seen = $7
		`
		_, err := database.ExecContext(ctx, query,
			seedChainID, rollup.Address, rollup.Name, rollup.Description, rollup.Category, now, now,
		)
		if err != nil {
			return fmt.Errorf("failed to insert rollup %s: %w", rollup.Name, err)
		}
		log.Printf("Added rollup: %s (%s)", rollup.Name, rollup.Address)
	}

	return nil
}

func addTestBlobs(ctx context.Context, database *db.DB) error {
	log.Println("Adding test blobs...")

	// Generate 100 test blobs
	startBlock := int64(1000000)
	timestamp := time.Now().UTC().Add(-24 * time.Hour) // Start from yesterday

	for i := 0; i < 100; i++ {
		// Select a random rollup
		rollupIndex := i % len(knownRollups)
		rollup := knownRollups[rollupIndex]

		// Create a blob
		blob := models.Blob{
			ChainID:           seedChainID,
			BlockNumber:       startBlock + int64(i/2), // Two blobs per block
			BlobIndex:         i % 2,                   // Alternate between 0 and 1
			TxHash:            fmt.Sprintf("0x%064x", i),
			FromAddress:       rollup.Address,
			UserAttribution:   rollup.Name,
			BlobSizeBytes:     int64(128 * 1024), // 128 KB
			BaseFeePerBlobGas: new(big.Int).SetUint64(uint64(100000 + i*1000)).String(),
			TipPerBlobGas:     new(big.Int).SetUint64(uint64(50000 + i*500)).String(),
			TotalCostWei:      new(big.Int).SetUint64(uint64(1000000 + i*10000)).String(),
			Timestamp:         timestamp.Add(time.Duration(i) * 15 * time.Second), // 15 seconds per blob
			Confirmed:         true,
			VersionedHash:     versionedHashPtr(fmt.Sprintf("0x01%062x", i)),
		}
		// Each rollup bids a different priority fee, with one of them
		// spiking every so often, so the tip charts show a contested market.
		priorityFee := uint64(rollupIndex+1) * 500_000_000
		if i%9 == 0 {
			priorityFee = 20_000_000_000
		}
		blob.MaxPriorityFeePerGas = weiPtr(priorityFee)
		blob.MaxFeePerGas = weiPtr(priorityFee + 30_000_000_000)
		blob.PriorityFeePerGas = weiPtr(priorityFee)
		// Derive the beacon slot from the timestamp the same way the indexer
		// does, so seeded rows look like freshly indexed ones.
		if clock, ok := beacon.ResolveClock(seedChainID, 0, 0); ok {
			if s, ok := clock.SlotAt(uint64(blob.Timestamp.Unix())); ok {
				slot := int64(s)
				blob.Slot = &slot
			}
		}

		// Insert the blob
		query := `
			INSERT INTO blobs (
				chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, versioned_hash, slot,
				max_priority_fee_per_gas, max_fee_per_gas, priority_fee_per_gas
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
			)
			ON CONFLICT (chain_id, block_number, blob_index) DO UPDATE SET
				tx_hash = $4,
				from_address = $5,
				user_attribution = $6,
				blob_size_bytes = $7,
				base_fee_per_blob_gas = $8,
				tip_per_blob_gas = $9,
				total_cost_wei = $10,
				timestamp = $11,
				versioned_hash = $12,
				slot = $13,
				max_priority_fee_per_gas = $14,
				max_fee_per_gas = $15,
				priority_fee_per_gas = $16
		`
		_, err := database.ExecContext(ctx, query,
			blob.ChainID, blob.BlockNumber, blob.BlobIndex, blob.TxHash, blob.FromAddress, blob.UserAttribution,
			blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostWei,
			blob.Timestamp, blob.VersionedHash, blob.Slot,
			blob.MaxPriorityFeePerGas, blob.MaxFeePerGas, blob.PriorityFeePerGas,
		)
		if err != nil {
			return fmt.Errorf("failed to insert blob %d: %w", i, err)
		}
	}

	// Add a few pending blobs to the dedicated mempool table. blob_index is
	// the per-transaction blob ordinal; each seeded tx carries one blob.
	for i := 0; i < 5; i++ {
		// Select a random rollup
		rollupIndex := i % len(knownRollups)
		rollup := knownRollups[rollupIndex]

		// Create a pending blob
		blob := models.Blob{
			ChainID:           seedChainID,
			BlobIndex:         0,
			TxHash:            fmt.Sprintf("0xpending%064x", i),
			FromAddress:       rollup.Address,
			UserAttribution:   rollup.Name,
			BlobSizeBytes:     int64(128 * 1024), // 128 KB
			BaseFeePerBlobGas: new(big.Int).SetUint64(uint64(100000)).String(),
			TipPerBlobGas:     new(big.Int).SetUint64(uint64(50000)).String(),
			TotalCostWei:      new(big.Int).SetUint64(uint64(1000000)).String(),
			Timestamp:         time.Now().UTC().Add(-time.Duration(i) * time.Minute),
			Confirmed:         false,
			VersionedHash:     versionedHashPtr(fmt.Sprintf("0x01%062x", 0x1000000+i)),
			Nonce:             uint64(1000 + i),

			MaxPriorityFeePerGas: weiPtr(uint64(rollupIndex+1) * 500_000_000),
			MaxFeePerGas:         weiPtr(uint64(rollupIndex+1)*500_000_000 + 30_000_000_000),
		}

		// Insert the pending blob
		query := `
			INSERT INTO mempool_blobs (
				chain_id, tx_hash, blob_index, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, versioned_hash, nonce, last_seen,
				max_priority_fee_per_gas, max_fee_per_gas
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
			)
			ON CONFLICT (chain_id, tx_hash, blob_index) DO UPDATE SET
				from_address = $4,
				user_attribution = $5,
				blob_size_bytes = $6,
				base_fee_per_blob_gas = $7,
				tip_per_blob_gas = $8,
				total_cost_wei = $9,
				timestamp = $10,
				versioned_hash = $11,
				nonce = $12,
				last_seen = $13,
				max_priority_fee_per_gas = $14,
				max_fee_per_gas = $15
		`
		_, err := database.ExecContext(ctx, query,
			blob.ChainID, blob.TxHash, blob.BlobIndex, blob.FromAddress, blob.UserAttribution,
			blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostWei,
			blob.Timestamp, blob.VersionedHash, int64(blob.Nonce), blob.Timestamp,
			blob.MaxPriorityFeePerGas, blob.MaxFeePerGas,
		)
		if err != nil {
			return fmt.Errorf("failed to insert pending blob %d: %w", i, err)
		}
	}

	log.Printf("Added 100 confirmed blobs and 5 pending blobs")
	return nil
}
