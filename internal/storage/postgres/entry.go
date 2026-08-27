package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/isyuah/gline/internal/domain"
)

type EntryRepository struct{ q dbtx }

func (r *EntryRepository) List(ctx context.Context, query domain.EntryQuery) (domain.EntryPage, error) {
	sqlText, args, err := buildEntryQuery(query)
	if err != nil {
		return domain.EntryPage{}, err
	}
	rows, err := r.q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return domain.EntryPage{}, classifyError(err)
	}
	defer rows.Close()

	entries := make([]domain.Entry, 0, query.Limit+1)
	for rows.Next() {
		var entry domain.Entry
		var attributes []byte
		if err := rows.Scan(
			&entry.ID, &entry.ProjectID, &entry.BatchID, &entry.AgentID, &entry.PipelineID,
			&entry.BatchSequence, &entry.Service, &entry.Host, &entry.Level, &entry.Message,
			&entry.ObservedAt, &entry.IngestedAt, &attributes,
		); err != nil {
			return domain.EntryPage{}, classifyError(err)
		}
		if err := json.Unmarshal(attributes, &entry.Attributes); err != nil || entry.Attributes == nil {
			return domain.EntryPage{}, fmt.Errorf("%w: entry attributes", ErrCorruptRow)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return domain.EntryPage{}, classifyError(err)
	}

	page := domain.EntryPage{Entries: entries}
	if len(entries) > query.Limit {
		page.Entries = entries[:query.Limit]
		last := page.Entries[len(page.Entries)-1]
		page.Next = &domain.EntryCursor{ObservedAt: last.ObservedAt, ID: last.ID}
	}
	return page, nil
}

func buildEntryQuery(query domain.EntryQuery) (string, []any, error) {
	if err := query.Validate(0, 500); err != nil {
		return "", nil, err
	}
	args := []any{query.ProjectID, query.From, query.To}
	var where strings.Builder
	where.WriteString("project_id = $1 AND observed_at >= $2 AND observed_at < $3")
	appendIn := func(column string, values []string) {
		if len(values) == 0 {
			return
		}
		where.WriteString(" AND ")
		where.WriteString(column)
		where.WriteString(" IN (")
		for i, value := range values {
			if i > 0 {
				where.WriteByte(',')
			}
			args = append(args, value)
			fmt.Fprintf(&where, "$%d", len(args))
		}
		where.WriteByte(')')
	}
	appendIn("service", query.Services)
	appendIn("host", query.Hosts)
	appendIn("level", query.Levels)
	if query.Message != "" {
		args = append(args, containsPattern(query.Message))
		fmt.Fprintf(&where, " AND message ILIKE $%d ESCAPE '\\'", len(args))
	}
	if query.Cursor != nil {
		args = append(args, query.Cursor.ObservedAt, query.Cursor.ID)
		fmt.Fprintf(&where, " AND (observed_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, query.Limit+1)
	querySQL := `SELECT id, project_id, batch_id, agent_id, pipeline_id,
       batch_sequence, service, host, level, message,
       observed_at, ingested_at, attributes
FROM log_entries
WHERE ` + where.String() + fmt.Sprintf(`
ORDER BY observed_at DESC, id DESC
LIMIT $%d`, len(args))
	return querySQL, args, nil
}

func containsPattern(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + replacer.Replace(value) + "%"
}
