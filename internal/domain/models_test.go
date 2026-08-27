package domain

import (
	"errors"
	"testing"
	"time"
)

const (
	projectOne  ProjectID  = "11111111-1111-1111-1111-111111111111"
	agentOne    AgentID    = "22222222-2222-2222-2222-222222222222"
	pipelineOne PipelineID = "33333333-3333-3333-3333-333333333333"
	batchOne    BatchID    = "44444444-4444-4444-4444-444444444444"
)

func TestProjectDisabledRejectsDataPlane(t *testing.T) {
	p := Project{ID: projectOne, Slug: "demo", Name: "Demo", Status: ProjectDisabled}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if err := p.CanIngest(); !errors.Is(err, ErrProjectDisabled) {
		t.Fatalf("CanIngest() error = %v, want ErrProjectDisabled", err)
	}
	if err := p.CanQuery(); !errors.Is(err, ErrProjectDisabled) {
		t.Fatalf("CanQuery() error = %v, want ErrProjectDisabled", err)
	}
}

func TestBatchRejectsCrossProjectEntry(t *testing.T) {
	now := time.Now().UTC()
	batch := Batch{
		ID: batchOne, ProjectID: projectOne, AgentID: agentOne, PipelineID: pipelineOne,
		PayloadBytes: 1, CreatedAt: now,
		Entries: []Entry{{
			ProjectID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", BatchSequence: 0,
			Service: "api", Host: "host", Level: "INFO", ObservedAt: now,
		}},
	}
	if err := batch.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate() error = %v, want ErrInvalid", err)
	}
}

func TestAPIKeyUsabilityAndScopes(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(time.Minute)
	key := APIKey{
		ID: "55555555-5555-5555-5555-555555555555", ProjectID: projectOne,
		Prefix: "gln_live_abc", SecretHash: []byte{1}, Scopes: []Scope{ScopeIngest},
		Status: KeyActive, ExpiresAt: &expires,
	}
	if err := key.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if !key.HasScope(ScopeIngest) || key.HasScope(ScopeQuery) {
		t.Fatal("scope lookup returned an invalid result")
	}
	if !key.UsableAt(now) || key.UsableAt(expires) {
		t.Fatal("expiry boundary returned an invalid result")
	}
}

func TestStoredBatchDistinguishesDuplicateFromConflict(t *testing.T) {
	stored := StoredBatch{
		ID: batchOne, ProjectID: projectOne, Status: BatchCommitted,
		PayloadHash: [32]byte{1},
	}
	candidate := Batch{ID: batchOne, ProjectID: projectOne, PayloadHash: [32]byte{1}}
	if err := stored.VerifyRetry(candidate); err != nil {
		t.Fatalf("VerifyRetry(same hash) error = %v", err)
	}
	candidate.PayloadHash[0] = 2
	if err := stored.VerifyRetry(candidate); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("VerifyRetry(different hash) error = %v, want ErrIdempotencyConflict", err)
	}
}
