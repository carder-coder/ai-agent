// 本文件提供 trace 查询与父对话、工具子调用的 JSONL 合并。
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/carder-coder/ai-agent/gateway/internal/trace"
)

const maxTraceResponseBytes = 8 << 20

var errTraceTooLarge = errors.New("trace exceeds response limit")

func traceQueryHandler(traceDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
			return
		}
		traceID := strings.TrimPrefix(r.URL.Path, "/v1/trace/")
		if strings.Contains(traceID, "/") || !trace.ValidID(traceID) {
			writeError(w, http.StatusBadRequest, "trace ID must be 32 lowercase hexadecimal characters", "invalid_request_error", "invalid_trace_id")
			return
		}
		body, err := readTraceFile(traceDir, traceID, maxTraceResponseBytes)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, "trace is not found", "invalid_request_error", "trace_not_found")
				return
			}
			log.Printf("读取 trace 失败 trace_id=%s: %v", traceID, err)
			if errors.Is(err, errTraceTooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "trace exceeds 8 MiB limit", "invalid_request_error", "trace_too_large")
				return
			}
			writeError(w, http.StatusInternalServerError, "trace is unavailable", "server_error", "trace_unavailable")
			return
		}
		body, err = aggregateConversationTrace(traceDir, traceID, body, maxTraceResponseBytes)
		if err != nil {
			log.Printf("汇总会话工具 trace 失败 conversation_id=%s: %v", traceID, err)
			if errors.Is(err, errTraceTooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "trace exceeds 8 MiB limit", "invalid_request_error", "trace_too_large")
				return
			}
			writeError(w, http.StatusInternalServerError, "trace is unavailable", "server_error", "trace_unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		if _, err := w.Write(body); err != nil {
			log.Printf("返回 trace 失败 trace_id=%s: %v", traceID, err)
		}
	}
}

func aggregateConversationTrace(directory, conversationID string, parent []byte, maximum int) ([]byte, error) {
	children, err := trace.ConversationChildren(directory, conversationID)
	if err != nil || len(children) == 0 {
		return parent, err
	}
	events := make([][]byte, 0, len(children)+4)
	var endEvent []byte
	for _, line := range bytes.Split(parent, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var event struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("decode parent trace event: %w", err)
		}
		if event.Kind == "run_end" {
			endEvent = bytes.Clone(line)
		} else {
			events = append(events, bytes.Clone(line))
		}
	}
	for _, childID := range children {
		child, err := readTraceFile(directory, childID, maximum)
		if err != nil {
			return nil, fmt.Errorf("read child trace %s: %w", childID, err)
		}
		for _, line := range bytes.Split(child, []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			var event struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal(line, &event); err != nil {
				return nil, fmt.Errorf("decode child trace event: %w", err)
			}
			if event.Kind == "tool" {
				events = append(events, bytes.Clone(line))
			}
		}
	}
	if endEvent != nil {
		events = append(events, endEvent)
	}
	result := append(bytes.Join(events, []byte{'\n'}), '\n')
	if len(result) > maximum {
		return nil, errTraceTooLarge
	}
	return result, nil
}

func readTraceFile(directory, id string, maximum int) ([]byte, error) {
	file, err := os.Open(filepath.Join(directory, id+".jsonl"))
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if len(body) > maximum {
		return nil, errTraceTooLarge
	}
	return body, nil
}
