package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	CachedTokens int `json:"cached_tokens"`
}

type Chunk struct {
	Data []byte
}

type ChunkStream interface {
	Next() (Chunk, error)
	Usage() Usage
	Close() error
}

type Response struct {
	StatusCode int
	Body       []byte
	RetryAfter string
	Stream     ChunkStream
	Usage      Usage
}

type Adapter interface {
	Chat(ctx context.Context, body []byte, stream bool) (Response, error)
}

type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]Adapter)}
}

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
	if _, exists := r.adapters[name]; exists {
		return fmt.Errorf("provider %q is already registered", name)
	}
	r.adapters[name] = adapter
	return nil
}

func (r *Registry) Get(name string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, exists := r.adapters[name]
	if !exists {
		return nil, fmt.Errorf("provider %q is not registered", name)
	}
	return adapter, nil
}
