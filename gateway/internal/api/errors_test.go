// 本文件验证聊天接口的输入校验、响应转换和基础路由行为。
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
)

func TestChatValidatesRequiredFields(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{name: "invalid JSON", body: `{"model":`, wantMessage: "body: must be a JSON object"},
		{name: "missing model", body: `{"messages":[{"role":"user","content":"hi"}]}`, wantMessage: "model: is required"},
		{name: "missing messages", body: `{"model":"chat"}`, wantMessage: "messages: is required"},
		{name: "invalid role", body: `{"model":"chat","messages":[{"role":"robot","content":"hi"}]}`, wantMessage: "messages[0].role: unsupported role"},
		{name: "invalid content type", body: `{"model":"chat","messages":[{"role":"user","content":12}]}`, wantMessage: "messages[0].content: must be a string or array"},
		{name: "invalid task type", body: `{"model":"chat","messages":[{"role":"user","content":"hi"}],"task_type":"unknown"}`, wantMessage: "task_type: must be simple or complex"},
	}

	handler := newTestRouter(t, config.Config{})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", response.Code, response.Body)
			}
			var envelope errorEnvelope
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("invalid error JSON: %v", err)
			}
			if !strings.Contains(envelope.Error.Message, test.wantMessage) {
				t.Fatalf("message = %q, want it to contain %q", envelope.Error.Message, test.wantMessage)
			}
			if envelope.Error.Type != "invalid_request_error" || envelope.Error.Code != "invalid_request" {
				t.Fatalf("unexpected error classification: %+v", envelope.Error)
			}
		})
	}
}

func TestChatRoutesByTaskTypeAndContextLength(t *testing.T) {
	traceDir := t.TempDir()
	var upstreamModel string
	var upstreamHasTaskType bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			return
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode upstream request: %v", err)
			return
		}
		if err := json.Unmarshal(payload["model"], &upstreamModel); err != nil {
			t.Errorf("decode upstream model: %v", err)
			return
		}
		_, upstreamHasTaskType = payload["task_type"]
		response, err := json.Marshal(map[string]any{"model": upstreamModel, "choices": []any{}})
		if err != nil {
			t.Errorf("encode upstream response: %v", err)
			return
		}
		_, _ = w.Write(response)
	}))
	defer upstream.Close()

	handler := newTestRouter(t, config.Config{
		APIKey:                "test-key",
		BaseURL:               upstream.URL,
		Model:                 "standard",
		TraceDir:              traceDir,
		RouteSimpleModel:      "economy",
		RouteComplexModel:     "premium",
		RouteContextThreshold: 40,
	})
	tests := []struct {
		name      string
		body      string
		wantModel string
		wantRoute string
	}{
		{
			name:      "explicit simple task",
			body:      `{"model":"client","messages":[{"role":"user","content":"hi"}],"task_type":"simple"}`,
			wantModel: "economy",
			wantRoute: `"reason":"simple_task"`,
		},
		{
			name:      "large context takes complex tier",
			body:      `{"model":"client","messages":[{"role":"user","content":"` + strings.Repeat("x", 80) + `"}],"task_type":"simple"}`,
			wantModel: "premium",
			wantRoute: `"reason":"context_threshold"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(
				http.MethodPost, "/v1/chat/completions", strings.NewReader(test.body),
			))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			if upstreamModel != test.wantModel {
				t.Fatalf("upstream model = %q, want %q", upstreamModel, test.wantModel)
			}
			if upstreamHasTaskType {
				t.Fatal("gateway-only task_type was forwarded upstream")
			}
			tracePath := filepath.Join(traceDir, response.Header().Get("X-Trace-ID")+".jsonl")
			trace, err := os.ReadFile(tracePath)
			if err != nil {
				t.Fatalf("read trace: %v", err)
			}
			if !strings.Contains(string(trace), test.wantRoute) {
				t.Fatalf("trace missing route decision %q: %s", test.wantRoute, trace)
			}
		})
	}
}

func TestChatForwardsRequestAndDoesNotPassUpstreamHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		var model string
		if err := json.Unmarshal(payload["model"], &model); err != nil || model != "ep-test" {
			t.Errorf("upstream model = %q, error = %v", model, err)
		}
		if _, ok := payload["temperature"]; !ok {
			t.Error("temperature field was not forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Secret", "must-not-leak")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-test","object":"chat.completion","choices":[]}`)
	}))
	defer upstream.Close()

	cfg := config.Config{
		APIKey:         "test-key",
		Model:          "ep-test",
		BaseURL:        upstream.URL + "/v1",
		RequestTimeout: time.Second,
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"public-model","messages":[{"role":"user","content":"hi"}],"temperature":0.2}`,
	))
	response := httptest.NewRecorder()
	newTestRouter(t, cfg).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}
	if response.Header().Get("X-Upstream-Secret") != "" {
		t.Fatal("upstream response header was forwarded")
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid response JSON: %v", err)
	}
	if result["id"] != "chatcmpl-test" {
		t.Fatalf("response body = %s", response.Body)
	}
}

func TestChatMapsUpstreamUnauthorized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", "Bearer realm=upstream")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid key","type":"authentication_error"}}`)
	}))
	defer upstream.Close()

	cfg := config.Config{APIKey: "bad-key", BaseURL: upstream.URL, RequestTimeout: time.Second}
	body := `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	response := httptest.NewRecorder()
	newTestRouter(t, cfg).ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if response.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("upstream authentication header was forwarded")
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid error JSON: %v", err)
	}
	if envelope.Error.Message != "invalid key" || envelope.Error.Type != "authentication_error" {
		t.Fatalf("unexpected error: %+v", envelope.Error)
	}
	if envelope.Error.Code != "invalid_api_key" {
		t.Fatalf("error code = %q", envelope.Error.Code)
	}
}

func TestChatUsesConfiguredMockProvider(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`,
	))
	response := httptest.NewRecorder()
	newTestRouter(t, config.Config{Provider: "mock"}).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var result struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode mock response: %v", err)
	}
	if result.Model != "public-model" || len(result.Choices) != 1 ||
		result.Choices[0].Message.Content != "mock response" {
		t.Fatalf("unexpected mock response: %+v", result)
	}
}

