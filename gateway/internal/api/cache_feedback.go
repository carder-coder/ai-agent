// 本文件处理语义缓存答案的正误反馈，并将反馈结果纳入网关指标。
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	semanticcache "github.com/carder-coder/ai-agent/gateway/internal/cache"
	"github.com/carder-coder/ai-agent/gateway/internal/metrics"
)

const maxFeedbackRequestBytes = 4 << 10

type cacheFeedbackRequest struct {
	TraceID string `json:"trace_id"`
	Correct *bool  `json:"correct"`
}

// cacheFeedbackHandler 接受每个语义缓存命中 trace 一次 correct/incorrect 反馈。
func cacheFeedbackHandler(store *semanticcache.Store, collector *metrics.Metrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxFeedbackRequestBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "body: unable to read request", "invalid_request_error", "invalid_request")
			return
		}
		if len(body) > maxFeedbackRequestBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "body: exceeds 4 KiB limit", "invalid_request_error", "request_too_large")
			return
		}
		var input cacheFeedbackRequest
		if err := json.Unmarshal(body, &input); err != nil || !validTraceID(input.TraceID) || input.Correct == nil {
			writeError(w, http.StatusBadRequest, "body: requires a valid trace_id and boolean correct", "invalid_request_error", "invalid_request")
			return
		}
		if err := store.Feedback(input.TraceID); err != nil {
			switch {
			case errors.Is(err, semanticcache.ErrFeedbackNotFound):
				writeError(w, http.StatusNotFound, "semantic cache hit was not found or has expired", "invalid_request_error", "cache_feedback_not_found")
			case errors.Is(err, semanticcache.ErrFeedbackAlreadyHandled):
				writeError(w, http.StatusConflict, "semantic cache feedback was already submitted", "invalid_request_error", "cache_feedback_duplicate")
			default:
				writeError(w, http.StatusServiceUnavailable, "semantic cache feedback capacity reached", "server_error", "cache_feedback_unavailable")
			}
			return
		}
		collector.ObserveCacheFeedback(*input.Correct)
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, "{\"ok\":true}\n"); err != nil {
			return
		}
	}
}

func validTraceID(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
