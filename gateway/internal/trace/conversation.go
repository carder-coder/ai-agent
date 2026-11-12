// 本文件持久化会话 trace 与工具子调用的关联，便于按父 trace ID 回放。
package trace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const maxConversationChildren = 1000

var conversationIndexMu sync.Mutex

// HasTrace 检查指定 ID 是否对应现有普通 trace 文件。
func HasTrace(directory, id string) (bool, error) {
	if !ValidID(id) {
		return false, errors.New("invalid trace ID")
	}
	info, err := os.Stat(filepath.Join(directory, id+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect parent trace: %w", err)
	}
	return info.Mode().IsRegular(), nil
}

// LinkConversation 将子调用 trace ID 关联到已存在的父对话 trace。
func LinkConversation(directory, conversationID, childID string) error {
	if !ValidID(conversationID) || !ValidID(childID) {
		return errors.New("invalid conversation or child trace ID")
	}
	exists, err := HasTrace(directory, conversationID)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("parent conversation trace does not exist")
	}

	conversationIndexMu.Lock()
	defer conversationIndexMu.Unlock()
	indexDirectory := filepath.Join(directory, "conversations")
	if err := os.MkdirAll(indexDirectory, 0o700); err != nil {
		return fmt.Errorf("create conversation index directory: %w", err)
	}
	path := filepath.Join(indexDirectory, conversationID+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open conversation index: %w", err)
	}
	line := []byte(childID + "\n")
	n, writeErr := file.Write(line)
	closeErr := file.Close()
	if writeErr == nil && n != len(line) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil || closeErr != nil {
		return errors.Join(
			wrapTraceCleanupError("write conversation index", writeErr),
			wrapTraceCleanupError("close conversation index", closeErr),
		)
	}
	return nil
}

// ConversationChildren 按登记顺序返回对话关联的子调用 trace ID。
func ConversationChildren(directory, conversationID string) ([]string, error) {
	if !ValidID(conversationID) {
		return nil, errors.New("invalid conversation trace ID")
	}
	conversationIndexMu.Lock()
	defer conversationIndexMu.Unlock()
	path := filepath.Join(directory, "conversations", conversationID+".jsonl")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open conversation index: %w", err)
	}
	const maxIndexBytes = 64 << 10
	data, readErr := io.ReadAll(io.LimitReader(file, maxIndexBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, fmt.Errorf("read conversation index: %w", errors.Join(readErr, closeErr))
	}
	if len(data) > maxIndexBytes {
		return nil, errors.New("conversation index exceeds size limit")
	}
	children := make([]string, 0)
	seen := make(map[string]struct{})
	for _, childID := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if childID == "" {
			continue
		}
		if !ValidID(childID) {
			return nil, errors.New("conversation index contains an invalid trace ID")
		}
		if _, exists := seen[childID]; exists {
			continue
		}
		seen[childID] = struct{}{}
		children = append(children, childID)
		if len(children) > maxConversationChildren {
			return nil, errors.New("conversation has too many child traces")
		}
	}
	return children, nil
}
