// 本文件将网关模型调用记录为与 Python replay.py 兼容的 JSONL 事件。
package trace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Usage 使用 replay.py 约定的字段名记录输入、输出、总量和缓存 token。
type Usage struct {
	In     int `json:"in"`
	Out    int `json:"out"`
	Total  int `json:"total"`
	Cached int `json:"cached"`
}

// Route 记录模型分级路由的输入与选择依据。
type Route struct {
	TaskType      string `json:"task_type"`
	ContextLength int    `json:"context_length"`
	Reason        string `json:"reason"`
}

// ToolEvent 保存工具调用的审计摘要，不应包含凭证或未脱敏的敏感文本。
type ToolEvent struct {
	Caller         string `json:"caller"`
	ConversationID string `json:"conversation_id,omitempty"`
	TraceID        string `json:"trace_id"`
	Name           string `json:"tool"`
	Parameters     any    `json:"parameters"`
	ResultSummary  any    `json:"result_summary"`
	DurationMS     int64  `json:"duration_ms"`
	Status         string `json:"status"`
}

// Recorder 将单次网关请求的模型调用和汇总写入 JSONL 文件。
type Recorder struct {
	mu        sync.Mutex
	file      *os.File
	usage     Usage
	costUnits int64
	closed    bool
}

// NewRecorder 在 directory 中独占创建 trace 文件并写入 run_start 事件。
func NewRecorder(directory, runID, task string) (*Recorder, error) {
	if runID == "" {
		var err error
		runID, err = NewID()
		if err != nil {
			return nil, fmt.Errorf("generate trace run ID: %w", err)
		}
	}
	if !ValidID(runID) {
		return nil, errors.New("trace run ID must be 32 lowercase hexadecimal characters")
	}
	if strings.TrimSpace(directory) == "" {
		directory = "traces"
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create trace directory: %w", err)
	}
	path := filepath.Join(directory, runID+".jsonl")
	// 独占创建避免同一 trace ID 覆盖已有记录；文件仅当前用户可读写。
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create trace file: %w", err)
	}
	recorder := &Recorder{file: file}
	if err := recorder.writeEvent("run_start", map[string]any{
		"run_id":       runID,
		"resumed_from": nil,
		"task":         task,
	}); err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(path)
		return nil, errors.Join(
			err,
			wrapTraceCleanupError("close incomplete trace file", closeErr),
			wrapTraceCleanupError("remove incomplete trace file", removeErr),
		)
	}
	return recorder, nil
}

// Path 返回当前 recorder 正在写入的 trace 文件路径。
func (r *Recorder) Path() string {
	return r.file.Name()
}

// RecordLLM 累计模型用量并写入与 Python replay.py 对齐的 llm 事件。
func (r *Recorder) RecordLLM(
	model string,
	firstByteMS int64,
	latencyMS int64,
	finishReason string,
	usage Usage,
	attempts int,
	costYuan float64,
	costNote string,
	route Route,
) error {
	if attempts < 1 {
		attempts = 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usage.In += usage.In
	r.usage.Out += usage.Out
	r.usage.Total += usage.Total
	r.usage.Cached += usage.Cached
	r.costUnits += int64(math.Round(costYuan * 10_000))
	return r.writeEventLocked("llm", map[string]any{
		"step":          1,
		"model":         model,
		"first_byte_ms": firstByteMS,
		"latency_ms":    latencyMS,
		"finish_reason": finishReason,
		"usage":         usage,
		"attempts":      attempts,
		"cost_yuan":     costYuan,
		"cost_note":     costNote,
		"route":         route,
		"tool_calls":    []string{},
	})
}

// RecordCache 累计缓存命中的 usage 与成本，但不伪装成一次模型调用。
func (r *Recorder) RecordCache(
	model, cacheID string,
	similarity float64,
	usage Usage,
	costYuan float64,
	route Route,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usage.In += usage.In
	r.usage.Out += usage.Out
	r.usage.Total += usage.Total
	r.usage.Cached += usage.Cached
	r.costUnits += int64(math.Round(costYuan * 10_000))
	return r.writeEventLocked("cache", map[string]any{
		"step":       1,
		"model":      model,
		"cache_id":   cacheID,
		"similarity": similarity,
		"usage":      usage,
		"cost_yuan":  costYuan,
		"route":      route,
	})
}

// RecordBudgetRejection 留下预算预检拒绝的估算成本与额度，便于排查限额命中。
func (r *Recorder) RecordBudgetRejection(estimate, dailyLimit, requestLimit float64, reason string) error {
	return r.writeEvent("budget_rejected", map[string]any{
		"estimated_cost_yuan": estimate,
		"daily_limit_yuan":    dailyLimit,
		"request_limit_yuan":  requestLimit,
		"reason":              reason,
	})
}

// RecordTool 写入包含调用方、脱敏参数、结果摘要、耗时与状态的工具审计事件。
func (r *Recorder) RecordTool(event ToolEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recordToolLocked("tool", event)
}

// RecordToolStart 在执行副作用前持久化调用方与脱敏参数，审计存储不可用时可阻止执行。
func (r *Recorder) RecordToolStart(event ToolEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recordToolLocked("tool_start", event)
}

func (r *Recorder) recordToolLocked(kind string, event ToolEvent) error {
	return r.writeEventLocked(kind, map[string]any{
		"caller":          event.Caller,
		"conversation_id": event.ConversationID,
		"trace_id":        event.TraceID,
		"tool":            event.Name,
		"parameters":      event.Parameters,
		"result_summary":  event.ResultSummary,
		"duration_ms":     event.DurationMS,
		"status":          event.Status,
	})
}

// End 写入包含累计用量的 run_end 事件并关闭 trace 文件。
func (r *Recorder) End(stopReason string, steps int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("trace recorder is already closed")
	}
	eventErr := r.writeEventLocked("run_end", map[string]any{
		"stop_reason":     stopReason,
		"steps":           steps,
		"usage":           r.usage,
		"total_cost_yuan": float64(r.costUnits) / 10_000,
		"cost_note":       "按 usage 计费，精度 0.0001 元",
	})
	r.closed = true
	closeErr := r.file.Close()
	if eventErr != nil && closeErr != nil {
		return errors.Join(eventErr, fmt.Errorf("close trace file: %w", closeErr))
	}
	if eventErr != nil {
		return eventErr
	}
	if closeErr != nil {
		return fmt.Errorf("close trace file: %w", closeErr)
	}
	return nil
}

func (r *Recorder) writeEvent(kind string, payload map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeEventLocked(kind, payload)
}

func (r *Recorder) writeEventLocked(kind string, payload map[string]any) error {
	if r.closed {
		return errors.New("trace recorder is already closed")
	}
	event := make(map[string]any, len(payload)+2)
	event["ts"] = time.Now().Format(time.RFC3339Nano)
	event["kind"] = kind
	for key, value := range payload {
		event[key] = value
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode trace event %q: %w", kind, err)
	}
	encoded = append(encoded, '\n')
	if n, err := r.file.Write(encoded); err != nil {
		return fmt.Errorf("write trace event %q: %w", kind, err)
	} else if n != len(encoded) {
		return fmt.Errorf("write trace event %q: %w", kind, io.ErrShortWrite)
	}
	return nil
}

func wrapTraceCleanupError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
