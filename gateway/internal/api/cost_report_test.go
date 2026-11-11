package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
)

func TestCostReportEndpointProvidesRequiredFields(t *testing.T) {
	handler := newTestRouter(t, config.Config{
		Provider:      "mock",
		Model:         "mock",
		MaxConcurrent: 1,
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/cost-report", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}

	var report map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode cost report: %v", err)
	}
	for _, field := range []string{
		"conversation_count",
		"total_cost_yuan",
		"cost_per_1000_conversations_yuan",
		"model_cost_shares",
		"semantic_cache_hit_ratio",
	} {
		if _, exists := report[field]; !exists {
			t.Errorf("cost report missing %q: %#v", field, report)
		}
	}
}

func TestCostReportEndpointAllowsOnlyGet(t *testing.T) {
	handler := newTestRouter(t, config.Config{Provider: "mock", MaxConcurrent: 1})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/cost-report", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.Code)
	}
}
