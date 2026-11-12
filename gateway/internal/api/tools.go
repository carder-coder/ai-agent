// 本文件实现工具执行 API 的请求边界、schema 校验和统一错误响应。
package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/tools"
	"github.com/carder-coder/ai-agent/gateway/internal/trace"
)

type toolRequest struct {
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
	ApprovalID string          `json:"approval_id,omitempty"`
}

type toolResponse struct {
	TraceID    string         `json:"trace_id"`
	Tool       string         `json:"tool"`
	Result     map[string]any `json:"result"`
	DurationMS int64          `json:"duration_ms"`
}

// toolExecuteHandler 只执行固定注册的工具，并在任何操作前完成完整参数校验。
func toolExecuteHandler(executor *tools.Executor, approvals *approvalStore, allowWrites bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
			return
		}
		var request toolRequest
		if !decodeStrictRequest(w, r, &request) {
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
		if definition.Write {
			if !allowWrites {
				writeError(w, http.StatusForbidden, "write tools are disabled", "permission_error", "tool_writes_disabled")
				return
			}
			callerID := callerIDFromRequest(r)
			if callerID == "" {
				writeError(w, http.StatusBadRequest, "X-Caller-ID is required for write tools", "invalid_request_error", "caller_id_required")
				return
			}
			if err := approvals.Consume(request.ApprovalID, callerID, request.Tool, arguments); err != nil {
				writeApprovalError(w, err)
				return
			}
		}

		started := time.Now()
		result, err := executor.Execute(r.Context(), request.Tool, arguments)
		if err != nil {
			log.Printf("工具执行失败 tool=%s trace_id=%s: %v", request.Tool, trace.IDFromContext(r.Context()), err)
			if errors.Is(err, tools.ErrTimeout) {
				writeError(w, http.StatusGatewayTimeout, "tool execution timed out", "timeout", "tool_timeout")
				return
			}
			if errors.Is(err, tools.ErrWritesDisabled) {
				writeError(w, http.StatusForbidden, "write tools are disabled", "permission_error", "tool_writes_disabled")
				return
			}
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

type approvalPrepareRequest struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type approvalPrepareResponse struct {
	ApprovalID string    `json:"approval_id"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type approvalConfirmation struct {
	Confirmed bool `json:"confirmed"`
}

// toolApprovalPrepareHandler 为已校验的写工具参数创建短时审批请求。
func toolApprovalPrepareHandler(approvals *approvalStore, allowWrites bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
			return
		}
		if !allowWrites {
			writeError(w, http.StatusForbidden, "write tools are disabled", "permission_error", "tool_writes_disabled")
			return
		}
		callerID := callerIDFromRequest(r)
		if callerID == "" {
			writeError(w, http.StatusBadRequest, "X-Caller-ID is required for write tools", "invalid_request_error", "caller_id_required")
			return
		}
		var request approvalPrepareRequest
		if !decodeStrictRequest(w, r, &request) {
			return
		}
		definition, exists := tools.Lookup(request.Tool)
		if !exists {
			writeError(w, http.StatusNotFound, "tool is not registered", "invalid_request_error", "tool_not_found")
			return
		}
		if !definition.Write {
			writeError(w, http.StatusBadRequest, "approval is only available for write tools", "invalid_request_error", "approval_not_required")
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
		id, expiresAt, err := approvals.Create(callerID, request.Tool, arguments)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "approval service is unavailable", "server_error", "approval_unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(approvalPrepareResponse{ApprovalID: id, ExpiresAt: expiresAt}); err != nil {
			log.Printf("编码审批响应失败 trace_id=%s: %v", trace.IDFromContext(r.Context()), err)
		}
	}
}

// toolApprovalConfirmHandler 只接受审批密钥持有者对指定审批请求的明确确认。
func toolApprovalConfirmHandler(approvals *approvalStore, token string, allowWrites bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
			return
		}
		if !allowWrites {
			writeError(w, http.StatusForbidden, "write tools are disabled", "permission_error", "tool_writes_disabled")
			return
		}
		if !validApprovalToken(r.Header.Get("Authorization"), token) {
			writeError(w, http.StatusUnauthorized, "approval authentication failed", "authentication_error", "invalid_approval_token")
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/tools/approvals/"), "/confirm")
		if id == r.URL.Path || !validApprovalID(id) {
			writeError(w, http.StatusNotFound, "approval is not found", "invalid_request_error", "approval_not_found")
			return
		}
		var confirmation approvalConfirmation
		if !decodeStrictRequest(w, r, &confirmation) {
			return
		}
		if !confirmation.Confirmed {
			writeError(w, http.StatusBadRequest, "confirmed must be true", "invalid_request_error", "approval_confirmation_required")
			return
		}
		if err := approvals.Confirm(id); err != nil {
			writeApprovalError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"approval_id": id, "status": "confirmed"}); err != nil {
			log.Printf("编码审批确认响应失败 trace_id=%s: %v", trace.IDFromContext(r.Context()), err)
		}
	}
}

func decodeStrictRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body: unable to read request", "invalid_request_error", "invalid_request")
		return false
	}
	if len(body) > maxRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "body: exceeds 1 MiB limit", "invalid_request_error", "request_too_large")
		return false
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
		writeError(w, http.StatusBadRequest, "body: must be a JSON object", "invalid_request_error", "invalid_request")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "body: must be a valid JSON object", "invalid_request_error", "invalid_request")
		return false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "body: must contain exactly one JSON object", "invalid_request_error", "invalid_request")
		return false
	}
	return true
}

func validApprovalToken(header, token string) bool {
	const prefix = "Bearer "
	return token != "" && strings.HasPrefix(header, prefix) &&
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, prefix)), []byte(token)) == 1
}

func callerIDFromRequest(r *http.Request) string {
	callerID := strings.TrimSpace(r.Header.Get("X-Caller-ID"))
	if len(callerID) == 0 || len(callerID) > 128 {
		return ""
	}
	for _, value := range callerID {
		if value < 0x21 || value > 0x7e {
			return ""
		}
	}
	return callerID
}
