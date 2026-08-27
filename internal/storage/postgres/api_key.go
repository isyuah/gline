package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

type APIKeyRepository struct{ q dbtx }

func (r *APIKeyRepository) Create(ctx context.Context, key domain.APIKey) (domain.APIKey, error) {
	if err := key.Validate(); err != nil {
		return domain.APIKey{}, err
	}
	scopes, err := json.Marshal(key.Scopes)
	if err != nil {
		return domain.APIKey{}, fmt.Errorf("encode api key scopes: %w", err)
	}
	return scanAPIKey(r.q.QueryRowContext(ctx, `
INSERT INTO api_keys (id, project_id, agent_id, name, prefix, secret_hash, scopes, status, expires_at)
VALUES ($1, $2, $3, $4, $5, $6,
		ARRAY(SELECT jsonb_array_elements_text($7::jsonb)), $8, $9)
RETURNING id, project_id, agent_id, name, prefix, secret_hash,
          array_to_json(scopes), status, expires_at, last_used_at, created_at, revoked_at`,
		key.ID, key.ProjectID, key.AgentID, key.Name, key.Prefix, key.SecretHash, scopes, key.Status, key.ExpiresAt))
}

// FindActiveByPrefix returns candidates because prefixes are unique only inside
// a Project. The authentication service must constant-time compare their hashes.
func (r *APIKeyRepository) FindActiveByPrefix(ctx context.Context, prefix string, now time.Time) ([]domain.APIKey, error) {
	rows, err := r.q.QueryContext(ctx, `
SELECT k.id, k.project_id, k.agent_id, k.name, k.prefix, k.secret_hash,
       array_to_json(k.scopes), k.status, k.expires_at, k.last_used_at, k.created_at, k.revoked_at
FROM api_keys k
JOIN projects p ON p.id = k.project_id
WHERE k.prefix = $1 AND k.status = 'active'
  AND (k.expires_at IS NULL OR k.expires_at > $2)
  AND p.status = 'active'`, prefix, now)
	if err != nil {
		return nil, classifyError(err)
	}
	defer rows.Close()

	var result []domain.APIKey
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, key)
	}
	return result, classifyError(rows.Err())
}

func (r *APIKeyRepository) List(ctx context.Context, projectID domain.ProjectID, limit int) ([]domain.APIKey, error) {
	if !projectID.Valid() || limit <= 0 || limit > 1000 {
		return nil, fmt.Errorf("%w: api key list", domain.ErrInvalid)
	}
	rows, err := r.q.QueryContext(ctx, `
SELECT id, project_id, agent_id, name, prefix, secret_hash,
       array_to_json(scopes), status, expires_at, last_used_at, created_at, revoked_at
FROM api_keys WHERE project_id = $1
ORDER BY created_at DESC, id DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, classifyError(err)
	}
	defer rows.Close()
	keys := make([]domain.APIKey, 0)
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, classifyError(rows.Err())
}

func (r *APIKeyRepository) Revoke(ctx context.Context, projectID domain.ProjectID, keyID domain.APIKeyID, revokedAt time.Time) (domain.APIKey, error) {
	return scanAPIKey(r.q.QueryRowContext(ctx, `
UPDATE api_keys
SET status = 'revoked', revoked_at = COALESCE(revoked_at, $3)
WHERE project_id = $1 AND id = $2 AND status IN ('active', 'revoked')
RETURNING id, project_id, agent_id, name, prefix, secret_hash,
          array_to_json(scopes), status, expires_at, last_used_at, created_at, revoked_at`,
		projectID, keyID, revokedAt))
}

func (r *APIKeyRepository) TouchLastUsed(ctx context.Context, projectID domain.ProjectID, keyID domain.APIKeyID, usedAt time.Time) error {
	result, err := r.q.ExecContext(ctx, `
UPDATE api_keys SET last_used_at = GREATEST(COALESCE(last_used_at, $3), $3)
WHERE project_id = $1 AND id = $2`, projectID, keyID, usedAt)
	if err != nil {
		return classifyError(err)
	}
	return requireAffected(result)
}

func scanAPIKey(row interface{ Scan(...any) error }) (domain.APIKey, error) {
	var key domain.APIKey
	var agentID sql.NullString
	var scopesJSON []byte
	var expiresAt, lastUsedAt, revokedAt sql.NullTime
	if err := row.Scan(
		&key.ID, &key.ProjectID, &agentID, &key.Name, &key.Prefix, &key.SecretHash,
		&scopesJSON, &key.Status, &expiresAt, &lastUsedAt, &key.CreatedAt, &revokedAt,
	); err != nil {
		return domain.APIKey{}, classifyError(err)
	}
	if agentID.Valid {
		id := domain.AgentID(agentID.String)
		key.AgentID = &id
	}
	key.ExpiresAt = nullableTime(expiresAt)
	key.LastUsedAt = nullableTime(lastUsedAt)
	key.RevokedAt = nullableTime(revokedAt)
	if err := json.Unmarshal(scopesJSON, &key.Scopes); err != nil {
		return domain.APIKey{}, fmt.Errorf("%w: decode api key scopes: %v", ErrCorruptRow, err)
	}
	if err := key.Validate(); err != nil {
		return domain.APIKey{}, fmt.Errorf("%w: %v", ErrCorruptRow, err)
	}
	return key, nil
}
