// 本文件将响应数据编码为逐事件 flush 的 SSE 流。
package sse

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// Writer 将上游数据编码为 SSE，并在每个事件后立即 flush。
type Writer struct {
	w       http.ResponseWriter
	flusher http.Flusher
	ctx     context.Context
	mu      sync.Mutex
	started bool
}

// NewWriter 创建 SSE writer；底层 ResponseWriter 必须支持 Flush。
func NewWriter(w http.ResponseWriter, ctx context.Context) (*Writer, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("HTTP response writer does not support flushing")
	}
	return &Writer{w: w, flusher: flusher, ctx: ctx}, nil
}

// Start 写入 SSE 响应头并立即发送，不能对同一 writer 重复调用。
func (w *Writer) Start() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return errors.New("SSE response already started")
	}
	w.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.w.Header().Set("Cache-Control", "no-cache")
	w.w.Header().Set("X-Accel-Buffering", "no")
	w.w.WriteHeader(http.StatusOK)
	// 立即 flush 响应头，避免代理缓冲导致客户端迟迟收不到流式响应。
	w.flusher.Flush()
	w.started = true
	return nil
}

// WriteData 写入一个 SSE data 事件并立即 flush。
func (w *Writer) WriteData(data []byte) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started {
		return errors.New("SSE response has not started")
	}
	for _, line := range strings.Split(string(data), "\n") {
		if _, err := fmt.Fprintf(w.w, "data: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(w.w, "\n"); err != nil {
		return err
	}
	// 每个事件单独 flush，确保数据块逐段到达客户端。
	w.flusher.Flush()
	return nil
}

// Heartbeat 发送 SSE 注释行，保持长连接活跃而不产生业务事件。
func (w *Writer) Heartbeat() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started {
		return errors.New("SSE response has not started")
	}
	if _, err := fmt.Fprint(w.w, ": keep-alive\n\n"); err != nil {
		return err
	}
	w.flusher.Flush()
	return nil
}
