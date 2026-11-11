package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
	"github.com/carder-coder/ai-agent/gateway/internal/provider"
	"github.com/carder-coder/ai-agent/gateway/internal/sse"
)

const maxResponseBytes = 4 << 20

type Client struct {
	apiKey string
	url    string
	http   *http.Client
}

func NewClient(cfg config.Config) *Client {
	return &Client{
		apiKey: cfg.APIKey,
		url:    strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions",
		http: &http.Client{
			Timeout: cfg.RequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

var _ provider.Adapter = (*Client)(nil)

func (c *Client) Chat(ctx context.Context, body []byte, stream bool) (provider.Response, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return provider.Response{}, fmt.Errorf("%w: 缺少 ARK_API_KEY", provider.ErrAuthentication)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return provider.Response{}, fmt.Errorf("构造豆包请求失败：%w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	} else {
		request.Header.Set("Accept", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return provider.Response{}, err
	}
	result := provider.Response{
		StatusCode: response.StatusCode,
		RetryAfter: response.Header.Get("Retry-After"),
	}
	if stream && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
			response.Body.Close()
			return provider.Response{}, errors.New("豆包流式响应 Content-Type 不是 text/event-stream")
		}
		result.Stream = &chatStream{body: response.Body, reader: sse.NewReader(response.Body)}
		return result, nil
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return provider.Response{}, fmt.Errorf("读取豆包响应失败：%w", err)
	}
	if len(responseBody) > maxResponseBytes {
		return provider.Response{}, fmt.Errorf("豆包响应超过 %d 字节上限", maxResponseBytes)
	}
	result.Body = responseBody
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		result.Usage = parseUsage(responseBody)
	}
	return result, nil
}

type chatStream struct {
	body   io.ReadCloser
	reader *sse.Reader
	usage  provider.Usage
	once   sync.Once
	err    error
}

func (s *chatStream) Next() (provider.Chunk, error) {
	event, err := s.reader.Next()
	if err != nil {
		return provider.Chunk{}, err
	}
	if event.Done {
		return provider.Chunk{Data: []byte("[DONE]"), Done: true}, nil
	}
	if usage := parseUsage([]byte(event.Data)); usage.TotalTokens > 0 {
		s.usage = usage
	}
	var chunk struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
		return provider.Chunk{}, fmt.Errorf("解析豆包 SSE JSON 失败：%w", err)
	}
	return provider.Chunk{Data: []byte(event.Data), Content: len(chunk.Choices) > 0}, nil
}

func (s *chatStream) Usage() provider.Usage {
	return s.usage
}

func (s *chatStream) Close() error {
	s.once.Do(func() {
		s.err = s.body.Close()
	})
	return s.err
}

func parseUsage(body []byte) provider.Usage {
	var response struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
			PromptDetails    struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return provider.Usage{}
	}
	return provider.Usage{
		InputTokens:  response.Usage.PromptTokens,
		OutputTokens: response.Usage.CompletionTokens,
		TotalTokens:  response.Usage.TotalTokens,
		CachedTokens: response.Usage.PromptDetails.CachedTokens,
	}
}
