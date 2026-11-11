// 本文件验证请求计数、错误计数、延迟和首字节指标的输出。
package metrics

import (
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
		`gateway_model_cost_yuan_total{model="economy"} 0.000000`,
		`gateway_model_request_duration_seconds_count{model="economy"} 2`,
	} {
		if !strings.Contains(body, metric) {
			t.Errorf("metrics output missing %q:\n%s", metric, body)
		}
	}
}
