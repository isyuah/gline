package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr         string
	DatabaseURL      string
	BootstrapToken   string
	APIKeyPepper     string
	AllowedOrigins   []string
	ShutdownTimeout  time.Duration
	DatabaseTimeout  time.Duration
	MaxRequestBytes  int64
	QueryMaxRange    time.Duration
	QueryTimeout     time.Duration
	QueryMaxPageSize int
	QueryConcurrency int
	MaintenanceEvery time.Duration
	AgentStaleAfter  time.Duration
	RetentionBatch   int
}

func FromEnv() (Config, error) {
	cfg := Config{
		HTTPAddr:         envOr("GLINE_HTTP_ADDR", ":8080"),
		DatabaseURL:      strings.TrimSpace(os.Getenv("GLINE_DATABASE_URL")),
		BootstrapToken:   strings.TrimSpace(os.Getenv("GLINE_BOOTSTRAP_TOKEN")),
		APIKeyPepper:     strings.TrimSpace(os.Getenv("GLINE_API_KEY_PEPPER")),
		AllowedOrigins:   splitCSV(os.Getenv("GLINE_ALLOWED_ORIGINS")),
		ShutdownTimeout:  15 * time.Second,
		DatabaseTimeout:  10 * time.Second,
		MaxRequestBytes:  8 << 20,
		QueryMaxRange:    7 * 24 * time.Hour,
		QueryTimeout:     10 * time.Second,
		QueryMaxPageSize: 500,
		QueryConcurrency: 8,
		MaintenanceEvery: time.Minute,
		AgentStaleAfter:  2 * time.Minute,
		RetentionBatch:   1000,
	}

	var err error
	if cfg.ShutdownTimeout, err = durationEnv("GLINE_SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseTimeout, err = durationEnv("GLINE_DATABASE_TIMEOUT", cfg.DatabaseTimeout); err != nil {
		return Config{}, err
	}
	if cfg.QueryMaxRange, err = durationEnv("GLINE_QUERY_MAX_RANGE", cfg.QueryMaxRange); err != nil {
		return Config{}, err
	}
	if cfg.QueryTimeout, err = durationEnv("GLINE_QUERY_TIMEOUT", cfg.QueryTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxRequestBytes, err = int64Env("GLINE_MAX_REQUEST_BYTES", cfg.MaxRequestBytes); err != nil {
		return Config{}, err
	}
	if pageSize, parseErr := int64Env("GLINE_QUERY_MAX_PAGE_SIZE", int64(cfg.QueryMaxPageSize)); parseErr != nil {
		return Config{}, parseErr
	} else {
		cfg.QueryMaxPageSize = int(pageSize)
	}
	if concurrency, parseErr := int64Env("GLINE_QUERY_CONCURRENCY", int64(cfg.QueryConcurrency)); parseErr != nil {
		return Config{}, parseErr
	} else {
		cfg.QueryConcurrency = int(concurrency)
	}
	if cfg.MaintenanceEvery, err = durationEnv("GLINE_MAINTENANCE_INTERVAL", cfg.MaintenanceEvery); err != nil {
		return Config{}, err
	}
	if cfg.AgentStaleAfter, err = durationEnv("GLINE_AGENT_STALE_AFTER", cfg.AgentStaleAfter); err != nil {
		return Config{}, err
	}
	if retentionBatch, parseErr := int64Env("GLINE_RETENTION_BATCH_SIZE", int64(cfg.RetentionBatch)); parseErr != nil {
		return Config{}, parseErr
	} else {
		cfg.RetentionBatch = int(retentionBatch)
	}

	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("GLINE_DATABASE_URL is required")
	}
	if len(cfg.BootstrapToken) < 24 {
		return Config{}, errors.New("GLINE_BOOTSTRAP_TOKEN must contain at least 24 characters")
	}
	if len(cfg.APIKeyPepper) < 24 {
		return Config{}, errors.New("GLINE_API_KEY_PEPPER must contain at least 24 characters")
	}
	if cfg.MaxRequestBytes <= 0 || cfg.QueryMaxPageSize <= 0 || cfg.QueryMaxPageSize > 1000 ||
		cfg.QueryConcurrency <= 0 || cfg.QueryConcurrency > 1024 || cfg.RetentionBatch <= 0 || cfg.RetentionBatch > 10_000 ||
		cfg.AgentStaleAfter <= cfg.MaintenanceEvery {
		return Config{}, errors.New("request and query limits must be positive and bounded")
	}
	return cfg, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(part); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return parsed, nil
}

func int64Env(name string, fallback int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}
