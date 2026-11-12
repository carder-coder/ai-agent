// 本文件负责脱敏工具参数与结果，避免审计 trace 保存原始提示或凭证。
package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

func auditParameters(raw json.RawMessage) any {
	var parameters any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&parameters); err != nil {
		return map[string]any{"redacted": true, "reason": "unparseable"}
	}
	return redactAuditValue(parameters, "")
}

func auditResultSummary(result map[string]any) any {
	return redactAuditValue(result, "")
}

func redactAuditValue(value any, field string) any {
	if sensitiveAuditField(field) {
		encoded, err := json.Marshal(value)
		if err != nil {
			return map[string]any{"redacted": true}
		}
		digest := sha256.Sum256(encoded)
		return map[string]any{
			"redacted": true,
			"sha256":   hex.EncodeToString(digest[:]),
			"bytes":    len(encoded),
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, child := range typed {
			redacted[key] = redactAuditValue(child, key)
		}
		return redacted
	case []any:
		redacted := make([]any, len(typed))
		for index, child := range typed {
			redacted[index] = redactAuditValue(child, field)
		}
		return redacted
	default:
		return value
	}
}

func sensitiveAuditField(field string) bool {
	name := strings.ToLower(field)
	for _, marker := range []string{"content", "text", "prompt", "message", "output", "token", "secret", "password", "authorization", "api_key"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}
