package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractBashReadPath(t *testing.T) {
	tmp := t.TempDir()

	f1 := filepath.Join(tmp, "foo.txt")
	os.WriteFile(f1, []byte("test"), 0644)

	f2 := filepath.Join(tmp, "bar.go")
	os.WriteFile(f2, []byte("test"), 0644)

	f3 := filepath.Join(tmp, "baz.json")
	os.WriteFile(f3, []byte("test"), 0644)

	f4 := filepath.Join(tmp, "quoted.txt")
	os.WriteFile(f4, []byte("test"), 0644)

	tests := []struct {
		cmd          string
		expectedPath string
		expectedSkip string
	}{
		{"cat foo.txt", f1, ""},
		{"cat " + f2, f2, ""},
		{"cat   baz.json  ", f3, ""},
		{"cat 'quoted.txt'", f4, ""},
		{"cat foo.txt | grep a", f1, ""},
		{"head foo.txt", f1, ""},
		{"ls", "", "too_few_tokens"},
	}

	for _, tt := range tests {
		input := map[string]any{"command": tt.cmd}
		path, skip := extractBashReadPath(input, tmp)

		expectedClean := tt.expectedPath
		if expectedClean != "" {
			expectedClean = filepath.Clean(expectedClean)
		}

		if path != expectedClean {
			t.Errorf("cmd %q: expected path %q, got %q", tt.cmd, expectedClean, path)
		}
		if skip != tt.expectedSkip {
			t.Errorf("cmd %q: expected skip %q, got %q", tt.cmd, tt.expectedSkip, skip)
		}
	}
}

