package query

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/domain"
	serverauth "github.com/isyuah/gline/internal/server/auth"
)

const queryProjectID domain.ProjectID = "11111111-1111-1111-1111-111111111111"

type projects struct{}

func (projects) Get(context.Context, domain.ProjectID) (domain.Project, error) {
	return domain.Project{ID: queryProjectID, Slug: "demo", Name: "Demo", Status: domain.ProjectActive}, nil
}

type entries struct {
	queries     []domain.EntryQuery
	err         error
	deadline    time.Time
	hasDeadline bool
}

type blockingEntries struct{}

func (blockingEntries) List(ctx context.Context, _ domain.EntryQuery) (domain.EntryPage, error) {
	<-ctx.Done()
	return domain.EntryPage{}, ctx.Err()
}

func (r *entries) List(ctx context.Context, query domain.EntryQuery) (domain.EntryPage, error) {
	r.queries = append(r.queries, query)
	r.deadline, r.hasDeadline = ctx.Deadline()
	if r.err != nil {
		return domain.EntryPage{}, r.err
	}
	return domain.EntryPage{Entries: []domain.Entry{{ID: 5}}, Next: &domain.EntryCursor{ObservedAt: query.From.Add(time.Minute), ID: 5}}, nil
}

type limiter struct {
	acquired int
	released int
	err      error
}

func (l *limiter) Acquire(context.Context, domain.ProjectID) (func(), error) {
	l.acquired++
	if l.err != nil {
		return nil, l.err
	}
	return func() { l.released++ }, nil
}

type queryObserver struct{ result string }

func (o *queryObserver) ObserveQuery(result, _ string, _ int, _ time.Duration) {
	o.result = result
}

func TestSearchBindsProjectAndCursorToNormalizedFilters(t *testing.T) {
	repository := &entries{}
	limit := &limiter{}
	service, err := NewService(projects{}, repository, limit, []byte("0123456789abcdef0123456789abcdef"), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	principal := queryPrincipal()
	params := Params{From: "2026-08-24T00:00:00+08:00", To: "2026-08-24T01:00:00+08:00", Services: []string{"api", "api"}, Levels: []string{"error"}, Limit: 25}
	page, err := service.Search(context.Background(), principal, params)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" || repository.queries[0].ProjectID != queryProjectID || len(repository.queries[0].Services) != 1 || repository.queries[0].Levels[0] != "ERROR" {
		t.Fatalf("page=%+v query=%+v", page, repository.queries[0])
	}
	params.Cursor = page.NextCursor
	if _, err := service.Search(context.Background(), principal, params); err != nil {
		t.Fatalf("valid cursor: %v", err)
	}
	if repository.queries[1].Cursor == nil || repository.queries[1].Cursor.ID != 5 {
		t.Fatalf("cursor query=%+v", repository.queries[1])
	}
	params.Services = []string{"other"}
	if _, err := service.Search(context.Background(), principal, params); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("filter-bound cursor error=%v", err)
	}
	if limit.acquired != 2 || limit.released != 2 {
		t.Fatalf("limiter acquired=%d released=%d", limit.acquired, limit.released)
	}
}

func TestSearchReleasesLimiterWhenRepositoryFails(t *testing.T) {
	repository := &entries{err: errors.New("database unavailable")}
	limit := &limiter{}
	service, _ := NewService(projects{}, repository, limit, []byte("0123456789abcdef0123456789abcdef"), DefaultConfig())
	_, _ = service.Search(context.Background(), queryPrincipal(), Params{From: "2026-08-24T00:00:00Z", To: "2026-08-24T01:00:00Z"})
	if limit.acquired != 1 || limit.released != 1 {
		t.Fatalf("limiter acquired=%d released=%d", limit.acquired, limit.released)
	}
}

func TestSearchBoundsRepositoryExecutionAndPreservesShorterClientDeadline(t *testing.T) {
	params := Params{From: "2026-08-24T00:00:00Z", To: "2026-08-24T01:00:00Z"}
	config := DefaultConfig()
	config.ExecutionTimeout = 100 * time.Millisecond

	t.Run("service maximum", func(t *testing.T) {
		repository := &entries{}
		service, err := NewService(projects{}, repository, nil, []byte("0123456789abcdef0123456789abcdef"), config)
		if err != nil {
			t.Fatal(err)
		}
		startedAt := time.Now()
		if _, err := service.Search(t.Context(), queryPrincipal(), params); err != nil {
			t.Fatal(err)
		}
		if !repository.hasDeadline {
			t.Fatal("repository context has no deadline")
		}
		if remaining := repository.deadline.Sub(startedAt); remaining <= 0 || remaining > config.ExecutionTimeout {
			t.Fatalf("repository deadline remaining = %v, want within (0, %v]", remaining, config.ExecutionTimeout)
		}
	})

	t.Run("client deadline", func(t *testing.T) {
		repository := &entries{}
		config := DefaultConfig()
		service, err := NewService(projects{}, repository, nil, []byte("0123456789abcdef0123456789abcdef"), config)
		if err != nil {
			t.Fatal(err)
		}
		clientDeadline := time.Now().Add(time.Second)
		ctx, cancel := context.WithDeadline(t.Context(), clientDeadline)
		defer cancel()
		if _, err := service.Search(ctx, queryPrincipal(), params); err != nil {
			t.Fatal(err)
		}
		if !repository.hasDeadline || !repository.deadline.Equal(clientDeadline) {
			t.Fatalf("repository deadline = %v, want client deadline %v", repository.deadline, clientDeadline)
		}
	})
}

func TestSearchClassifiesExecutionDeadline(t *testing.T) {
	config := DefaultConfig()
	config.ExecutionTimeout = time.Millisecond
	observer := &queryObserver{}
	service, err := NewService(projects{}, blockingEntries{}, nil, []byte("0123456789abcdef0123456789abcdef"), config, WithObserver(observer))
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Search(t.Context(), queryPrincipal(), Params{
		From: "2026-08-24T00:00:00Z", To: "2026-08-24T01:00:00Z",
	})
	if !errors.Is(err, ErrExecutionTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Search() error = %v", err)
	}
	if observer.result != "timeout" {
		t.Fatalf("observer result = %q", observer.result)
	}
}

func TestSearchClassifiesCapacityRejection(t *testing.T) {
	observer := &queryObserver{}
	limit := &limiter{err: ErrCapacityLimited}
	service, err := NewService(projects{}, &entries{}, limit, []byte("0123456789abcdef0123456789abcdef"), DefaultConfig(), WithObserver(observer))
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Search(t.Context(), queryPrincipal(), Params{
		From: "2026-08-24T00:00:00Z", To: "2026-08-24T01:00:00Z",
	})
	if !errors.Is(err, ErrCapacityLimited) || observer.result != "rate_limited" {
		t.Fatalf("Search() error = %v, observer result = %q", err, observer.result)
	}
}

func queryPrincipal() serverauth.Principal {
	return serverauth.Principal{KeyID: "55555555-5555-5555-5555-555555555555", ProjectID: queryProjectID, Scopes: map[domain.Scope]struct{}{domain.ScopeQuery: {}}}
}
