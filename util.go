package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	xxhash "github.com/cespare/xxhash/v2"
)

var envRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=.*$`)

// commandSpec holds the parsed environment overrides and argument vector for a
// hook command string (e.g. "READ_ONCE_MODE=deny ~/.claude/read-once/read-once hook").
type commandSpec struct {
	env  map[string]string
	argv []string
}

func parseJSONMap(raw []byte) (map[string]any, error) {
	var out map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// splitBy splits s where isSep returns true (outside quotes), tracking escape sequences.
// If trimSegments is true, each segment is trimmed and empties are dropped.
// If preserveEscapes is true, backslashes are kept in output (for pipe splitting where
// \| should produce a literal pipe, not a split point).
func splitBy(s string, isSep func(rune) bool, trimSegments, preserveEscapes, preserveQuotes bool) ([]string, bool) {
	inSingle := false
	inDouble := false
	escaped := false
	cur := make([]rune, 0, len(s))
	var out []string
	flush := func() {
		seg := string(cur)
		if trimSegments {
			seg = strings.TrimSpace(seg)
		}
		if seg != "" {
			out = append(out, seg)
		}
		cur = cur[:0]
	}
	for _, r := range s {
		if escaped {
			cur = append(cur, r)
			escaped = false
			continue
		}
		switch r {
		case '\\':
			if inSingle {
				cur = append(cur, r)
				continue
			}
			if preserveEscapes {
				cur = append(cur, r)
			}
			escaped = true
		case '\'':
			if inDouble {
				cur = append(cur, r)
				continue
			}
			if preserveQuotes {
				cur = append(cur, r)
			}
			inSingle = !inSingle
		case '"':
			if inSingle {
				cur = append(cur, r)
				continue
			}
			if preserveQuotes {
				cur = append(cur, r)
			}
			inDouble = !inDouble
		default:
			if !inSingle && !inDouble && isSep(r) {
				flush()
				continue
			}
			cur = append(cur, r)
		}
	}
	if escaped || inSingle || inDouble {
		return nil, false
	}
	flush()
	return out, len(out) > 0
}

func isWhitespace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func shellSplit(s string) ([]string, bool) {
	return splitBy(s, isWhitespace, false, false, false)
}

func splitCommand(s string) []string {
	toks, ok := shellSplit(s)
	if ok {
		return toks
	}
	return strings.Fields(strings.TrimSpace(s))
}

func parseCommandSpec(command string) commandSpec {
	tokens := splitCommand(command)
	spec := commandSpec{env: map[string]string{}, argv: []string{}}
	for i, t := range tokens {
		if len(spec.argv) == 0 && envRe.MatchString(t) {
			parts := strings.SplitN(t, "=", 2)
			spec.env[parts[0]] = parts[1]
			continue
		}
		spec.argv = append(spec.argv, tokens[i:]...)
		break
	}
	return spec
}

func resolveExecutable(bin string) (string, bool) {
	p := expandHome(bin)
	if strings.Contains(p, "/") {
		if fileExists(p) {
			return p, true
		}
		return p, false
	}
	lp, err := exec.LookPath(p)
	if err != nil {
		return p, false
	}
	return lp, true
}

func optimalHookCommand(hookCommand string) string {
	return "READ_ONCE_MODE=warn READ_ONCE_MODE_UNCHANGED=warn READ_ONCE_MODE_CHANGED=warn READ_ONCE_DIFF=1 READ_ONCE_DIFF_MAX=80 READ_ONCE_DIFF_SUMMARY_MAX_HUNKS=16 READ_ONCE_HASH=1 READ_ONCE_HASH_ALGO=xxhash READ_ONCE_MAX_BYTES=524288 " + hookCommand
}

func hasKey(m map[string]any, key string) bool {
	v, ok := m[key]
	return ok && v != nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		return err
	}
	return nil
}

func filesEqual(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if sa.Size() != sb.Size() {
		return false, nil
	}
	ab, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ab, bb), nil
}

func runOutput(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func getEnvInt(k string, fallback int) int {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func getEnv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any) int {
	switch val := v.(type) {
	case float64:
		return int(val)
	case int:
		return val
	case int64:
		return int(val)
	case string:
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return 0
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func firstString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func matcherMatchesTool(matcher, toolName string) bool {
	matcher = strings.TrimSpace(matcher)
	if matcher == "" {
		return false
	}
	if matcher == toolName {
		return true
	}
	re, err := regexp.Compile(matcher)
	if err != nil {
		return false
	}
	return re.MatchString(toolName)
}

func readMatcherReadOnceCommand(settings map[string]any, matcherName string) string {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return ""
	}
	pre, ok := hooks["PreToolUse"].([]any)
	if !ok {
		return ""
	}
	for _, item := range pre {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		matcher, _ := m["matcher"].(string)
		if !matcherMatchesTool(matcher, matcherName) {
			continue
		}
		hs, ok := m["hooks"].([]any)
		if !ok {
			continue
		}
		for _, h := range hs {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			cmd, _ := hm["command"].(string)
			if strings.Contains(cmd, "read-once") {
				return cmd
			}
		}
	}
	return ""
}

func hasReadOnceHookForTool(settings map[string]any, matcherName string) bool {
	return strings.TrimSpace(readMatcherReadOnceCommand(settings, matcherName)) != ""
}

var codexHooksRe = regexp.MustCompile(`(?m)^\s*codex_hooks\s*=\s*true\b`)

func codexHooksEnabled(raw string) bool {
	return codexHooksRe.MatchString(raw)
}

func fileContentHash(path, algo string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only file, close error is meaningless
	var h io.Writer
	var sumFn func([]byte) []byte
	switch strings.ToLower(strings.TrimSpace(algo)) {
	case "sha256":
		hs := sha256.New()
		h = hs
		sumFn = hs.Sum
	default:
		hs := xxhash.New()
		h = hs
		sumFn = hs.Sum
	}
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(sumFn(nil))
}

// computeAdaptiveTTL returns a TTL that grows with session duration.
// Starts at baseTTL, scales up to 3x over 20 minutes of session life.
// ponytail: simple linear ramp, no ML needed.
func computeAdaptiveTTL(baseTTL int64, cacheFile string, now int64) int64 {
	// Find the earliest cache entry to estimate session start.
	var earliest = now
	scanJSONL(cacheFile, func(c cacheEntry) {
		if c.Ts > 0 && c.Ts < earliest {
			earliest = c.Ts
		}
	})
	sessionAge := now - earliest
	if sessionAge <= 0 {
		return baseTTL
	}
	// Linear ramp: 0min→1x, 10min→2x, 20min→3x, capped at 3x.
	multiplier := 1.0 + math.Min(float64(sessionAge)/600.0, 2.0)
	adaptive := int64(float64(baseTTL) * multiplier)
	maxTTL := baseTTL * 3
	if adaptive > maxTTL {
		adaptive = maxTTL
	}
	return adaptive
}
