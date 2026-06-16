package attribution

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	blobListSource         = "blob-list"
	defaultBlobListBaseURL = "https://raw.githubusercontent.com/ahkc4/blob-list/main/artifacts/by-chain"
	defaultRequestTimeout  = 10 * time.Second
	maxBlobListBodyBytes   = 10 << 20

	claimStatusActive        = "active"
	claimStatusDisputed      = "disputed"
	claimConfidenceConfirmed = "confirmed"
	claimConfidenceProbable  = "probable"
	claimConfidencePossible  = "possible"
)

// BlobListConfig controls dynamic loading of ahkc4/blob-list artifacts.
type BlobListConfig struct {
	Enabled         bool
	BaseURL         string
	RefreshInterval time.Duration
	RequestTimeout  time.Duration
}

func (cfg BlobListConfig) withDefaults() BlobListConfig {
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBlobListBaseURL
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	return cfg
}

// Claim represents a blob-list attribution claim.
type Claim struct {
	ChainID        int    `db:"chain_id"`
	Source         string `db:"source"`
	Address        string `db:"address"`
	EntityID       string `db:"entity_id"`
	Name           string `db:"name"`
	Category       string `db:"category"`
	Role           string `db:"role"`
	Confidence     string `db:"confidence"`
	Status         string `db:"status"`
	ValidFromBlock int64  `db:"valid_from_block"`
	ValidToBlock   *int64 `db:"valid_to_block"`
}

type blobListArtifact struct {
	SchemaVersion   int                        `json:"schema_version"`
	SubmissionChain string                     `json:"submission_chain"`
	Addresses       map[string][]blobListClaim `json:"addresses"`
}

type blobListClaim struct {
	EntityID       string `json:"entity_id"`
	Name           string `json:"name"`
	Category       string `json:"category"`
	Role           string `json:"role"`
	Confidence     string `json:"confidence"`
	Status         string `json:"status"`
	ValidFromBlock int64  `json:"valid_from_block"`
	ValidToBlock   *int64 `json:"valid_to_block"`
}

type blobListSyncStats struct {
	Claims              int
	CurrentUsers        int
	CurrentBlock        int64
	ChangedAddresses    int
	BlobsCleared        int64
	BlobsReattributed   int64
	PendingReattributed int64
	BlobUsersDeleted    int64
	BlobUsersUpserted   int64
}

// RefreshBlobList fetches the latest blob-list artifact, updates the runtime
// mappings, and syncs derived database attribution state.
func (s *Service) RefreshBlobList(ctx context.Context) error {
	cfg := s.blobList.withDefaults()
	if !cfg.Enabled {
		return nil
	}

	claims, err := s.fetchBlobListClaims(ctx, cfg)
	if err != nil {
		return err
	}

	stats := blobListSyncStats{Claims: len(claims)}
	if s.db != nil {
		stats, err = s.syncBlobListClaims(ctx, claims)
		if err != nil {
			return err
		}
	}

	s.setClaims(claims, stats.CurrentBlock)

	logger.Info("Blob-list attributions refreshed",
		zap.Int("chain_id", s.networkID),
		zap.Int("claims", stats.Claims),
		zap.Int("current_users", stats.CurrentUsers),
		zap.Int("changed_addresses", stats.ChangedAddresses),
		zap.Int64("blobs_cleared", stats.BlobsCleared),
		zap.Int64("blobs_reattributed", stats.BlobsReattributed),
		zap.Int64("pending_reattributed", stats.PendingReattributed),
		zap.Int64("blob_users_deleted", stats.BlobUsersDeleted),
		zap.Int64("blob_users_upserted", stats.BlobUsersUpserted))

	return nil
}

func (s *Service) startBlobListRefresh(ctx context.Context) {
	interval := s.blobList.RefreshInterval
	if interval <= 0 {
		return
	}

	s.refreshMu.Lock()
	if s.refreshing {
		s.refreshMu.Unlock()
		return
	}
	s.refreshing = true
	s.refreshMu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.RefreshBlobList(ctx); err != nil {
					logger.Error("Failed to refresh blob-list attributions",
						zap.Int("chain_id", s.networkID),
						zap.Error(err))
				}
			}
		}
	}()
}

