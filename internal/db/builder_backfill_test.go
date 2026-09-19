package db

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

func builderBackfillFixture(blockNumber int64) models.BlockBuilder {
	return models.BlockBuilder{
		ChainID:        1,
		BlockNumber:    blockNumber,
		BlockTimestamp: time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC),
		FeeRecipient:   "0xbuilder",
		ExtraData:      "0x6265617665726275696c642e6f7267",
		BuilderKey:     "beaverbuild",
		BuilderName:    "beaverbuild",
		TxCount:        3,
	}
}

func TestBlocksMissingBlockBuilders(t *testing.T) {
	ctx := context.Background()

	t.Run("lists the window's builder-less blocks", func(t *testing.T) {
		database, mock := newMockDB(t)
		mock.ExpectQuery(regexp.QuoteMeta("FROM indexed_blocks ib")).
			WithArgs(1, int64(100), int64(102)).
			WillReturnRows(sqlmock.NewRows([]string{"block_number", "block_hash"}).
				AddRow(100, "0xaa").AddRow(102, "0xcc"))

		blocks, err := database.BlocksMissingBlockBuilders(ctx, 1, 100, 102)

		if err != nil {
			t.Fatalf("BlocksMissingBlockBuilders() error = %v", err)
		}
		want := []MissingBuilderBlock{{BlockNumber: 100, BlockHash: "0xaa"}, {BlockNumber: 102, BlockHash: "0xcc"}}
		if !reflect.DeepEqual(blocks, want) {
			t.Fatalf("blocks = %v, want %v", blocks, want)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("rejects inverted bounds", func(t *testing.T) {
		database, _ := newMockDB(t)
		if _, err := database.BlocksMissingBlockBuilders(ctx, 1, 5, 4); err == nil {
			t.Fatal("expected inverted bounds to be rejected")
		}
	})

	t.Run("wraps a query failure", func(t *testing.T) {
		database, mock := newMockDB(t)
		mock.ExpectQuery(regexp.QuoteMeta("FROM indexed_blocks ib")).WillReturnError(errors.New("down"))

		if _, err := database.BlocksMissingBlockBuilders(ctx, 1, 1, 2); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestInsertBackfilledBlockBuilders(t *testing.T) {
	ctx := context.Background()

	t.Run("writes builders and tx indexes in one transaction", func(t *testing.T) {
		database, mock := newMockDB(t)
		payment, to := "1230000", "0xproposer"
		row := builderBackfillFixture(100)
		row.ProposerPaymentWei, row.ProposerPaymentTo = &payment, &to

		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO block_builders")).
			WithArgs(1, pq.Array([]int64{100, 101}),
				pq.Array([]string{"2026-09-04 10:00:00", "2026-09-04 10:00:00"}),
				pq.Array([]string{"0xbuilder", "0xbuilder"}),
				sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
				pq.Array([]int64{3, 3}),
				// A row without a proposer payment passes the empty string,
				// which the statement turns back into NULL.
				pq.Array([]string{payment, ""}),
				pq.Array([]string{to, ""})).
			WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectExec(regexp.QuoteMeta("UPDATE blobs b")).
			WithArgs(1, pq.Array([]int64{100}), pq.Array([]string{"0xa"}), pq.Array([]int64{4})).
			WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectCommit()

		inserted, indexed, err := database.InsertBackfilledBlockBuilders(ctx, 1,
			[]models.BlockBuilder{row, builderBackfillFixture(101)},
			[]BlobTxIndexUpdate{{BlockNumber: 100, TxHash: "0xa", TxIndex: 4}})

		if err != nil {
			t.Fatalf("InsertBackfilledBlockBuilders() error = %v", err)
		}
		if inserted != 2 || indexed != 2 {
			t.Fatalf("(inserted, indexed) = (%d, %d), want (2, 2)", inserted, indexed)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("an empty batch writes nothing", func(t *testing.T) {
		database, mock := newMockDB(t)

		inserted, indexed, err := database.InsertBackfilledBlockBuilders(ctx, 1, nil, nil)

		if err != nil || inserted != 0 || indexed != 0 {
			t.Fatalf("= (%d, %d, %v)", inserted, indexed, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("builder rows without blob transactions skip the blobs update", func(t *testing.T) {
		database, mock := newMockDB(t)
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO block_builders")).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		inserted, indexed, err := database.InsertBackfilledBlockBuilders(ctx, 1,
			[]models.BlockBuilder{builderBackfillFixture(100)}, nil)

		if err != nil || inserted != 1 || indexed != 0 {
			t.Fatalf("= (%d, %d, %v)", inserted, indexed, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("a begin failure is reported", func(t *testing.T) {
		database, mock := newMockDB(t)
		mock.ExpectBegin().WillReturnError(errors.New("no connection"))

		if _, _, err := database.InsertBackfilledBlockBuilders(ctx, 1,
			[]models.BlockBuilder{builderBackfillFixture(100)}, nil); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("an insert failure rolls the batch back", func(t *testing.T) {
		database, mock := newMockDB(t)
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO block_builders")).WillReturnError(errors.New("deadlock"))
		mock.ExpectRollback()

		if _, _, err := database.InsertBackfilledBlockBuilders(ctx, 1,
			[]models.BlockBuilder{builderBackfillFixture(100)}, nil); err == nil {
			t.Fatal("expected an error")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("a tx index failure rolls the builder rows back too", func(t *testing.T) {
		database, mock := newMockDB(t)
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO block_builders")).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta("UPDATE blobs b")).WillReturnError(errors.New("deadlock"))
		mock.ExpectRollback()

		if _, _, err := database.InsertBackfilledBlockBuilders(ctx, 1,
			[]models.BlockBuilder{builderBackfillFixture(100)},
			[]BlobTxIndexUpdate{{BlockNumber: 100, TxHash: "0xa", TxIndex: 0}}); err == nil {
			t.Fatal("expected an error")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("a commit failure is reported", func(t *testing.T) {
		database, mock := newMockDB(t)
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO block_builders")).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit().WillReturnError(errors.New("connection lost"))

		if _, _, err := database.InsertBackfilledBlockBuilders(ctx, 1,
			[]models.BlockBuilder{builderBackfillFixture(100)}, nil); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("unreadable row counts are reported", func(t *testing.T) {
		database, mock := newMockDB(t)
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO block_builders")).
			WillReturnResult(sqlmock.NewErrorResult(errors.New("no count")))
		mock.ExpectRollback()

		if _, _, err := database.InsertBackfilledBlockBuilders(ctx, 1,
			[]models.BlockBuilder{builderBackfillFixture(100)}, nil); err == nil {
			t.Fatal("expected an error")
		}

		database, mock = newMockDB(t)
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO block_builders")).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta("UPDATE blobs b")).
			WillReturnResult(sqlmock.NewErrorResult(errors.New("no count")))
		mock.ExpectRollback()

		if _, _, err := database.InsertBackfilledBlockBuilders(ctx, 1,
			[]models.BlockBuilder{builderBackfillFixture(100)},
			[]BlobTxIndexUpdate{{BlockNumber: 100, TxHash: "0xa", TxIndex: 0}}); err == nil {
			t.Fatal("expected an error")
		}
	})
}
