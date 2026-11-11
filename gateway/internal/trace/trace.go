package trace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Usage struct {
	In     int `json:"in"`
	Out    int `json:"out"`
	Total  int `json:"total"`
	Cached int `json:"cached"`
}

type Recorder struct {
	mu     sync.Mutex
	file   *os.File
	usage  Usage
	closed bool
}

func NewRecorder(directory, runID, task string) (*Recorder, error) {
	if runID == "" {
		var err error
		runID, err = NewID()
		if err != nil {
			return nil, fmt.Errorf("generate trace run ID: %w", err)
		}
	}
	if !validRunID(runID) {
		return nil, errors.New("trace run ID must be 32 lowercase hexadecimal characters")
	}
	if strings.TrimSpace(directory) == "" {
		directory = "traces"
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create trace directory: %w", err)
	}
	path := filepath.Join(directory, runID+".jsonl")
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

func (r *Recorder) Path() string {
	return r.file.Name()
}

func (r *Recorder) RecordLLM(
	model string,
	firstByteMS int64,
	latencyMS int64,
	finishReason string,
	usage Usage,
	attempts int,
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
	return r.writeEventLocked("llm", map[string]any{
		"step":          1,
		"model":         model,
		"first_byte_ms": firstByteMS,
		"latency_ms":    latencyMS,
		"finish_reason": finishReason,
		"usage":         usage,
		"attempts":      attempts,
		"cost_yuan":     0.0,
		"cost_note":     "待计费",
		"tool_calls":    []string{},
	})
}

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
		"total_cost_yuan": 0.0,
		"cost_note":       "待计费",
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

func validRunID(runID string) bool {
	if len(runID) != 32 {
		return false
	}
	for _, value := range runID {
		if !((value >= '0' && value <= '9') || (value >= 'a' && value <= 'f')) {
			return false
		}
	}
	return true
}
