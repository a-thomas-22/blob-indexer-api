package attribution

import (
	"context"
	"strings"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// Service handles attribution of blob transactions to known users
type Service struct {
	db *db.DB
	// Map of known addresses to user names for quick lookups
	knownUsers map[string]string
	// Network ID
	networkID int
}

// NewService creates a new attribution service
func NewService(database *db.DB) *Service {
	return &Service{
		db:         database,
		knownUsers: make(map[string]string),
		networkID:  1, // Default to mainnet
	}
}

// Initialize loads known users from the database
func (s *Service) Initialize(ctx context.Context) error {
	logger.Info("Initializing attribution service", zap.Int("network_id", s.networkID))

	// Load known users from the database
	var users []models.BlobUser
	query := "SELECT * FROM blob_users WHERE network_id = $1"
	if err := s.db.SelectContext(ctx, &users, query, s.networkID); err != nil {
		logger.Error("Failed to load known users",
			zap.Int("network_id", s.networkID),
			zap.Error(err))
		return err
	}

	// Populate the known users map
	for _, user := range users {
		s.knownUsers[strings.ToLower(user.Address)] = user.Name
	}

	logger.Info("Attribution service initialized",
		zap.Int("network_id", s.networkID),
		zap.Int("known_users", len(s.knownUsers)))
	return nil
}

// SetNetworkID sets the network ID for the service
func (s *Service) SetNetworkID(networkID int) {
	s.networkID = networkID
}

// GetUserAttribution gets the user attribution for an address
func (s *Service) GetUserAttribution(address string) string {
	// Normalize the address
	normalizedAddress := strings.ToLower(address)

	// Check if the address is a known user
	if name, ok := s.knownUsers[normalizedAddress]; ok {
		return name
	}

	// Unknown user
	return ""
}

// UpdateUserLastSeen updates the last seen timestamp for a user
func (s *Service) UpdateUserLastSeen(ctx context.Context, address string) error {
	// Normalize the address
	normalizedAddress := strings.ToLower(address)

	// Check if the address is a known user
	if _, ok := s.knownUsers[normalizedAddress]; ok {
		// Update the last seen timestamp
		query := "UPDATE blob_users SET last_seen = $1 WHERE address = $2 AND network_id = $3"
		_, err := s.db.ExecContext(ctx, query, time.Now(), normalizedAddress, s.networkID)
		if err != nil {
			logger.Error("Failed to update user last seen",
				zap.Int("network_id", s.networkID),
				zap.String("address", normalizedAddress),
				zap.Error(err))
		}
		return err
	}

	return nil
}

// BatchUpdateUserLastSeen updates the last seen timestamp for multiple users in a single query.
// This avoids the N+1 query problem when updating many users at once.
func (s *Service) BatchUpdateUserLastSeen(ctx context.Context, addresses []string) error {
	if len(addresses) == 0 {
		return nil
	}

	// Filter to only known users and normalize addresses
	knownAddresses := make([]string, 0, len(addresses))
	for _, addr := range addresses {
		normalized := strings.ToLower(addr)
		if _, ok := s.knownUsers[normalized]; ok {
			knownAddresses = append(knownAddresses, normalized)
		}
	}

	if len(knownAddresses) == 0 {
		return nil
	}

	// Update all known users in a single query
	query := "UPDATE blob_users SET last_seen = $1 WHERE address = ANY($2) AND network_id = $3"
	_, err := s.db.ExecContext(ctx, query, time.Now(), pq.Array(knownAddresses), s.networkID)
	if err != nil {
		logger.Error("Failed to batch update user last seen",
			zap.Int("network_id", s.networkID),
			zap.Int("address_count", len(knownAddresses)),
			zap.Error(err))
	}
	return err
}

// AddKnownUser adds a new known user
func (s *Service) AddKnownUser(ctx context.Context, address, name, description, category string) error {
	// Normalize the address
	normalizedAddress := strings.ToLower(address)

	// Check if the user already exists
	if _, ok := s.knownUsers[normalizedAddress]; ok {
		logger.Info("Updating existing known user",
			zap.Int("network_id", s.networkID),
			zap.String("address", normalizedAddress),
			zap.String("name", name))

		// Update the existing user
		query := `
			UPDATE blob_users 
			SET name = $1, description = $2, category = $3, last_seen = $4
			WHERE address = $5 AND network_id = $6
		`
		_, err := s.db.ExecContext(ctx, query, name, description, category, time.Now(), normalizedAddress, s.networkID)
		if err != nil {
			logger.Error("Failed to update known user",
				zap.Int("network_id", s.networkID),
				zap.String("address", normalizedAddress),
				zap.Error(err))
		}
		return err
	}

	logger.Info("Adding new known user",
		zap.Int("network_id", s.networkID),
		zap.String("address", normalizedAddress),
		zap.String("name", name))

	// Add a new user
	now := time.Now()
	query := `
		INSERT INTO blob_users (network_id, address, name, description, category, first_seen, last_seen)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
	_, err := s.db.ExecContext(ctx, query, s.networkID, normalizedAddress, name, description, category, now, now)
	if err != nil {
		logger.Error("Failed to add known user",
			zap.Int("network_id", s.networkID),
			zap.String("address", normalizedAddress),
			zap.Error(err))
		return err
	}

	// Add to the known users map
	s.knownUsers[normalizedAddress] = name
	return nil
}

// GetKnownUsers gets all known users
func (s *Service) GetKnownUsers(ctx context.Context) ([]models.BlobUser, error) {
	var users []models.BlobUser
	query := "SELECT * FROM blob_users WHERE network_id = $1 ORDER BY name"
	err := s.db.SelectContext(ctx, &users, query, s.networkID)
	if err != nil {
		logger.Error("Failed to get known users",
			zap.Int("network_id", s.networkID),
			zap.Error(err))
	}
	return users, err
}

// GetTopBlobUsers gets the top blob users by number of blobs
func (s *Service) GetTopBlobUsers(ctx context.Context, limit, offset int) ([]models.BlobUserStats, error) {
	var result []models.BlobUserStats

	query := `
		SELECT
			from_address,
			user_attribution,
			COUNT(*) as blob_count,
			SUM(total_cost_eth::numeric) as total_cost_eth,
			MAX(timestamp) as last_timestamp
		FROM blobs
		WHERE network_id = $1
		GROUP BY from_address, user_attribution
		ORDER BY blob_count DESC
		LIMIT $2 OFFSET $3
	`
	err := s.db.SelectContext(ctx, &result, query, s.networkID, limit, offset)
	if err != nil {
		logger.Error("Failed to get top blob users",
			zap.Int("network_id", s.networkID),
			zap.Error(err))
	}
	return result, err
}
