package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

func shouldTrackByPolicy(path string) bool {
	includes := parseListEnv("READ_ONCE_INCLUDE")
	excludes := parseListEnv("READ_ONCE_EXCLUDE")
	if len(includes) > 0 && !matchesAnyPattern(includes, path) {
		return false
	}
	if len(excludes) > 0 && matchesAnyPattern(excludes, path) {
		return false
	}
	return true
}

// shouldBypassPath returns true for paths that should never be tracked regardless of policy.
// IMPORTANT: bypass is checked before READ_ONCE_INCLUDE/EXCLUDE policy (see runHookMode),
// so these rules are intentionally non-overridable. Only add paths here that are universally
// noise: compiled artifacts, dependency trees, generated caches. For user-configurable
// exclusions use READ_ONCE_EXCLUDE instead.
func shouldBypassPath(path string) bool {
	p := strings.ToLower(filepath.ToSlash(path))
	defaultSegments := []string{
		// VCS / dependency trees
		"/.git/", "/node_modules/", "/vendor/",
		// Build / output dirs
		"/dist/", "/build/", "/target/", "/.next/", "/coverage/",
		// Generic caches
		"/.cache/", "/snapshots/",
		// Python caches and virtual envs
		"/__pycache__/", "/.venv/", "/.mypy_cache/", "/.tox/", "/.pytest_cache/",
		// Infrastructure-as-code caches
		"/.terraform/", "/.terragrunt-cache/",
	}
	for _, seg := range defaultSegments {
		if strings.Contains(p, seg) {
			return true
		}
	}
	base := strings.ToLower(filepath.Base(path))
	defaultSuffixes := []string{
		".min.js", ".map", ".lock",
		".png", ".jpg", ".jpeg", ".gif", ".pdf",
		".zip", ".tar", ".gz", ".ico", ".woff", ".woff2",
		// Protobuf generated files
		".pb.go", ".pb.gw.go",
	}
	for _, s := range defaultSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	if strings.HasPrefix(base, "generated.") || strings.HasSuffix(base, ".generated") || strings.Contains(base, ".generated.") {
		return true
	}
	return false
}

func parseListEnv(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func matchesAnyPattern(patterns []string, path string) bool {
	norm := filepath.ToSlash(path)
	base := filepath.Base(path)
	for _, p := range patterns {
		if matchPattern(p, norm, base) {
			return true
		}
	}
	return false
}

func matchPattern(pattern, normPath, base string) bool {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "re:") {
		re, err := regexp.Compile(strings.TrimPrefix(p, "re:"))
		return err == nil && re.MatchString(normPath)
	}
	if strings.ContainsAny(p, "*?[") {
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
		if ok, _ := filepath.Match(p, normPath); ok {
			return true
		}
		if ok, _ := filepath.Match("*/"+p, normPath); ok {
			return true
		}
	}
	return strings.Contains(normPath, p) || strings.Contains(base, p)
}

// isLikelyBinary returns true if the file appears to contain binary content.
// Known limitation: UTF-16 encoded text files (common on Windows) contain 0x00 bytes as the
// high byte of every ASCII character. The null-byte short-circuit at "if b == 0 { return true }"
// will misclassify UTF-16 LE/BE text files as binary, causing them to be silently skipped by
// the hook. If UTF-16 support is needed, add a BOM check (0xFF 0xFE or 0xFE 0xFF) before
// the loop and treat those files as text.
func isLikelyBinary(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	if n == 0 {
		return false
	}
	buf = buf[:n]
	nonText := 0
	for _, b := range buf {
		if b == 0 {
			return true
		}
		if b == '\n' || b == '\r' || b == '\t' {
			continue
		}
		if b < 0x20 || b == 0x7f {
			nonText++
		}
	}
	if utf8.Valid(buf) {
		return float64(nonText)/float64(n) > 0.10
	}
	return float64(nonText)/float64(n) > 0.03
}
