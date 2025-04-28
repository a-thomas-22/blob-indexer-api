package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"time"

	"github.com/alecthomas/blob-indexer-api/internal/config"
	"github.com/alecthomas/blob-indexer-api/internal/db"
	"github.com/alecthomas/blob-indexer-api/internal/db/models"
)

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
	cfg := &config.Config{
		DatabaseURL:    os.Getenv("DB_URL"),
		IndexerVersion: "test-data-generator",
	}

	// Use default database URL if not provided
	if cfg.DatabaseURL == "" {
		cfg.DatabaseURL = "postgres://postgres:postgres@localhost:5432/blobindexer?sslmode=disable"
	}

	// Create context
	ctx := context.Background()

	// Connect to database
	database, err := db.Connect(ctx, cfg.DatabaseURL)
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

func addKnownRollups(ctx context.Context, database *db.DB) error {
	log.Println("Adding known rollups...")

	// Add each known rollup
	for _, rollup := range knownRollups {
		now := time.Now()
		query := `
			INSERT INTO blob_users (address, name, description, category, first_seen, last_seen)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (address) DO UPDATE SET
				name = $2,
				description = $3,
				category = $4,
				last_seen = $6
		`
		_, err := database.ExecContext(ctx, query,
			rollup.Address, rollup.Name, rollup.Description, rollup.Category, now, now,
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
	timestamp := time.Now().Add(-24 * time.Hour) // Start from yesterday

	for i := 0; i < 100; i++ {
		// Select a random rollup
		rollupIndex := i % len(knownRollups)
		rollup := knownRollups[rollupIndex]

		// Create a blob
		blob := models.Blob{
			BlockNumber:       startBlock + int64(i/2), // Two blobs per block
			BlobIndex:         i % 2,                   // Alternate between 0 and 1
			TxHash:            fmt.Sprintf("0x%064x", i),
			FromAddress:       rollup.Address,
			UserAttribution:   rollup.Name,
			BlobSizeBytes:     int64(128 * 1024), // 128 KB
			BaseFeePerBlobGas: new(big.Int).SetUint64(uint64(100000 + i*1000)).String(),
			TipPerBlobGas:     new(big.Int).SetUint64(uint64(50000 + i*500)).String(),
			TotalCostETH:      new(big.Int).SetUint64(uint64(1000000 + i*10000)).String(),
			Timestamp:         timestamp.Add(time.Duration(i) * 15 * time.Second), // 15 seconds per blob
			Confirmed:         true,
			IndexerVersion:    "test-data-generator",
		}

		// Insert the blob
		query := `
			INSERT INTO blobs (
				block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_eth,
				timestamp, confirmed, indexer_version
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
			)
			ON CONFLICT (block_number, blob_index) DO UPDATE SET
				tx_hash = $3,
				from_address = $4,
				user_attribution = $5,
				blob_size_bytes = $6,
				base_fee_per_blob_gas = $7,
				tip_per_blob_gas = $8,
				total_cost_eth = $9,
				timestamp = $10,
				confirmed = $11,
				indexer_version = $12
		`
		_, err := database.ExecContext(ctx, query,
			blob.BlockNumber, blob.BlobIndex, blob.TxHash, blob.FromAddress, blob.UserAttribution,
			blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostETH,
			blob.Timestamp, blob.Confirmed, blob.IndexerVersion,
		)
		if err != nil {
			return fmt.Errorf("failed to insert blob %d: %w", i, err)
		}
	}

	// Add a few pending blobs
	for i := 0; i < 5; i++ {
		// Select a random rollup
		rollupIndex := i % len(knownRollups)
		rollup := knownRollups[rollupIndex]

		// Create a pending blob
		blob := models.Blob{
			BlockNumber:       -1, // Pending
			BlobIndex:         0,
			TxHash:            fmt.Sprintf("0xpending%064x", i),
			FromAddress:       rollup.Address,
			UserAttribution:   rollup.Name,
			BlobSizeBytes:     int64(128 * 1024), // 128 KB
			BaseFeePerBlobGas: new(big.Int).SetUint64(uint64(100000)).String(),
			TipPerBlobGas:     new(big.Int).SetUint64(uint64(50000)).String(),
			TotalCostETH:      new(big.Int).SetUint64(uint64(1000000)).String(),
			Timestamp:         time.Now().Add(-time.Duration(i) * time.Minute),
			Confirmed:         false,
			IndexerVersion:    "test-data-generator",
		}

		// Insert the pending blob
		query := `
			INSERT INTO blobs (
				block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_eth,
				timestamp, confirmed, indexer_version
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
			)
			ON CONFLICT (tx_hash) DO UPDATE SET
				from_address = $4,
				user_attribution = $5,
				blob_size_bytes = $6,
				base_fee_per_blob_gas = $7,
				tip_per_blob_gas = $8,
				total_cost_eth = $9,
				timestamp = $10,
				confirmed = $11,
				indexer_version = $12
		`
		_, err := database.ExecContext(ctx, query,
			blob.BlockNumber, blob.BlobIndex, blob.TxHash, blob.FromAddress, blob.UserAttribution,
			blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostETH,
			blob.Timestamp, blob.Confirmed, blob.IndexerVersion,
		)
		if err != nil {
			return fmt.Errorf("failed to insert pending blob %d: %w", i, err)
		}
	}

	log.Printf("Added 100 confirmed blobs and 5 pending blobs")
	return nil
}
