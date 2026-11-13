// 本文件用仓库内固定样本集验证越权与注入尝试都会被拒绝且不产生副作用。
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

type toolAbuseCase struct {
	Name         string          `json:"name"`
	Endpoint     string          `json:"endpoint"`
	Caller       string          `json:"caller"`
	Body         json.RawMessage `json:"body"`
	ExpectStatus int             `json:"expect_status"`
	ExpectCode   string          `json:"expect_code"`
	Note         string          `json:"note"`
}

type toolAbuseCaseFile struct {
	Cases []toolAbuseCase `json:"cases"`
}

func loadToolAbuseCases(t *testing.T) []toolAbuseCase {
	t.Helper()
	path := filepath.Join("..", "..", "..", "cases", "tool_abuse_cases.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read abuse cases %s: %v", path, err)
	}
	var file toolAbuseCaseFile
	if err := json.Unmarshal(content, &file); err != nil {
		t.Fatalf("decode abuse cases: %v", err)
	}
	if len(file.Cases) == 0 {
		t.Fatal("abuse case set is empty")
	}
	return file.Cases
}

func TestToolAbuseCasesAreRejectedWithoutSideEffects(t *testing.T) {
	const approvalToken = "approval-secret-for-tests-32-characters"
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	handler := newTestRouter(t, config.Config{
		Provider:          "mock",
		TraceDir:          filepath.Join(root, "traces"),
		ToolWorkspace:     workspace,
		ToolsAllowWrites:  true,
		ToolApprovalToken: approvalToken,
	})

	for _, testCase := range loadToolAbuseCases(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			path := "/v1/tools/execute"
			if testCase.Endpoint == "approval_prepare" {
				path = "/v1/tools/approvals"
			}
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(testCase.Body)))
			if testCase.Caller != "" {
				request.Header.Set("X-Caller-ID", testCase.Caller)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != testCase.ExpectStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, testCase.ExpectStatus, response.Body)
			}
			var envelope errorEnvelope
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error body: %v; body=%s", err, response.Body)
			}
			if envelope.Error.Code != testCase.ExpectCode {
				t.Fatalf("error code = %q, want %q; body=%s", envelope.Error.Code, testCase.ExpectCode, response.Body)
			}
		})
	}

	// 所有用例结束后工作区与父目录都不应出现被写入的文件（例如 ../escape.txt）。
	if entries, err := os.ReadDir(workspace); err == nil && len(entries) > 0 {
		t.Fatalf("abuse cases wrote files into the workspace: %v", entries)
	}
	if entries, err := os.ReadDir(root); err != nil {
		t.Fatalf("read workspace root: %v", err)
	} else {
		for _, entry := range entries {
			if entry.Name() == "escape.txt" || strings.HasSuffix(entry.Name(), ".txt") {
				t.Fatalf("abuse cases escaped the workspace: %v", entry.Name())
			}
		}
	}
}

func TestToolExecuteRejectsOversizedAndOverlongPayloads(t *testing.T) {
	handler := newTestRouter(t, config.Config{Provider: "mock"})
	oversized := `{"tool":"inspect_text","arguments":{"text":"` + strings.Repeat("a", 1<<20) + `"}}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost, "/v1/tools/execute", strings.NewReader(oversized),
	))
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "request_too_large") {
		t.Fatalf("oversized body returned %d body=%s", response.Code, response.Body)
	}

	overlong := `{"tool":"inspect_text","arguments":{"text":"` + strings.Repeat("b", 4097) + `"}}`
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost, "/v1/tools/execute", strings.NewReader(overlong),
	))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_tool_arguments") {
		t.Fatalf("overlong text returned %d body=%s", response.Code, response.Body)
	}
}
