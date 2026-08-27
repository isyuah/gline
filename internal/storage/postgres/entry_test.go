package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/isyuah/gline/internal/domain"
)

func TestBuildEntryQueryAlwaysScopesAndUsesKeyset(t *testing.T) {
	query := domain.EntryQuery{
		ProjectID: "11111111-1111-1111-1111-111111111111",
		From:      time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC),
		Services:  []string{"api"}, Levels: []string{"ERROR"}, Message: `%_\`,
		Cursor: &domain.EntryCursor{ObservedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), ID: 42},
		Limit:  100,
	}
	sqlText, args, err := buildEntryQuery(query)
	if err != nil {
		t.Fatalf("buildEntryQuery() error = %v", err)
	}
	for _, want := range []string{
		"project_id = $1", "observed_at >= $2", "observed_at < $3",
		"(observed_at, id) <", "ORDER BY observed_at DESC, id DESC",
	} {
		if !strings.Contains(sqlText, want) {
			t.Fatalf("query missing %q:\n%s", want, sqlText)
		}
	}
	if strings.Contains(strings.ToUpper(sqlText), "OFFSET") {
		t.Fatalf("keyset query contains OFFSET:\n%s", sqlText)
	}
	if got := args[len(args)-1]; got != 101 {
		t.Fatalf("fetch limit = %v, want 101", got)
	}
	if got, want := containsPattern(`%_\`), `%\%\_\\%`; got != want {
		t.Fatalf("containsPattern() = %q, want %q", got, want)
	}
}
