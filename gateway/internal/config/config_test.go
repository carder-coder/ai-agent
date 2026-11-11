// 本文件验证配置默认值、环境变量覆盖和非法参数拒绝行为。
package config

import (
	"testing"
	"time"
)

func TestLoadUsesGatewayDefaults(t *testing.T) {
	t.Setenv("GATEWAY_PROVIDER", "")
	t.Setenv("GATEWAY_PORT", "")
	t.Setenv("GATEWAY_MAX_CONCURRENCY", "")
	t.Setenv("GATEWAY_MAX_RETRIES", "")
	t.Setenv("GATEWAY_TIMEOUT", "")
	t.Setenv("ARK_PRICE_IN", "")
	t.Setenv("ARK_PRICE_OUT", "")
	t.Setenv("ARK_PRICE_CACHED", "")
	t.Setenv("GATEWAY_ROUTE_CONTEXT_THRESHOLD", "")
	t.Setenv("GATEWAY_ROUTE_SIMPLE_MODEL", "")
	t.Setenv("GATEWAY_ROUTE_COMPLEX_MODEL", "")
	t.Setenv("ARK_MODEL_MINI", "")
	t.Setenv("ARK_MODEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Port != 8080 || cfg.Address() != ":8080" {
		t.Fatalf("unexpected address: %s", cfg.Address())
	}
	if cfg.Provider != "doubao" {
		t.Fatalf("default provider = %q, want doubao", cfg.Provider)
	}
	if cfg.MaxConcurrent != 200 || cfg.MaxRetries != 3 || cfg.RequestTimeout != 30*time.Second {
		t.Fatalf("unexpected request limits: %+v", cfg)
	}
	if cfg.PriceIn != 0.8 || cfg.PriceOut != 3.2 || cfg.PriceCached != 0.016 {
		t.Fatalf("unexpected default prices: %+v", cfg)
	}
	if cfg.RouteContextThreshold != 8192 {
		t.Fatalf("route context threshold = %d, want 8192", cfg.RouteContextThreshold)
	}
}

func TestLoadRejectsInvalidPort(t *testing.T) {
	t.Setenv("GATEWAY_PORT", "70000")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted an invalid port")
	}
}

func TestLoadRejectsTooManyRetries(t *testing.T) {
	t.Setenv("GATEWAY_MAX_RETRIES", "4")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted more retry attempts than the backoff schedule supports")
	}
}

func TestLoadUsesEnvironmentOverrides(t *testing.T) {
	t.Setenv("GATEWAY_PORT", "9090")
	t.Setenv("GATEWAY_MAX_CONCURRENCY", "50")
	t.Setenv("GATEWAY_MAX_RETRIES", "0")
	t.Setenv("GATEWAY_TIMEOUT", "2s")
	t.Setenv("GATEWAY_ROUTE_SIMPLE_MODEL", "economy")
	t.Setenv("GATEWAY_ROUTE_COMPLEX_MODEL", "premium")
	t.Setenv("GATEWAY_ROUTE_CONTEXT_THRESHOLD", "4096")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Address() != ":9090" || cfg.MaxConcurrent != 50 || cfg.MaxRetries != 0 ||
		cfg.RequestTimeout != 2*time.Second {
		t.Fatalf("environment overrides not applied: %+v", cfg)
	}
	if cfg.RouteSimpleModel != "economy" || cfg.RouteComplexModel != "premium" ||
		cfg.RouteContextThreshold != 4096 {
		t.Fatalf("route overrides not applied: %+v", cfg)
	}
}
