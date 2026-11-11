package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
	"github.com/carder-coder/ai-agent/gateway/internal/cost"
)

func TestBudgetPreflightRejectsOversizedRequestsAndWritesTrace(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		_, _ = io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	tests := []struct {
		name         string
		dailyLimit   float64
		requestLimit float64
		wantCode     string
	}{
		{name: "per-request limit", dailyLimit: 1, requestLimit: 0.0005, wantCode: "request_cost_limit_exceeded"},
		{name: "daily budget", dailyLimit: 0.0005, requestLimit: 0.5, wantCode: "daily_budget_exceeded"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			traceDir := t.TempDir()
			handler := newTestRouter(t, config.Config{
				APIKey:                "test-key",
				Model:                 "chat",
				BaseURL:               upstream.URL,
				TraceDir:              traceDir,
				DailyBudgetYuan:       test.dailyLimit,
				RequestMaxCostYuan:    test.requestLimit,
				BudgetMaxOutputTokens: 1,
				PriceOut:              1,
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
				`{"model":"chat","messages":[{"role":"user","content":"hi"}],"max_tokens":1000}`,
			))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			var envelope errorEnvelope
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if envelope.Error.Code != test.wantCode {
				t.Fatalf("error code = %q, want %q", envelope.Error.Code, test.wantCode)
			}
			tracePath := filepath.Join(traceDir, response.Header().Get("X-Trace-ID")+".jsonl")
			traceBody, err := os.ReadFile(tracePath)
			if err != nil {
				t.Fatalf("read rejection trace: %v", err)
			}
			if !strings.Contains(string(traceBody), `"kind":"budget_rejected"`) ||
				!strings.Contains(string(traceBody), test.wantCode) {
				t.Fatalf("trace missing budget rejection: %s", traceBody)
			}
		})
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0 for rejected requests", upstreamCalls.Load())
	}
}

func TestPrepareBudgetEstimateAddsConfiguredDefaultOutputCap(t *testing.T) {
	fields := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"user","content":"hi"}]`),
	}
	if _, err := prepareBudgetEstimate(fields, 128, cost.Prices{Output: 1}); err != nil {
		t.Fatalf("prepareBudgetEstimate() error = %v", err)
	}
	var limit int
	if err := json.Unmarshal(fields["max_tokens"], &limit); err != nil {
		t.Fatalf("decode default max_tokens: %v", err)
	}
	if limit != 128 {
		t.Fatalf("max_tokens = %d, want 128", limit)
	}
}

func TestMissingProviderUsageChargesReservedEstimate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()

	statePath := filepath.Join(t.TempDir(), "budget.json")
	handler := newTestRouter(t, config.Config{
		APIKey:             "test-key",
		Model:              "chat",
		BaseURL:            upstream.URL,
		TraceDir:           t.TempDir(),
		BudgetStateFile:    statePath,
		DailyBudgetYuan:    1,
		RequestMaxCostYuan: 0.5,
		PriceIn:            1,
		PriceOut:           1,
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"chat","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`,
	))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}

	stateBody, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read budget state: %v", err)
	}
	var state struct {
		SpentUnits int64 `json:"spent_units"`
	}
	if err := json.Unmarshal(stateBody, &state); err != nil {
		t.Fatalf("decode budget state: %v", err)
	}
	if state.SpentUnits == 0 {
		t.Fatal("request without provider usage was recorded as zero cost")
	}
}
