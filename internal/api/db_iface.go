package api

import (
	"context"
	"database/sql"
)

// DBProvider abstracts the database methods required by the API layer.
// *db.DB satisfies this interface.
type DBProvider interface {
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	Stats() sql.DBStats
}
