//go:build integration

package postgres_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/domain"
	"github.com/isyuah/gline/internal/storage/postgres"
	"github.com/isyuah/gline/migrations"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgreSQLTenantTransactionAndIdempotencyContracts(t *testing.T) {
	dsn := os.Getenv("GLINE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set GLINE_TEST_DATABASE_URL to an expendable PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}
	schema := "gline_test_" + randomHex(t, 8)
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	})

	testDSN := withSearchPath(t, dsn, schema)
	db, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := postgres.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := postgres.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}

	store := postgres.New(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	projectID := domain.ProjectID("11111111-1111-1111-1111-111111111111")
	agentID := domain.AgentID("22222222-2222-2222-2222-222222222222")
	pipelineID := domain.PipelineID("33333333-3333-3333-3333-333333333333")
	batchID := domain.BatchID("44444444-4444-4444-4444-444444444444")
	createControlPlane(t, ctx, store, projectID, agentID, pipelineID, "one")

	hash := sha256.Sum256([]byte("canonical-payload"))
	batch := domain.Batch{
		ID: batchID, ProjectID: projectID, AgentID: agentID, PipelineID: pipelineID,
		PayloadHash: hash, PayloadBytes: 64, CreatedAt: now,
		Entries: []domain.Entry{{
			BatchSequence: 0, Service: "api", Host: "host-one", Level: "INFO",
			Message: "started", ObservedAt: now, Attributes: map[string]any{"region": "local"},
		}},
	}
	tx, err := store.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := tx.Ingest().InsertBatch(ctx, batch, now)
	if err != nil || !inserted {
		t.Fatalf("InsertBatch() inserted=%v error=%v", inserted, err)
	}
	if err := tx.Ingest().InsertEntries(ctx, batch); err != nil {
		t.Fatalf("InsertEntries() error = %v", err)
	}
	if _, err := tx.Usage().Add(ctx, projectID, now, 1, 64, 0); err != nil {
		t.Fatalf("Usage.Add() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	duplicateTx, err := store.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer duplicateTx.Rollback()
	inserted, err = duplicateTx.Ingest().InsertBatch(ctx, batch, now)
	if err != nil || inserted {
		t.Fatalf("duplicate InsertBatch() inserted=%v error=%v", inserted, err)
	}
	stored, err := duplicateTx.Ingest().FindBatch(ctx, projectID, batchID)
	if err != nil || stored.PayloadHash != hash {
		t.Fatalf("FindBatch() hash=%x error=%v", stored.PayloadHash, err)
	}

	page, err := store.Entries().List(ctx, domain.EntryQuery{
		ProjectID: projectID, From: now.Add(-time.Minute), To: now.Add(time.Minute), Limit: 10,
	})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Message != "started" {
		t.Fatalf("Entries.List() page=%+v error=%v", page, err)
	}

	projectTwo := domain.ProjectID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	agentTwo := domain.AgentID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	pipelineTwo := domain.PipelineID("cccccccc-cccc-cccc-cccc-cccccccccccc")
	createControlPlane(t, ctx, store, projectTwo, agentTwo, pipelineTwo, "two")
	batch.ProjectID, batch.AgentID, batch.PipelineID = projectTwo, agentTwo, pipelineTwo
	tenantTx, err := store.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	inserted, err = tenantTx.Ingest().InsertBatch(ctx, batch, now)
	if err != nil || !inserted {
		t.Fatalf("same batch id in another project inserted=%v error=%v", inserted, err)
	}
	if err := tenantTx.Ingest().InsertEntries(ctx, batch); err != nil {
		t.Fatalf("same batch id entries error = %v", err)
	}
	if err := tenantTx.Commit(); err != nil {
		t.Fatalf("same batch id commit error = %v", err)
	}

	rollbackBatch := batch
	rollbackBatch.ID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	rollbackTx, err := store.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if inserted, err := rollbackTx.Ingest().InsertBatch(ctx, rollbackBatch, now); err != nil || !inserted {
		t.Fatalf("rollback InsertBatch() inserted=%v error=%v", inserted, err)
	}
	if err := rollbackTx.Ingest().InsertEntries(ctx, rollbackBatch); err != nil {
		t.Fatalf("rollback InsertEntries() error = %v", err)
	}
	if err := rollbackTx.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	verifyTx, err := store.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verifyTx.Rollback()
	if _, err := verifyTx.Ingest().FindBatch(ctx, projectTwo, rollbackBatch.ID); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("rolled back batch error = %v, want ErrNotFound", err)
	}

	_, err = store.Pipelines().Create(ctx, domain.Pipeline{
		ID: "dddddddd-dddd-dddd-dddd-dddddddddddd", ProjectID: projectTwo, AgentID: agentID,
		Name: "cross", Service: "cross", Config: []byte(`{}`), ConfigVersion: 1,
		Status: domain.PipelineEnabled, ReportedStatus: domain.PipelineStopped,
	})
	if !errors.Is(err, postgres.ErrConflict) {
		t.Fatalf("cross-project pipeline error = %v, want ErrConflict", err)
	}
}

func createControlPlane(t *testing.T, ctx context.Context, store *postgres.Store, projectID domain.ProjectID, agentID domain.AgentID, pipelineID domain.PipelineID, suffix string) {
	t.Helper()
	if _, err := store.Projects().Create(ctx, domain.Project{
		ID: projectID, Slug: "project-" + suffix, Name: "Project " + suffix, Status: domain.ProjectActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Agents().Create(ctx, domain.Agent{
		ID: agentID, ProjectID: projectID, Name: "agent-" + suffix,
		Hostname: "host-" + suffix, Version: "test", Status: domain.AgentActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pipelines().Create(ctx, domain.Pipeline{
		ID: pipelineID, ProjectID: projectID, AgentID: agentID,
		Name: "pipeline-" + suffix, Service: "api", Config: []byte(`{}`),
		ConfigVersion: 1, Status: domain.PipelineEnabled, ReportedStatus: domain.PipelineStopped,
	}); err != nil {
		t.Fatal(err)
	}
}

func randomHex(t *testing.T, bytes int) string {
	t.Helper()
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buffer)
}

func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatalf("GLINE_TEST_DATABASE_URL must be a PostgreSQL URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
