package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/carder-coder/ai-agent/gateway/internal/provider"
	"github.com/carder-coder/ai-agent/gateway/internal/provider/doubao"
)

const maxRequestBytes = 1 << 20

type chatMessage struct {
	Role     string          `json:"role"`
	Content  json.RawMessage `json:"content"`
	ToolCall json.RawMessage `json:"tool_calls"`
}

func chatHandler(adapter provider.Adapter, configuredModel string) http.HandlerFunc {
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
		if stream {
			writeError(w, http.StatusNotImplemented, "stream: streaming is not implemented yet", "server_error", "not_implemented")
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

		result, err := adapter.Chat(r.Context(), upstreamBody, false)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			if errors.Is(err, doubao.ErrMissingAPIKey) {
				writeError(w, http.StatusUnauthorized, err.Error(), "authentication_error", "invalid_api_key")
				return
			}
			if doubao.IsTimeout(err) {
				writeError(w, http.StatusGatewayTimeout, "upstream request timed out", "timeout", "upstream_timeout")
				return
			}
			writeError(w, http.StatusBadGateway, "upstream request failed", "upstream_error", "upstream_error")
			return
		}
		if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
			writeUpstreamError(w, result)
			return
		}
		var response map[string]json.RawMessage
		if err := json.Unmarshal(result.Body, &response); err != nil || response == nil {
			writeError(w, http.StatusBadGateway, "upstream returned an invalid JSON response", "upstream_error", "invalid_upstream_response")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, bytes.NewReader(append(result.Body, '\n')))
	}
}

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
