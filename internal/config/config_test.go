package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("SERVER_HOST", "")
	t.Setenv("SERVER_PORT", "")
	t.Setenv("SERVER_BASE_PATH", "")
	t.Setenv("APP_ENV", "")
	t.Setenv("REDIS_URL", "")
	t.Setenv("RATE_LIMIT_WINDOW", "")
	t.Setenv("IDEMPOTENCY_TTL", "")
	t.Setenv("IDEMPOTENCY_LOCK_TTL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Host != "0.0.0.0" || cfg.Port != 8080 || cfg.BasePath != "/v1" || cfg.Environment != "development" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.RedisURL != "" || cfg.RateLimitWindow != time.Minute || cfg.IdempotencyTTL != 24*time.Hour || cfg.IdempotencyLockTTL != 30*time.Second {
		t.Fatalf("unexpected redis/middleware defaults: %+v", cfg)
	}
}

func TestLoadCustomAndValidation(t *testing.T) {
	t.Setenv("REDIS_URL", "redis://localhost:6379/1")
	t.Setenv("RATE_LIMIT_WINDOW", "30s")
	t.Setenv("IDEMPOTENCY_TTL", "12h")
	t.Setenv("IDEMPOTENCY_LOCK_TTL", "15s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RedisURL != "redis://localhost:6379/1" || cfg.RateLimitWindow != 30*time.Second || cfg.IdempotencyTTL != 12*time.Hour || cfg.IdempotencyLockTTL != 15*time.Second {
		t.Fatalf("unexpected custom config: %+v", cfg)
	}

	t.Setenv("RATE_LIMIT_WINDOW", "-5s")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-positive RATE_LIMIT_WINDOW")
	}
}
