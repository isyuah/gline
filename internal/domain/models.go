package domain

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

type Project struct {
	ID        ProjectID
	Slug      string
	Name      string
	Status    ProjectStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (p Project) Validate() error {
	if !p.ID.Valid() || strings.TrimSpace(p.Slug) == "" || strings.TrimSpace(p.Name) == "" || !p.Status.Valid() {
		return fmt.Errorf("%w: project", ErrInvalid)
	}
	return nil
}

func (p Project) CanIngest() error {
	if p.Status != ProjectActive {
		return ErrProjectDisabled
	}
	return nil
}

func (p Project) CanQuery() error { return p.CanIngest() }

type APIKey struct {
	ID         APIKeyID
	ProjectID  ProjectID
	AgentID    *AgentID
	Name       string
	Prefix     string
	SecretHash []byte
	Scopes     []Scope
	Status     KeyStatus
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

func (k APIKey) HasScope(want Scope) bool {
	for _, scope := range k.Scopes {
		if scope == want {
			return true
		}
	}
	return false
}

func (k APIKey) UsableAt(now time.Time) bool {
	if k.Status != KeyActive {
		return false
	}
	return k.ExpiresAt == nil || now.Before(*k.ExpiresAt)
}

func (k APIKey) Validate() error {
	if !k.ID.Valid() || !k.ProjectID.Valid() || strings.TrimSpace(k.Prefix) == "" || len(k.SecretHash) == 0 || !k.Status.Valid() {
		return fmt.Errorf("%w: api key", ErrInvalid)
	}
	if k.AgentID != nil && !k.AgentID.Valid() {
		return fmt.Errorf("%w: api key agent", ErrInvalid)
	}
	if len(k.Name) > 128 {
		return fmt.Errorf("%w: api key name", ErrInvalid)
	}
	if len(k.Scopes) == 0 {
		return fmt.Errorf("%w: api key scopes", ErrInvalid)
	}
	if (k.Status == KeyRevoked) != (k.RevokedAt != nil) {
		return fmt.Errorf("%w: api key revocation state", ErrInvalid)
	}
	if k.Status == KeyExpired && k.ExpiresAt == nil {
		return fmt.Errorf("%w: api key expiry state", ErrInvalid)
	}
	seen := make(map[Scope]struct{}, len(k.Scopes))
	for _, scope := range k.Scopes {
		if !scope.Valid() {
			return fmt.Errorf("%w: scope %q", ErrInvalid, scope)
		}
		if _, ok := seen[scope]; ok {
			return fmt.Errorf("%w: duplicate scope %q", ErrInvalid, scope)
		}
		seen[scope] = struct{}{}
	}
	return nil
}

type Agent struct {
	ID            AgentID
	ProjectID     ProjectID
	Name          string
	Hostname      string
	Version       string
	Status        AgentStatus
	LastHeartbeat *time.Time
	LastSeenIP    net.IP
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (a Agent) Validate() error {
	if !a.ID.Valid() || !a.ProjectID.Valid() || strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Hostname) == "" || !a.Status.Valid() {
		return fmt.Errorf("%w: agent", ErrInvalid)
	}
	return nil
}

type Pipeline struct {
	ID             PipelineID
	ProjectID      ProjectID
	AgentID        AgentID
	Name           string
	Service        string
	Config         json.RawMessage
	ConfigVersion  int64
	Status         PipelineStatus
	ReportedStatus ReportedPipelineStatus
	ReportedAt     *time.Time
	LastError      *string
	UpdatedAt      time.Time
}

func (p Pipeline) Validate() error {
	if !p.ID.Valid() || !p.ProjectID.Valid() || !p.AgentID.Valid() || strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Service) == "" {
		return fmt.Errorf("%w: pipeline identity", ErrInvalid)
	}
	if p.ConfigVersion <= 0 || !p.Status.Valid() || !p.ReportedStatus.Valid() || !ValidJSONObject(p.Config) {
		return fmt.Errorf("%w: pipeline state", ErrInvalid)
	}
	return nil
}

func ValidJSONObject(raw json.RawMessage) bool {
	if !json.Valid(raw) {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

type Batch struct {
	ID           BatchID
	ProjectID    ProjectID
	AgentID      AgentID
	PipelineID   PipelineID
	Sequence     int64
	PayloadHash  [32]byte
	PayloadBytes int
	Entries      []Entry
	CreatedAt    time.Time
}

func (b Batch) Validate() error {
	if !b.ID.Valid() || !b.ProjectID.Valid() || !b.AgentID.Valid() || !b.PipelineID.Valid() || b.Sequence < 0 || b.PayloadBytes <= 0 || b.CreatedAt.IsZero() {
		return fmt.Errorf("%w: batch", ErrInvalid)
	}
	if len(b.Entries) == 0 {
		return fmt.Errorf("%w: batch entries", ErrInvalid)
	}
	for i := range b.Entries {
		entry := &b.Entries[i]
		if entry.BatchSequence != i {
			return fmt.Errorf("%w: entry sequence %d", ErrInvalid, entry.BatchSequence)
		}
		if err := entry.ValidateForBatch(b); err != nil {
			return err
		}
	}
	return nil
}

type StoredBatch struct {
	ID           BatchID
	ProjectID    ProjectID
	AgentID      AgentID
	PipelineID   PipelineID
	Sequence     int64
	PayloadHash  [32]byte
	EntryCount   int
	PayloadBytes int
	Status       BatchStatus
	CreatedAt    time.Time
	CommittedAt  *time.Time
	ErrorCode    *string
}

// VerifyRetry checks the stable idempotency contract after an insert loses the
// (project_id, batch_id) race. A nil result means duplicate, while a different
// canonical payload is a terminal conflict.
func (b StoredBatch) VerifyRetry(candidate Batch) error {
	if b.ProjectID != candidate.ProjectID || b.ID != candidate.ID || b.Status != BatchCommitted {
		return fmt.Errorf("%w: retry does not address a committed batch", ErrInvalid)
	}
	if subtle.ConstantTimeCompare(b.PayloadHash[:], candidate.PayloadHash[:]) != 1 {
		return ErrIdempotencyConflict
	}
	return nil
}

type Entry struct {
	ID            EntryID
	ProjectID     ProjectID
	BatchID       BatchID
	AgentID       AgentID
	PipelineID    PipelineID
	BatchSequence int
	Service       string
	Host          string
	Level         string
	Message       string
	ObservedAt    time.Time
	IngestedAt    time.Time
	Attributes    map[string]any
}

func (e Entry) ValidateForBatch(batch Batch) error {
	if e.ProjectID != "" && e.ProjectID != batch.ProjectID {
		return fmt.Errorf("%w: entry project differs from batch", ErrInvalid)
	}
	if e.BatchID != "" && e.BatchID != batch.ID {
		return fmt.Errorf("%w: entry batch differs from batch", ErrInvalid)
	}
	if e.AgentID != "" && e.AgentID != batch.AgentID {
		return fmt.Errorf("%w: entry agent differs from batch", ErrInvalid)
	}
	if e.PipelineID != "" && e.PipelineID != batch.PipelineID {
		return fmt.Errorf("%w: entry pipeline differs from batch", ErrInvalid)
	}
	if e.BatchSequence < 0 || strings.TrimSpace(e.Service) == "" || strings.TrimSpace(e.Host) == "" || strings.TrimSpace(e.Level) == "" || e.ObservedAt.IsZero() {
		return fmt.Errorf("%w: entry", ErrInvalid)
	}
	return nil
}

type QuarantineBatch struct {
	ID          QuarantineID
	ProjectID   ProjectID
	BatchID     BatchID
	Payload     []byte
	PayloadHash [32]byte
	ErrorCode   string
	ErrorDetail string
	Status      QuarantineStatus
	Attempts    int
	CreatedAt   time.Time
	ClaimedAt   *time.Time
	ResolvedAt  *time.Time
}

type AuditEvent struct {
	ID         AuditID
	ProjectID  *ProjectID
	ActorType  string
	ActorID    string
	Action     string
	Resource   string
	ResourceID string
	Outcome    AuditOutcome
	Metadata   json.RawMessage
	CreatedAt  time.Time
}

type UsageBucket struct {
	ProjectID     ProjectID
	BucketStart   time.Time
	Entries       int64
	Bytes         int64
	FailedBatches int64
}

type RetentionPolicy struct {
	ProjectID ProjectID
	MaxAge    time.Duration
	MaxBytes  *int64
	Enabled   bool
	UpdatedAt time.Time
}
