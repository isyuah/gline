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
	t.Setenv("GLINE_QUERY_TIMEOUT", "3s")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxRequestBytes != 4096 || cfg.QueryTimeout != 3*time.Second || len(cfg.AllowedOrigins) != 2 {
		t.Fatalf("config = %+v, want parsed request limit and origins", cfg)
	}
}
