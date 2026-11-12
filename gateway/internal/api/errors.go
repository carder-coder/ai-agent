// 本文件定义统一的 OpenAI 风格错误响应结构和编码方法。
package api

import (
	"encoding/json"
	"net/http"
)

type errorEnvelope struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Message string       `json:"message"`
	Type    string       `json:"type"`
	Code    string       `json:"code"`
	Details []fieldError `json:"details,omitempty"`
}

type fieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// writeError 将内部错误编码为统一的 OpenAI 风格 JSON envelope。
func writeError(w http.ResponseWriter, status int, message, errorType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(errorEnvelope{
		Error: apiError{Message: message, Type: errorType, Code: code},
	}); err != nil {
		http.Error(w, "failed to encode error response", http.StatusInternalServerError)
	}
}

// writeDetailedError 在统一错误 envelope 中返回可由调用方定位的字段原因。
func writeDetailedError(w http.ResponseWriter, status int, message, errorType, code string, details []fieldError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(errorEnvelope{
		Error: apiError{Message: message, Type: errorType, Code: code, Details: details},
	}); err != nil {
		http.Error(w, "failed to encode error response", http.StatusInternalServerError)
	}
}
