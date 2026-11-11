package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
	"github.com/carder-coder/ai-agent/gateway/internal/provider"
)

const maxResponseBytes = 4 << 20

var ErrMissingAPIKey = errors.New("缺少 ARK_API_KEY")
var ErrStreamNotImplemented = errors.New("豆包 SSE streaming is not implemented yet")

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
	if stream {
		return provider.Response{}, ErrStreamNotImplemented
	}
	result, err := c.Complete(ctx, body)
	if err != nil {
		return provider.Response{}, err
	}
	return provider.Response{
		StatusCode: result.StatusCode,
		Body:       result.Body,
		RetryAfter: result.RetryAfter,
		Usage:      parseUsage(result.Body),
	}, nil
}

func (c *Client) Complete(ctx context.Context, body []byte) (provider.Response, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return provider.Response{}, ErrMissingAPIKey
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return provider.Response{}, fmt.Errorf("构造豆包请求失败：%w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		return provider.Response{}, err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return provider.Response{}, fmt.Errorf("读取豆包响应失败：%w", err)
	}
	if len(responseBody) > maxResponseBytes {
		return provider.Response{}, fmt.Errorf("豆包响应超过 %d 字节上限", maxResponseBytes)
	}
	return provider.Response{
		StatusCode: response.StatusCode,
		Body:       responseBody,
		RetryAfter: response.Header.Get("Retry-After"),
	}, nil
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

func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
