package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
)

func TestToolExecuteRejectsInvalidArgumentsWithFieldDetails(t *testing.T) {
	handler := newTestRouter(t, config.Config{Provider: "mock"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/execute",
		strings.NewReader(`{"tool":"inspect_text","arguments":{"text":12,"mode":"shell","extra":true}}`),
	))

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", response.Code, response.Body)
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if envelope.Error.Code != "invalid_tool_arguments" || len(envelope.Error.Details) != 3 {
		t.Fatalf("unexpected validation response: %+v", envelope.Error)
	}
	if envelope.Error.Details[0].Field != "arguments.extra" ||
		envelope.Error.Details[1].Field != "arguments.mode" ||
		envelope.Error.Details[2].Field != "arguments.text" {
		t.Fatalf("details are not deterministic and field-scoped: %+v", envelope.Error.Details)
	}
}

func TestToolExecuteReturnsTraceIDAndResult(t *testing.T) {
	handler := newTestRouter(t, config.Config{Provider: "mock"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/execute",
		strings.NewReader(`{"tool":"inspect_text","arguments":{"text":"hello world"}}`),
	))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body)
	}
	var result struct {
		TraceID string         `json:"trace_id"`
		Tool    string         `json:"tool"`
		Result  map[string]int `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.TraceID == "" || result.TraceID != response.Header().Get("X-Trace-ID") ||
		result.Tool != "inspect_text" || result.Result["characters"] != 11 || result.Result["words"] != 2 {
		t.Fatalf("unexpected tool response: %+v", result)
	}
}

func TestToolExecuteRejectsUnknownToolsAndUnknownRequestFields(t *testing.T) {
	handler := newTestRouter(t, config.Config{Provider: "mock"})
	for _, body := range []string{
		`{"tool":"unknown","arguments":{}}`,
		`{"tool":"inspect_text","arguments":{"text":"ok"},"command":"whoami"}`,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(
			http.MethodPost, "/v1/tools/execute", strings.NewReader(body),
		))
		if response.Code != http.StatusNotFound && response.Code != http.StatusBadRequest {
			t.Fatalf("body=%s returned %d, want unknown tool or invalid request", body, response.Code)
		}
	}
}

func TestWorkspaceStatusRejectsCommandLikeArguments(t *testing.T) {
	handler := newTestRouter(t, config.Config{Provider: "mock"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/execute",
		strings.NewReader(`{"tool":"workspace_status","arguments":{"format":"short; whoami"}}`),
	))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_tool_arguments") {
		t.Fatalf("command-like argument was not rejected: status=%d body=%s", response.Code, response.Body)
	}
}

func TestWriteNoteRequiresConfirmedBoundApprovalAndCannotReplay(t *testing.T) {
	workspace := t.TempDir()
	const callerID = "agent-worker"
	const approvalToken = "approval-secret-for-tests-32-characters"
	handler := newTestRouter(t, config.Config{
		Provider:          "mock",
		ToolWorkspace:     workspace,
		ToolsAllowWrites:  true,
		ToolApprovalToken: approvalToken,
	})
	arguments := `{"filename":"approved.txt","content":"approved text"}`
	prepare := httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/approvals",
		strings.NewReader(`{"tool":"write_note","arguments":`+arguments+`}`),
	)
	prepare.Header.Set("X-Caller-ID", callerID)
	prepared := httptest.NewRecorder()
	handler.ServeHTTP(prepared, prepare)
	if prepared.Code != http.StatusOK {
		t.Fatalf("prepare status = %d, body=%s", prepared.Code, prepared.Body)
	}
	var approval approvalPrepareResponse
	if err := json.Unmarshal(prepared.Body.Bytes(), &approval); err != nil {
		t.Fatalf("decode prepared approval: %v", err)
	}
	if !validApprovalID(approval.ApprovalID) || !approval.ExpiresAt.After(time.Now()) {
		t.Fatalf("invalid prepared approval: %+v", approval)
	}

	execute := func(requestArguments, approvalID, requestCaller string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/tools/execute",
			strings.NewReader(`{"tool":"write_note","arguments":`+requestArguments+`,"approval_id":"`+approvalID+`"}`),
		)
		request.Header.Set("X-Caller-ID", requestCaller)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := execute(arguments, approval.ApprovalID, callerID); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "approval_not_confirmed") {
		t.Fatalf("unconfirmed execution status = %d, body=%s", response.Code, response.Body)
	}
	if _, err := os.Stat(filepath.Join(workspace, "approved.txt")); !os.IsNotExist(err) {
		t.Fatalf("unconfirmed request created a file: %v", err)
	}

	confirm := httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/approvals/"+approval.ApprovalID+"/confirm",
		strings.NewReader(`{"confirmed":true}`),
	)
	confirm.Header.Set("Authorization", "Bearer wrong-token")
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, confirm)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("wrong approval token status = %d, body=%s", unauthorized.Code, unauthorized.Body)
	}

	confirm = httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/approvals/"+approval.ApprovalID+"/confirm",
		strings.NewReader(`{"confirmed":false}`),
	)
	confirm.Header.Set("Authorization", "Bearer "+approvalToken)
	unconfirmed := httptest.NewRecorder()
	handler.ServeHTTP(unconfirmed, confirm)
	if unconfirmed.Code != http.StatusBadRequest {
		t.Fatalf("missing explicit confirmation status = %d, body=%s", unconfirmed.Code, unconfirmed.Body)
	}

	confirm = httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/approvals/"+approval.ApprovalID+"/confirm",
		strings.NewReader(`{"confirmed":true}`),
	)
	confirm.Header.Set("Authorization", "Bearer "+approvalToken)
	confirmed := httptest.NewRecorder()
	handler.ServeHTTP(confirmed, confirm)
	if confirmed.Code != http.StatusOK {
		t.Fatalf("confirm status = %d, body=%s", confirmed.Code, confirmed.Body)
	}

	changedArguments := `{"filename":"approved.txt","content":"different text"}`
	if response := execute(arguments, approval.ApprovalID, "another-caller"); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "approval_mismatch") {
		t.Fatalf("different caller status = %d, body=%s", response.Code, response.Body)
	}
	if response := execute(changedArguments, approval.ApprovalID, callerID); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "approval_mismatch") {
		t.Fatalf("changed arguments status = %d, body=%s", response.Code, response.Body)
	}
	if response := execute(arguments, approval.ApprovalID, callerID); response.Code != http.StatusOK {
		t.Fatalf("approved execution status = %d, body=%s", response.Code, response.Body)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "approved.txt"))
	if err != nil || string(content) != "approved text" {
		t.Fatalf("written file = %q, err=%v", content, err)
	}
	if response := execute(arguments, approval.ApprovalID, callerID); response.Code != http.StatusNotFound ||
		!strings.Contains(response.Body.String(), "approval_not_found") {
		t.Fatalf("replayed execution status = %d, body=%s", response.Code, response.Body)
	}
}

func TestWriteToolsAreDisabledByDefaultAndRejectUnsafeFilename(t *testing.T) {
	workspace := t.TempDir()
	handler := newTestRouter(t, config.Config{Provider: "mock", ToolWorkspace: workspace})
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/approvals",
		strings.NewReader(`{"tool":"write_note","arguments":{"filename":"../escape.txt","content":"no"}}`),
	)
	request.Header.Set("X-Caller-ID", "agent-worker")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "tool_writes_disabled") {
		t.Fatalf("write tools were not disabled by default: status=%d body=%s", response.Code, response.Body)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(workspace), "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("unsafe path created a file: %v", err)
	}
}
