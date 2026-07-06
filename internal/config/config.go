// Package config loads application configuration from environment variables.
package config

import "os"

// Config holds all runtime configuration for ticketqueue services.
type Config struct {
	DatabaseURL string
	RedisAddr   string
	Port        string
	JWTSecret   string
}

// Load reads configuration from the environment, applying local-dev
// defaults for anything unset. Once we run in real environments,
// JWT_SECRET must be set explicitly and Load will fail fast if it is
// empty; for now local dev gets a default.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL: getenv("DATABASE_URL", "postgres://ticketqueue:localdev@localhost:5434/ticketqueue?sslmode=disable"),
		RedisAddr:   getenv("REDIS_ADDR", "localhost:6379"),
		Port:        getenv("PORT", "8080"),
		JWTSecret:   getenv("JWT_SECRET", "change-me-in-real-env"),
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
