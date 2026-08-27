package migrations

import (
	"io/fs"
	"strings"
	"testing"
)

func TestEmbeddedSchemaProtectsTenantAndPaginationContracts(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	var schema strings.Builder
	var count int
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		body, err := fs.ReadFile(FS, entry.Name())
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", entry.Name(), err)
		}
		schema.Write(body)
		count++
	}
	if count != 5 {
		t.Fatalf("migration count = %d, want 5", count)
	}
	for _, contract := range []string{
		"PRIMARY KEY (project_id, id)",
		"FOREIGN KEY (project_id, agent_id, pipeline_id)",
		"UNIQUE (project_id, batch_id, batch_sequence)",
		"ON log_entries (project_id, observed_at DESC, id DESC)",
		"CHECK (octet_length(payload_hash) = 32)",
	} {
		if !strings.Contains(schema.String(), contract) {
			t.Fatalf("schema missing stable contract %q", contract)
		}
	}
}
