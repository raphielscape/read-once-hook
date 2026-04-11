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
		{"cat < foo.txt", "", "unsafe_shell_construct"},
		{"cat foo.txt | grep a", f1, ""}, // Piping is allowed, it splits on | and checks first segment
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
