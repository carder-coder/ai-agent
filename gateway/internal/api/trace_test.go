package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
)

func TestChatWritesReplayableTraceForNormalAndStreamingResponses(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-streaming"
		streamField := "false"
		if stream {
			name = "streaming"
			streamField = "true"
		}
		t.Run(name, func(t *testing.T) {
			traceDir := t.TempDir()
			handler := newTestRouter(t, config.Config{Provider: "mock", TraceDir: traceDir})
			body := `{"model":"mock-model","messages":[{"role":"user","content":"private prompt"}],"stream":` + streamField + `}`
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}

			files, err := filepath.Glob(filepath.Join(traceDir, "*.jsonl"))
			if err != nil {
				t.Fatalf("find trace: %v", err)
			}
			if len(files) != 1 {
				t.Fatalf("trace files = %d, want 1", len(files))
			}
			content, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatalf("read trace: %v", err)
			}
			if strings.Contains(string(content), "private prompt") || strings.Contains(string(content), "Authorization") {
				t.Fatalf("trace contains request data or credentials: %s", content)
			}
			events := make([]map[string]any, 0, 3)
			scanner := bufio.NewScanner(strings.NewReader(string(content)))
			for scanner.Scan() {
				var event map[string]any
				if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
					t.Fatalf("decode trace event: %v", err)
				}
				events = append(events, event)
			}
			if err := scanner.Err(); err != nil {
				t.Fatalf("scan trace: %v", err)
			}
			if len(events) != 3 || events[0]["kind"] != "run_start" ||
				events[1]["kind"] != "llm" || events[2]["kind"] != "run_end" {
				t.Fatalf("unexpected trace events: %#v", events)
			}
			if events[0]["run_id"] != response.Header().Get("X-Trace-ID") ||
				events[2]["stop_reason"] != "completed" {
				t.Fatalf("trace IDs or completion status do not match response: %#v", events)
			}
			usage, ok := events[1]["usage"].(map[string]any)
			if !ok || usage["total"] != float64(5) || events[1]["attempts"] != float64(1) {
				t.Fatalf("LLM usage or attempts missing from trace: %#v", events[1])
			}
			if events[1]["finish_reason"] != "stop" {
				t.Fatalf("finish_reason = %v, want stop", events[1]["finish_reason"])
			}
			if _, ok := events[1]["first_byte_ms"]; !ok {
				t.Fatalf("first_byte_ms missing from trace: %#v", events[1])
			}
		})
	}
}
