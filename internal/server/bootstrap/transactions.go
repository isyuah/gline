package bootstrap

import (
	"context"
	"database/sql"

	"github.com/isyuah/gline/internal/server/control"
	"github.com/isyuah/gline/internal/server/ingest"
	"github.com/isyuah/gline/internal/server/operations"
	"github.com/isyuah/gline/internal/storage/postgres"
)

func controlTransactions(store *postgres.Store) control.WithinTx {
	return func(ctx context.Context, fn func(control.Repositories) error) error {
		tx, err := store.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		repositories := control.Repositories{
			Projects: tx.Projects(), Keys: tx.APIKeys(), Agents: tx.Agents(),
			Pipelines: tx.Pipelines(), Retention: tx.Retention(), Audit: tx.Audit(),
		}
		if err := fn(repositories); err != nil {
			return err
		}
		return tx.Commit()
	}
}

func ingestTransactions(store *postgres.Store) ingest.WithinTx {
	return func(ctx context.Context, fn func(ingest.Repositories) error) error {
		tx, err := store.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		repositories := ingest.Repositories{
			Projects: tx.Projects(), Agents: tx.Agents(), Pipelines: tx.Pipelines(),
			Batches: tx.Ingest(), Usage: tx.Usage(),
		}
		if err := fn(repositories); err != nil {
			return err
		}
		return tx.Commit()
	}
}

func operationTransactions(store *postgres.Store) operations.WithinTx {
	return func(ctx context.Context, fn func(operations.Repositories) error) error {
		tx, err := store.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		repositories := operations.Repositories{
			Retention: tx.Retention(), Quarantine: tx.Quarantine(), Audit: tx.Audit(),
		}
		if err := fn(repositories); err != nil {
			return err
		}
		return tx.Commit()
	}
}
