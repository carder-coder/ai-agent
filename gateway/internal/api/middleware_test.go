// 本文件验证请求中间件的幂等、恢复、trace 注入和限流行为。
package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/carder-coder/ai-agent/gateway/internal/trace"
)

func TestIdempotencyReplaysResponseWithoutRepeatingHandler(t *testing.T) {
	var calls atomic.Int32
	handler := NewIdempotencyCache().Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	first := runIdempotentRequest(handler, "key-1", "same body")
	second := runIdempotentRequest(handler, "key-1", "same body")
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("statuses = %d, %d", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() || calls.Load() != 1 {
		t.Fatalf("first=%q second=%q calls=%d", first.Body, second.Body, calls.Load())
	}
	if second.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("duplicate response was not marked as replayed")
	}
}

func TestIdempotencyRejectsReusedKeyWithDifferentRequest(t *testing.T) {
	var calls atomic.Int32
	handler := NewIdempotencyCache().Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))

	first := runIdempotentRequest(handler, "key-1", "body one")
	second := runIdempotentRequest(handler, "key-1", "body two")
	if first.Code != http.StatusOK || second.Code != http.StatusConflict || calls.Load() != 1 {
		t.Fatalf("statuses = %d, %d; calls = %d", first.Code, second.Code, calls.Load())
	}
	if !strings.Contains(second.Body.String(), "idempotency_key_reused") {
		t.Fatalf("unexpected conflict body: %s", second.Body)
	}
}

func TestIdempotencyScopesCacheByAuthorization(t *testing.T) {
	var calls atomic.Int32
	handler := NewIdempotencyCache().Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	for _, authorization := range []string{"Bearer first", "Bearer second"} {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("body"))
		request.Header.Set("Idempotency-Key", "key-1")
		request.Header.Set("Authorization", authorization)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	if calls.Load() != 2 {
		t.Fatalf("handler calls = %d, want 2", calls.Load())
	}
}

func TestConcurrentIdempotentRequestsShareInFlightResult(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	handler := NewIdempotencyCache().Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write([]byte("shared"))
	}))
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() { responses <- runIdempotentRequest(handler, "key-1", "body") }()
	<-started
	go func() { responses <- runIdempotentRequest(handler, "key-1", "body") }()
	close(release)

	first, second := <-responses, <-responses
	if calls.Load() != 1 || first.Body.String() != "shared" || second.Body.String() != "shared" {
		t.Fatalf("calls=%d responses=%q,%q", calls.Load(), first.Body, second.Body)
	}
}

func TestIdempotencyDoesNotReinvokeOversizedResponse(t *testing.T) {
	var calls atomic.Int32
	responseBody := strings.Repeat("x", maxIdempotencyBodyBytes+1)
	handler := NewIdempotencyCache().Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, responseBody)
	}))

	first := runIdempotentRequest(handler, "key-1", "body")
	second := runIdempotentRequest(handler, "key-1", "body")
	if first.Code != http.StatusOK || second.Code != http.StatusConflict || calls.Load() != 1 {
		t.Fatalf("statuses = %d, %d; calls = %d", first.Code, second.Code, calls.Load())
	}
}

func TestIdempotencyReplaysCompletedStream(t *testing.T) {
	var calls atomic.Int32
	handler := NewIdempotencyCache().Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: first\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	first := runIdempotentRequest(handler, "key-1", "same")
	second := runIdempotentRequest(handler, "key-1", "same")
	if calls.Load() != 1 || first.Body.String() != second.Body.String() ||
		second.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("calls=%d first=%q second=%q", calls.Load(), first.Body, second.Body)
	}
}

func TestRecoveryMiddlewareConvertsPanicToOpenAIError(t *testing.T) {
	handler := recoveryMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("test panic")
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusInternalServerError ||
		!strings.Contains(response.Body.String(), `"type":"server_error"`) {
		t.Fatalf("unexpected recovery response: status=%d body=%s", response.Code, response.Body)
	}
}

func TestRequestLoggingInjectsTraceID(t *testing.T) {
	var contextID string
	handler := requestLoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contextID = trace.IDFromContext(r.Context())
		_, _ = w.Write([]byte("ok"))
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	headerID := response.Header().Get("X-Trace-ID")
	if contextID == "" || contextID != headerID || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(headerID) {
		t.Fatalf("context trace ID = %q, response trace ID = %q", contextID, headerID)
	}
}

func TestConcurrencyMiddlewareRejectsExcessRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := concurrencyMiddleware(1, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte("ok"))
	}))
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/", nil))
		close(done)
	}()
	<-started

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	if second.Code != http.StatusServiceUnavailable || !strings.Contains(second.Body.String(), "concurrency_limit") {
		t.Fatalf("unexpected overload response: status=%d body=%s", second.Code, second.Body)
	}
	close(release)
	<-done
	if first.Code != http.StatusOK {
		t.Fatalf("first request status = %d", first.Code)
	}
}

func runIdempotentRequest(handler http.Handler, key, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
