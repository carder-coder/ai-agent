// 本文件实现 OpenAI 兼容聊天请求的校验、上游调用、错误映射和流式响应。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/cost"
	"github.com/carder-coder/ai-agent/gateway/internal/metrics"
	"github.com/carder-coder/ai-agent/gateway/internal/provider"
	"github.com/carder-coder/ai-agent/gateway/internal/retry"
	"github.com/carder-coder/ai-agent/gateway/internal/sse"
	"github.com/carder-coder/ai-agent/gateway/internal/trace"
)

const maxRequestBytes = 1 << 20

type chatMessage struct {
	Role     string          `json:"role"`
	Content  json.RawMessage `json:"content"`
	ToolCall json.RawMessage `json:"tool_calls"`
}

// chatHandler 校验聊天请求并调用 Provider；成功请求会记录兼容重放器的 trace。
func chatHandler(
	adapter provider.Adapter,
	configuredModel, traceDir string,
	collector *metrics.Metrics,
	prices cost.Prices,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "body: unable to read request", "invalid_request_error", "invalid_request")
			return
		}
		if len(body) > maxRequestBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "body: exceeds 1 MiB limit", "invalid_request_error", "request_too_large")
			return
		}
		fields, requestedModel, stream, validationError := validateChatRequest(body)
		if validationError != "" {
			writeError(w, http.StatusBadRequest, validationError, "invalid_request_error", "invalid_request")
			return
		}
		model := strings.TrimSpace(configuredModel)
		if model == "" {
			model = requestedModel
		}
		if err := setJSONField(fields, "model", model); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to prepare upstream request", "server_error", "internal_error")
			return
		}
		upstreamBody, err := json.Marshal(fields)
		if err != nil {
			writeError(w, http.StatusBadRequest, "body: must be valid JSON", "invalid_request_error", "invalid_request")
			return
		}

		started := time.Now()
		var usage provider.Usage
		success := false
		requestCost := 0.0
		costNote := "按 usage 计费，精度 0.0001 元"
		defer func() {
			collector.ObserveModel(
				model,
				time.Since(started),
				usage.InputTokens,
				usage.OutputTokens,
				usage.CachedTokens,
				requestCost,
				success,
			)
		}()
		recorder, traceErr := trace.NewRecorder(traceDir, trace.IDFromContext(r.Context()), "POST /v1/chat/completions")
		if traceErr != nil {
			log.Printf("创建请求 trace 失败：%v", traceErr)
		}
		stopReason := "internal_error"
		if recorder != nil {
			defer func() {
				if err := recorder.End(stopReason, 1); err != nil {
					log.Printf("写入请求 trace 结束事件失败：%v", err)
				}
			}()
		}

		result, err := adapter.Chat(r.Context(), upstreamBody, stream)
		if err != nil {
			if r.Context().Err() != nil {
				stopReason = "cancelled"
				return
			}
			if errors.Is(err, provider.ErrAuthentication) {
				stopReason = "authentication_error"
				writeError(w, http.StatusUnauthorized, "provider authentication failed", "authentication_error", "invalid_api_key")
				return
			}
			if errors.Is(err, retry.ErrCircuitOpen) {
				stopReason = "circuit_open"
				writeError(w, http.StatusServiceUnavailable, "provider circuit breaker is open", "server_error", "circuit_open")
				return
			}
			if provider.IsTimeout(err) {
				stopReason = "timeout"
				writeError(w, http.StatusGatewayTimeout, "upstream request timed out", "timeout", "upstream_timeout")
				return
			}
			stopReason = "upstream_error"
			writeError(w, http.StatusBadGateway, "upstream request failed", "upstream_error", "upstream_error")
			return
		}
		if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
			stopReason = "upstream_error"
			writeUpstreamError(w, result)
			return
		}
		if stream {
			if result.Stream == nil {
				stopReason = "invalid_upstream_response"
				writeError(w, http.StatusBadGateway, "upstream did not return a stream", "upstream_error", "invalid_upstream_response")
				return
			}
			firstByte, finishReason, completed := writeStreamResponse(w, r, result.Stream, started)
			if completed {
				stopReason = "completed"
			} else {
				stopReason = "stream_interrupted"
			}
			if finishReason == "" {
				finishReason = "stop"
			}
			usage = result.Stream.Usage()
			success = completed
			requestCost, err = cost.Calculate(usage, prices)
			if err != nil {
				success = false
				costNote = "计费失败: " + err.Error()
				log.Printf("计算流式请求成本失败：%v", err)
			}
			recordTraceLLM(recorder, model, started, firstByte, finishReason, usage, result.Attempts, requestCost, costNote)
			return
		}
		var response map[string]json.RawMessage
		if err := json.Unmarshal(result.Body, &response); err != nil || response == nil {
			stopReason = "invalid_upstream_response"
			writeError(w, http.StatusBadGateway, "upstream returned an invalid JSON response", "upstream_error", "invalid_upstream_response")
			return
		}
		usage = result.Usage
		requestCost, err = cost.Calculate(usage, prices)
		if err != nil {
			stopReason = "invalid_usage"
			writeError(w, http.StatusBadGateway, "provider returned invalid token usage", "upstream_error", "invalid_usage")
			return
		}
		recordTraceLLM(
			recorder,
			model,
			started,
			time.Since(started),
			responseFinishReason(result.Body),
			usage,
			result.Attempts,
			requestCost,
			costNote,
		)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, bytes.NewReader(append(result.Body, '\n'))); err != nil {
			stopReason = "client_disconnected"
			log.Printf("写入聊天响应失败：%v", err)
			return
		}
		stopReason = "completed"
		success = true
	}
}

