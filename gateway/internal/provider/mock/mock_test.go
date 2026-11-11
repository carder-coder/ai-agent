package mock

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

func TestMockReturnsConfiguredRateLimit(t *testing.T) {
	adapter := NewWithOptions(Options{StatusCode: 429})
	response, err := adapter.Chat(context.Background(), []byte(`{"model":"test"}`), false)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if response.StatusCode != 429 || response.RetryAfter != "1" {
		t.Fatalf("unexpected mock response: %+v", response)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatalf("invalid error body: %v", err)
	}
}

func TestMockDelayHonorsContextCancellation(t *testing.T) {
	adapter := NewWithOptions(Options{Delay: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := adapter.Chat(ctx, []byte(`{"model":"test"}`), false); !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat() error = %v, want context.Canceled", err)
	}
}

func TestMockCanInterruptUpstream(t *testing.T) {
	adapter := NewWithOptions(Options{Interrupt: true})
	if _, err := adapter.Chat(context.Background(), []byte(`{"model":"test"}`), false); err == nil {
		t.Fatal("Chat() succeeded despite configured interruption")
	}
}

func TestMockStreamingReturnsChunksAndUsage(t *testing.T) {
	adapter := NewWithOptions(Options{})
	response, err := adapter.Chat(context.Background(), []byte(`{"model":"test"}`), true)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if response.Stream == nil || response.Usage.TotalTokens != 5 {
		t.Fatalf("unexpected stream response: %+v", response)
	}
	var chunks int
	for {
		_, err := response.Stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		chunks++
	}
	if chunks != 3 {
		t.Fatalf("stream yielded %d chunks, want 3", chunks)
	}
}
