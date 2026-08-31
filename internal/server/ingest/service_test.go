package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/domain"
	"github.com/isyuah/gline/internal/server/admission"
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

func TestAcceptDrainsPausedPipelineButRejectsDisabledPipeline(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		status  domain.PipelineStatus
		wantErr error
	}{
		{name: "paused backlog drains", status: domain.PipelinePaused},
		{name: "disabled is a hard boundary", status: domain.PipelineDisabled, wantErr: ErrPipelineUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			batches := &batchRepo{inserted: true}
			service, _ := NewService(testWithinTx(batches, &usageRepo{}, nil, test.status), func() time.Time { return now })
			_, err := service.Accept(t.Context(), testPrincipal(), testBatch(now))
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Accept() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestAcceptCommitsOnlyAcceptedAdmissionCost(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		inserted   bool
		commitErr  error
		wantCommit int
	}{
		{name: "accepted", inserted: true, wantCommit: 1},
		{name: "duplicate is refunded"},
		{name: "transaction failure is refunded", inserted: true, commitErr: errors.New("commit failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := testBatch(now)
			batches := &batchRepo{inserted: test.inserted, stored: domain.StoredBatch{
				ID: batchID, ProjectID: projectID, Status: domain.BatchCommitted,
				PayloadHash: candidate.PayloadHash, EntryCount: 1,
			}}
			reservation := &fakeReservation{}
			controller := &fakeAdmission{reservation: reservation}
			service, err := NewService(testWithinTx(batches, &usageRepo{}, test.commitErr), func() time.Time { return now }, WithAdmission(controller))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = service.Accept(t.Context(), testPrincipal(), candidate)
			if reservation.commits != test.wantCommit || reservation.releases != 1 {
				t.Fatalf("reservation commits=%d releases=%d", reservation.commits, reservation.releases)
			}
			if controller.keyID != testPrincipal().KeyID || controller.projectID != projectID || controller.entries != 1 || controller.bytes != 10 {
				t.Fatalf("admission input = %+v", controller)
			}
		})
	}
}

func TestAcceptStopsBeforeTransactionWhenAdmissionIsLimited(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	controller := &fakeAdmission{err: &admission.LimitError{
		Reason: admission.ReasonProjectInflight, RetryAfter: time.Second,
	}}
	service, err := NewService(func(context.Context, func(Repositories) error) error {
		t.Fatal("transaction started after admission rejection")
		return nil
	}, func() time.Time { return now }, WithAdmission(controller))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Accept(t.Context(), testPrincipal(), testBatch(now)); !errors.Is(err, admission.ErrLimited) {
		t.Fatalf("Accept() error = %v", err)
	}
}

type fakeAdmission struct {
	reservation *fakeReservation
	keyID       domain.APIKeyID
	projectID   domain.ProjectID
	entries     int
	bytes       int64
	err         error
}

func (a *fakeAdmission) AllowIngest(_ context.Context, keyID domain.APIKeyID, projectID domain.ProjectID, entries int, bytes int64) (admission.Reservation, error) {
	a.keyID, a.projectID, a.entries, a.bytes = keyID, projectID, entries, bytes
	return a.reservation, a.err
}

type fakeReservation struct {
	commits  int
	releases int
}

func (r *fakeReservation) Commit()  { r.commits++ }
func (r *fakeReservation) Release() { r.releases++ }

func testWithinTx(batches *batchRepo, usage *usageRepo, commitErr error, pipelineStatuses ...domain.PipelineStatus) WithinTx {
	pipelineStatus := domain.PipelineEnabled
	if len(pipelineStatuses) > 0 {
		pipelineStatus = pipelineStatuses[0]
	}
	return func(ctx context.Context, fn func(Repositories) error) error {
		repos := Repositories{
			Projects:  projectRepo{project: domain.Project{ID: projectID, Slug: "demo", Name: "Demo", Status: domain.ProjectActive}},
			Agents:    agentRepo{agent: domain.Agent{ID: agentID, ProjectID: projectID, Name: "agent", Hostname: "host", Status: domain.AgentActive}},
			Pipelines: pipelineRepo{pipeline: domain.Pipeline{ID: pipelineID, ProjectID: projectID, AgentID: agentID, Name: "pipe", Service: "api", Config: []byte(`{}`), ConfigVersion: 1, Status: pipelineStatus, ReportedStatus: domain.PipelineRunning}},
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