// recordTraceLLM 将 Provider 用量和延迟写入当前请求的 trace。
func recordTraceLLM(
	recorder *trace.Recorder,
	model string,
	started time.Time,
	firstByte time.Duration,
	finishReason string,
	usage provider.Usage,
	attempts int,
	costYuan float64,
	costNote string,
) {
	if recorder == nil {
		return
	}
	err := recorder.RecordLLM(
		model,
		firstByte.Milliseconds(),
		time.Since(started).Milliseconds(),
		finishReason,
		trace.Usage{
			In:     usage.InputTokens,
			Out:    usage.OutputTokens,
			Total:  usage.TotalTokens,
			Cached: usage.CachedTokens,
		},
		attempts,
		costYuan,
		costNote,
	)
	if err != nil {
		log.Printf("写入请求 trace 模型事件失败：%v", err)
	}
}

// responseFinishReason 从非流式 Chat Completions 响应中提取结束原因。
func responseFinishReason(body []byte) string {
	var response struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &response); err == nil && len(response.Choices) > 0 &&
		response.Choices[0].FinishReason != "" {
		return response.Choices[0].FinishReason
	}
	return "stop"
}

// writeStreamResponse 将上游 chunk 逐个写入 SSE，并返回首字节耗时、结束原因和完成状态。
func writeStreamResponse(w http.ResponseWriter, r *http.Request, stream provider.ChunkStream, started time.Time) (firstByte time.Duration, finishReason string, completed bool) {
	defer func() {
		if err := stream.Close(); err != nil {
			log.Printf("关闭 provider 流失败：%v", err)
		}
	}()
	writer, err := sse.NewWriter(w, r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this server", "server_error", "streaming_unsupported")
		return 0, "", false
	}
	if err := writer.Start(); err != nil {
		if r.Context().Err() == nil {
			writeError(w, http.StatusInternalServerError, "failed to start SSE response", "server_error", "stream_start_failed")
		}
		return 0, "", false
	}

	type nextResult struct {
		chunk provider.Chunk
		err   error
	}
	streamCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	events := make(chan nextResult, 1)
	// 单独读取上游流，使处理协程能响应客户端取消并停止等待下一块数据。
	go func() {
		defer close(events)
		for {
			chunk, err := stream.Next()
			select {
			case events <- nextResult{chunk: chunk, err: err}:
			case <-streamCtx.Done():
				return
			}
			if err != nil || chunk.Done {
				return
			}
		}
	}()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return firstByte, finishReason, false
		case <-heartbeat.C:
			if err := writer.Heartbeat(); err != nil {
				if r.Context().Err() == nil {
					log.Printf("写 SSE 心跳失败：%v", err)
				}
				return firstByte, finishReason, false
			}
		case next, ok := <-events:
			if !ok {
				writeStreamDone(writer)
				return firstByte, finishReason, true
			}
			if errors.Is(next.err, io.EOF) {
				writeStreamDone(writer)
				return firstByte, finishReason, true
			}
			if next.err != nil {
				if r.Context().Err() == nil {
					log.Printf("读取 provider SSE 流失败：%v", next.err)
					if err := writeStreamError(writer); err != nil {
						log.Printf("写 SSE 错误事件失败：%v", err)
					}
				}
				return firstByte, finishReason, false
			}
			if firstByte == 0 && len(next.chunk.Data) > 0 {
				firstByte = time.Since(started)
			}
			if reason := streamFinishReason(next.chunk.Data); reason != "" {
				finishReason = reason
			}
			if err := writer.WriteData(next.chunk.Data); err != nil {
				if r.Context().Err() == nil {
					log.Printf("写 SSE 数据块失败：%v", err)
				}
				return firstByte, finishReason, false
			}
			if next.chunk.Done {
				return firstByte, finishReason, true
			}
		}
	}
}

