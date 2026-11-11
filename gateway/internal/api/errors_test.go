package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	}

	handler := NewRouter(config.Config{})
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
	NewRouter(cfg).ServeHTTP(response, request)

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
	NewRouter(cfg).ServeHTTP(response, request)

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

func TestChatRejectsStreamingUntilSSEIsImplemented(t *testing.T) {
	body := `{"model":"chat","messages":[{"role":"user","content":"hi"}],"stream":true}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	response := httptest.NewRecorder()
	NewRouter(config.Config{}).ServeHTTP(response, request)

	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), "stream:") {
		t.Fatalf("expected field-level streaming message, got %s", response.Body)
	}
}
