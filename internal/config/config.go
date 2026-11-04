// Package config loads application configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"slices"
	"strconv"
)

// Config holds all runtime configuration for ticketqueue services.
type Config struct {
	DatabaseURL string
	RedisAddr   string
	Port        string
	JWTSecret   string
	PaymentURL  string
	PaymentKey  string
	// MetricsPort is where the worker serves /metrics; the API and payment
	// service use their own main ports.
	MetricsPort string
	// WebhookSecret signs provider webhooks. Empty disables the receiver:
	// unsigned webhooks are worse than no webhooks, and reconciliation already
	// covers everything the webhook does.
	WebhookSecret string
	// TrustedProxyHops is how many proxies in front of the API append to
	// X-Forwarded-For: 0 when clients connect directly, 1 behind the ALB.
	TrustedProxyHops int
}

// publishedJWTSecrets have appeared in this public repo, so they are refused
// like a missing secret: anyone could sign an admin token with them.
var publishedJWTSecrets = []string{
	"change-me-in-real-env",
	"localdev-jwt-secret-not-for-production-0000",
}

// minJWTSecretBytes matches the 256-bit key HS256 expects.
const minJWTSecretBytes = 32

// Load reads configuration from the environment, applying local-dev defaults for
// settings that are harmless to default. Secrets are not among them: JWT_SECRET
// signs admin tokens, so a missing or known value fails startup.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:   getenv("DATABASE_URL", "postgres://ticketqueue:localdev@localhost:5434/ticketqueue?sslmode=disable"),
		RedisAddr:     getenv("REDIS_ADDR", "localhost:6379"),
		Port:          getenv("PORT", "8080"),
		JWTSecret:     os.Getenv("JWT_SECRET"),
		PaymentURL:    getenv("PAYMENT_URL", "http://localhost:8081"),
		PaymentKey:    getenv("PAYMENT_API_KEY", "ps_test_localdev"),
		MetricsPort:   getenv("METRICS_PORT", "9091"),
		WebhookSecret: os.Getenv("WEBHOOK_SECRET"),
	}
	if slices.Contains(publishedJWTSecrets, cfg.JWTSecret) || len(cfg.JWTSecret) < minJWTSecretBytes {
		return Config{}, fmt.Errorf("JWT_SECRET must be set to a private value of at least %d bytes", minJWTSecretBytes)
	}
	hops, err := strconv.Atoi(getenv("TRUSTED_PROXY_HOPS", "0"))
	if err != nil || hops < 0 {
		return Config{}, fmt.Errorf("TRUSTED_PROXY_HOPS must be a non-negative integer")
	}
	cfg.TrustedProxyHops = hops
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
