// 本文件验证写操作审批的发起与确认都会写入可核对的审计事件。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
)

func readTraceEvents(t *testing.T, traceDir, traceID string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(traceDir, traceID+".jsonl"))
	if err != nil {
		t.Fatalf("read trace %s: %v", traceID, err)
	}
	return string(content)
}

func TestApprovalPrepareAndConfirmAreAudited(t *testing.T) {
	traceDir := t.TempDir()
	workspace := t.TempDir()
	const callerID = "agent-worker"
	const approvalToken = "approval-secret-for-tests-32-characters"
	handler := newTestRouter(t, config.Config{
		Provider:          "mock",
		TraceDir:          traceDir,
		ToolWorkspace:     workspace,
		ToolsAllowWrites:  true,
		ToolApprovalToken: approvalToken,
	})

	arguments := `{"filename":"audited.txt","content":"secret body"}`
	prepareRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/approvals",
		strings.NewReader(`{"tool":"write_note","arguments":`+arguments+`}`),
	)
	prepareRequest.Header.Set("X-Caller-ID", callerID)
	prepareResponse := httptest.NewRecorder()
	handler.ServeHTTP(prepareResponse, prepareRequest)
	if prepareResponse.Code != http.StatusOK {
		t.Fatalf("prepare status = %d body=%s", prepareResponse.Code, prepareResponse.Body)
	}
	prepareTrace := readTraceEvents(t, traceDir, prepareResponse.Header().Get("X-Trace-ID"))
	for _, want := range []string{`"kind":"approval"`, `"action":"prepare"`, `"tool":"write_note"`, `"status":"prepared"`, callerID} {
		if !strings.Contains(prepareTrace, want) {
			t.Fatalf("prepare trace is missing %q: %s", want, prepareTrace)
		}
	}
	if !strings.Contains(prepareTrace, `"redacted":true`) || strings.Contains(prepareTrace, "secret body") {
		t.Fatalf("prepare trace must redact write content: %s", prepareTrace)
	}

	var prepared approvalPrepareResponse
	if err := json.Unmarshal(prepareResponse.Body.Bytes(), &prepared); err != nil {
		t.Fatalf("decode prepared approval: %v", err)
	}
	confirmRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/approvals/"+prepared.ApprovalID+"/confirm",
		strings.NewReader(`{"confirmed":true}`),
	)
	confirmRequest.Header.Set("Authorization", "Bearer "+approvalToken)
	confirmResponse := httptest.NewRecorder()
	handler.ServeHTTP(confirmResponse, confirmRequest)
	if confirmResponse.Code != http.StatusOK {
		t.Fatalf("confirm status = %d body=%s", confirmResponse.Code, confirmResponse.Body)
	}
	confirmTrace := readTraceEvents(t, traceDir, confirmResponse.Header().Get("X-Trace-ID"))
	for _, want := range []string{`"action":"confirm"`, `"status":"confirmed"`, prepared.ApprovalID, `"approver":"token-sha256:`} {
		if !strings.Contains(confirmTrace, want) {
			t.Fatalf("confirm trace is missing %q: %s", want, confirmTrace)
		}
	}
	if strings.Contains(confirmTrace, approvalToken) {
		t.Fatalf("confirm trace leaked the approval token: %s", confirmTrace)
	}

	executeRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/tools/execute",
		strings.NewReader(`{"tool":"write_note","arguments":`+arguments+`,"approval_id":"`+prepared.ApprovalID+`"}`),
	)
	executeRequest.Header.Set("X-Caller-ID", callerID)
	executeResponse := httptest.NewRecorder()
	handler.ServeHTTP(executeResponse, executeRequest)
	if executeResponse.Code != http.StatusOK {
		t.Fatalf("execute status = %d body=%s", executeResponse.Code, executeResponse.Body)
	}
	executeTrace := readTraceEvents(t, traceDir, executeResponse.Header().Get("X-Trace-ID"))
	if !strings.Contains(executeTrace, prepared.ApprovalID) || !strings.Contains(executeTrace, `"status":"completed"`) {
		t.Fatalf("execute trace cannot be tied back to the approval: %s", executeTrace)
	}
}
