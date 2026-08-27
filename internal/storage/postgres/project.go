package postgres

import (
	"context"
	"fmt"

	"github.com/isyuah/gline/internal/domain"
)

type ProjectRepository struct{ q dbtx }

func (r *ProjectRepository) Create(ctx context.Context, project domain.Project) (domain.Project, error) {
	if err := project.Validate(); err != nil {
		return domain.Project{}, err
	}
	row := r.q.QueryRowContext(ctx, `
INSERT INTO projects (id, slug, name, status)
VALUES ($1, $2, $3, $4)
RETURNING id, slug, name, status, created_at, updated_at`,
		project.ID, project.Slug, project.Name, project.Status)
	return scanProject(row)
}

func (r *ProjectRepository) Get(ctx context.Context, id domain.ProjectID) (domain.Project, error) {
	if err := domain.ValidateProjectID(id); err != nil {
		return domain.Project{}, err
	}
	return scanProject(r.q.QueryRowContext(ctx, `
SELECT id, slug, name, status, created_at, updated_at
FROM projects WHERE id = $1`, id))
}

func (r *ProjectRepository) GetBySlug(ctx context.Context, slug string) (domain.Project, error) {
	return scanProject(r.q.QueryRowContext(ctx, `
SELECT id, slug, name, status, created_at, updated_at
FROM projects WHERE slug = $1`, slug))
}

func (r *ProjectRepository) List(ctx context.Context, limit int) ([]domain.Project, error) {
	if limit <= 0 || limit > 1000 {
		return nil, fmt.Errorf("%w: project list limit", domain.ErrInvalid)
	}
	rows, err := r.q.QueryContext(ctx, `
SELECT id, slug, name, status, created_at, updated_at
FROM projects ORDER BY created_at, id LIMIT $1`, limit)
	if err != nil {
		return nil, classifyError(err)
	}
	defer rows.Close()
	projects := make([]domain.Project, 0)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, classifyError(rows.Err())
}

func (r *ProjectRepository) SetStatus(ctx context.Context, id domain.ProjectID, status domain.ProjectStatus) (domain.Project, error) {
	if !id.Valid() || !status.Valid() {
		return domain.Project{}, fmt.Errorf("%w: project status update", domain.ErrInvalid)
	}
	return scanProject(r.q.QueryRowContext(ctx, `
UPDATE projects SET status = $2, updated_at = now()
WHERE id = $1
RETURNING id, slug, name, status, created_at, updated_at`, id, status))
}

func scanProject(row interface{ Scan(...any) error }) (domain.Project, error) {
	var p domain.Project
	if err := row.Scan(&p.ID, &p.Slug, &p.Name, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return domain.Project{}, classifyError(err)
	}
	if err := p.Validate(); err != nil {
		return domain.Project{}, fmt.Errorf("%w: %v", ErrCorruptRow, err)
	}
	return p, nil
}
