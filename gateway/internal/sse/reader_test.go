// 本文件验证 SSE 数据事件、多行数据和末尾未终止事件的解析。
package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReaderParsesDataAndDoneEvents(t *testing.T) {
	reader := NewReader(strings.NewReader("event: message\ndata: {\"content\":\"hi\"}\n\ndata: [DONE]\n\n"))
	event, err := reader.Next()
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if event.Data != `{"content":"hi"}` || event.Done {
		t.Fatalf("unexpected event: %+v", event)
	}
	event, err = reader.Next()
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if event.Data != "[DONE]" || !event.Done {
		t.Fatalf("unexpected done event: %+v", event)
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() error = %v, want EOF", err)
	}
}

func TestReaderJoinsMultipleDataLines(t *testing.T) {
	reader := NewReader(strings.NewReader(": comment\ndata: first\ndata: second\n\n"))
	event, err := reader.Next()
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if event.Data != "first\nsecond" {
		t.Fatalf("data = %q", event.Data)
	}
}

func TestReaderFlushesFinalEventWithoutBlankTerminator(t *testing.T) {
	reader := NewReader(strings.NewReader("data: final"))
	event, err := reader.Next()
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if event.Data != "final" {
		t.Fatalf("data = %q", event.Data)
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() error = %v, want EOF", err)
	}
}