func (s *Service) fetchBlobListClaims(ctx context.Context, cfg BlobListConfig) ([]Claim, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()

	url := s.blobListURL(cfg)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create blob-list request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "blob-indexer-api")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch blob-list artifact: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("blob-list artifact returned status %s", resp.Status)
	}

	var artifact blobListArtifact
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxBlobListBodyBytes))
	if err := decoder.Decode(&artifact); err != nil {
		return nil, fmt.Errorf("failed to decode blob-list artifact: %w", err)
	}

	expectedChain := fmt.Sprintf("eip155-%d", s.networkID)
	if artifact.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported blob-list schema version %d", artifact.SchemaVersion)
	}
	if artifact.SubmissionChain != expectedChain {
		return nil, fmt.Errorf("blob-list submission chain %q does not match %q", artifact.SubmissionChain, expectedChain)
	}

	claims := make([]Claim, 0, len(artifact.Addresses))
	for address, entries := range artifact.Addresses {
		normalized := normalizeAddress(address)
		for _, entry := range entries {
			if entry.Name == "" || entry.EntityID == "" {
				continue
			}
			claims = append(claims, Claim{
				ChainID:        s.networkID,
				Source:         blobListSource,
				Address:        normalized,
				EntityID:       entry.EntityID,
				Name:           entry.Name,
				Category:       entry.Category,
				Role:           entry.Role,
				Confidence:     entry.Confidence,
				Status:         entry.Status,
				ValidFromBlock: entry.ValidFromBlock,
				ValidToBlock:   entry.ValidToBlock,
			})
		}
	}

	sortClaims(claims)
	return claims, nil
}

func (s *Service) blobListURL(cfg BlobListConfig) string {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	return fmt.Sprintf("%s/eip155-%d.json", baseURL, s.networkID)
}

func (s *Service) setClaims(claims []Claim, currentBlock ...int64) {
	claimsByAddr := make(map[string][]Claim)
	for _, claim := range claims {
		claim.Address = normalizeAddress(claim.Address)
		claimsByAddr[claim.Address] = append(claimsByAddr[claim.Address], claim)
	}
	for address := range claimsByAddr {
		sortClaims(claimsByAddr[address])
	}

	atBlock := int64(-1)
	if len(currentBlock) > 0 {
		atBlock = currentBlock[0]
	}
	currentUsers := bestClaimsAtBlock(claims, atBlock)

	s.claimsMu.Lock()
	s.claimsByAddr = claimsByAddr
	s.claimsMu.Unlock()

	s.knownUsersMu.Lock()
	s.knownUsers = make(map[string]string, len(currentUsers))
	for address, claim := range currentUsers {
		s.knownUsers[address] = claim.Name
	}
	s.knownUsersMu.Unlock()
}

func (s *Service) bestClaimForBlockLocked(address string, blockNumber int64) (Claim, bool) {
	claims := s.claimsByAddr[address]
	for _, claim := range claims {
		if claim.matchesBlock(blockNumber) {
			return claim, true
		}
	}
	return Claim{}, false
}

func (claim Claim) matchesBlock(blockNumber int64) bool {
	if strings.EqualFold(claim.Status, claimStatusDisputed) {
		return false
	}
	if blockNumber < 0 {
		return strings.EqualFold(claim.Status, claimStatusActive) && claim.ValidToBlock == nil && claim.ValidFromBlock <= 0
	}
	if blockNumber < claim.ValidFromBlock {
		return false
	}
	if claim.ValidToBlock != nil && blockNumber > *claim.ValidToBlock {
		return false
	}
	return true
}

func sortClaims(claims []Claim) {
	sort.SliceStable(claims, func(i, j int) bool {
		return claimSortLess(claims[i], claims[j])
	})
}

func claimSortLess(a, b Claim) bool {
	if aScore, bScore := claimScore(a), claimScore(b); aScore != bScore {
		return aScore > bScore
	}
	if a.ValidFromBlock != b.ValidFromBlock {
		return a.ValidFromBlock > b.ValidFromBlock
	}
	if (a.ValidToBlock == nil) != (b.ValidToBlock == nil) {
		return a.ValidToBlock == nil
	}
	return a.Name < b.Name
}

func claimScore(claim Claim) int {
	score := 0
	switch strings.ToLower(claim.Status) {
	case claimStatusActive:
		score += 100
	case claimStatusDisputed:
		score -= 100
	}
	switch strings.ToLower(claim.Confidence) {
	case claimConfidenceConfirmed:
		score += 30
	case claimConfidenceProbable:
		score += 20
	case claimConfidencePossible:
		score += 10
	}
	return score
}

func bestClaimsAtBlock(claims []Claim, blockNumber int64) map[string]Claim {
	best := make(map[string]Claim)
	for _, claim := range claims {
		if !claim.matchesBlock(blockNumber) {
			continue
		}
		existing, ok := best[claim.Address]
		if !ok || claimSortLess(claim, existing) {
			best[claim.Address] = claim
		}
	}
	return best
}

