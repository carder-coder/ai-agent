// 本文件验证豆包适配器的 SSE 解析、用量提取和响应格式检查。
package doubao

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
)

func TestChatReadsSSEChunksAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := NewClient(config.Config{
		APIKey:         "test-key",
		BaseURL:        server.URL,
		RequestTimeout: time.Second,
	})
	response, err := client.Chat(context.Background(), []byte(`{"model":"test","stream":true}`), true)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if response.Stream == nil || response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected response: %+v", response)
	}
	defer response.Stream.Close()

	first, err := response.Stream.Next()
	if err != nil || !first.Content || !strings.Contains(string(first.Data), "hello") {
		t.Fatalf("first chunk = %+v, error = %v", first, err)
	}
	if _, err := response.Stream.Next(); err != nil {
		t.Fatalf("usage chunk error = %v", err)
	}
	if usage := response.Stream.Usage(); usage.TotalTokens != 6 || usage.InputTokens != 4 {
		t.Fatalf("usage = %+v", usage)
	}
	done, err := response.Stream.Next()
	if err != nil || !done.Done || string(done.Data) != "[DONE]" {
		t.Fatalf("done chunk = %+v, error = %v", done, err)
	}
	if _, err := response.Stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() error = %v, want EOF", err)
	}
}

func TestChatRejectsSuccessfulResponseWithoutSSEContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer server.Close()

	client := NewClient(config.Config{APIKey: "test-key", BaseURL: server.URL})
	if _, err := client.Chat(context.Background(), []byte(`{"model":"test"}`), true); err == nil {
		t.Fatal("Chat() accepted a non-SSE streaming response")
	}
}
