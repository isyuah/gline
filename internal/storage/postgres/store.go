package postgres

import (
	"context"
	"database/sql"
	"errors"
)

var (
	ErrNotFound       = errors.New("postgres: not found")
	ErrConflict       = errors.New("postgres: constraint conflict")
	ErrCorruptRow     = errors.New("postgres: corrupt row")
	ErrMigrationDrift = errors.New("postgres: applied migration checksum changed")
)

type dbtx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store {
	if db == nil {
		panic("postgres.New: nil database")
	}
	return &Store{db: db}
}

// Open uses a driver registered by the executable. The adapter deliberately
// does not blank-import a driver, keeping that deployment choice at the edge.
func Open(driverName, dataSourceName string) (*Store, error) {
	db, err := sql.Open(driverName, dataSourceName)
	if err != nil {
		return nil, err
	}
	return New(db), nil
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Stats() sql.DBStats             { return s.db.Stats() }

type Tx struct {
	tx *sql.Tx
}

func (s *Store) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, classifyError(err)
	}
	return &Tx{tx: tx}, nil
}

func (tx *Tx) Commit() error   { return classifyError(tx.tx.Commit()) }
func (tx *Tx) Rollback() error { return ignoreDone(tx.tx.Rollback()) }

func ignoreDone(err error) error {
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return classifyError(err)
}

func (s *Store) Projects() *ProjectRepository      { return &ProjectRepository{q: s.db} }
func (s *Store) APIKeys() *APIKeyRepository        { return &APIKeyRepository{q: s.db} }
func (s *Store) Agents() *AgentRepository          { return &AgentRepository{q: s.db} }
func (s *Store) Pipelines() *PipelineRepository    { return &PipelineRepository{q: s.db} }
func (s *Store) Entries() *EntryRepository         { return &EntryRepository{q: s.db} }
func (s *Store) Quarantine() *QuarantineRepository { return &QuarantineRepository{q: s.db} }
func (s *Store) Retention() *RetentionRepository   { return &RetentionRepository{q: s.db} }
func (s *Store) Audit() *AuditRepository           { return &AuditRepository{q: s.db} }
func (s *Store) Usage() *UsageRepository           { return &UsageRepository{q: s.db} }
func (tx *Tx) Projects() *ProjectRepository        { return &ProjectRepository{q: tx.tx} }
func (tx *Tx) APIKeys() *APIKeyRepository          { return &APIKeyRepository{q: tx.tx} }
func (tx *Tx) Agents() *AgentRepository            { return &AgentRepository{q: tx.tx} }
func (tx *Tx) Pipelines() *PipelineRepository      { return &PipelineRepository{q: tx.tx} }
func (tx *Tx) Ingest() *IngestRepository           { return &IngestRepository{q: tx.tx} }
func (tx *Tx) Entries() *EntryRepository           { return &EntryRepository{q: tx.tx} }
func (tx *Tx) Quarantine() *QuarantineRepository   { return &QuarantineRepository{q: tx.tx} }
func (tx *Tx) Retention() *RetentionRepository     { return &RetentionRepository{q: tx.tx} }
func (tx *Tx) Audit() *AuditRepository             { return &AuditRepository{q: tx.tx} }
func (tx *Tx) Usage() *UsageRepository             { return &UsageRepository{q: tx.tx} }
