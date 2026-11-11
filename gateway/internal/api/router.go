// 本文件装配 Provider、HTTP 路由、指标采集器和请求中间件。
package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/budget"
	semanticcache "github.com/carder-coder/ai-agent/gateway/internal/cache"
	"github.com/carder-coder/ai-agent/gateway/internal/config"
	"github.com/carder-coder/ai-agent/gateway/internal/cost"
	"github.com/carder-coder/ai-agent/gateway/internal/metrics"
	"github.com/carder-coder/ai-agent/gateway/internal/provider"
	"github.com/carder-coder/ai-agent/gateway/internal/provider/doubao"
	"github.com/carder-coder/ai-agent/gateway/internal/provider/mock"
	"github.com/carder-coder/ai-agent/gateway/internal/retry"
	"github.com/carder-coder/ai-agent/gateway/internal/routing"
)

// NewRouter 根据配置装配 Provider、路由和中间件，返回可直接运行的 HTTP handler。
func NewRouter(cfg config.Config) (http.Handler, error) {
	if cfg.MaxConcurrent < 1 {
		return nil, fmt.Errorf("GATEWAY_MAX_CONCURRENCY must be positive")
	}
	if strings.TrimSpace(cfg.TraceDir) == "" {
		cfg.TraceDir = "traces"
	}
	if cfg.DailyBudgetYuan == 0 {
		cfg.DailyBudgetYuan = config.DefaultDailyBudgetYuan
	}
	if cfg.RequestMaxCostYuan == 0 {
		cfg.RequestMaxCostYuan = config.DefaultRequestMaxCostYuan
	}
	if cfg.BudgetMaxOutputTokens == 0 {
		cfg.BudgetMaxOutputTokens = config.DefaultBudgetMaxOutputTokens
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
	semanticCache := semanticcache.New(cfg.SemanticCacheThreshold, 30*time.Minute)
	statePath := cfg.BudgetStateFile
	if strings.TrimSpace(statePath) == "" {
		statePath = filepath.Join(cfg.TraceDir, "budget.json")
	}
	budgetLedger, err := budget.New(cfg.DailyBudgetYuan, cfg.RequestMaxCostYuan, statePath)
	if err != nil {
		return nil, fmt.Errorf("initialize budget ledger: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/v1/chat/completions", chatHandler(
		guardedAdapter,
		cfg.TraceDir,
		collector,
		cost.Prices{Input: cfg.PriceIn, Output: cfg.PriceOut, Cached: cfg.PriceCached},
		routing.Rules{
			DefaultModel:     cfg.Model,
			SimpleModel:      cfg.RouteSimpleModel,
			ComplexModel:     cfg.RouteComplexModel,
			ContextThreshold: cfg.RouteContextThreshold,
		},
		semanticCache,
		budgetLedger,
		cfg.BudgetMaxOutputTokens,
	))
	mux.HandleFunc("/v1/cache/feedback", cacheFeedbackHandler(semanticCache, collector))
	mux.HandleFunc("/cost-report", costReportHandler(collector))
	mux.Handle("/metrics", collector)
	// 从外到内依次采集指标、注入 trace、限流、处理幂等请求并恢复 panic。
	handler := NewIdempotencyCache().Wrap(recoveryMiddleware(mux))
	handler = concurrencyMiddleware(cfg.MaxConcurrent, handler)
	handler = requestLoggingMiddleware(handler)
	return collector.Wrap(handler), nil
}

// healthHandler 为负载均衡器提供只接受 GET 的存活检查。
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
