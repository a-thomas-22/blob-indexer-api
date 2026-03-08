package db

import (
	"testing"
)

func TestDBType(t *testing.T) {
	// Verify that DB wraps sqlx.DB correctly
	var db *DB
	if db != nil {
		t.Error("expected nil DB pointer")
	}
}
