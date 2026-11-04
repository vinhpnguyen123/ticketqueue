package config

import "testing"

func TestLoadTrustedProxyHops(t *testing.T) {
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
	for _, v := range []string{"one", "-1"} {
		t.Setenv("TRUSTED_PROXY_HOPS", v)
		if _, err := Load(); err == nil {
			t.Errorf("TRUSTED_PROXY_HOPS=%q: Load returned no error", v)
		}
	}
}
