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

func TestChainConfigForNetwork_CachesResult(t *testing.T) {
	a := newTestAPI()
	a.blobScheduleCache = make(map[int]blobScheduleCacheEntry)

	first := a.chainConfigForNetwork(context.Background(), 560048)
	if first == nil {
		t.Fatal("nil config")
	}
	second := a.chainConfigForNetwork(context.Background(), 560048)
	if first != second {
		t.Error("expected cached config pointer to be reused on the second call")
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
