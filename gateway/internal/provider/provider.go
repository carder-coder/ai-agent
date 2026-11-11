// 本文件定义上游 Provider 适配器、响应/用量类型和线程安全注册表。
package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
)

// ErrAuthentication 标识 Provider 凭证缺失或认证失败。
var ErrAuthentication = errors.New("provider authentication failed")

// Usage 表示一次模型调用的 token 用量；CachedTokens 是输入 token 的子集。
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	CachedTokens int `json:"cached_tokens"`
}

// Chunk 表示从上游流中读取的一块数据及其结束状态。
type Chunk struct {
	Data    []byte
	Content bool
	Done    bool
}

// ChunkStream 定义可逐块读取、查询用量并关闭的 Provider 响应流。
type ChunkStream interface {
	// Next 返回下一块数据；流结束时返回 io.EOF。
	Next() (Chunk, error)
	// Usage 返回流中已解析到的 token 用量。
	Usage() Usage
	// Close 释放底层响应体及相关资源。
	Close() error
}

// Response 保存 Provider 的 HTTP 状态、响应体或流及请求用量。
type Response struct {
	StatusCode int
	Body       []byte
	RetryAfter string
	Stream     ChunkStream
	Usage      Usage
	Attempts   int
}

// Adapter 将统一的聊天调用转换为具体模型服务的请求。
type Adapter interface {
	// Chat 发起聊天请求；stream 为 true 时成功响应应提供 ChunkStream。
	Chat(ctx context.Context, body []byte, stream bool) (Response, error)
}

// Registry 保存名称到 Provider Adapter 的映射。
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry 创建一个空的 Provider 注册表。
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]Adapter)}
}

// Register 添加唯一名称的 Provider；空名称、nil adapter 或重复名称会报错。
func (r *Registry) Register(name string, adapter Adapter) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("provider name must not be empty")
	}
	if adapter == nil {
		return fmt.Errorf("provider %q has no adapter", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// 注册表可能在服务装配期间被多个组件访问，锁同时保护唯一性检查与写入。
	if _, exists := r.adapters[name]; exists {
		return fmt.Errorf("provider %q is already registered", name)
	}
	r.adapters[name] = adapter
	return nil
}

// Get 按名称返回已注册的 Provider Adapter。
func (r *Registry) Get(name string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, exists := r.adapters[name]
	if !exists {
		return nil, fmt.Errorf("provider %q is not registered", name)
	}
	return adapter, nil
}

// IsTimeout 判断错误是否表示 context deadline 或网络操作超时。
func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