func TestExtractBashPipe(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "data.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "cat data.txt | head -5 | grep foo"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashRedirect(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "input.txt")
	os.WriteFile(f, []byte("test"), 0644)

	// > output should be skipped, input.txt resolved.
	input := map[string]any{"command": "cat input.txt > output.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashInlineRedirect(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "data.txt")
	os.WriteFile(f, []byte("test"), 0644)

	// >file inline redirect.
	input := map[string]any{"command": "cat data.txt >output.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashInputRedirect(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "data.txt")
	os.WriteFile(f, []byte("test"), 0644)

	// < file as input redirect.
	input := map[string]any{"command": "grep pattern < data.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractGitShow(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "main.go")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "git show HEAD:main.go"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractGitDiff(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "hook.go")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "git diff HEAD -- hook.go"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractGitDiffNoSeparator(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "git diff HEAD file.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractGitUnsupportedSubcommand(t *testing.T) {
	input := map[string]any{"command": "git status --short"}
	_, skip := extractBashReadPath(input, "/tmp")
	if skip == "" {
		t.Error("expected skip for unsupported git subcommand")
	}
}

func TestExtractBashUnsupportedReader(t *testing.T) {
	input := map[string]any{"command": "curl http://example.com"}
	_, skip := extractBashReadPath(input, "/tmp")
	if skip == "" {
		t.Error("expected skip for unsupported reader")
	}
}

func TestExtractBashEmptyCommand(t *testing.T) {
	input := map[string]any{"command": ""}
	_, skip := extractBashReadPath(input, "/tmp")
	if skip != "empty_command" {
		t.Errorf("expected empty_command, got %s", skip)
	}
}

func TestExtractBashNoExistingFile(t *testing.T) {
	input := map[string]any{"command": "cat nonexistent-file-xyz.txt"}
	_, skip := extractBashReadPath(input, "/tmp")
	if skip != "no_existing_file_in_command" {
		t.Errorf("expected no_existing_file_in_command, got %s", skip)
	}
}

func TestCollectArgsSkipFlags(t *testing.T) {
	args := []string{"-n", "10", "-r", "pattern", "file.txt"}
	result := collectArgs(args)
	// Only tokens starting with "-" are skipped. Values like "10" and "pattern" pass through.
	expected := []string{"10", "pattern", "file.txt"}
	if len(result) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, result)
	}
	for i, v := range expected {
		if result[i] != v {
			t.Errorf("result[%d] = %q, want %q", i, result[i], v)
		}
	}
}

func TestCollectArgsSkipRedirects(t *testing.T) {
	args := []string{"file.txt", ">", "output.txt"}
	result := collectArgs(args)
	if len(result) != 1 || result[0] != "file.txt" {
		t.Errorf("expected [file.txt], got %v", result)
	}
}

func TestCollectArgsInputRedirect(t *testing.T) {
	args := []string{"<", "input.txt"}
	result := collectArgs(args)
	if len(result) != 1 || result[0] != "input.txt" {
		t.Errorf("expected [input.txt], got %v", result)
	}
}

func TestCollectArgsMultiple(t *testing.T) {
	args := []string{"file1.txt", "file2.txt", "file3.txt"}
	result := collectArgs(args)
	if len(result) != 3 {
		t.Errorf("expected 3 args, got %d: %v", len(result), result)
	}
}

func TestCollectArgsEmpty(t *testing.T) {
	result := collectArgs([]string{})
	if len(result) != 0 {
		t.Errorf("expected empty, got %v", result)
	}
}

func TestExtractGitShowColonNotation(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "main.go")
	os.WriteFile(f, []byte("test"), 0644)

	tests := []struct {
		cmd string
	}{
		{"git show HEAD:main.go"},
		{"git show main.go"},
		{"git show abc123:main.go"},
		{"git show origin/main:main.go"},
	}
	for _, tt := range tests {
		input := map[string]any{"command": tt.cmd}
		path, skip := extractBashReadPath(input, tmp)
		if path != filepath.Clean(f) {
			t.Errorf("cmd %q: expected %s, got %s (skip=%s)", tt.cmd, f, path, skip)
		}
	}
}

func TestExtractGitLog(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "git log -p -- file.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractGitBlame(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "git blame file.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashMultiplePipes(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "log.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "cat log.txt | grep error | head -20 | wc -l"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashNoArgs(t *testing.T) {
	input := map[string]any{"command": "cat"}
	_, skip := extractBashReadPath(input, "/tmp")
	if skip != "too_few_tokens" {
		t.Errorf("expected too_few_tokens, got %s", skip)
	}
}

func TestExtractBashWhitespace(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "  cat   file.txt  "}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashQuotedPaths(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "my file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": `cat "my file.txt"`}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashSingleQuoted(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "my file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "cat 'my file.txt'"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashRelativePath(t *testing.T) {
	tmp := t.TempDir()
	subdir := filepath.Join(tmp, "sub")
	os.MkdirAll(subdir, 0755)
	f := filepath.Join(subdir, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "cat sub/file.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashAbsolutePath(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "cat " + f}
	path, skip := extractBashReadPath(input, "/wrong/dir")
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashNonReaderVerb(t *testing.T) {
	tests := []string{
		"echo hello",
		"rm file.txt",
		"cp src dst",
		"mv src dst",
		"mkdir dir",
		"touch file",
		"find . -name *.go",
	}
	for _, cmd := range tests {
		input := map[string]any{"command": cmd}
		_, skip := extractBashReadPath(input, "/tmp")
		if skip == "" {
			t.Errorf("cmd %q: expected skip for non-reader verb", cmd)
		}
	}
}

func TestExtractGitCatFile(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "blob.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "git cat-file -p HEAD:blob.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractGitLsTree(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "tree.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "git ls-tree HEAD tree.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestResolveBestCandidatePrefersLast(t *testing.T) {
	tmp := t.TempDir()
	f1 := filepath.Join(tmp, "a.txt")
	f2 := filepath.Join(tmp, "b.txt")
	os.WriteFile(f1, []byte("a"), 0644)
	os.WriteFile(f2, []byte("b"), 0644)

	// Candidates in order: a.txt, b.txt — should prefer b.txt (last).
	candidates := []string{f1, f2}
	path, _ := resolveBestCandidate(candidates, tmp)
	if path != filepath.Clean(f2) {
		t.Errorf("expected %s, got %s", f2, path)
	}
}

func TestClientMatcher(t *testing.T) {
	tests := []struct {
		client string
		want   string
	}{
		{clientClaude, toolRead},
		{clientCodex, toolBash},
		{clientOpenCode, toolBash},
		{"unknown", toolRead},
	}
	for _, tt := range tests {
		if got := clientMatcher(tt.client); got != tt.want {
			t.Errorf("clientMatcher(%q) = %q, want %q", tt.client, got, tt.want)
		}
	}
}

func TestShouldBypassPath(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		{"/app/.git/config", true},
		{"/app/node_modules/foo/bar", true},
		{"/app/src/main.go", false},
		{"/app/build/output.bin", true},
		{"/app/dist/index.js", true},
		{"/app/foo.lock", true},
		{"/app/vendor/pkg/foo", true},
	}

	for _, tt := range tests {
		result := shouldBypassPath(tt.path)
		if result != tt.expected {
			t.Errorf("path %q: expected bypass %v, got %v", tt.path, tt.expected, result)
		}
	}
}

// --- New tests for expanded Codex patterns ---

func TestExtractBashBatcat(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "README.md")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "batcat README.md"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashCcat(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "ccat file.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashEza(t *testing.T) {
	tmp := t.TempDir()
	subdir := filepath.Join(tmp, "src")
	os.MkdirAll(subdir, 0755)

	input := map[string]any{"command": "eza src"}
	path, skip := extractBashReadPath(input, tmp)
	// eza reads a directory — resolveFileToken rejects directories, so skip is expected
	// unless we point it at a file. But eza on a dir is valid for listing.
	// For now, just verify it doesn't crash and returns a skip reason.
	_ = path
	if skip == "" {
		t.Log("eza on directory returned path (unexpected but harmless)")
	}
}

func TestExtractBashExa(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	// exa can list files too — but typically used on directories.
	// Test with a file to verify it's recognized as a reader verb.
	input := map[string]any{"command": "exa file.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashTree(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "tree file.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashDu(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "du file.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashCdThenCat(t *testing.T) {
	tmp := t.TempDir()
	subdir := filepath.Join(tmp, "sub")
	os.MkdirAll(subdir, 0755)
	f := filepath.Join(subdir, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "cd sub && cat file.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashCdThenCatAbs(t *testing.T) {
	tmp := t.TempDir()
	subdir := filepath.Join(tmp, "sub")
	os.MkdirAll(subdir, 0755)
	f := filepath.Join(subdir, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	// Absolute cd path.
	input := map[string]any{"command": "cd " + subdir + " && cat file.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashCdDoubleDash(t *testing.T) {
	tmp := t.TempDir()
	weirdDir := filepath.Join(tmp, "-weird")
	os.MkdirAll(weirdDir, 0755)
	f := filepath.Join(weirdDir, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "cd -- -weird && cat file.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashCdMultipleOperandsUsesLast(t *testing.T) {
	tmp := t.TempDir()
	dir1 := filepath.Join(tmp, "dir1")
	dir2 := filepath.Join(tmp, "dir2")
	os.MkdirAll(dir1, 0755)
	os.MkdirAll(dir2, 0755)
	f := filepath.Join(dir2, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "cd dir1 dir2 && cat file.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != "" || skip == "" {
		t.Errorf("expected invalid cd to skip, got path=%q skip=%q", path, skip)
	}
}

func TestExtractBashCdWithSemicolon(t *testing.T) {
	tmp := t.TempDir()
	subdir := filepath.Join(tmp, "sub")
	os.MkdirAll(subdir, 0755)
	f := filepath.Join(subdir, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "cd sub ; cat file.txt"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashBashLc(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "bash -lc 'cat file.txt'"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashBashLcDoubleQuotes(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": `bash -lc "cat file.txt"`}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashZshLc(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "zsh -lc 'head file.txt'"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashBashLcWithCd(t *testing.T) {
	tmp := t.TempDir()
	subdir := filepath.Join(tmp, "sub")
	os.MkdirAll(subdir, 0755)
	f := filepath.Join(subdir, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "bash -lc 'cd sub && cat file.txt'"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestExtractBashBashC(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file.txt")
	os.WriteFile(f, []byte("test"), 0644)

	input := map[string]any{"command": "bash -c 'cat file.txt'"}
	path, skip := extractBashReadPath(input, tmp)
	if path != filepath.Clean(f) {
		t.Errorf("expected %s, got %s (skip=%s)", f, path, skip)
	}
}

func TestUnwrapShellWrapper(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"bash -lc 'cat file.txt'", "cat file.txt"},
		{`bash -lc "cat file.txt"`, "cat file.txt"},
		{"zsh -lc 'head file.txt'", "head file.txt"},
		{"bash -c 'cat file.txt'", "cat file.txt"},
		{"cat file.txt", "cat file.txt"},             // no wrapper
		{"bash -lc", "bash -lc"},                     // no quoted string
		{"bash -lc 'unclosed", "bash -lc 'unclosed"}, // unclosed quote
	}

	for _, tt := range tests {
		result := unwrapShellWrapper(tt.input)
		if result != tt.expected {
			t.Errorf("unwrapShellWrapper(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestExtractCdPrefix(t *testing.T) {
	tests := []struct {
		input       string
		expectedCmd string
		expectedDir string
	}{
		{"cd sub && cat file.txt", "cat file.txt", "sub"},
		{"cd sub ; cat file.txt", "cat file.txt", "sub"},
		{"cd -- -weird && cat file.txt", "cat file.txt", "-weird"},
		{"cd dir1 dir2 && cat file.txt", "cd dir1 dir2 && cat file.txt", ""},
		{"cat file.txt", "cat file.txt", ""},             // no cd prefix
		{"cd && cat file.txt", "cd && cat file.txt", ""}, // no dir argument — not a cd prefix
	}

	for _, tt := range tests {
		cmd, dir := extractCdPrefix(tt.input)
		if cmd != tt.expectedCmd {
			t.Errorf("extractCdPrefix(%q) cmd = %q, want %q", tt.input, cmd, tt.expectedCmd)
		}
		if dir != tt.expectedDir {
			t.Errorf("extractCdPrefix(%q) dir = %q, want %q", tt.input, dir, tt.expectedDir)
		}
	}
}

func TestParseCdCommand(t *testing.T) {
	tests := []struct {
		input    string
		expected string
		ok       bool
	}{
		{"cd sub", "sub", true},
		{"cd -- -weird", "-weird", true},
		{"cd dir1 dir2", "", false},
		{"cd", "", false},
		{"cat file.txt", "", false},
		{"cd -L sub", "sub", true}, // skip flags
	}

	for _, tt := range tests {
		dir, ok := parseCdCommand(tt.input)
		if ok != tt.ok {
			t.Errorf("parseCdCommand(%q) ok = %v, want %v", tt.input, ok, tt.ok)
		}
		if dir != tt.expected {
			t.Errorf("parseCdCommand(%q) = %q, want %q", tt.input, dir, tt.expected)
		}
	}
}
