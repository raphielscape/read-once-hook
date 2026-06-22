package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testBin = "/tmp/read-once-testbin"

func TestMain(m *testing.M) {
	// Build binary for subprocess tests.
	if err := exec.Command("go", "build", "-o", testBin, ".").Run(); err != nil {
		os.Exit(1)
	}
	os.Exit(m.Run())
}

type hookResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func runHook(t *testing.T, input map[string]any, env ...string) hookResult {
	t.Helper()
	inputJSON, _ := json.Marshal(input)

	cmd := exec.Command(testBin, "hook")
	cmd.Stdin = strings.NewReader(string(inputJSON))
	cmd.Env = append(os.Environ(), env...)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run hook: %v", err)
		}
	}
	return hookResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}
}

func makeInput(toolName, command, sessionID, cwd string) map[string]any {
	return map[string]any{
		"tool_name":  toolName,
		"tool_input": map[string]any{"command": command},
		"session_id": sessionID,
		"cwd":        cwd,
	}
}

// --- First read (cache miss) ---

func TestHookFirstReadPassesThrough(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("content"), 0644)

	r := runHook(t, makeInput("Bash", "cat file.txt", "first-read-test", tmp),
		"READ_ONCE_HOME="+tmp)
	if r.exitCode != 0 {
		t.Errorf("expected exit 0, got %d (stdout=%q stderr=%q)", r.exitCode, r.stdout, r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("expected empty stdout, got %q", r.stdout)
	}
}

// --- Second read: warn mode (Claude) ---

func TestHookSecondReadWarnClaude(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("content"), 0644)

	session := "warn-claude-test"
	// First read.
	runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp)
	// Second read — should emit JSON advisory.
	r := runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_MODE=warn")
	if r.exitCode != 0 {
		t.Errorf("expected exit 0 for warn, got %d", r.exitCode)
	}
	if !strings.Contains(r.stdout, "permissionDecision") {
		t.Errorf("expected JSON advisory, got stdout=%q", r.stdout)
	}
}

// --- Second read: deny mode (Codex) ---

func TestHookSecondReadDenyCodex(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("content"), 0644)

	session := "deny-codex-test"
	runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp)
	r := runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_CLIENT=codex", "READ_ONCE_MODE=deny")
	if r.exitCode != 2 {
		t.Errorf("expected exit 2 for deny, got %d (stderr=%q)", r.exitCode, r.stderr)
	}
	if !strings.Contains(r.stderr, "already in context") {
		t.Errorf("expected 'already in context' in stderr, got %q", r.stderr)
	}
}

// --- Second read: deny mode (Claude) ---

func TestHookSecondReadDenyClaude(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("content"), 0644)

	session := "deny-claude-test"
	runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp)
	r := runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_MODE=deny")
	if r.exitCode != 2 {
		t.Errorf("expected exit 2 for deny, got %d (stderr=%q)", r.exitCode, r.stderr)
	}
	if !strings.Contains(r.stderr, "already in context") {
		t.Errorf("expected 'already in context' in stderr, got %q", r.stderr)
	}
}

// --- Disabled hook ---

func TestHookDisabled(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("content"), 0644)

	session := "disabled-test"
	runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp)
	r := runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_DISABLED=1")
	if r.exitCode != 0 {
		t.Errorf("expected exit 0 when disabled, got %d", r.exitCode)
	}
	if r.stdout != "" || r.stderr != "" {
		t.Errorf("expected empty output when disabled, got stdout=%q stderr=%q", r.stdout, r.stderr)
	}
}

// --- Empty stdin ---

func TestHookEmptyStdin(t *testing.T) {
	tmp := t.TempDir()
	cmd := exec.Command(testBin, "hook")
	cmd.Stdin = strings.NewReader("")
	cmd.Env = append(os.Environ(), "READ_ONCE_HOME="+tmp)
	err := cmd.Run()
	if err != nil {
		t.Errorf("expected exit 0 for empty stdin, got %v", err)
	}
}

// --- Invalid JSON ---

func TestHookInvalidJSON(t *testing.T) {
	tmp := t.TempDir()
	cmd := exec.Command(testBin, "hook")
	cmd.Stdin = strings.NewReader("not json at all")
	cmd.Env = append(os.Environ(), "READ_ONCE_HOME="+tmp)
	err := cmd.Run()
	if err != nil {
		t.Errorf("expected exit 0 for invalid JSON, got %v", err)
	}
}

// --- Missing tool_input ---

