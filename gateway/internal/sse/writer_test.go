// 本文件验证 SSE 写入器的逐事件 flush、心跳和取消处理。
package sse

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriterFlushesEachDataEvent(t *testing.T) {
	response := &flushCounter{ResponseRecorder: httptest.NewRecorder()}
	writer, err := NewWriter(response, context.Background())
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := writer.WriteData([]byte(`{"choices":[]}`)); err != nil {
		t.Fatalf("WriteData() error = %v", err)
	}
	if err := writer.WriteData([]byte("[DONE]")); err != nil {
		t.Fatalf("WriteData() error = %v", err)
	}
	if response.Code != 200 || !response.Flushed || response.flushes != 3 {
		t.Fatalf("status = %d, flushed = %t, flushes = %d", response.Code, response.Flushed, response.flushes)
	}
	if response.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}
	if got := response.Body.String(); !strings.Contains(got, "data: {\"choices\":[]}\n\n") ||
		!strings.Contains(got, "data: [DONE]\n\n") {
		t.Fatalf("unexpected SSE body: %q", got)
	}
}

func TestWriterSendsHeartbeatComment(t *testing.T) {
	response := &flushCounter{ResponseRecorder: httptest.NewRecorder()}
	writer, err := NewWriter(response, context.Background())
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := writer.Heartbeat(); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if response.Body.String() != ": keep-alive\n\n" {
		t.Fatalf("heartbeat body = %q", response.Body.String())
	}
}

func TestWriterChecksContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writer, err := NewWriter(httptest.NewRecorder(), ctx)
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}
	if err := writer.Start(); err == nil {
		t.Fatal("Start() succeeded after context cancellation")
	}
}

type flushCounter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (w *flushCounter) Flush() {
	w.flushes++
	w.ResponseRecorder.Flush()
}
