package operations

import (
	"context"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/domain"
	"github.com/isyuah/gline/internal/protocol/ingestv1"
	serverauth "github.com/isyuah/gline/internal/server/auth"
	"github.com/isyuah/gline/internal/server/ingest"
)

const (
	operationsProjectID    domain.ProjectID    = "11111111-1111-4111-8111-111111111111"
	operationsQuarantineID domain.QuarantineID = "22222222-2222-4222-8222-222222222222"
)

type retentionRepo struct{ policy domain.RetentionPolicy }

func (r *retentionRepo) UpsertPolicy(_ context.Context, policy domain.RetentionPolicy) (domain.RetentionPolicy, error) {
	r.policy = policy
	return policy, nil
}

type quarantineRepo struct {
	batch        domain.QuarantineBatch
	releaseCalls int
}

func (r *quarantineRepo) Claim(context.Context, domain.ProjectID, domain.QuarantineID) (domain.QuarantineBatch, error) {
	r.batch.Status = domain.QuarantineReplaying
	r.batch.Attempts++
	return r.batch, nil
}

func (r *quarantineRepo) MarkTerminal(_ context.Context, _ domain.ProjectID, _ domain.QuarantineID, status domain.QuarantineStatus, detail string, at time.Time) error {
	r.batch.Status = status
	r.batch.ErrorDetail = detail
	r.batch.ResolvedAt = &at
	return nil
}

func (r *quarantineRepo) ReleaseForRetry(_ context.Context, _ domain.ProjectID, _ domain.QuarantineID, detail string) error {
	r.releaseCalls++
	r.batch.Status = domain.QuarantinePending
	r.batch.ErrorDetail = detail
	return nil
}

type auditRepo struct{ events []domain.AuditEvent }

func (r *auditRepo) Append(_ context.Context, event domain.AuditEvent) (domain.AuditEvent, error) {
	r.events = append(r.events, event)
	return event, nil
}

type ingestStub struct{ calls int }

func (s *ingestStub) Accept(context.Context, serverauth.Principal, domain.Batch) (ingest.Result, error) {
	s.calls++
	return ingest.Result{}, nil
}

func TestSetRetentionPersistsPolicyAndAuditInOneTransaction(t *testing.T) {
	retention := &retentionRepo{}
	audit := &auditRepo{}
	transactions := 0
	service, err := New(func(ctx context.Context, fn func(Repositories) error) error {
		transactions++
		return fn(Repositories{Retention: retention, Audit: audit})
	}, &ingestStub{}, ingestv1.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	policy := domain.RetentionPolicy{ProjectID: operationsProjectID, MaxAge: 30 * 24 * time.Hour, Enabled: true}
	saved, err := service.SetRetention(context.Background(), operationsPrincipal(domain.ScopeRetentionManage), policy)
	if err != nil {
		t.Fatal(err)
	}
	if transactions != 1 || saved.MaxAge != policy.MaxAge || retention.policy.ProjectID != operationsProjectID {
		t.Fatalf("transactions=%d saved=%+v retention=%+v", transactions, saved, retention.policy)
	}
	if len(audit.events) != 1 || audit.events[0].Action != "retention.set" || audit.events[0].Outcome != domain.AuditSuccess {
		t.Fatalf("audit events=%+v", audit.events)
	}
}

func TestReplayInvalidPayloadReleasesClaimForRetry(t *testing.T) {
	quarantine := &quarantineRepo{batch: domain.QuarantineBatch{
		ID: operationsQuarantineID, ProjectID: operationsProjectID,
		BatchID: "33333333-3333-4333-8333-333333333333", Payload: []byte(`{"version":`),
		Status: domain.QuarantinePending,
	}}
	audit := &auditRepo{}
	ingester := &ingestStub{}
	service, err := New(func(ctx context.Context, fn func(Repositories) error) error {
		return fn(Repositories{Quarantine: quarantine, Audit: audit})
	}, ingester, ingestv1.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.ReplayQuarantine(context.Background(), operationsPrincipal(domain.ScopeQuarantineReplay), operationsProjectID, operationsQuarantineID)
	if err == nil {
		t.Fatalf("replay error=%v", err)
	}
	if ingester.calls != 0 || quarantine.releaseCalls != 1 || quarantine.batch.Status != domain.QuarantinePending {
		t.Fatalf("ingest calls=%d release calls=%d batch=%+v", ingester.calls, quarantine.releaseCalls, quarantine.batch)
	}
	if len(audit.events) != 2 || audit.events[0].Action != "quarantine.replay.claim" || audit.events[1].Action != "quarantine.replay.release" || audit.events[1].Outcome != domain.AuditFailed {
		t.Fatalf("audit events=%+v", audit.events)
	}
}

func operationsPrincipal(scopes ...domain.Scope) serverauth.Principal {
	values := make(map[domain.Scope]struct{}, len(scopes))
	for _, scope := range scopes {
		values[scope] = struct{}{}
	}
	return serverauth.Principal{
		KeyID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ProjectID: operationsProjectID, Scopes: values,
	}
}
