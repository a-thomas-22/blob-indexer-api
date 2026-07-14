package api

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// errScheduleDB returns an error from every SelectContext, exercising the
// resolver's fallback to the compiled chain config.
type errScheduleDB struct{}

func (errScheduleDB) SelectContext(context.Context, interface{}, string, ...interface{}) error {
	return errors.New("db down")
}
func (errScheduleDB) GetContext(context.Context, interface{}, string, ...interface{}) error {
	return nil
}
func (errScheduleDB) ExecContext(context.Context, string, ...interface{}) (sql.Result, error) {
	return nil, nil
}
func (errScheduleDB) Stats() sql.DBStats { return sql.DBStats{} }

// countingScheduleDB records how many times the blob-schedule query is issued so
// a cache hit (which skips the query) is observable. It always returns an empty
// schedule.
type countingScheduleDB struct{ selects int }

func (c *countingScheduleDB) SelectContext(_ context.Context, dest interface{}, _ string, _ ...interface{}) error {
	if _, ok := dest.(*[]blobScheduleQueryRow); ok {
		c.selects++
	}
	return nil
}
func (c *countingScheduleDB) GetContext(context.Context, interface{}, string, ...interface{}) error {
	return nil
}
func (c *countingScheduleDB) ExecContext(context.Context, string, ...interface{}) (sql.Result, error) {
	return nil, nil
}
func (c *countingScheduleDB) Stats() sql.DBStats { return sql.DBStats{} }

func TestChainConfigForNetwork_CachesResult(t *testing.T) {
	// Use an unknown chain ID: syntheticChainConfig allocates a fresh config on
	// every build, so a rebuild would return a different pointer. 560048 (Hoodi)
	// would return the compiled global singleton and make the pointer check pass
	// even without caching.
	const unknownChain = 424242
	db := &countingScheduleDB{}
	a := newTestAPIWithDB(db)
	a.blobScheduleCache = make(map[int]blobScheduleCacheEntry)

	first := a.chainConfigForNetwork(context.Background(), unknownChain)
	if first == nil {
		t.Fatal("nil config")
	}
	second := a.chainConfigForNetwork(context.Background(), unknownChain)
	if first != second {
		t.Error("expected cached config pointer to be reused on the second call")
	}
	if db.selects != 1 {
		t.Errorf("expected exactly 1 schedule query (second call served from cache), got %d", db.selects)
	}
}

func TestChainConfigForNetwork_DBErrorFallsBack(t *testing.T) {
	a := newTestAPIWithDB(errScheduleDB{})
	a.blobScheduleCache = make(map[int]blobScheduleCacheEntry)

	cfg := a.chainConfigForNetwork(context.Background(), 560048)
	if cfg == nil || cfg.ChainID.Int64() != 560048 {
		t.Fatalf("fallback config = %+v, want compiled Hoodi (560048)", cfg)
	}
	// A DB error must not populate the cache.
	if _, ok := a.blobScheduleCache[560048]; ok {
		t.Error("DB error should not have cached a config")
	}
}
