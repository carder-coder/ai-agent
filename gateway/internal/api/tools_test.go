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
	"github.com/carder-coder/ai-agent/gateway/internal/trace"
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
	if !trace.ValidID(approval.ApprovalID) || !approval.ExpiresAt.After(time.Now()) {
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

func TestToolExecutionAuditIsRedactedAndRetrievableByTraceID(t *testing.T) {
	traceDir := t.TempDir()
	handler := newTestRouter(t, config.Config{Provider: "mock", TraceDir: traceDir})
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/execute",
		strings.NewReader(`{"tool":"inspect_text","arguments":{"text":"private prompt"}}`),
	)
	request.Header.Set("X-Caller-ID", "audit-worker")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("execute status = %d, body=%s", response.Code, response.Body)
	}

	traceID := response.Header().Get("X-Trace-ID")
	tracePath := filepath.Join(traceDir, traceID+".jsonl")
	fileContent, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	if strings.Contains(string(fileContent), "private prompt") ||
		!strings.Contains(string(fileContent), `"caller":"audit-worker"`) ||
		!strings.Contains(string(fileContent), `"kind":"tool"`) ||
		!strings.Contains(string(fileContent), `"status":"completed"`) ||
		!strings.Contains(string(fileContent), `"sha256"`) {
		t.Fatalf("trace is missing audit fields or includes raw text: %s", fileContent)
	}
	if count := strings.Count(string(fileContent), `"kind":`); count != 4 {
		t.Fatalf("trace event count = %d, want run_start/tool_start/tool/run_end", count)
	}

	traceResponse := httptest.NewRecorder()
	handler.ServeHTTP(traceResponse, httptest.NewRequest(http.MethodGet, "/v1/trace/"+traceID, nil))
	if traceResponse.Code != http.StatusOK ||
		traceResponse.Header().Get("Content-Type") != "application/x-ndjson" ||
		traceResponse.Body.String() != string(fileContent) {
		t.Fatalf("trace endpoint response status=%d type=%q body=%s",
			traceResponse.Code, traceResponse.Header().Get("Content-Type"), traceResponse.Body)
	}
}

func TestTraceEndpointRejectsInvalidAndMissingIDs(t *testing.T) {
	handler := newTestRouter(t, config.Config{Provider: "mock"})
	for _, test := range []struct {
		id        string
		status    int
		errorCode string
	}{
		{id: "invalid", status: http.StatusBadRequest, errorCode: "invalid_trace_id"},
		{id: strings.Repeat("0", 32), status: http.StatusNotFound, errorCode: "trace_not_found"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/trace/"+test.id, nil))
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.errorCode) {
			t.Fatalf("trace %q returned status=%d body=%s", test.id, response.Code, response.Body)
		}
	}
}

func TestConversationTraceIncludesToolSubcalls(t *testing.T) {
	traceDir := t.TempDir()
	conversationID, err := trace.NewID()
	if err != nil {
		t.Fatalf("create conversation ID: %v", err)
	}
	recorder, err := trace.NewRecorder(traceDir, conversationID, "POST /v1/chat/completions")
	if err != nil {
		t.Fatalf("create parent trace: %v", err)
	}
	if err := recorder.End("completed", 0); err != nil {
		t.Fatalf("finish parent trace: %v", err)
	}

	handler := newTestRouter(t, config.Config{Provider: "mock", TraceDir: traceDir})
	childIDs := make([]string, 0, 2)
	for _, text := range []string{"first child", "second child"} {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/tools/execute",
			strings.NewReader(`{"tool":"inspect_text","arguments":{"text":"`+text+`"}}`),
		)
		request.Header.Set("X-Conversation-ID", conversationID)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("child tool status = %d, body=%s", response.Code, response.Body)
		}
		var child struct {
			TraceID string `json:"trace_id"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &child); err != nil {
			t.Fatalf("decode child tool response: %v", err)
		}
		childIDs = append(childIDs, child.TraceID)
	}

	combined := httptest.NewRecorder()
	handler.ServeHTTP(combined, httptest.NewRequest(http.MethodGet, "/v1/trace/"+conversationID, nil))
	body := combined.Body.String()
	if combined.Code != http.StatusOK ||
		strings.Count(body, `"kind":"tool"`) != 2 ||
		strings.Index(body, `"kind":"tool"`) < strings.Index(body, `"kind":"run_start"`) ||
		strings.LastIndex(body, `"kind":"tool"`) > strings.LastIndex(body, `"kind":"run_end"`) ||
		!strings.Contains(body, `"conversation_id":"`+conversationID+`"`) {
		t.Fatalf("parent trace does not include linked child tool event: status=%d body=%s", combined.Code, body)
	}
	for _, childID := range childIDs {
		if !strings.Contains(body, `"trace_id":"`+childID+`"`) {
			t.Fatalf("parent event does not link child trace ID %q: %s", childID, body)
		}
	}
}
