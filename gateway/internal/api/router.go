package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
	"github.com/carder-coder/ai-agent/gateway/internal/metrics"
	"github.com/carder-coder/ai-agent/gateway/internal/provider"
	"github.com/carder-coder/ai-agent/gateway/internal/provider/doubao"
	"github.com/carder-coder/ai-agent/gateway/internal/provider/mock"
	"github.com/carder-coder/ai-agent/gateway/internal/retry"
)

func NewRouter(cfg config.Config) (http.Handler, error) {
	if cfg.MaxConcurrent < 1 {
		return nil, fmt.Errorf("GATEWAY_MAX_CONCURRENCY must be positive")
	}
	registry := provider.NewRegistry()
	if err := registry.Register("doubao", doubao.NewClient(cfg)); err != nil {
		return nil, fmt.Errorf("register doubao provider: %w", err)
	}
	if err := registry.Register("mock", mock.New()); err != nil {
		return nil, fmt.Errorf("register mock provider: %w", err)
	}
	providerName := strings.TrimSpace(cfg.Provider)
	if providerName == "" {
		providerName = "doubao"
	}
	adapter, err := registry.Get(providerName)
	if err != nil {
		return nil, err
	}
	guardedAdapter, err := retry.NewAdapter(adapter, cfg.MaxRetries)
	if err != nil {
		return nil, err
	}

	collector := metrics.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/v1/chat/completions", chatHandler(guardedAdapter, cfg.Model, cfg.TraceDir))
	mux.Handle("/metrics", collector)
	handler := NewIdempotencyCache().Wrap(recoveryMiddleware(mux))
	handler = concurrencyMiddleware(cfg.MaxConcurrent, handler)
	handler = requestLoggingMiddleware(handler)
	return collector.Wrap(handler), nil
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("{\"status\":\"ok\"}\n")); err != nil {
		return
	}
}
