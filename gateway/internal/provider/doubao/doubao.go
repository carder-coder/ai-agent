package doubao

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
)

const maxResponseBytes = 4 << 20

var ErrMissingAPIKey = errors.New("缺少 ARK_API_KEY")

type Result struct {
	StatusCode int
	Body       []byte
	RetryAfter string
}

type Client struct {
	apiKey string
	model  string
	url    string
	http   *http.Client
}

func NewClient(cfg config.Config) *Client {
	return &Client{
		apiKey: cfg.APIKey,
		model:  cfg.Model,
		url:    strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions",
		http: &http.Client{
			Timeout: cfg.RequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (c *Client) Complete(ctx context.Context, body []byte) (Result, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return Result{}, ErrMissingAPIKey
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("构造豆包请求失败：%w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("读取豆包响应失败：%w", err)
	}
	if len(responseBody) > maxResponseBytes {
		return Result{}, fmt.Errorf("豆包响应超过 %d 字节上限", maxResponseBytes)
	}
	return Result{
		StatusCode: response.StatusCode,
		Body:       responseBody,
		RetryAfter: response.Header.Get("Retry-After"),
	}, nil
}

func (c *Client) Model(requested string) string {
	if c.model != "" {
		return c.model
	}
	return requested
}

func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
