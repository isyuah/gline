package query

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/isyuah/gline/internal/domain"
	serverauth "github.com/isyuah/gline/internal/server/auth"
)

var (
	ErrInvalidTimeRange = errors.New("invalid query time range")
	ErrInvalidLimit     = errors.New("invalid query limit")
	ErrInvalidFilter    = errors.New("invalid query filter")
	ErrInvalidCursor    = errors.New("invalid query cursor")
)

type ProjectRepository interface {
	Get(context.Context, domain.ProjectID) (domain.Project, error)
}

type EntryRepository interface {
	List(context.Context, domain.EntryQuery) (domain.EntryPage, error)
}

type Limiter interface {
	Acquire(context.Context, domain.ProjectID) (release func(), err error)
}

type Config struct {
	MaxRange         time.Duration
	ExecutionTimeout time.Duration
	DefaultLimit     int
	MaxLimit         int
	MaxFilterItems   int
	MaxFilterBytes   int
	MaxMessageBytes  int
}

func DefaultConfig() Config {
	return Config{MaxRange: 7 * 24 * time.Hour, ExecutionTimeout: 10 * time.Second, DefaultLimit: 100, MaxLimit: 500, MaxFilterItems: 32, MaxFilterBytes: 128, MaxMessageBytes: 512}
}

type Params struct {
	From     string
	To       string
	Services []string
	Hosts    []string
	Levels   []string
	Message  string
	Limit    int
	Cursor   string
}

type Page struct {
	Entries    []domain.Entry
	NextCursor string
}

type Service struct {
	projects ProjectRepository
	entries  EntryRepository
	limiter  Limiter
	cursors  *CursorCodec
	config   Config
	observer Observer
}

type Observer interface {
	ObserveQuery(result, filterShape string, rows int, duration time.Duration)
}

type Option func(*Service)

func WithObserver(observer Observer) Option {
	return func(service *Service) { service.observer = observer }
}

func NewService(projects ProjectRepository, entries EntryRepository, limiter Limiter, cursorSecret []byte, config Config, options ...Option) (*Service, error) {
	if projects == nil || entries == nil {
		return nil, errors.New("query service requires project and entry repositories")
	}
	if config.ExecutionTimeout == 0 {
		config.ExecutionTimeout = DefaultConfig().ExecutionTimeout
	}
	if config.MaxRange <= 0 || config.ExecutionTimeout <= 0 || config.DefaultLimit <= 0 || config.MaxLimit < config.DefaultLimit || config.MaxFilterItems <= 0 || config.MaxFilterBytes <= 0 || config.MaxMessageBytes <= 0 {
		return nil, errors.New("query service has invalid limits")
	}
	codec, err := NewCursorCodec(cursorSecret)
	if err != nil {
		return nil, err
	}
	service := &Service{projects: projects, entries: entries, limiter: limiter, cursors: codec, config: config}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service, nil
}

func (s *Service) Search(ctx context.Context, principal serverauth.Principal, params Params) (result Page, err error) {
	started := time.Now()
	shape := filterShape(params)
	defer func() {
		if s.observer == nil {
			return
		}
		outcome := "rejected"
		if err == nil {
			outcome = "success"
		}
		s.observer.ObserveQuery(outcome, shape, len(result.Entries), time.Since(started))
	}()
	if err := principal.Require(domain.ScopeQuery); err != nil {
		return Page{}, err
	}
	queryCtx, cancel := context.WithTimeout(ctx, s.config.ExecutionTimeout)
	defer cancel()
	query, filterHash, err := s.buildQuery(principal.ProjectID, params)
	if err != nil {
		return Page{}, err
	}
	project, err := s.projects.Get(queryCtx, principal.ProjectID)
	if err != nil {
		return Page{}, err
	}
	if err := project.CanQuery(); err != nil {
		return Page{}, err
	}
	if params.Cursor != "" {
		position, err := s.cursors.Decode(params.Cursor, principal.ProjectID, filterHash)
		if err != nil {
			return Page{}, err
		}
		if position.ObservedAt.Before(query.From) || !position.ObservedAt.Before(query.To) {
			return Page{}, ErrInvalidCursor
		}
		query.Cursor = &position
	}
	if s.limiter != nil {
		release, err := s.limiter.Acquire(queryCtx, principal.ProjectID)
		if err != nil {
			return Page{}, err
		}
		if release == nil {
			return Page{}, errors.New("query limiter returned a nil release function")
		}
		defer release()
	}
	page, err := s.entries.List(queryCtx, query)
	if err != nil {
		return Page{}, err
	}
	result = Page{Entries: page.Entries}
	if page.Next != nil {
		result.NextCursor, err = s.cursors.Encode(principal.ProjectID, filterHash, *page.Next)
		if err != nil {
			return Page{}, err
		}
	}
	return result, nil
}

