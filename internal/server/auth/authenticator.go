package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/isyuah/gline/internal/domain"
)

type KeyRepository interface {
	FindActiveByPrefix(context.Context, string, time.Time) ([]domain.APIKey, error)
}

type keyUsageRepository interface {
	TouchLastUsed(context.Context, domain.ProjectID, domain.APIKeyID, time.Time) error
}

type Clock func() time.Time

const (
	lastUsedObservationInterval  = time.Minute
	lastUsedObservationTimeout   = 2 * time.Second
	maxUsageObservationsInFlight = 8
)

type Option func(*Authenticator)

func WithLogger(logger *slog.Logger) Option {
	return func(authenticator *Authenticator) {
		if logger != nil {
			authenticator.logger = logger
		}
	}
}

type Authenticator struct {
	keys              KeyRepository
	pepper            []byte
	now               Clock
	logger            *slog.Logger
	usageMu           sync.Mutex
	nextUsageAttempt  map[domain.APIKeyID]time.Time
	usageObservations chan struct{}
}

func NewAuthenticator(keys KeyRepository, pepper []byte, now Clock, options ...Option) (*Authenticator, error) {
	if keys == nil || len(pepper) < 16 {
		return nil, errors.New("authenticator requires repository and at least 16 pepper bytes")
	}
	if now == nil {
		now = time.Now
	}
	authenticator := &Authenticator{
		keys:              keys,
		pepper:            append([]byte(nil), pepper...),
		now:               now,
		logger:            slog.Default(),
		nextUsageAttempt:  make(map[domain.APIKeyID]time.Time),
		usageObservations: make(chan struct{}, maxUsageObservationsInFlight),
	}
	for _, option := range options {
		if option != nil {
			option(authenticator)
		}
	}
	return authenticator, nil
}

func ParseKey(raw string) (prefix, secret string, err error) {
	if len(raw) > 768 || strings.Count(raw, ".") != 1 {
		return "", "", ErrInvalidCredential
	}
	prefix, secret, _ = strings.Cut(raw, ".")
	if len(prefix) < 4 || len(prefix) > 128 || len(secret) < 16 || len(secret) > 512 {
		return "", "", ErrInvalidCredential
	}
	for _, value := range prefix {
		if !(unicode.IsLetter(value) || unicode.IsDigit(value) || value == '_' || value == '-') || value > unicode.MaxASCII {
			return "", "", ErrInvalidCredential
		}
	}
	for _, value := range secret {
		if !(unicode.IsLetter(value) || unicode.IsDigit(value) || value == '_' || value == '-') || value > unicode.MaxASCII {
			return "", "", ErrInvalidCredential
		}
	}
	return prefix, secret, nil
}

func HashSecret(secret string, pepper []byte) [sha256.Size]byte {
	mac := hmac.New(sha256.New, pepper)
	_, _ = mac.Write([]byte(secret))
	var result [sha256.Size]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func (a *Authenticator) Authenticate(ctx context.Context, raw string) (Principal, error) {
	prefix, secret, err := ParseKey(raw)
	if err != nil {
		return Principal{}, ErrInvalidCredential
	}
	now := a.now().UTC()
	candidates, err := a.keys.FindActiveByPrefix(ctx, prefix, now)
	if err != nil {
		return Principal{}, fmt.Errorf("find api key candidate: %w", err)
	}
	want := HashSecret(secret, a.pepper)
	match := -1
	for index := range candidates {
		candidate := candidates[index]
		if len(candidate.SecretHash) != sha256.Size {
			// Compare with a fixed-size value even for a corrupt candidate.
			var invalid [sha256.Size]byte
			_ = subtle.ConstantTimeCompare(want[:], invalid[:])
			continue
		}
		matched := subtle.ConstantTimeCompare(want[:], candidate.SecretHash)
		usable := subtle.ConstantTimeByteEq(byte(boolInt(candidate.UsableAt(now))), 1)
		if matched&usable == 1 {
			if match >= 0 {
				// A credential resolving to multiple tenants is not a valid identity.
				return Principal{}, ErrInvalidCredential
			}
			match = index
		}
	}
	if match < 0 {
		return Principal{}, ErrInvalidCredential
	}
	principal, err := NewPrincipal(candidates[match])
	if err != nil {
		return Principal{}, fmt.Errorf("invalid api key record: %w", err)
	}
	a.observeLastUsed(ctx, candidates[match], now)
	return principal, nil
}

func (a *Authenticator) observeLastUsed(ctx context.Context, key domain.APIKey, observedAt time.Time) {
	usage, ok := a.keys.(keyUsageRepository)
	if !ok || key.LastUsedAt != nil && observedAt.Sub(*key.LastUsedAt) < lastUsedObservationInterval {
		return
	}

	a.usageMu.Lock()
	nextAttempt := a.nextUsageAttempt[key.ID]
	if !nextAttempt.IsZero() && observedAt.Before(nextAttempt) {
		a.usageMu.Unlock()
		return
	}
	a.nextUsageAttempt[key.ID] = observedAt.Add(lastUsedObservationInterval)
	a.usageMu.Unlock()

	select {
	case a.usageObservations <- struct{}{}:
	case <-ctx.Done():
		return
	default:
		return
	}

	go func() {
		defer func() { <-a.usageObservations }()
		usageCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lastUsedObservationTimeout)
		defer cancel()
		if err := usage.TouchLastUsed(usageCtx, key.ProjectID, key.ID, observedAt); err != nil {
			a.logger.Warn("record API key usage", "key_id", key.ID, "error", err)
		}
	}()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
