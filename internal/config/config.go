package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config encapsulates server runtime settings.
type Config struct {
	Host               string
	Port               int
	BasePath           string
	Environment        string
	RedisURL           string
	RateLimitWindow    time.Duration
	IdempotencyTTL     time.Duration
	IdempotencyLockTTL time.Duration
}

// Load extracts configuration from environment variables with sensible production defaults.
func Load() (*Config, error) {
	cfg := &Config{
		Host:               getEnv("SERVER_HOST", "0.0.0.0"),
		Port:               8080,
		BasePath:           getEnv("SERVER_BASE_PATH", "/v1"),
		Environment:        getEnv("APP_ENV", "development"),
		RedisURL:           getEnv("REDIS_URL", ""),
		RateLimitWindow:    time.Minute,
		IdempotencyTTL:     24 * time.Hour,
		IdempotencyLockTTL: 30 * time.Second,
	}

	if portStr := os.Getenv("SERVER_PORT"); portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("invalid SERVER_PORT value %q: %w", portStr, err)
		}
		cfg.Port = p
	}

	var err error
	if cfg.RateLimitWindow, err = parseDurationEnv("RATE_LIMIT_WINDOW", cfg.RateLimitWindow); err != nil {
		return nil, err
	}
	if cfg.IdempotencyTTL, err = parseDurationEnv("IDEMPOTENCY_TTL", cfg.IdempotencyTTL); err != nil {
		return nil, err
	}
	if cfg.IdempotencyLockTTL, err = parseDurationEnv("IDEMPOTENCY_LOCK_TTL", cfg.IdempotencyLockTTL); err != nil {
		return nil, err
	}

	return cfg, nil
}

func parseDurationEnv(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid %s value %q: must be a positive duration", key, raw)
	}
	return d, nil
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return defaultVal
}
