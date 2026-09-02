// Package config loads and validates process configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds every knob the service reads at startup. It is validated once,
// at boot, so a misconfigured process fails immediately instead of at first use.
type Config struct {
	DatabaseURL     string
	HTTPAddr        string
	LogLevel        string
	ShutdownTimeout time.Duration
}

// Load reads configuration from the environment, applying defaults for
// everything except DATABASE_URL, which has no sensible default.
func Load() (Config, error) {
	c := Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		HTTPAddr:        envOr("HTTP_ADDR", ":8080"),
		LogLevel:        envOr("LOG_LEVEL", "info"),
		ShutdownTimeout: 15 * time.Second,
	}

	if v := os.Getenv("SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err)
		}
		c.ShutdownTimeout = d
	}

	if c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// EnvInt reads an integer environment variable, returning fallback when unset.
func EnvInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
