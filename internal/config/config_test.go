package config

import (
	"os"
	"strings"
	"testing"
)

const strongSecret = "0123456789abcdef0123456789abcdef"

func TestLoadTrustedProxyHops(t *testing.T) {
	t.Setenv("JWT_SECRET", strongSecret)
	t.Setenv("TRUSTED_PROXY_HOPS", "1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TrustedProxyHops != 1 {
		t.Errorf("TrustedProxyHops = %d, want 1", cfg.TrustedProxyHops)
	}
}

func TestLoadRejectsInvalidProxyHops(t *testing.T) {
	t.Setenv("JWT_SECRET", strongSecret)
	for _, v := range []string{"one", "-1"} {
		t.Setenv("TRUSTED_PROXY_HOPS", v)
		if _, err := Load(); err == nil {
			t.Errorf("TRUSTED_PROXY_HOPS=%q: Load returned no error", v)
		}
	}
}

// A secret anyone can read in the repo lets anyone sign an admin token.
func TestLoadRejectsWeakJWTSecret(t *testing.T) {
	for name, v := range map[string]string{
		"missing":     "",
		"placeholder": "change-me-in-real-env",
		"short":       strings.Repeat("a", 31),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("JWT_SECRET", v)
			if _, err := Load(); err == nil {
				t.Error("Load accepted the secret")
			}
		})
	}
}

func TestLoadAcceptsStrongJWTSecret(t *testing.T) {
	t.Setenv("JWT_SECRET", strongSecret)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.JWTSecret != strongSecret {
		t.Errorf("JWTSecret = %q", cfg.JWTSecret)
	}
}

// Empty disables the receiver, so empty and unset must both stay empty: a default
// here would be a signing key published in the repo.
func TestWebhookSecretHasNoDefault(t *testing.T) {
	t.Setenv("JWT_SECRET", strongSecret)

	t.Setenv("WEBHOOK_SECRET", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebhookSecret != "" {
		t.Errorf("empty: WebhookSecret = %q", cfg.WebhookSecret)
	}

	os.Unsetenv("WEBHOOK_SECRET") // t.Setenv above restores it after the test
	if cfg, err = Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebhookSecret != "" {
		t.Errorf("unset: WebhookSecret = %q", cfg.WebhookSecret)
	}
}