func streamFinishReason(body []byte) string {
	var response struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &response); err == nil && len(response.Choices) > 0 &&
		response.Choices[0].FinishReason != nil {
		return *response.Choices[0].FinishReason
	}
	return ""
}

// writeStreamError 在 SSE 已开始后发送 OpenAI 风格错误事件及结束标记。
func writeStreamError(writer *sse.Writer) error {
	body, marshalErr := json.Marshal(errorEnvelope{
		Error: apiError{
			Message: "upstream stream failed",
			Type:    "upstream_error",
			Code:    "stream_error",
		},
	})
	if marshalErr != nil {
		return marshalErr
	}
	if writeErr := writer.WriteData(body); writeErr != nil {
		return writeErr
	}
	return writer.WriteData([]byte("[DONE]"))
}

func writeStreamDone(writer *sse.Writer) {
	if err := writer.WriteData([]byte("[DONE]")); err != nil {
		log.Printf("写 SSE 结束标记失败：%v", err)
	}
}

// validateChatRequest 检查 OpenAI 兼容请求的必要字段，并返回可继续转发的原始 JSON 字段。
func validateChatRequest(body []byte) (map[string]json.RawMessage, string, bool, string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, "", false, "body: must be a JSON object"
	}
	modelRaw, exists := fields["model"]
	if !exists {
		return nil, "", false, "model: is required"
	}
	var model string
	if err := json.Unmarshal(modelRaw, &model); err != nil || strings.TrimSpace(model) == "" {
		return nil, "", false, "model: must be a non-empty string"
	}

	messagesRaw, exists := fields["messages"]
	if !exists {
		return nil, "", false, "messages: is required"
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil || len(messages) == 0 {
		return nil, "", false, "messages: must be a non-empty array"
	}
	for i, raw := range messages {
		var message chatMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, "", false, fmt.Sprintf("messages[%d]: must be an object", i)
		}
		switch message.Role {
		case "system", "developer", "user", "assistant", "tool", "function":
		default:
			return nil, "", false, fmt.Sprintf("messages[%d].role: unsupported role", i)
		}
		hasToolCalls := len(message.ToolCall) > 0 && string(message.ToolCall) != "null"
		hasContent := len(message.Content) > 0 && string(message.Content) != "null"
		if hasContent && !isStringOrArray(message.Content) {
			return nil, "", false, fmt.Sprintf("messages[%d].content: must be a string or array", i)
		}
		if !hasContent && !(message.Role == "assistant" && hasToolCalls) {
			return nil, "", false, fmt.Sprintf("messages[%d].content: is required", i)
		}
	}

	var stream bool
	if streamRaw, exists := fields["stream"]; exists {
		if err := json.Unmarshal(streamRaw, &stream); err != nil {
			return nil, "", false, "stream: must be a boolean"
		}
	}
	return fields, model, stream, ""
}

func setJSONField(fields map[string]json.RawMessage, key, value string) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	fields[key] = encoded
	return nil
}

// writeUpstreamError 将上游 HTTP 状态和错误消息映射为网关统一错误响应。
func writeUpstreamError(w http.ResponseWriter, result provider.Response) {
	status := http.StatusBadGateway
	errorType, code := "upstream_error", "upstream_error"
	switch result.StatusCode {
	case http.StatusUnauthorized:
		status, errorType, code = http.StatusUnauthorized, "authentication_error", "invalid_api_key"
	case http.StatusTooManyRequests:
		status, errorType, code = http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded"
		if retryAfter := validRetryAfter(result.RetryAfter); retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
	}
	message := "upstream returned an error"
	var response struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(result.Body, &response) == nil && strings.TrimSpace(response.Error.Message) != "" {
		message = response.Error.Message
	}
	writeError(w, status, message, errorType, code)
}

// validRetryAfter 仅透传符合秒数或 HTTP 日期格式的 Retry-After 值。
func validRetryAfter(value string) string {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return value
	}
	if _, err := http.ParseTime(value); err == nil {
		return value
	}
	return ""
}

func isStringOrArray(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	return len(value) > 0 && (value[0] == '"' || value[0] == '[')
}
