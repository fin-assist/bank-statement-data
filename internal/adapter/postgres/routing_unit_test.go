package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/fin-assist/go-grpc/reqctx"
	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type recordingBeginner struct {
	tx   pgx.Tx
	opts pgx.TxOptions
}

func (b *recordingBeginner) BeginTx(_ context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	b.opts = opts
	return b.tx, nil
}

func TestUnitRoutingUsesOwnedTransactionBeforeReadPrimaryHint(t *testing.T) {
	tx := unitTransaction{}
	b := &recordingBeginner{tx: tx}
	primary, replica := &pgxpool.Pool{}, &pgxpool.Pool{}
	db := &DB{primary: primary, replica: replica, transactions: pgstore.New(b)}
	ctx := t.Context()
	if db.Primary() != primary || db.Writer(ctx) != primary || db.Reader(ctx) != replica || db.Reader(reqctx.WithReadPrimary(ctx)) != primary {
		t.Fatal("pool routing changed")
	}
	if _, ok := Tx(ctx); ok {
		t.Fatal("transaction outside Run")
	}
	err := db.Run(reqctx.WithReadPrimary(ctx), func(ctx context.Context) error {
		if own, ok := db.Tx(ctx); !ok || own != tx || db.Reader(ctx) != tx || db.Writer(ctx) != tx {
			t.Fatal("owned transaction lost or replaced by a pool")
		}
		if carried, ok := Tx(ctx); !ok || carried != tx {
			t.Fatal("transaction not available to shared libraries")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly}
	if err := db.RunWith(ctx, opts, func(context.Context) error { return nil }); err != nil || b.opts != opts {
		t.Fatalf("explicit transaction options: %v %+v", err, b.opts)
	}
	boom := errors.New("operation failed")
	if err := db.RunMapped(ctx, pgx.TxOptions{}, nil, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("unmapped operation error: %v", err)
	}
}
