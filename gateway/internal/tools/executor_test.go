package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceStatusCommandUsesFixedArguments(t *testing.T) {
	tests := []struct {
		format string
		args   []string
	}{
		{format: "", args: []string{"-c", "core.fsmonitor=false", "status", "--short"}},
		{format: "short", args: []string{"-c", "core.fsmonitor=false", "status", "--short"}},
		{format: "branch", args: []string{"-c", "core.fsmonitor=false", "status", "--short", "--branch"}},
	}
	for _, test := range tests {
		spec, err := workspaceStatusCommand(map[string]any{"format": test.format})
		if err != nil {
			t.Fatalf("workspaceStatusCommand(%q): %v", test.format, err)
		}
		if spec.name != "git" || strings.Join(spec.args, " ") != strings.Join(test.args, " ") {
			t.Fatalf("workspaceStatusCommand(%q) = %+v", test.format, spec)
		}
	}
	if _, err := workspaceStatusCommand(map[string]any{"format": "short; whoami"}); err == nil {
		t.Fatal("workspaceStatusCommand accepted non-whitelisted arguments")
	}
}

func TestCappedBufferLimitsCombinedOutput(t *testing.T) {
	buffer := &cappedBuffer{limit: 5}
	if _, err := buffer.Write([]byte("123")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := buffer.Write([]byte("4567")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if got := string(buffer.bytes()); got != "12345" || !buffer.truncatedValue() {
		t.Fatalf("buffer = %q, truncated = %v", got, buffer.truncatedValue())
	}
}

func TestRunCommandHonorsExpiredRequestContext(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, _, err := runCommand(ctx, ".", commandSpec{name: "git"}, maxCommandOutput)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("runCommand() error = %v, want ErrTimeout", err)
	}
}

func TestExecutorRunsWorkspaceStatusInConfiguredDirectory(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	workspace := t.TempDir()
	initRepo := exec.Command(gitPath, "init", "-q")
	initRepo.Dir = workspace
	if output, err := initRepo.CombinedOutput(); err != nil {
		t.Fatalf("initialize temporary repository: %v: %s", err, output)
	}

	executor, err := NewExecutor(workspace, false)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	result, err := executor.Execute(context.Background(), "workspace_status", map[string]any{"format": "branch"})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	output, ok := result["output"].(string)
	if !ok || !strings.Contains(output, "## ") || result["truncated"] != false {
		t.Fatalf("unexpected workspace status result: %#v", result)
	}
}

func TestSafeCommandEnvironmentOmitsProviderCredentials(t *testing.T) {
	t.Setenv("ARK_API_KEY", "do-not-inherit")
	for _, entry := range safeCommandEnvironment() {
		if strings.HasPrefix(entry, "ARK_API_KEY=") {
			t.Fatal("provider credentials were inherited by the subprocess")
		}
	}
}

func TestNewExecutorRejectsUnresolvableWorkspace(t *testing.T) {
	file := filepath.Join(t.TempDir(), "workspace-file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("create workspace test file: %v", err)
	}
	if _, err := NewExecutor(file, false); err == nil {
		t.Fatal("NewExecutor accepted a file as the tool workspace")
	}
}