func filterShape(params Params) string {
	hasService := len(params.Services) > 0
	hasLevel := len(params.Levels) > 0
	switch {
	case strings.TrimSpace(params.Message) != "":
		return "message_time"
	case hasService && hasLevel && len(params.Hosts) == 0:
		return "service_level_time"
	case hasService && !hasLevel && len(params.Hosts) == 0:
		return "service_time"
	case hasLevel && !hasService && len(params.Hosts) == 0:
		return "level_time"
	case !hasService && !hasLevel && len(params.Hosts) == 0:
		return "time_only"
	default:
		return "other_bounded"
	}
}

func (s *Service) buildQuery(projectID domain.ProjectID, params Params) (domain.EntryQuery, [sha256.Size]byte, error) {
	from, err := parseTime(params.From)
	if err != nil {
		return domain.EntryQuery{}, [sha256.Size]byte{}, ErrInvalidTimeRange
	}
	to, err := parseTime(params.To)
	if err != nil || !from.Before(to) || to.Sub(from) > s.config.MaxRange {
		return domain.EntryQuery{}, [sha256.Size]byte{}, ErrInvalidTimeRange
	}
	limit := params.Limit
	if limit == 0 {
		limit = s.config.DefaultLimit
	}
	if limit < 1 || limit > s.config.MaxLimit {
		return domain.EntryQuery{}, [sha256.Size]byte{}, ErrInvalidLimit
	}
	services, err := normalizeFilter(params.Services, s.config.MaxFilterItems, s.config.MaxFilterBytes, false)
	if err != nil {
		return domain.EntryQuery{}, [sha256.Size]byte{}, err
	}
	hosts, err := normalizeFilter(params.Hosts, s.config.MaxFilterItems, s.config.MaxFilterBytes, false)
	if err != nil {
		return domain.EntryQuery{}, [sha256.Size]byte{}, err
	}
	levels, err := normalizeFilter(params.Levels, s.config.MaxFilterItems, s.config.MaxFilterBytes, true)
	if err != nil {
		return domain.EntryQuery{}, [sha256.Size]byte{}, err
	}
	message := strings.TrimSpace(params.Message)
	if !utf8.ValidString(message) || strings.ContainsRune(message, 0) || len(message) > s.config.MaxMessageBytes {
		return domain.EntryQuery{}, [sha256.Size]byte{}, ErrInvalidFilter
	}
	query := domain.EntryQuery{ProjectID: projectID, From: from, To: to, Services: services, Hosts: hosts, Levels: levels, Message: message, Limit: limit}
	if err := query.Validate(s.config.MaxRange, s.config.MaxLimit); err != nil {
		return domain.EntryQuery{}, [sha256.Size]byte{}, err
	}
	hash, err := queryFilterHash(query)
	return query, hash, err
}

func parseTime(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, ErrInvalidTimeRange
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil || parsed.Year() < 1970 || parsed.Year() > 9999 {
		return time.Time{}, ErrInvalidTimeRange
	}
	return parsed.UTC(), nil
}

