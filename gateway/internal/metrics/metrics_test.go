// 本文件验证请求计数、错误计数、延迟和首字节指标的输出。
package metrics

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsExposeRequestsErrorsAndLatency(t *testing.T) {
	metrics := New()
	metrics.observe(120*time.Millisecond, 20*time.Millisecond, true, http.StatusOK)
	metrics.observe(2*time.Second, 0, false, http.StatusBadGateway)

	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, metric := range []string{
		"gateway_requests_per_second ",
		"gateway_requests_total 2",
		"gateway_errors_total 1",
		"gateway_request_duration_seconds_count 2",
		"gateway_first_byte_seconds_count 1",
	} {
		if !strings.Contains(body, metric) {
			t.Errorf("metrics output missing %q:\n%s", metric, body)
		}
	}
	if response.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}
}

func TestCostReportIncludesUnitCostModelSharesAndCacheRatio(t *testing.T) {
	metrics := New()
	metrics.ObserveModel("economy", time.Second, 100, 50, 0, 0.03, true)
	metrics.ObserveModel("main", time.Second, 100, 50, 0, 0.07, true)
	metrics.ObserveCacheLookup(false)
	metrics.ObserveCacheLookup(false)
	metrics.ObserveCacheLookup(false)
	metrics.ObserveCacheLookup(true)

	report := metrics.CostReport()
	if report.ConversationCount != 2 || report.TotalCostYuan != 0.1 ||
		math.Abs(report.CostPer1000ConversationsYuan-50) > 1e-9 ||
		math.Abs(report.SemanticCacheHitRatio-0.25) > 1e-9 {
		t.Fatalf("unexpected cost report totals: %+v", report)
	}
	if len(report.ModelCostShares) != 2 ||
		report.ModelCostShares[0].Model != "economy" ||
		math.Abs(report.ModelCostShares[0].CostShare-0.3) > 1e-9 ||
		report.ModelCostShares[1].Model != "main" ||
		math.Abs(report.ModelCostShares[1].CostShare-0.7) > 1e-9 {
		t.Fatalf("unexpected model cost shares: %+v", report.ModelCostShares)
	}
}

func TestCostReportUsesZeroForAnEmptyWindow(t *testing.T) {
	report := New().CostReport()
	if report.ConversationCount != 0 || report.TotalCostYuan != 0 ||
		report.CostPer1000ConversationsYuan != 0 || report.SemanticCacheHitRatio != 0 ||
		len(report.ModelCostShares) != 0 {
		t.Fatalf("unexpected empty cost report: %+v", report)
	}
}

func TestMetricsWrapTracksStatusAndFirstByte(t *testing.T) {
	metrics := New()
	handler := metrics.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad"))
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(response.Body.String(), "gateway_errors_total 1") ||
		!strings.Contains(response.Body.String(), "gateway_first_byte_seconds_count 1") {
		t.Fatalf("metrics did not record the wrapped request:\n%s", response.Body)
	}
}

func TestMetricsExposeModelUsageCostAndLatency(t *testing.T) {
	metrics := New()
	metrics.ObserveModel("economy", 250*time.Millisecond, 120, 30, 20, 0, true)
	metrics.ObserveModel("economy", time.Second, 5, 0, 0, 0, false)

	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, metric := range []string{
		`gateway_model_requests_total{model="economy"} 2`,
		`gateway_model_errors_total{model="economy"} 1`,
		`gateway_model_input_tokens_total{model="economy"} 125`,
		`gateway_model_output_tokens_total{model="economy"} 30`,
		`gateway_model_cached_input_tokens_total{model="economy"} 20`,
		`gateway_model_cost_yuan_total{model="economy"} 0.0000`,
		`gateway_model_request_duration_seconds_count{model="economy"} 2`,
	} {
		if !strings.Contains(body, metric) {
			t.Errorf("metrics output missing %q:\n%s", metric, body)
		}
	}
}

func TestMetricsExposeSemanticCacheHitAndFeedbackCounts(t *testing.T) {
	metrics := New()
	metrics.ObserveCacheLookup(false)
	metrics.ObserveCacheLookup(true)
	metrics.ObserveCacheFeedback(true)
	metrics.ObserveCacheFeedback(false)

	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, metric := range []string{
		"gateway_semantic_cache_lookups_total 2",
		"gateway_semantic_cache_hits_total 1",
		`gateway_semantic_cache_feedback_total{result="correct"} 1`,
		`gateway_semantic_cache_feedback_total{result="incorrect"} 1`,
		"gateway_semantic_cache_hit_ratio 0.500000",
		"gateway_semantic_cache_incorrect_feedback_ratio 0.500000",
	} {
		if !strings.Contains(body, metric) {
			t.Errorf("metrics output missing %q:\n%s", metric, body)
		}
	}
}
