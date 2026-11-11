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

func TestChatMapsUpstreamRateLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	defer upstream.Close()

	cfg := config.Config{
		APIKey:         "test-key",
		BaseURL:        upstream.URL,
		RequestTimeout: time.Second,
		MaxRetries:     0,
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`,
	))
	response := httptest.NewRecorder()
	newTestRouter(t, cfg).ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "3" {
		t.Fatalf("status = %d, Retry-After = %q", response.Code, response.Header().Get("Retry-After"))
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid error JSON: %v", err)
	}
	if envelope.Error.Type != "rate_limit_error" {
		t.Fatalf("error type = %q", envelope.Error.Type)
	}
}

func TestChatMapsUpstreamTimeoutToGatewayTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer upstream.Close()

	cfg := config.Config{
		APIKey:         "test-key",
		BaseURL:        upstream.URL,
		RequestTimeout: 20 * time.Millisecond,
		MaxRetries:     0,
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`,
	))
	response := httptest.NewRecorder()
	newTestRouter(t, cfg).ServeHTTP(response, request)

	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid error JSON: %v", err)
	}
	if envelope.Error.Type != "timeout" || envelope.Error.Code != "upstream_timeout" {
		t.Fatalf("unexpected timeout error: %+v", envelope.Error)
	}
}

func TestChatOpensCircuitAfterRepeatedUpstreamFailures(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"unavailable"}}`)
	}))
	defer upstream.Close()
	handler := newTestRouter(t, config.Config{
		APIKey:         "test-key",
		BaseURL:        upstream.URL,
		RequestTimeout: time.Second,
		MaxRetries:     0,
	})
	body := `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 5; i++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("failure %d status = %d, body = %s", i+1, response.Code, response.Body)
		}
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || calls.Load() != 5 {
		t.Fatalf("status = %d, upstream calls = %d", response.Code, calls.Load())
	}
}