func normalizeFilter(values []string, maxItems, maxBytes int, upper bool) ([]string, error) {
	if len(values) > maxItems {
		return nil, ErrInvalidFilter
	}
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if upper {
			value = strings.ToUpper(value)
		}
		if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, 0) || len(value) > maxBytes {
			return nil, ErrInvalidFilter
		}
		unique[value] = struct{}{}
	}
	normalized := make([]string, 0, len(unique))
	for value := range unique {
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func queryFilterHash(query domain.EntryQuery) ([sha256.Size]byte, error) {
	payload := struct {
		From     string   `json:"from"`
		To       string   `json:"to"`
		Services []string `json:"services"`
		Hosts    []string `json:"hosts"`
		Levels   []string `json:"levels"`
		Message  string   `json:"message"`
	}{query.From.UTC().Format(time.RFC3339Nano), query.To.UTC().Format(time.RFC3339Nano), query.Services, query.Hosts, query.Levels, query.Message}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("encode query fingerprint: %w", err)
	}
	return sha256.Sum256(encoded), nil
}

type CursorCodec struct{ secret []byte }

type cursorPayload struct {
	Version    int    `json:"v"`
	ProjectID  string `json:"project_id"`
	FilterHash string `json:"filter_hash"`
	ObservedAt string `json:"observed_at"`
	ID         int64  `json:"id"`
}

func NewCursorCodec(secret []byte) (*CursorCodec, error) {
	if len(secret) < 16 {
		return nil, errors.New("cursor codec requires at least 16 secret bytes")
	}
	return &CursorCodec{secret: append([]byte(nil), secret...)}, nil
}

func (c *CursorCodec) Encode(projectID domain.ProjectID, filterHash [sha256.Size]byte, position domain.EntryCursor) (string, error) {
	if !projectID.Valid() || position.ObservedAt.IsZero() || position.ID <= 0 {
		return "", ErrInvalidCursor
	}
	payload := cursorPayload{Version: 1, ProjectID: string(projectID), FilterHash: hex.EncodeToString(filterHash[:]), ObservedAt: position.ObservedAt.UTC().Format(time.RFC3339Nano), ID: int64(position.ID)}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode cursor: %w", err)
	}
	mac := hmac.New(sha256.New, c.secret)
	_, _ = mac.Write(encoded)
	return base64.RawURLEncoding.EncodeToString(encoded) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (c *CursorCodec) Decode(raw string, expectedProject domain.ProjectID, expectedFilterHash [sha256.Size]byte) (domain.EntryCursor, error) {
	if len(raw) > 2_048 || strings.Count(raw, ".") != 1 {
		return domain.EntryCursor{}, ErrInvalidCursor
	}
	payloadPart, macPart, _ := strings.Cut(raw, ".")
	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil || len(payload) > 1_024 {
		return domain.EntryCursor{}, ErrInvalidCursor
	}
	providedMAC, err := base64.RawURLEncoding.DecodeString(macPart)
	if err != nil || len(providedMAC) != sha256.Size {
		return domain.EntryCursor{}, ErrInvalidCursor
	}
	mac := hmac.New(sha256.New, c.secret)
	_, _ = mac.Write(payload)
	if subtle.ConstantTimeCompare(providedMAC, mac.Sum(nil)) != 1 {
		return domain.EntryCursor{}, ErrInvalidCursor
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var decoded cursorPayload
	if err := decoder.Decode(&decoded); err != nil {
		return domain.EntryCursor{}, ErrInvalidCursor
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return domain.EntryCursor{}, ErrInvalidCursor
	}
	if decoded.Version != 1 || domain.ProjectID(decoded.ProjectID) != expectedProject || decoded.FilterHash != hex.EncodeToString(expectedFilterHash[:]) || decoded.ID <= 0 {
		return domain.EntryCursor{}, ErrInvalidCursor
	}
	observedAt, err := time.Parse(time.RFC3339Nano, decoded.ObservedAt)
	if err != nil {
		return domain.EntryCursor{}, ErrInvalidCursor
	}
	return domain.EntryCursor{ObservedAt: observedAt.UTC(), ID: domain.EntryID(decoded.ID)}, nil
}
