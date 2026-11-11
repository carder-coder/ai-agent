package config

import (
	"testing"
	"time"
)

func TestLoadUsesGatewayDefaults(t *testing.T) {
	t.Setenv("GATEWAY_PORT", "")
	t.Setenv("GATEWAY_MAX_CONCURRENCY", "")
	t.Setenv("GATEWAY_MAX_RETRIES", "")
	t.Setenv("GATEWAY_TIMEOUT", "")
	t.Setenv("ARK_PRICE_IN", "")
	t.Setenv("ARK_PRICE_OUT", "")
	t.Setenv("ARK_PRICE_CACHED", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Port != 8080 || cfg.Address() != ":8080" {
		t.Fatalf("unexpected address: %s", cfg.Address())
	}
	if cfg.MaxConcurrent != 200 || cfg.MaxRetries != 2 || cfg.RequestTimeout != 30*time.Second {
		t.Fatalf("unexpected request limits: %+v", cfg)
	}
	if cfg.PriceIn != 0.8 || cfg.PriceOut != 3.2 || cfg.PriceCached != 0.016 {
		t.Fatalf("unexpected default prices: %+v", cfg)
	}
}

func TestLoadRejectsInvalidPort(t *testing.T) {
	t.Setenv("GATEWAY_PORT", "70000")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted an invalid port")
	}
}

func TestLoadUsesEnvironmentOverrides(t *testing.T) {
	t.Setenv("GATEWAY_PORT", "9090")
	t.Setenv("GATEWAY_MAX_CONCURRENCY", "50")
	t.Setenv("GATEWAY_MAX_RETRIES", "0")
	t.Setenv("GATEWAY_TIMEOUT", "2s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Address() != ":9090" || cfg.MaxConcurrent != 50 || cfg.MaxRetries != 0 ||
		cfg.RequestTimeout != 2*time.Second {
		t.Fatalf("environment overrides not applied: %+v", cfg)
	}
}
