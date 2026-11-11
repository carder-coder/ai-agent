package mock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/provider"
)

type Options struct {
	Delay      time.Duration
	StatusCode int
	Interrupt  bool
}

type Adapter struct {
	options Options
}

var _ provider.Adapter = (*Adapter)(nil)

func New() *Adapter {
	return NewWithOptions(Options{Delay: 5 * time.Millisecond})
}

func NewWithOptions(options Options) *Adapter {
	return &Adapter{options: options}
}

func (m *Adapter) Chat(ctx context.Context, body []byte, stream bool) (provider.Response, error) {
	if m.options.Delay > 0 {
		timer := time.NewTimer(m.options.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return provider.Response{}, ctx.Err()
		case <-timer.C:
		}
	}
	if m.options.Interrupt {
		return provider.Response{}, errors.New("mock upstream interrupted")
	}
	if m.options.StatusCode != 0 && m.options.StatusCode != 200 {
		return provider.Response{
			StatusCode: m.options.StatusCode,
			Body:       []byte(fmt.Sprintf(`{"error":{"message":"mock upstream status %d"}}`, m.options.StatusCode)),
			RetryAfter: "1",
		}, nil
	}

	var request struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return provider.Response{}, fmt.Errorf("decode mock request: %w", err)
	}
	usage := provider.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}
	if stream {
		return provider.Response{
			StatusCode: http.StatusOK,
			Stream: &mockStream{
				chunks: []provider.Chunk{
					{Data: []byte(`{"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"mock"},"finish_reason":null}]}`), Content: true},
					{Data: []byte(`{"id":"chatcmpl-mock","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)},
					{Data: []byte("[DONE]"), Done: true},
				},
				usage: usage,
			},
			Usage: usage,
		}, nil
	}

	response := map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": 1,
		"model":   request.Model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]string{
					"role":    "assistant",
					"content": "mock response",
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     usage.InputTokens,
			"completion_tokens": usage.OutputTokens,
			"total_tokens":      usage.TotalTokens,
		},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return provider.Response{}, fmt.Errorf("encode mock response: %w", err)
	}
	return provider.Response{StatusCode: http.StatusOK, Body: encoded, Usage: usage}, nil
}

type mockStream struct {
	chunks []provider.Chunk
	index  int
	usage  provider.Usage
}

func (s *mockStream) Next() (provider.Chunk, error) {
	if s.index >= len(s.chunks) {
		return provider.Chunk{}, io.EOF
	}
	chunk := s.chunks[s.index]
	s.index++
	return chunk, nil
}

func (s *mockStream) Usage() provider.Usage {
	return s.usage
}

func (s *mockStream) Close() error {
	return nil
}