func TestChatStreamsMockChunksAndRecordsFirstByte(t *testing.T) {
	handler := newTestRouter(t, config.Config{Provider: "mock"})
	body := `{"model":"chat","messages":[{"role":"user","content":"hi"}],"stream":true}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	response := httptest.NewRecorder()
	started := time.Now()
	handler.ServeHTTP(response, request)
	firstByte := time.Since(started)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if firstByte >= 800*time.Millisecond {
		t.Fatalf("mock first byte took %s, want under 800ms", firstByte)
	}
	if response.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}
	streamBody := response.Body.String()
	if !strings.Contains(streamBody, `data: {"id":"chatcmpl-mock"`) ||
		!strings.Contains(streamBody, "data: [DONE]\n\n") {
		t.Fatalf("unexpected stream body: %s", streamBody)
	}

	metricsResponse := httptest.NewRecorder()
	handler.ServeHTTP(metricsResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricsResponse.Body.String(), "gateway_first_byte_seconds_count 1") {
		t.Fatalf("first-byte metric not recorded:\n%s", metricsResponse.Body)
	}
}

func TestNewRouterRejectsUnregisteredProvider(t *testing.T) {
	if _, err := NewRouter(config.Config{Provider: "missing", MaxConcurrent: 1}); err == nil {
		t.Fatal("NewRouter() accepted an unregistered provider")
	}
}

func TestNewRouterRejectsWritesWithoutApprovalToken(t *testing.T) {
	if _, err := NewRouter(config.Config{
		Provider: "mock", ToolsAllowWrites: true, ToolApprovalToken: "short",
	}); err == nil {
		t.Fatal("NewRouter() enabled write tools without an approval token")
	}
}

func newTestRouter(t *testing.T, cfg config.Config) http.Handler {
	t.Helper()
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 200
	}
	if cfg.TraceDir == "" {
		cfg.TraceDir = t.TempDir()
	}
	if cfg.ToolWorkspace == "" {
		cfg.ToolWorkspace = t.TempDir()
	}
	handler, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	return handler
}
