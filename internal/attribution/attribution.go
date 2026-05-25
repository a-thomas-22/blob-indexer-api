package attribution

import (
	"context"
	"strings"
	"sync"
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
	// Map of active attribution mappings to user names for quick lookups.
	knownUsers   map[string]string
	knownUsersMu sync.RWMutex
	claimsByAddr map[string][]Claim
	claimsMu     sync.RWMutex
	blobList     BlobListConfig
	refreshMu    sync.Mutex
	refreshing   bool
	// Network ID
	networkID int
}

// normalizeAddress lowercases an Ethereum address for consistent map lookups and DB queries.
func normalizeAddress(address string) string {
	return strings.ToLower(address)
}

// NewService creates a new attribution service. If networkID is omitted, mainnet (1) is used.
func NewService(database *db.DB, networkID ...int) *Service {
	effectiveNetworkID := 1
	if len(networkID) > 0 {
		effectiveNetworkID = networkID[0]
	}

	return &Service{
		db:           database,
		knownUsers:   make(map[string]string),
		claimsByAddr: make(map[string][]Claim),
		networkID:    effectiveNetworkID,
	}
}

// ConfigureBlobList configures dynamic blob-list attribution loading.
func (s *Service) ConfigureBlobList(cfg BlobListConfig) {
	s.blobList = cfg.withDefaults()
}

// Initialize loads attribution mappings and starts background refreshes.
func (s *Service) Initialize(ctx context.Context) error {
	logger.Info("Initializing attribution service", zap.Int("network_id", s.networkID))

	if s.blobList.Enabled {
		if err := s.RefreshBlobList(ctx); err != nil {
			logger.Error("Failed to refresh blob-list attributions",
				zap.Int("network_id", s.networkID),
				zap.Error(err))
		}
		s.startBlobListRefresh(ctx)
	}

	s.knownUsersMu.Lock()
	knownUsersCount := len(s.knownUsers)
	s.knownUsersMu.Unlock()

	logger.Info("Attribution service initialized",
		zap.Int("network_id", s.networkID),
		zap.Int("known_users", knownUsersCount))
	return nil
}

// SetNetworkID sets the network ID for the service
func (s *Service) SetNetworkID(networkID int) {
	s.networkID = networkID
}

// GetUserAttribution gets the current user attribution for an address.
func (s *Service) GetUserAttribution(address string) string {
	normalizedAddress := normalizeAddress(address)

	s.knownUsersMu.RLock()
	if name, ok := s.knownUsers[normalizedAddress]; ok {
		s.knownUsersMu.RUnlock()
		return name
	}
	s.knownUsersMu.RUnlock()

	return ""
}

// GetUserAttributionForBlock gets the user attribution for an address at a
// specific block. A negative block number means the current active attribution.
func (s *Service) GetUserAttributionForBlock(address string, blockNumber int64) string {
	if blockNumber < 0 {
		return s.GetUserAttribution(address)
	}

	// Normalize the address
	normalizedAddress := normalizeAddress(address)

	s.claimsMu.RLock()
	if claim, ok := s.bestClaimForBlockLocked(normalizedAddress, blockNumber); ok {
		s.claimsMu.RUnlock()
		return claim.Name
	}
	s.claimsMu.RUnlock()

	// Unknown user
	return ""
}

// UpdateUserLastSeen updates the last seen timestamp for a user
func (s *Service) UpdateUserLastSeen(ctx context.Context, address string) error {
	// Normalize the address
	normalizedAddress := normalizeAddress(address)

	// Check if the address is a known user
	s.knownUsersMu.RLock()
	_, ok := s.knownUsers[normalizedAddress]
	s.knownUsersMu.RUnlock()
	if ok {
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
	s.knownUsersMu.RLock()
	for _, addr := range addresses {
		normalized := normalizeAddress(addr)
		if _, ok := s.knownUsers[normalized]; ok {
			knownAddresses = append(knownAddresses, normalized)
		}
	}
	s.knownUsersMu.RUnlock()

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
	normalizedAddress := normalizeAddress(address)

	// Check if the user already exists
	s.knownUsersMu.RLock()
	_, exists := s.knownUsers[normalizedAddress]
	s.knownUsersMu.RUnlock()
	if exists {
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
	s.knownUsersMu.Lock()
	s.knownUsers[normalizedAddress] = name
	s.knownUsersMu.Unlock()
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
