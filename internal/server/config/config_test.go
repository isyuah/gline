package config

import (
	"testing"
	"time"
)

func TestFromEnvRequiresSecretsAndDatabase(t *testing.T) {
	t.Setenv("GLINE_DATABASE_URL", "")
	t.Setenv("GLINE_BOOTSTRAP_TOKEN", "")
	t.Setenv("GLINE_API_KEY_PEPPER", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv() error = nil, want required configuration error")
	}
}

func TestFromEnvParsesBoundedConfiguration(t *testing.T) {
	t.Setenv("GLINE_DATABASE_URL", "postgres://localhost/gline")
	t.Setenv("GLINE_BOOTSTRAP_TOKEN", "bootstrap-token-with-enough-entropy")
	t.Setenv("GLINE_API_KEY_PEPPER", "api-key-pepper-with-enough-entropy")
	t.Setenv("GLINE_ALLOWED_ORIGINS", "http://localhost:5173, http://localhost:4173")
	t.Setenv("GLINE_MAX_REQUEST_BYTES", "4096")
	t.Setenv("GLINE_INGEST_REQUESTS_PER_MINUTE", "120")
	t.Setenv("GLINE_INGEST_ENTRIES_PER_MINUTE", "24000")
	t.Setenv("GLINE_INGEST_BYTES_PER_MINUTE", "1048576")
	t.Setenv("GLINE_INGEST_MAX_INFLIGHT", "4")
	t.Setenv("GLINE_QUERY_TIMEOUT", "3s")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxRequestBytes != 4096 || cfg.QueryTimeout != 3*time.Second || len(cfg.AllowedOrigins) != 2 ||
		cfg.IngestRequestsPerMinute != 120 || cfg.IngestEntriesPerMinute != 24000 ||
		cfg.IngestBytesPerMinute != 1048576 || cfg.IngestMaxInflight != 4 {
		t.Fatalf("config = %+v, want parsed request limit and origins", cfg)
	}
}

func TestFromEnvRejectsAdmissionCapacityBelowOneProtocolBatch(t *testing.T) {
	t.Setenv("GLINE_DATABASE_URL", "postgres://localhost/gline")
	t.Setenv("GLINE_BOOTSTRAP_TOKEN", "bootstrap-token-with-enough-entropy")
	t.Setenv("GLINE_API_KEY_PEPPER", "api-key-pepper-with-enough-entropy")
	t.Setenv("GLINE_INGEST_ENTRIES_PER_MINUTE", "1000")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv() error = nil, want admission capacity error")
	}
}
