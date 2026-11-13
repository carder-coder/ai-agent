// 本文件实现工具执行 API 的请求边界、schema 校验和统一错误响应。
package api

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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
func toolExecuteHandler(
	traceDir string,
	executor *tools.Executor,
	approvals *approvalStore,
	allowWrites bool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		traceID := trace.IDFromContext(r.Context())
		recorder, err := trace.NewRecorder(traceDir, traceID, "POST /v1/tools/execute")
		if err != nil {
			log.Printf("创建工具执行 trace 失败 trace_id=%s: %v", traceID, err)
			writeError(w, http.StatusServiceUnavailable, "tool audit trace is unavailable", "server_error", "trace_unavailable")
			return
		}
		auditStarted := time.Now()
		audit := trace.ToolEvent{
			Caller:        callerIDFromRequest(r),
			TraceID:       traceID,
			Parameters:    map[string]any{},
			ResultSummary: map[string]any{},
			Status:        "invalid_request",
		}
		if audit.Caller == "" {
			audit.Caller = "anonymous"
		}
		defer func() {
			audit.DurationMS = time.Since(auditStarted).Milliseconds()
			if err := recorder.RecordTool(audit); err != nil {
				log.Printf("写入工具审计事件失败 trace_id=%s: %v", traceID, err)
			}
			if err := recorder.End(audit.Status, 1); err != nil {
				log.Printf("结束工具执行 trace 失败 trace_id=%s: %v", traceID, err)
			}
			if audit.ConversationID != "" {
				if err := trace.LinkConversation(traceDir, audit.ConversationID, traceID); err != nil {
					log.Printf("关联会话工具 trace 失败 conversation_id=%s child_trace_id=%s: %v",
						audit.ConversationID, traceID, err)
				}
			}
		}()
		if r.Method != http.MethodPost {
			audit.Status = "method_not_allowed"
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
			return
		}
		var request toolRequest
		if !decodeStrictRequest(w, r, &request) {
			return
		}
		conversationID := r.Header.Get("X-Conversation-ID")
		if conversationID != "" {
			if !trace.ValidID(conversationID) {
				audit.Status = "invalid_conversation_id"
				audit.ResultSummary = map[string]any{"error_code": audit.Status}
				writeError(w, http.StatusBadRequest, "X-Conversation-ID must be a trace ID", "invalid_request_error", audit.Status)
				return
			}
			exists, err := trace.HasTrace(traceDir, conversationID)
			if err != nil {
				log.Printf("检查会话 trace 失败 conversation_id=%s: %v", conversationID, err)
				audit.Status = "trace_unavailable"
				audit.ResultSummary = map[string]any{"error_code": audit.Status}
				writeError(w, http.StatusInternalServerError, "conversation trace is unavailable", "server_error", audit.Status)
				return
			}
			if !exists {
				audit.Status = "conversation_not_found"
				audit.ResultSummary = map[string]any{"error_code": audit.Status}
				writeError(w, http.StatusNotFound, "conversation trace is not found", "invalid_request_error", audit.Status)
				return
			}
			audit.ConversationID = conversationID
		}
		audit.Name = request.Tool
		audit.Parameters = map[string]any{
			"arguments":   auditParameters(request.Arguments),
			"approval_id": request.ApprovalID,
		}
		definition, exists := tools.Lookup(request.Tool)
		if !exists {
			audit.Status = "tool_not_found"
			audit.ResultSummary = map[string]any{"error_code": audit.Status}
			writeError(w, http.StatusNotFound, "tool is not registered", "invalid_request_error", "tool_not_found")
			return
		}
		issues, arguments := tools.ValidateArguments(request.Arguments, definition)
		if len(issues) > 0 {
			audit.Status = "invalid_tool_arguments"
			audit.ResultSummary = map[string]any{"field_errors": issues}
			details := make([]fieldError, len(issues))
			for index, issue := range issues {
				details[index] = fieldError{Field: issue.Field, Message: issue.Message}
			}
			writeDetailedError(w, http.StatusBadRequest, "tool arguments failed schema validation", "invalid_request_error", "invalid_tool_arguments", details)
			return
		}
		audit.Status = "started"
		if err := recorder.RecordToolStart(audit); err != nil {
			audit.Status = "trace_unavailable"
			log.Printf("持久化工具执行开始事件失败 trace_id=%s: %v", traceID, err)
			writeError(w, http.StatusServiceUnavailable, "tool audit trace is unavailable", "server_error", "trace_unavailable")
			return
		}
		if definition.Write {
			if !allowWrites {
				audit.Status = "tool_writes_disabled"
				audit.ResultSummary = map[string]any{"error_code": audit.Status}
				writeError(w, http.StatusForbidden, "write tools are disabled", "permission_error", "tool_writes_disabled")
				return
			}
			callerID := callerIDFromRequest(r)
			if callerID == "" {
				audit.Status = "caller_id_required"
				audit.ResultSummary = map[string]any{"error_code": audit.Status}
				writeError(w, http.StatusBadRequest, "X-Caller-ID is required for write tools", "invalid_request_error", "caller_id_required")
				return
			}
			if err := approvals.Consume(request.ApprovalID, callerID, request.Tool, arguments); err != nil {
				audit.Status = approvalErrorCode(err)
				audit.ResultSummary = map[string]any{"error_code": audit.Status}
				writeApprovalError(w, err)
				return
			}
		}

		started := time.Now()
		result, err := executor.Execute(r.Context(), request.Tool, arguments)
		if err != nil {
			audit.Status = "tool_execution_failed"
			log.Printf("工具执行失败 tool=%s trace_id=%s: %v", request.Tool, trace.IDFromContext(r.Context()), err)
			if errors.Is(err, tools.ErrTimeout) {
				audit.Status = "tool_timeout"
				audit.ResultSummary = map[string]any{"error_code": audit.Status}
				writeError(w, http.StatusGatewayTimeout, "tool execution timed out", "timeout", "tool_timeout")
				return
			}
			if errors.Is(err, tools.ErrWritesDisabled) {
				audit.Status = "tool_writes_disabled"
				audit.ResultSummary = map[string]any{"error_code": audit.Status}
				writeError(w, http.StatusForbidden, "write tools are disabled", "permission_error", "tool_writes_disabled")
				return
			}
			audit.ResultSummary = map[string]any{"error_code": audit.Status}
			writeError(w, http.StatusBadGateway, "tool execution failed", "server_error", "tool_execution_failed")
			return
		}
		audit.Status = "completed"
		audit.ResultSummary = auditResultSummary(result)
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

// approvalAudit 统一记录审批请求的发起、确认与失败原因。
type approvalAudit struct {
	recorder *trace.Recorder
	event    trace.ApprovalEvent
	started  time.Time
}

// beginApprovalAudit 为审批请求开启独立 trace；审计不可用时由调用方失败关闭。
func beginApprovalAudit(traceDir, action string, r *http.Request) (*approvalAudit, error) {
	recorder, err := trace.NewRecorder(traceDir, trace.IDFromContext(r.Context()), r.Method+" "+r.URL.Path)
	if err != nil {
		return nil, err
	}
	return &approvalAudit{
		recorder: recorder,
		started:  time.Now(),
		event: trace.ApprovalEvent{
			Action: action,
			Caller: callerIDFromRequest(r),
		},
	}, nil
}

// finish 写入审批事件与 run_end；写入失败只记录日志，不改变已返回的结果。
func (a *approvalAudit) finish(status string) {
	a.event.Status = status
	a.event.DurationMS = time.Since(a.started).Milliseconds()
	if err := a.recorder.RecordApproval(a.event); err != nil {
		log.Printf("写入审批审计事件失败 approval_id=%s: %v", a.event.ApprovalID, err)
	}
	if err := a.recorder.End(status, 1); err != nil {
		log.Printf("结束审批审计 trace 失败 approval_id=%s: %v", a.event.ApprovalID, err)
	}
}

// approvalKeyID 用审批密钥摘要标识确认人，避免把密钥原文写进审计。
func approvalKeyID(token string) string {
	digest := sha256.Sum256([]byte(token))
	return "token-sha256:" + hex.EncodeToString(digest[:])[:12]
}

// toolApprovalPrepareHandler 为已校验的写工具参数创建短时审批请求，并落审计。
func toolApprovalPrepareHandler(traceDir string, approvals *approvalStore, allowWrites bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := "invalid_request"
		audit, auditErr := beginApprovalAudit(traceDir, "prepare", r)
		if audit != nil {
			defer func() { audit.finish(status) }()
		} else {
			log.Printf("创建审批审计 trace 失败：%v", auditErr)
		}
		if r.Method != http.MethodPost {
			status = "method_not_allowed"
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", status)
			return
		}
		if !allowWrites {
			status = "tool_writes_disabled"
			writeError(w, http.StatusForbidden, "write tools are disabled", "permission_error", status)
			return
		}
		if audit == nil {
			status = "trace_unavailable"
			writeError(w, http.StatusServiceUnavailable, "approval audit trace is unavailable", "server_error", status)
			return
		}
		callerID := callerIDFromRequest(r)
		if callerID == "" {
			status = "caller_id_required"
			writeError(w, http.StatusBadRequest, "X-Caller-ID is required for write tools", "invalid_request_error", status)
			return
		}
		var request approvalPrepareRequest
		if !decodeStrictRequest(w, r, &request) {
			return
		}
		audit.event.Tool = request.Tool
		audit.event.Parameters = auditParameters(request.Arguments)
		definition, exists := tools.Lookup(request.Tool)
		if !exists {
			status = "tool_not_found"
			writeError(w, http.StatusNotFound, "tool is not registered", "invalid_request_error", status)
			return
		}
		if !definition.Write {
			status = "approval_not_required"
			writeError(w, http.StatusBadRequest, "approval is only available for write tools", "invalid_request_error", status)
			return
		}
		issues, arguments := tools.ValidateArguments(request.Arguments, definition)
		if len(issues) > 0 {
			status = "invalid_tool_arguments"
			details := make([]fieldError, len(issues))
			for index, issue := range issues {
				details[index] = fieldError{Field: issue.Field, Message: issue.Message}
			}
			writeDetailedError(w, http.StatusBadRequest, "tool arguments failed schema validation", "invalid_request_error", status, details)
			return
		}
		id, expiresAt, err := approvals.Create(callerID, request.Tool, arguments)
		if err != nil {
			status = "approval_unavailable"
			writeError(w, http.StatusServiceUnavailable, "approval service is unavailable", "server_error", status)
			return
		}
		audit.event.ApprovalID = id
		status = "prepared"
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(approvalPrepareResponse{ApprovalID: id, ExpiresAt: expiresAt}); err != nil {
			log.Printf("编码审批响应失败 trace_id=%s: %v", trace.IDFromContext(r.Context()), err)
		}
	}
}

