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
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
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
