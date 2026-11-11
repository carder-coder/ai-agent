package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
)

func TestSemanticCacheHitUsageAndFeedback(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-source","model":"chat","choices":[{"message":{"role":"assistant","content":"cached answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`)
	}))
	defer upstream.Close()

	handler := newTestRouter(t, config.Config{
		APIKey:                 "test-key",
		Model:                  "chat",
		BaseURL:                upstream.URL,
		RequestTimeout:         time.Second,
		TraceDir:               t.TempDir(),
		PriceIn:                0.8,
		PriceOut:               3.2,
		PriceCached:            0.016,
		SemanticCacheThreshold: 0.95,
	})
	body := `{"model":"client","messages":[{"role":"user","content":"请总结人工智能技术在教育领域的主要优势与实际风险"}]}`
	responses := make([]*httptest.ResponseRecorder, 0, 3)
	for range 3 {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
		responses = append(responses, response)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want one miss only", calls.Load())
	}
	if responses[0].Header().Get("X-Semantic-Cache") != "MISS" ||
		responses[1].Header().Get("X-Semantic-Cache") != "HIT" ||
		responses[2].Header().Get("X-Semantic-Cache") != "HIT" {
		t.Fatalf("unexpected cache headers: %q, %q, %q",
			responses[0].Header().Get("X-Semantic-Cache"),
			responses[1].Header().Get("X-Semantic-Cache"),
			responses[2].Header().Get("X-Semantic-Cache"))
	}
	var cached struct {
		ID    string `json:"id"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			OutputTokens int `json:"completion_tokens"`
			TotalTokens  int `json:"total_tokens"`
			Details      struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(responses[1].Body.Bytes(), &cached); err != nil {
		t.Fatalf("decode cache response: %v", err)
	}
	if cached.ID == "chatcmpl-source" || cached.Usage.PromptTokens != 10_000 ||
		cached.Usage.OutputTokens != 0 || cached.Usage.TotalTokens != 10_000 ||
		cached.Usage.Details.CachedTokens != 10_000 {
		t.Fatalf("unexpected cached response: %+v", cached)
	}

	for _, feedback := range []struct {
		response *httptest.ResponseRecorder
		correct  bool
	}{
		{response: responses[1], correct: false},
		{response: responses[2], correct: true},
	} {
		body, _ := json.Marshal(map[string]any{
			"trace_id": feedback.response.Header().Get("X-Trace-ID"),
			"correct":  feedback.correct,
		})
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, httptest.NewRequest(
			http.MethodPost, "/v1/cache/feedback", strings.NewReader(string(body)),
		))
		if result.Code != http.StatusOK {
			t.Fatalf("feedback status = %d, body = %s", result.Code, result.Body)
		}
	}
	duplicateBody := `{"trace_id":"` + responses[1].Header().Get("X-Trace-ID") + `","correct":true}`
	duplicate := httptest.NewRecorder()
	handler.ServeHTTP(duplicate, httptest.NewRequest(
		http.MethodPost, "/v1/cache/feedback", strings.NewReader(duplicateBody),
	))
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate feedback status = %d, want 409", duplicate.Code)
	}

	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, metric := range []string{
		"gateway_semantic_cache_lookups_total 3",
		"gateway_semantic_cache_hits_total 2",
		`gateway_semantic_cache_feedback_total{result="correct"} 1`,
		`gateway_semantic_cache_feedback_total{result="incorrect"} 1`,
	} {
		if !strings.Contains(metrics.Body.String(), metric) {
			t.Errorf("metrics missing %q:\n%s", metric, metrics.Body)
		}
	}
}
