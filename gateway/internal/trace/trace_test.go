// 本文件验证 trace JSONL 字段兼容性、ID 校验和防覆盖行为。
package trace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRecorderWritesReplayCompatibleJSONL(t *testing.T) {
	runID, err := NewID()
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	recorder, err := NewRecorder(t.TempDir(), runID, "POST /v1/chat/completions")
	if err != nil {
		t.Fatalf("NewRecorder() error = %v", err)
	}
	usage := Usage{In: 3, Out: 2, Total: 5}
	if err := recorder.RecordLLM(
		"mock-model", 5, 6, "stop", usage, 1, 0.0123, "按 usage 计费",
		Route{TaskType: "simple", ContextLength: 12, Reason: "simple_task"},
	); err != nil {
		t.Fatalf("RecordLLM() error = %v", err)
	}
	if err := recorder.End("completed", 1); err != nil {
		t.Fatalf("End() error = %v", err)
	}

	file, err := os.Open(recorder.Path())
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer file.Close()

	var events []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode trace event: %v", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if len(events) != 3 || events[0]["kind"] != "run_start" ||
		events[1]["kind"] != "llm" || events[2]["kind"] != "run_end" {
		t.Fatalf("unexpected trace events: %#v", events)
	}
	llmUsage, ok := events[1]["usage"].(map[string]any)
	if !ok || llmUsage["in"] != float64(3) || llmUsage["out"] != float64(2) ||
		events[1]["first_byte_ms"] != float64(5) || events[1]["attempts"] != float64(1) ||
		events[1]["cost_yuan"] != float64(0.0123) || events[1]["cost_note"] != "按 usage 计费" ||
		events[1]["route"] == nil {
		t.Fatalf("LLM event does not match replay schema: %#v", events[1])
	}
	endUsage, ok := events[2]["usage"].(map[string]any)
	if !ok || endUsage["total"] != float64(5) || events[2]["total_cost_yuan"] != float64(0.0123) {
		t.Fatalf("run_end event does not include totals: %#v", events[2])
	}
}

func TestNewRecorderRejectsUnsafeRunIDAndDuplicateTrace(t *testing.T) {
	directory := t.TempDir()
	if _, err := NewRecorder(directory, "../outside", "task"); err == nil {
		t.Fatal("NewRecorder() accepted an unsafe run ID")
	}
	runID, err := NewID()
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	first, err := NewRecorder(directory, runID, "task")
	if err != nil {
		t.Fatalf("NewRecorder() error = %v", err)
	}
	if err := first.End("completed", 0); err != nil {
		t.Fatalf("End() error = %v", err)
	}
	if _, err := NewRecorder(directory, runID, "task"); err == nil {
		t.Fatal("NewRecorder() overwrote an existing trace file")
	}
	if _, err := os.Stat(filepath.Join(directory, runID+".jsonl")); err != nil {
		t.Fatalf("trace file missing: %v", err)
	}
}
