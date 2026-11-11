// 本文件生成请求 trace ID，并提供将 ID 存取于 context 的辅助函数。
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type contextKey struct{}

// NewID 使用密码学安全随机数生成 32 位小写十六进制 trace ID。
func NewID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

// WithID 将请求 trace ID 写入 context，供处理链后续读取。
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// IDFromContext 返回 context 中的 trace ID；未设置时返回空字符串。
func IDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}