func TestHookMissingToolInput(t *testing.T) {
	tmp := t.TempDir()
	input := map[string]any{"tool_name": "Bash", "session_id": "x"}
	inputJSON, _ := json.Marshal(input)
	cmd := exec.Command(testBin, "hook")
	cmd.Stdin = strings.NewReader(string(inputJSON))
	cmd.Env = append(os.Environ(), "READ_ONCE_HOME="+tmp)
	err := cmd.Run()
	if err != nil {
		t.Errorf("expected exit 0 for missing tool_input, got %v", err)
	}
}

// --- Unknown tool ---

func TestHookUnknownTool(t *testing.T) {
	tmp := t.TempDir()
	input := map[string]any{
		"tool_name":  "Write",
		"tool_input": map[string]any{"file_path": "/foo"},
		"session_id": "x",
	}
	inputJSON, _ := json.Marshal(input)
	cmd := exec.Command(testBin, "hook")
	cmd.Stdin = strings.NewReader(string(inputJSON))
	cmd.Env = append(os.Environ(), "READ_ONCE_HOME="+tmp)
	err := cmd.Run()
	if err != nil {
		t.Errorf("expected exit 0 for unknown tool, got %v", err)
	}
}

// --- TTL expiration ---

func TestHookTTLExpired(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("content"), 0644)

	session := "ttl-test"
	// First read.
	runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp)
	// Set TTL to 0 so next read is expired.
	r := runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_TTL=0")
	if r.exitCode != 0 {
		t.Errorf("expected exit 0 for expired TTL, got %d", r.exitCode)
	}
}

// --- Changed file ---

func TestHookChangedFile(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("version1"), 0644)

	session := "changed-test"
	runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_HASH=1")
	// Change the file. Force a different mtime to avoid same-second race.
	os.WriteFile(f, []byte("version2"), 0644)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(f, future, future)
	r := runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_MODE=deny", "READ_ONCE_HASH=1")
	if r.exitCode != 2 {
		t.Errorf("expected exit 2 for changed file deny, got %d", r.exitCode)
	}
	if !strings.Contains(r.stderr, "changed since last read") {
		t.Errorf("expected 'changed since last read' in stderr, got %q", r.stderr)
	}
}

// --- Codex default mode is deny ---

func TestHookCodexDefaultDeny(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("content"), 0644)

	session := "codex-default-test"
	runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp)
	// No READ_ONCE_MODE set — Codex should default to deny.
	r := runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_CLIENT=codex")
	if r.exitCode != 2 {
		t.Errorf("expected exit 2 for Codex default deny, got %d (stderr=%q)", r.exitCode, r.stderr)
	}
}

// --- Claude default mode is warn ---

func TestHookClaudeDefaultWarn(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("content"), 0644)

	session := "claude-default-test"
	runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp)
	// No READ_ONCE_MODE set — Claude should default to warn.
	r := runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp)
	if r.exitCode != 0 {
		t.Errorf("expected exit 0 for Claude default warn, got %d", r.exitCode)
	}
}

// --- Bypassed path ---

func TestHookBypassedPath(t *testing.T) {
	tmp := t.TempDir()
	subdir := filepath.Join(tmp, "node_modules", "pkg")
	os.MkdirAll(subdir, 0755)
	f := filepath.Join(subdir, "index.js")
	os.WriteFile(f, []byte("code"), 0644)

	session := "bypass-test"
	runHook(t, makeInput("Bash", "cat node_modules/pkg/index.js", session, tmp),
		"READ_ONCE_HOME="+tmp)
	// Second read should still pass through (bypass path not tracked).
	r := runHook(t, makeInput("Bash", "cat node_modules/pkg/index.js", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_MODE=deny")
	if r.exitCode != 0 {
		t.Errorf("expected exit 0 for bypassed path, got %d", r.exitCode)
	}
}

// --- Binary file ---

func TestHookBinaryFile(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "image.png")
	os.WriteFile(f, []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x00}, 0644)

	session := "binary-test"
	runHook(t, makeInput("Bash", "cat image.png", session, tmp),
		"READ_ONCE_HOME="+tmp)
	// Second read should pass through (binary files not tracked).
	r := runHook(t, makeInput("Bash", "cat image.png", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_MODE=deny")
	if r.exitCode != 0 {
		t.Errorf("expected exit 0 for binary file, got %d", r.exitCode)
	}
}

// --- Missing session ID ---

func TestHookMissingSessionID(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("content"), 0644)

	input := map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": "cat file.txt"},
		"cwd":        tmp,
	}
	r := runHook(t, input, "READ_ONCE_HOME="+tmp)
	if r.exitCode != 0 {
		t.Errorf("expected exit 0 for missing session ID, got %d", r.exitCode)
	}
}

// --- File not found ---

