package auth

import (
	"errors"
	"fmt"

	"github.com/isyuah/gline/internal/domain"
)

var (
	ErrInvalidCredential = errors.New("invalid api key")
	ErrScopeDenied       = domain.ErrScopeDenied
	ErrProjectMismatch   = errors.New("principal belongs to another project")
	ErrAgentMismatch     = errors.New("api key belongs to another agent")
)

type Principal struct {
	KeyID     domain.APIKeyID
	ProjectID domain.ProjectID
	AgentID   *domain.AgentID
	Scopes    map[domain.Scope]struct{}
}

func NewPrincipal(key domain.APIKey) (Principal, error) {
	if err := key.Validate(); err != nil {
		return Principal{}, err
	}
	principal := Principal{
		KeyID: key.ID, ProjectID: key.ProjectID, AgentID: key.AgentID,
		Scopes: make(map[domain.Scope]struct{}, len(key.Scopes)),
	}
	for _, scope := range key.Scopes {
		principal.Scopes[scope] = struct{}{}
	}
	return principal, nil
}

func (p Principal) Valid() bool {
	if !p.KeyID.Valid() || !p.ProjectID.Valid() || len(p.Scopes) == 0 {
		return false
	}
	if p.AgentID != nil && !p.AgentID.Valid() {
		return false
	}
	for scope := range p.Scopes {
		if !scope.Valid() {
			return false
		}
	}
	return true
}

func (p Principal) Has(scope domain.Scope) bool {
	_, ok := p.Scopes[scope]
	return ok
}

func (p Principal) Require(scope domain.Scope) error {
	if !p.Valid() || !scope.Valid() || !p.Has(scope) {
		return fmt.Errorf("%w: %s", ErrScopeDenied, scope)
	}
	return nil
}

func (p Principal) RequireProject(projectID domain.ProjectID) error {
	if !p.Valid() || !projectID.Valid() || p.ProjectID != projectID {
		return ErrProjectMismatch
	}
	return nil
}

func (p Principal) RequireAgent(agentID domain.AgentID) error {
	if !agentID.Valid() {
		return fmt.Errorf("%w: invalid agent", domain.ErrInvalid)
	}
	if p.AgentID != nil && *p.AgentID != agentID {
		return ErrAgentMismatch
	}
	return nil
}