func changedClaimAddresses(previous, current []Claim) []string {
	previousByAddress := claimsByAddress(previous)
	currentByAddress := claimsByAddress(current)
	addresses := make(map[string]struct{}, len(previousByAddress)+len(currentByAddress))
	for address := range previousByAddress {
		addresses[address] = struct{}{}
	}
	for address := range currentByAddress {
		addresses[address] = struct{}{}
	}

	changed := make([]string, 0, len(addresses))
	for address := range addresses {
		if !sameClaims(previousByAddress[address], currentByAddress[address]) {
			changed = append(changed, address)
		}
	}
	sort.Strings(changed)
	return changed
}

func claimsByAddress(claims []Claim) map[string][]Claim {
	byAddress := make(map[string][]Claim)
	for _, claim := range claims {
		claim.Address = normalizeAddress(claim.Address)
		byAddress[claim.Address] = append(byAddress[claim.Address], claim)
	}
	for address := range byAddress {
		sortClaims(byAddress[address])
	}
	return byAddress
}

func sameClaims(a, b []Claim) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameClaim(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sameClaim(a, b Claim) bool {
	if a.ChainID != b.ChainID ||
		a.Source != b.Source ||
		a.Address != b.Address ||
		a.EntityID != b.EntityID ||
		a.Name != b.Name ||
		a.Category != b.Category ||
		a.Role != b.Role ||
		a.Confidence != b.Confidence ||
		a.Status != b.Status ||
		a.ValidFromBlock != b.ValidFromBlock {
		return false
	}
	if (a.ValidToBlock == nil) != (b.ValidToBlock == nil) {
		return false
	}
	return a.ValidToBlock == nil || *a.ValidToBlock == *b.ValidToBlock
}

func makeAddressSet(addresses []string) map[string]struct{} {
	set := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		set[address] = struct{}{}
	}
	return set
}

func (s *Service) syncBlobListClaims(ctx context.Context, claims []Claim) (blobListSyncStats, error) {
	stats := blobListSyncStats{Claims: len(claims)}

	tx, err := s.db.BeginTxx(ctx, &sql.TxOptions{})
	if err != nil {
		return stats, fmt.Errorf("failed to begin blob-list sync transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	previous := make([]Claim, 0, len(claims))
	if err := tx.SelectContext(ctx, &previous, `
		SELECT chain_id, source, address, entity_id, name, category, role, confidence, status, valid_from_block, valid_to_block
		FROM blob_attribution_claims
		WHERE chain_id = $1 AND source = $2
	`, s.networkID, blobListSource); err != nil {
		return stats, fmt.Errorf("failed to load previous blob-list claims: %w", err)
	}

	if err := tx.GetContext(ctx, &stats.CurrentBlock, `
		SELECT COALESCE(MAX(block_number), -1)
		FROM blobs
		WHERE chain_id = $1 AND block_number >= 0
	`, s.networkID); err != nil {
		return stats, fmt.Errorf("failed to load latest attribution block: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM blob_attribution_claims
		WHERE chain_id = $1 AND source = $2
	`, s.networkID, blobListSource); err != nil {
		return stats, fmt.Errorf("failed to delete previous blob-list claims: %w", err)
	}

	for _, claim := range claims {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO blob_attribution_claims (
				chain_id, source, address, entity_id, name, category, role,
				confidence, status, valid_from_block, valid_to_block, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())
		`, claim.ChainID, claim.Source, claim.Address, claim.EntityID, claim.Name, claim.Category,
			claim.Role, claim.Confidence, claim.Status, claim.ValidFromBlock, claim.ValidToBlock); err != nil {
			return stats, fmt.Errorf("failed to insert blob-list claim for %s: %w", claim.Address, err)
		}
	}

	currentUsers := bestClaimsAtBlock(claims, stats.CurrentBlock)
	previousCurrentUsers := bestClaimsAtBlock(previous, stats.CurrentBlock)
	changedAddresses := changedClaimAddresses(previous, claims)
	changedAddressSet := makeAddressSet(changedAddresses)
	stats.CurrentUsers = len(currentUsers)
	stats.ChangedAddresses = len(changedAddresses)

	if len(previousCurrentUsers) > 0 {
		deletedAddresses := make([]string, 0)
		for address := range previousCurrentUsers {
			if _, ok := currentUsers[address]; !ok {
				deletedAddresses = append(deletedAddresses, address)
			}
		}
		if len(deletedAddresses) > 0 {
			res, err := tx.ExecContext(ctx, `
				DELETE FROM blob_users
				WHERE chain_id = $1 AND address = ANY($2)
			`, s.networkID, pq.Array(deletedAddresses))
			if err != nil {
				return stats, fmt.Errorf("failed to delete stale blob users: %w", err)
			}
			stats.BlobUsersDeleted = rowsAffected(res)
		}
	}

	for _, claim := range currentUsers {
		description := claim.description()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO blob_users (chain_id, address, name, description, category, first_seen, last_seen)
			VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
			ON CONFLICT (chain_id, address) DO UPDATE SET
				name = EXCLUDED.name,
				description = EXCLUDED.description,
				category = EXCLUDED.category
		`, s.networkID, claim.Address, claim.Name, description, claim.Category); err != nil {
			return stats, fmt.Errorf("failed to upsert blob user %s: %w", claim.Address, err)
		}
		stats.BlobUsersUpserted++
	}

	if len(changedAddresses) > 0 {
		res, err := tx.ExecContext(ctx, `
			UPDATE blobs
			SET user_attribution = ''
			WHERE chain_id = $1
				AND LOWER(from_address) = ANY($2)
				AND COALESCE(user_attribution, '') <> ''
		`, s.networkID, pq.Array(changedAddresses))
		if err != nil {
			return stats, fmt.Errorf("failed to clear old blob attributions: %w", err)
		}
		stats.BlobsCleared = rowsAffected(res)
	}

	sortClaimsForApplication(claims)
	for _, claim := range claims {
		if _, ok := changedAddressSet[claim.Address]; !ok {
			continue
		}
		if strings.EqualFold(claim.Status, claimStatusDisputed) {
			continue
		}
		res, err := updateBlobsForClaim(ctx, tx, s.networkID, claim)
		if err != nil {
			return stats, err
		}
		stats.BlobsReattributed += rowsAffected(res)
	}

	for _, claim := range currentUsers {
		if _, ok := changedAddressSet[claim.Address]; !ok {
			continue
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE blobs
			SET user_attribution = $1
			WHERE chain_id = $2
				AND LOWER(from_address) = $3
				AND block_number < 0
				AND COALESCE(user_attribution, '') IS DISTINCT FROM $1
		`, claim.Name, s.networkID, claim.Address)
		if err != nil {
			return stats, fmt.Errorf("failed to update pending blob attributions for %s: %w", claim.Address, err)
		}
		stats.PendingReattributed += rowsAffected(res)
	}

	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("failed to commit blob-list sync: %w", err)
	}

	return stats, nil
}

type claimExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func updateBlobsForClaim(ctx context.Context, execer claimExecutor, networkID int, claim Claim) (sql.Result, error) {
	if claim.ValidToBlock == nil {
		res, err := execer.ExecContext(ctx, `
			UPDATE blobs
			SET user_attribution = $1
			WHERE chain_id = $2
				AND LOWER(from_address) = $3
				AND block_number >= $4
				AND block_number >= 0
				AND COALESCE(user_attribution, '') IS DISTINCT FROM $1
		`, claim.Name, networkID, claim.Address, claim.ValidFromBlock)
		if err != nil {
			return nil, fmt.Errorf("failed to update blob attributions for %s: %w", claim.Address, err)
		}
		return res, nil
	}

	res, err := execer.ExecContext(ctx, `
		UPDATE blobs
		SET user_attribution = $1
		WHERE chain_id = $2
			AND LOWER(from_address) = $3
			AND block_number >= $4
			AND block_number <= $5
			AND block_number >= 0
			AND COALESCE(user_attribution, '') IS DISTINCT FROM $1
	`, claim.Name, networkID, claim.Address, claim.ValidFromBlock, *claim.ValidToBlock)
	if err != nil {
		return nil, fmt.Errorf("failed to update blob attributions for %s: %w", claim.Address, err)
	}
	return res, nil
}

func sortClaimsForApplication(claims []Claim) {
	sort.SliceStable(claims, func(i, j int) bool {
		if aScore, bScore := claimScore(claims[i]), claimScore(claims[j]); aScore != bScore {
			return aScore < bScore
		}
		return claims[i].ValidFromBlock < claims[j].ValidFromBlock
	})
}

func (claim Claim) description() string {
	parts := []string{
		fmt.Sprintf("source=%s", claim.Source),
		fmt.Sprintf("entity_id=%s", claim.EntityID),
	}
	if claim.Role != "" {
		parts = append(parts, fmt.Sprintf("role=%s", claim.Role))
	}
	if claim.Confidence != "" {
		parts = append(parts, fmt.Sprintf("confidence=%s", claim.Confidence))
	}
	if claim.Status != "" {
		parts = append(parts, fmt.Sprintf("status=%s", claim.Status))
	}
	return strings.Join(parts, "; ")
}

func rowsAffected(res sql.Result) int64 {
	if res == nil {
		return 0
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0
	}
	return rows
}