func TestHookFileNotFound(t *testing.T) {
	tmp := t.TempDir()
	session := "notfound-test"
	r := runHook(t, makeInput("Bash", "cat nonexistent.txt", session, tmp),
		"READ_ONCE_HOME="+tmp)
	if r.exitCode != 0 {
		t.Errorf("expected exit 0 for missing file, got %d", r.exitCode)
	}
}

// --- Multiple reads in session ---

func TestHookMultipleReads(t *testing.T) {
	tmp := t.TempDir()
	f1 := filepath.Join(tmp, "a.txt")
	f2 := filepath.Join(tmp, "b.txt")
	os.WriteFile(f1, []byte("a"), 0644)
	os.WriteFile(f2, []byte("b"), 0644)

	session := "multi-test"
	// Read a.txt — miss.
	runHook(t, makeInput("Bash", "cat a.txt", session, tmp),
		"READ_ONCE_HOME="+tmp)
	// Read b.txt — miss.
	runHook(t, makeInput("Bash", "cat b.txt", session, tmp),
		"READ_ONCE_HOME="+tmp)
	// Re-read a.txt — hit.
	r := runHook(t, makeInput("Bash", "cat a.txt", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_MODE=deny")
	if r.exitCode != 2 {
		t.Errorf("expected exit 2 for re-read a.txt, got %d", r.exitCode)
	}
	// Re-read b.txt — hit.
	r = runHook(t, makeInput("Bash", "cat b.txt", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_MODE=deny")
	if r.exitCode != 2 {
		t.Errorf("expected exit 2 for re-read b.txt, got %d", r.exitCode)
	}
}

// --- Codex tool name normalization (shell → Bash) ---

func TestHookCodexShellToolName(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("content"), 0644)

	session := "shell-tool-test"
	// First read with "shell" tool name.
	runHook(t, map[string]any{
		"tool_name":  "shell",
		"tool_input": map[string]any{"command": "cat file.txt"},
		"session_id": session,
		"cwd":        tmp,
	}, "READ_ONCE_HOME="+tmp)
	// Second read with "shell" — should be treated as Bash.
	r := runHook(t, map[string]any{
		"tool_name":  "shell",
		"tool_input": map[string]any{"command": "cat file.txt"},
		"session_id": session,
		"cwd":        tmp,
	}, "READ_ONCE_HOME="+tmp, "READ_ONCE_MODE=deny")
	if r.exitCode != 2 {
		t.Errorf("expected exit 2 for shell→Bash normalization, got %d", r.exitCode)
	}
}

// --- Pipeline: second segment is read ---

func TestHookPipelineSecondSegmentRead(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "data.txt")
	os.WriteFile(f, []byte("test"), 0644)

	session := "pipeline-test"
	runHook(t, makeInput("Bash", "grep pattern data.txt", session, tmp),
		"READ_ONCE_HOME="+tmp)
	r := runHook(t, makeInput("Bash", "grep pattern data.txt", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_MODE=deny")
	if r.exitCode != 2 {
		t.Errorf("expected exit 2 for pipeline re-read, got %d", r.exitCode)
	}
}

// --- Stats file ---

func TestHookStatsWritten(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("content"), 0644)

	session := "stats-test"
	runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp)
	runHook(t, makeInput("Bash", "cat file.txt", session, tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_MODE=warn")

	statsFile := filepath.Join(tmp, "stats.jsonl")
	data, err := os.ReadFile(statsFile)
	if err != nil {
		t.Fatalf("stats file not found: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 stat entries, got %d", len(lines))
	}
	// First should be miss, second should be hit.
	var ev1, ev2 map[string]any
	json.Unmarshal([]byte(lines[0]), &ev1)
	json.Unmarshal([]byte(lines[1]), &ev2)
	if ev1["event"] != "miss" {
		t.Errorf("expected first event 'miss', got %q", ev1["event"])
	}
	if ev2["event"] != "hit" {
		t.Errorf("expected second event 'hit', got %q", ev2["event"])
	}
}

// --- Session isolation ---

func TestHookSessionIsolation(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("content"), 0644)

	// Session A reads file.
	runHook(t, makeInput("Bash", "cat file.txt", "session-a-iso", tmp),
		"READ_ONCE_HOME="+tmp)
	// Session B should not see session A's cache.
	r := runHook(t, makeInput("Bash", "cat file.txt", "session-b-iso", tmp),
		"READ_ONCE_HOME="+tmp, "READ_ONCE_MODE=deny")
	if r.exitCode != 0 {
		t.Errorf("expected exit 0 for different session, got %d", r.exitCode)
	}
}
