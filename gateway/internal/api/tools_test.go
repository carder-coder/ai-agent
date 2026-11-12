package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
)

func TestToolExecuteRejectsInvalidArgumentsWithFieldDetails(t *testing.T) {
	handler := newTestRouter(t, config.Config{Provider: "mock"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/execute",
		strings.NewReader(`{"tool":"inspect_text","arguments":{"text":12,"mode":"shell","extra":true}}`),
	))

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", response.Code, response.Body)
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if envelope.Error.Code != "invalid_tool_arguments" || len(envelope.Error.Details) != 3 {
		t.Fatalf("unexpected validation response: %+v", envelope.Error)
	}
	if envelope.Error.Details[0].Field != "arguments.extra" ||
		envelope.Error.Details[1].Field != "arguments.mode" ||
		envelope.Error.Details[2].Field != "arguments.text" {
		t.Fatalf("details are not deterministic and field-scoped: %+v", envelope.Error.Details)
	}
}

func TestToolExecuteReturnsTraceIDAndResult(t *testing.T) {
	handler := newTestRouter(t, config.Config{Provider: "mock"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/execute",
		strings.NewReader(`{"tool":"inspect_text","arguments":{"text":"hello world"}}`),
	))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body)
	}
	var result struct {
		TraceID string         `json:"trace_id"`
		Tool    string         `json:"tool"`
		Result  map[string]int `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.TraceID == "" || result.TraceID != response.Header().Get("X-Trace-ID") ||
		result.Tool != "inspect_text" || result.Result["characters"] != 11 || result.Result["words"] != 2 {
		t.Fatalf("unexpected tool response: %+v", result)
	}
}

func TestToolExecuteRejectsUnknownToolsAndUnknownRequestFields(t *testing.T) {
	handler := newTestRouter(t, config.Config{Provider: "mock"})
	for _, body := range []string{
		`{"tool":"unknown","arguments":{}}`,
		`{"tool":"inspect_text","arguments":{"text":"ok"},"command":"whoami"}`,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(
			http.MethodPost, "/v1/tools/execute", strings.NewReader(body),
		))
		if response.Code != http.StatusNotFound && response.Code != http.StatusBadRequest {
			t.Fatalf("body=%s returned %d, want unknown tool or invalid request", body, response.Code)
		}
	}
}
