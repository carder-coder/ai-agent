// 本文件执行固定白名单命令，并对运行时间、输出大小和子进程环境做限制。
package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const (
	commandTimeout   = 3 * time.Second
	maxCommandOutput = 16 << 10
)

var ErrTimeout = errors.New("tool execution timed out")

// Executor 将工具名称映射到固定实现，绝不把请求内容拼接为 shell 命令。
type Executor struct {
	workspace string
}

type commandSpec struct {
	name string
	args []string
}

type cappedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

// NewExecutor 创建将命令限制在指定工作区的工具执行器。
func NewExecutor(workspace string) (*Executor, error) {
	if workspace == "" {
		workspace = "."
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve tool workspace: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect tool workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("tool workspace must be a directory")
	}
	return &Executor{workspace: absolute}, nil
}

// Execute 运行纯计算工具或固定只读命令，并返回有界的结构化结果。
func (e *Executor) Execute(ctx context.Context, name string, arguments map[string]any) (map[string]any, error) {
	if name != "workspace_status" {
		return Execute(name, arguments)
	}
	spec, err := workspaceStatusCommand(arguments)
	if err != nil {
		return nil, err
	}
	output, truncated, err := runCommand(ctx, e.workspace, spec, maxCommandOutput)
	if err != nil {
		return nil, err
	}
	return map[string]any{"output": output, "truncated": truncated}, nil
}

func workspaceStatusCommand(arguments map[string]any) (commandSpec, error) {
	format, _ := arguments["format"].(string)
	switch format {
	case "", "short":
		return commandSpec{name: "git", args: []string{"-c", "core.fsmonitor=false", "status", "--short"}}, nil
	case "branch":
		return commandSpec{name: "git", args: []string{"-c", "core.fsmonitor=false", "status", "--short", "--branch"}}, nil
	default:
		return commandSpec{}, fmt.Errorf("workspace_status format was not schema validated")
	}
}

func runCommand(parent context.Context, workspace string, spec commandSpec, outputLimit int) (string, bool, error) {
	ctx, cancel := context.WithTimeout(parent, commandTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", false, ErrTimeout
		}
		return "", false, err
	}

	buffer := &cappedBuffer{limit: outputLimit}
	command := exec.CommandContext(ctx, spec.name, spec.args...)
	command.Dir = workspace
	command.Env = safeCommandEnvironment()
	command.Stdout = buffer
	command.Stderr = buffer
	if err := command.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", buffer.truncatedValue(), ErrTimeout
		}
		return "", buffer.truncatedValue(), fmt.Errorf("run allowlisted command: %w", err)
	}
	return string(buffer.bytes()), buffer.truncatedValue(), nil
}

func (b *cappedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	available := b.limit - len(b.data)
	if available > len(value) {
		available = len(value)
	}
	if available > 0 {
		b.data = append(b.data, value[:available]...)
	}
	if available < len(value) {
		b.truncated = true
	}
	return len(value), nil
}

func (b *cappedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.data)
}

func (b *cappedBuffer) truncatedValue() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func safeCommandEnvironment() []string {
	keys := []string{"PATH", "SYSTEMROOT", "WINDIR"}
	environment := make([]string, 0, len(keys)+4)
	for _, key := range keys {
		if value, exists := os.LookupEnv(key); exists {
			environment = append(environment, key+"="+value)
		}
	}
	return append(environment,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	)
}
