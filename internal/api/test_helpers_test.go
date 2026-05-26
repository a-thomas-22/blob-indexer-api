package api

import (
	"context"
	"database/sql"
	"reflect"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

const validTestTxHash = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// mockDB implements DBProvider for testing.
type mockDB struct {
	selectFn func(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	getFn    func(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	execFn   func(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

func (m *mockDB) SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	if m.selectFn != nil {
		return m.selectFn(ctx, dest, query, args...)
	}
	return nil
}

func (m *mockDB) GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	if m.getFn != nil {
		return m.getFn(ctx, dest, query, args...)
	}
	return nil
}

func (m *mockDB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if m.execFn != nil {
		return m.execFn(ctx, query, args...)
	}
	return nil, nil
}

func (m *mockDB) Stats() sql.DBStats {
	return sql.DBStats{
		MaxOpenConnections: 25,
		OpenConnections:    5,
		InUse:              2,
		Idle:               3,
	}
}

func newTestAPI() *API {
	return newTestAPIWithDB(nil)
}

func newTestAPIWithDB(db DBProvider) *API {
	if db == nil {
		db = &mockDB{}
	}
	return &API{
		db: db,
		networks: map[int]config.NetworkConfig{
			42: {Name: "testnet", ChainID: 42, Enabled: true},
		},
		config: &config.Config{
			Server:  config.ServerConfig{Port: 8080, DevMode: true},
			Indexer: config.IndexerConfig{Version: "test-v1"},
		},
		startTime:      time.Now(),
		statsCache:     make(map[int]statsCacheEntry),
		topUsersCache:  make(map[string]topUsersCacheEntry),
		breakdownCache: make(map[string]userBreakdownCacheEntry),
		rollingCache:   make(map[string]rollingStatsCacheEntry),
		mempoolCache:   make(map[int]mempoolPressureCacheEntry),
	}
}

// setSliceResult is a helper to assign mock data to a dest pointer via reflection.
func setSliceResult(dest, src interface{}) {
	dv := reflect.ValueOf(dest)
	if dv.Kind() == reflect.Pointer {
		dv = dv.Elem()
	}
	sv := reflect.ValueOf(src)
	dv.Set(sv)
}

// setStructResult copies a struct value into the dest pointer.
func setStructResult(dest, src interface{}) {
	dv := reflect.ValueOf(dest)
	if dv.Kind() == reflect.Pointer {
		dv = dv.Elem()
	}
	sv := reflect.ValueOf(src)
	if sv.Kind() == reflect.Pointer {
		sv = sv.Elem()
	}
	dv.Set(sv)
}

const validTestAddress = "0x1234567890abcdef1234567890abcdef12345678"
