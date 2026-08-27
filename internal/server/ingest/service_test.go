package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/domain"
	serverauth "github.com/isyuah/gline/internal/server/auth"
)

const (
	projectID  domain.ProjectID  = "11111111-1111-1111-1111-111111111111"
	agentID    domain.AgentID    = "22222222-2222-2222-2222-222222222222"
	pipelineID domain.PipelineID = "33333333-3333-3333-3333-333333333333"
	batchID    domain.BatchID    = "44444444-4444-4444-4444-444444444444"
)

type projectRepo struct{ project domain.Project }

func (r projectRepo) Get(context.Context, domain.ProjectID) (domain.Project, error) {
	return r.project, nil
}

type agentRepo struct{ agent domain.Agent }

func (r agentRepo) Get(context.Context, domain.ProjectID, domain.AgentID) (domain.Agent, error) {
	return r.agent, nil
}

type pipelineRepo struct{ pipeline domain.Pipeline }

func (r pipelineRepo) Get(context.Context, domain.ProjectID, domain.PipelineID) (domain.Pipeline, error) {
	return r.pipeline, nil
}

type batchRepo struct {
	inserted    bool
	stored      domain.StoredBatch
	entriesCall int
}

func (r *batchRepo) InsertBatch(context.Context, domain.Batch, time.Time) (bool, error) {
	return r.inserted, nil
}
func (r *batchRepo) FindBatch(context.Context, domain.ProjectID, domain.BatchID) (domain.StoredBatch, error) {
	return r.stored, nil
}
func (r *batchRepo) InsertEntries(context.Context, domain.Batch) error { r.entriesCall++; return nil }

type usageRepo struct{ calls int }

func (r *usageRepo) Add(context.Context, domain.ProjectID, time.Time, int64, int64, int64) (domain.UsageBucket, error) {
	r.calls++
	return domain.UsageBucket{}, nil
}

func TestAcceptOnlyAcknowledgesAfterCommit(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	batches := &batchRepo{inserted: true}
	usage := &usageRepo{}
	commitErr := errors.New("commit failed")
	service, err := NewService(testWithinTx(batches, usage, commitErr), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Accept(context.Background(), testPrincipal(), testBatch(now)); !errors.Is(err, commitErr) {
		t.Fatalf("Accept() error = %v", err)
	}
	if batches.entriesCall != 1 || usage.calls != 1 {
		t.Fatalf("transaction work entries=%d usage=%d", batches.entriesCall, usage.calls)
	}
}

func TestAcceptSeparatesAcceptedDuplicateAndConflict(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	candidate := testBatch(now)
	tests := []struct {
		name     string
		inserted bool
		stored   domain.StoredBatch
		status   Status
		wantErr  error
	}{
		{name: "accepted", inserted: true, status: StatusAccepted},
		{name: "duplicate", stored: domain.StoredBatch{ID: batchID, ProjectID: projectID, Status: domain.BatchCommitted, PayloadHash: candidate.PayloadHash, EntryCount: 1}, status: StatusDuplicate},
		{name: "conflict", stored: domain.StoredBatch{ID: batchID, ProjectID: projectID, Status: domain.BatchCommitted, PayloadHash: [32]byte{9}, EntryCount: 1}, wantErr: domain.ErrIdempotencyConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			batches := &batchRepo{inserted: test.inserted, stored: test.stored}
			usage := &usageRepo{}
			service, _ := NewService(testWithinTx(batches, usage, nil), func() time.Time { return now })
			result, err := service.Accept(context.Background(), testPrincipal(), candidate)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr == nil && result.Status != test.status {
				t.Fatalf("status = %s", result.Status)
			}
			if !test.inserted && (batches.entriesCall != 0 || usage.calls != 0) {
				t.Fatalf("retry wrote entries=%d usage=%d", batches.entriesCall, usage.calls)
			}
		})
	}
}

func testWithinTx(batches *batchRepo, usage *usageRepo, commitErr error) WithinTx {
	return func(ctx context.Context, fn func(Repositories) error) error {
		repos := Repositories{
			Projects:  projectRepo{project: domain.Project{ID: projectID, Slug: "demo", Name: "Demo", Status: domain.ProjectActive}},
			Agents:    agentRepo{agent: domain.Agent{ID: agentID, ProjectID: projectID, Name: "agent", Hostname: "host", Status: domain.AgentActive}},
			Pipelines: pipelineRepo{pipeline: domain.Pipeline{ID: pipelineID, ProjectID: projectID, AgentID: agentID, Name: "pipe", Service: "api", Config: []byte(`{}`), ConfigVersion: 1, Status: domain.PipelineEnabled, ReportedStatus: domain.PipelineRunning}},
			Batches:   batches, Usage: usage,
		}
		if err := fn(repos); err != nil {
			return err
		}
		return commitErr
	}
}

func testPrincipal() serverauth.Principal {
	return serverauth.Principal{KeyID: "55555555-5555-5555-5555-555555555555", ProjectID: projectID, Scopes: map[domain.Scope]struct{}{domain.ScopeIngest: {}}}
}

func testBatch(now time.Time) domain.Batch {
	return domain.Batch{
		ID: batchID, ProjectID: projectID, AgentID: agentID, PipelineID: pipelineID,
		PayloadHash: [32]byte{1}, PayloadBytes: 10, CreatedAt: now,
		Entries: []domain.Entry{{ProjectID: projectID, BatchID: batchID, AgentID: agentID, PipelineID: pipelineID, BatchSequence: 0, Service: "api", Host: "host", Level: "INFO", Message: "ok", ObservedAt: now, Attributes: map[string]any{}}},
	}
}
