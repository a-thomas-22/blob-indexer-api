package db

import (
	"context"
	"database/sql"
)

type sqlxSurface interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
}

// Ensure DB continues to expose sqlx methods via embedding, without wrappers.
var _ sqlxSurface = (*DB)(nil)
