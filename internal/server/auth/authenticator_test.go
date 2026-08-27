package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

type keyRepo struct {
	keys []domain.APIKey
	err  error
}

type usageKeyRepo struct {
	keyRepo
	touched chan domain.APIKeyID
	err     error
}

func (r *usageKeyRepo) TouchLastUsed(_ context.Context, _ domain.ProjectID, keyID domain.APIKeyID, _ time.Time) error {
	r.touched <- keyID
	return r.err
}

func (r keyRepo) FindActiveByPrefix(context.Context, string, time.Time) ([]domain.APIKey, error) {
	return r.keys, r.err
}

func TestAuthenticateReturnsTenantBoundPrincipal(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	pepper := []byte("0123456789abcdef0123456789abcdef")
	hash := HashSecret("0123456789abcdef0123456789abcdef", pepper)
	key := domain.APIKey{
		ID: "55555555-5555-5555-5555-555555555555", ProjectID: "11111111-1111-1111-1111-111111111111",
		Prefix: "glk_test", SecretHash: hash[:], Scopes: []domain.Scope{domain.ScopeIngest}, Status: domain.KeyActive,
	}
	authenticator, err := NewAuthenticator(keyRepo{keys: []domain.APIKey{key}}, pepper, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authenticator.Authenticate(context.Background(), "glk_test.0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if principal.ProjectID != key.ProjectID || !principal.Has(domain.ScopeIngest) {
		t.Fatalf("principal = %+v", principal)
	}
	if err := principal.Require(domain.ScopeQuery); !errors.Is(err, domain.ErrScopeDenied) {
		t.Fatalf("scope error = %v", err)
	}
}

func TestAuthenticateRejectsWrongSecretAndAmbiguousCredential(t *testing.T) {
	pepper := []byte("0123456789abcdef0123456789abcdef")
	hash := HashSecret("0123456789abcdef0123456789abcdef", pepper)
	key := domain.APIKey{ID: "55555555-5555-5555-5555-555555555555", ProjectID: "11111111-1111-1111-1111-111111111111", Prefix: "glk_test", SecretHash: hash[:], Scopes: []domain.Scope{domain.ScopeIngest}, Status: domain.KeyActive}
	authenticator, _ := NewAuthenticator(keyRepo{keys: []domain.APIKey{key}}, pepper, time.Now)
	if _, err := authenticator.Authenticate(context.Background(), "glk_test.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("wrong secret error = %v", err)
	}
	keyTwo := key
	keyTwo.ID = "66666666-6666-6666-6666-666666666666"
	keyTwo.ProjectID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	authenticator, _ = NewAuthenticator(keyRepo{keys: []domain.APIKey{key, keyTwo}}, pepper, time.Now)
	if _, err := authenticator.Authenticate(context.Background(), "glk_test.0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("ambiguous secret error = %v", err)
	}
}

func TestAuthenticateDoesNotDependOnLastUsedObservation(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	pepper := []byte("0123456789abcdef0123456789abcdef")
	hash := HashSecret("0123456789abcdef0123456789abcdef", pepper)
	key := domain.APIKey{
		ID: "55555555-5555-5555-5555-555555555555", ProjectID: "11111111-1111-1111-1111-111111111111",
		Prefix: "glk_test", SecretHash: hash[:], Scopes: []domain.Scope{domain.ScopeIngest}, Status: domain.KeyActive,
	}
	repository := &usageKeyRepo{keyRepo: keyRepo{keys: []domain.APIKey{key}}, touched: make(chan domain.APIKeyID, 1), err: errors.New("database write unavailable")}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authenticator, err := NewAuthenticator(repository, pepper, func() time.Time { return now }, WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}

	principal, err := authenticator.Authenticate(t.Context(), "glk_test.0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("Authenticate() error = %v, want valid principal despite observation failure", err)
	}
	if principal.ProjectID != key.ProjectID {
		t.Fatalf("principal project = %s, want %s", principal.ProjectID, key.ProjectID)
	}
	select {
	case touched := <-repository.touched:
		if touched != key.ID {
			t.Fatalf("observed key = %s, want %s", touched, key.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("last-used observation was not attempted")
	}
}

func TestAuthenticateSkipsRecentLastUsedObservation(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	recent := now.Add(-30 * time.Second)
	pepper := []byte("0123456789abcdef0123456789abcdef")
	hash := HashSecret("0123456789abcdef0123456789abcdef", pepper)
	key := domain.APIKey{
		ID: "55555555-5555-5555-5555-555555555555", ProjectID: "11111111-1111-1111-1111-111111111111",
		Prefix: "glk_test", SecretHash: hash[:], Scopes: []domain.Scope{domain.ScopeIngest}, Status: domain.KeyActive, LastUsedAt: &recent,
	}
	repository := &usageKeyRepo{keyRepo: keyRepo{keys: []domain.APIKey{key}}, touched: make(chan domain.APIKeyID, 1)}
	authenticator, err := NewAuthenticator(repository, pepper, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	if _, err := authenticator.Authenticate(t.Context(), "glk_test.0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if len(repository.touched) != 0 {
		t.Fatal("recently used key triggered another observation")
	}
}
