// 本文件实现工具执行 API 的请求边界、schema 校验和统一错误响应。
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/tools"
	"github.com/carder-coder/ai-agent/gateway/internal/trace"
)

type toolRequest struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolResponse struct {
	TraceID    string         `json:"trace_id"`
	Tool       string         `json:"tool"`
	Result     map[string]any `json:"result"`
	DurationMS int64          `json:"duration_ms"`
}

// toolExecuteHandler 只执行固定注册的工具，并在任何操作前完成完整参数校验。
func toolExecuteHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "body: unable to read request", "invalid_request_error", "invalid_request")
			return
		}
		if len(body) > maxRequestBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "body: exceeds 1 MiB limit", "invalid_request_error", "request_too_large")
			return
		}

		var request toolRequest
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "body: must be a valid tool request object", "invalid_request_error", "invalid_request")
			return
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "body: must contain exactly one JSON object", "invalid_request_error", "invalid_request")
			return
		}
		definition, exists := tools.Lookup(request.Tool)
		if !exists {
			writeError(w, http.StatusNotFound, "tool is not registered", "invalid_request_error", "tool_not_found")
			return
		}
		issues, arguments := tools.ValidateArguments(request.Arguments, definition)
		if len(issues) > 0 {
			details := make([]fieldError, len(issues))
			for index, issue := range issues {
				details[index] = fieldError{Field: issue.Field, Message: issue.Message}
			}
			writeDetailedError(w, http.StatusBadRequest, "tool arguments failed schema validation", "invalid_request_error", "invalid_tool_arguments", details)
			return
		}

		started := time.Now()
		result, err := tools.Execute(request.Tool, arguments)
		if err != nil {
			log.Printf("工具执行失败 tool=%s trace_id=%s: %v", request.Tool, trace.IDFromContext(r.Context()), err)
			writeError(w, http.StatusBadGateway, "tool execution failed", "server_error", "tool_execution_failed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(toolResponse{
			TraceID:    trace.IDFromContext(r.Context()),
			Tool:       request.Tool,
			Result:     result,
			DurationMS: time.Since(started).Milliseconds(),
		}); err != nil {
			log.Printf("编码工具响应失败 tool=%s trace_id=%s: %v", request.Tool, trace.IDFromContext(r.Context()), err)
			return
		}
	}
}
