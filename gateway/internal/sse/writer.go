package sse

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

type Writer struct {
	w       http.ResponseWriter
	flusher http.Flusher
	ctx     context.Context
	mu      sync.Mutex
	started bool
}

func NewWriter(w http.ResponseWriter, ctx context.Context) (*Writer, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("HTTP response writer does not support flushing")
	}
	return &Writer{w: w, flusher: flusher, ctx: ctx}, nil
}

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
	w.flusher.Flush()
	w.started = true
	return nil
}

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
	w.flusher.Flush()
	return nil
}

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