// toolApprovalConfirmHandler 只接受审批密钥持有者对指定审批请求的明确确认，并落审计。
func toolApprovalConfirmHandler(traceDir string, approvals *approvalStore, token string, allowWrites bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := "invalid_request"
		audit, auditErr := beginApprovalAudit(traceDir, "confirm", r)
		if audit != nil {
			defer func() { audit.finish(status) }()
		} else {
			log.Printf("创建审批审计 trace 失败：%v", auditErr)
		}
		if r.Method != http.MethodPost {
			status = "method_not_allowed"
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", status)
			return
		}
		if !allowWrites {
			status = "tool_writes_disabled"
			writeError(w, http.StatusForbidden, "write tools are disabled", "permission_error", status)
			return
		}
		if audit == nil {
			status = "trace_unavailable"
			writeError(w, http.StatusServiceUnavailable, "approval audit trace is unavailable", "server_error", status)
			return
		}
		if !validBearerToken(r.Header.Get("Authorization"), token) {
			status = "invalid_approval_token"
			writeError(w, http.StatusUnauthorized, "approval authentication failed", "authentication_error", status)
			return
		}
		audit.event.Approver = approvalKeyID(token)
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/tools/approvals/"), "/confirm")
		if id == r.URL.Path || !trace.ValidID(id) {
			status = "approval_not_found"
			writeError(w, http.StatusNotFound, "approval is not found", "invalid_request_error", status)
			return
		}
		audit.event.ApprovalID = id
		var confirmation approvalConfirmation
		if !decodeStrictRequest(w, r, &confirmation) {
			return
		}
		if !confirmation.Confirmed {
			status = "approval_confirmation_required"
			writeError(w, http.StatusBadRequest, "confirmed must be true", "invalid_request_error", status)
			return
		}
		if err := approvals.Confirm(id); err != nil {
			status = approvalErrorCode(err)
			writeApprovalError(w, err)
			return
		}
		status = "confirmed"
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

func validBearerToken(header, token string) bool {
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
