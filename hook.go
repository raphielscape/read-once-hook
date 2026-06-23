package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// hookContext bundles the parameters shared across cache-hit handling functions,
// avoiding the need to pass ~15 individual arguments.
type hookContext struct {
	cacheFile  string
	statsFile  string
	snapFile   string
	cacheKey   string
	filePath   string
	session    string // short hash of session ID
	clientName string

	unchangedMode string
	changedMode   string

	last cacheEntry // previous cache entry (may be zero-value on miss)

	currentMtime        string // unix seconds as string
	currentMtimeDisplay string // human-readable (RFC3339)
	currentHash         string

	tokens       int64
	effectiveTTL int64
	now          int64
	decay        int64
	autoAllow    int

	diffMode bool
	hashMode bool
}

// handleCacheHit checks whether a cached entry should block the re-read.
// Returns true if the cache hit was handled (caller should return nil).
func handleCacheHit(ctx hookContext) bool {
	entryAge := ctx.now - ctx.last.Ts
	if ctx.last.Ts <= 0 {
		entryAge = 0
	}

	// TTL expired — allow the re-read.
	if entryAge >= ctx.effectiveTTL {
		_ = appendJSONLine(ctx.cacheFile, cacheEntry{
			Path:   ctx.cacheKey,
			Mtime:  ctx.currentMtime,
			Ts:     ctx.now,
			Tokens: ctx.tokens,
			Hash:   ctx.currentHash,
		})
		_ = appendJSONLine(ctx.statsFile, map[string]any{
			"ts":      ctx.now,
			"path":    ctx.filePath,
			"tokens":  ctx.tokens,
			"session": ctx.session,
			"event":   eventExpired,
		})
		if ctx.diffMode {
			_ = copyFile(ctx.filePath, ctx.snapFile, 0o644)
		}
		return true
	}

	// Auto-allow after N consecutive blocked attempts.
	attempts := 1
	if ctx.now-ctx.last.LastAttemptTs <= ctx.decay {
		attempts = ctx.last.Attempts + 1
	}
	if ctx.autoAllow > 0 && attempts >= ctx.autoAllow {
		_ = appendJSONLine(ctx.cacheFile, cacheEntry{
			Path:   ctx.cacheKey,
			Mtime:  ctx.currentMtime,
			Ts:     ctx.now,
			Tokens: ctx.tokens,
			Hash:   ctx.currentHash,
		})
		_ = appendJSONLine(ctx.statsFile, map[string]any{
			"ts":      ctx.now,
			"path":    ctx.filePath,
			"tokens":  ctx.tokens,
			"session": ctx.session,
			"event":   eventAutoAllow,
		})
		if ctx.diffMode {
			_ = copyFile(ctx.filePath, ctx.snapFile, 0o644)
		}
		return true
	}

	// Cache hit — update sliding TTL and emit advisory.
	_ = appendJSONLine(ctx.cacheFile, cacheEntry{
		Path:          ctx.cacheKey,
		Mtime:         ctx.currentMtime,
		Ts:            ctx.now, // ponytail: sliding TTL — reset clock on access
		Tokens:        ctx.tokens,
		Hash:          ctx.currentHash,
		LastAttemptTs: ctx.now,
		Attempts:      attempts,
	})
	_ = appendJSONLine(ctx.statsFile, map[string]any{
		"ts":           ctx.now,
		"path":         ctx.filePath,
		"tokens_saved": ctx.tokens,
		"session":      ctx.session,
		"event":        eventHit,
	})

	minutesAgo := entryAge / 60
	ttlMin := ctx.effectiveTTL / 60
	reason := fmt.Sprintf(
		"read-once: %s is already in context (read %dm ago, unchanged; mtime=%s). Re-read allowed after %dm.",
		filepath.Base(ctx.filePath), minutesAgo, formatUnixMtime(ctx.currentMtime), ttlMin,
	)
	if ctx.autoAllow > 0 {
		reason += fmt.Sprintf(" Attempt %d/%d before auto-allow.", attempts, ctx.autoAllow)
	}
	emitHookDecision(ctx.unchangedMode, reason, ctx.clientName)
	return true
}

// toolInputResult holds the parsed output of parseToolInput.
type toolInputResult struct {
	filePath  string
	session   string
	cwd       string
	cacheKey  string
	hasRange  bool
	readLimit int
	toolName  string
	skip      string // non-empty means caller should return nil
}

// parseToolInput reads stdin, parses the JSON hook input, and extracts the
// file path, session ID, and cache key. Returns skip="" on success.
func parseToolInput(cacheDir, clientName string) toolInputResult {
	inputRaw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return toolInputResult{skip: "stdin_read_error"}
	}
	if len(bytes.TrimSpace(inputRaw)) == 0 {
		return toolInputResult{skip: "empty_stdin"}
	}

	var in map[string]any
	if err := json.Unmarshal(inputRaw, &in); err != nil {
		return toolInputResult{skip: "malformed_json"}
	}
	toolName := firstString(
		asString(in["tool_name"]),
		asString(in["toolName"]),
		asString(in["tool"]),
		asString(in["name"]),
	)
	toolInput := asMap(in["tool_input"])
	if toolInput == nil {
		toolInput = asMap(in["toolInput"])
	}
	if toolInput == nil {
		toolInput = asMap(in["input"])
	}
	if toolInput == nil {
		return toolInputResult{skip: "no_tool_input"}
	}
	toolName = strings.TrimSpace(toolName)
	// Normalize Codex tool names to match our switch cases.
	if toolName == "shell" {
		toolName = toolBash
	}
	filePath := ""
	skipReason := ""
	switch toolName {
	case toolRead:
		filePath = firstString(
			asString(toolInput["file_path"]),
			asString(toolInput["path"]),
		)
		if filePath == "" {
			skipReason = "read_missing_file_path"
		}
	case toolBash:
		filePath, skipReason = extractBashReadPath(toolInput, asString(in["cwd"]))
	default:
		debugSkip(cacheDir, "unsupported_tool:"+toolName, "")
		return toolInputResult{skip: "unsupported_tool"}
	}
	sessionID := firstString(
		asString(in["session_id"]),
		asString(in["conversation_id"]),
		asString(in["thread_id"]),
	)
	cwd := asString(in["cwd"])
	if filePath != "" && cwd != "" && !filepath.IsAbs(filePath) {
		filePath = filepath.Join(cwd, filePath)
	}
	if filePath != "" {
		filePath = filepath.Clean(filePath)
	}
	if filePath == "" || sessionID == "" {
		if filePath == "" {
			debugSkip(cacheDir, "path_not_trackable:"+skipReason, asString(toolInput["command"]))
		} else {
			debugSkip(cacheDir, "missing_session_id", asString(toolInput["command"]))
		}
		return toolInputResult{skip: "path_or_session_empty"}
	}

	cacheKey := filePath
	hasRange := false
	readLimit := 0
	if toolName == toolRead {
		var readOffset int
		if hasKey(toolInput, "offset") {
			readOffset = asInt(toolInput["offset"])
			hasRange = true
		}
		if hasKey(toolInput, "limit") {
			readLimit = asInt(toolInput["limit"])
			hasRange = true
		}
		if hasRange {
			cacheKey = fmt.Sprintf("%s:%d:%d", filePath, readOffset, readLimit)
		}
	}

	return toolInputResult{
		filePath:  filePath,
		session:   sessionID,
		cwd:       cwd,
		cacheKey:  cacheKey,
		hasRange:  hasRange,
		readLimit: readLimit,
		toolName:  toolName,
	}
}

// resolveSession computes the session hash, cache/stats file paths, and
// snapshot directory from the session ID.
func resolveSession(cacheDir, sessionID, cacheKey string) (sessionHash, cacheFile, statsFile, snapDir, snapFile string) {
	sessionHash = shortHash(sessionID)
	cacheFile = filepath.Join(cacheDir, "session-"+sessionHash+".jsonl")
	statsFile = filepath.Join(cacheDir, "stats.jsonl")
	snapDir = filepath.Join(cacheDir, "snapshots")
	pathHash := shortHash(cacheKey)
	snapFile = filepath.Join(snapDir, sessionHash+"-"+pathHash)
	return
}

func runHookMode(cacheDir, clientName string) error {
	if getEnv("READ_ONCE_DISABLED", "0") == "1" {
		return nil
	}

	in := parseToolInput(cacheDir, clientName)
	if in.skip != "" {
		return nil
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil //nolint:nilerr // hook must not crash if cache dir creation fails
	}

	defaultModeStr := getEnv("READ_ONCE_MODE", "")
	if defaultModeStr == "" && clientName == clientCodex {
		defaultModeStr = modeDeny
	}
	defaultMode := getMode(defaultModeStr)
	unchangedMode := getMode(getEnv("READ_ONCE_MODE_UNCHANGED", defaultMode))
	changedMode := getMode(getEnv("READ_ONCE_MODE_CHANGED", defaultMode))
	ttl := int64(getEnvInt("READ_ONCE_TTL", 300))
	diffMode := getEnv("READ_ONCE_DIFF", "0") == "1"
	diffMax := getEnvInt("READ_ONCE_DIFF_MAX", 40)
	diffSummaryMaxHunks := getEnvInt("READ_ONCE_DIFF_SUMMARY_MAX_HUNKS", 12)
	hashMode := getEnv("READ_ONCE_HASH", "1") == "1"
	hashAlgo := strings.ToLower(getEnv("READ_ONCE_HASH_ALGO", "xxhash"))
	maxBytes := int64(getEnvInt("READ_ONCE_MAX_BYTES", 1024*1024))
	decay := int64(getEnvInt("READ_ONCE_DECAY", 60))
	autoAllow := getEnvInt("READ_ONCE_AUTO_ALLOW", 2)
	now := time.Now().Unix()

	if shouldBypassPath(in.filePath) || !shouldTrackByPolicy(in.filePath) {
		if shouldBypassPath(in.filePath) {
			debugSkip(cacheDir, "bypass_path_filter", in.filePath)
		} else {
			debugSkip(cacheDir, "policy_filter_excluded", in.filePath)
		}
		return nil
	}

	snapDir := filepath.Join(cacheDir, "snapshots")
	if diffMode && in.hasRange {
		diffMode = false
	}
	if diffMode {
		_ = os.MkdirAll(snapDir, 0o755)
	}

	runCleanup(cacheDir, snapDir, now)

	sessionHash, cacheFile, statsFile, _, snapFile := resolveSession(cacheDir, in.session, in.cacheKey)

	adaptiveTTL := computeAdaptiveTTL(ttl, cacheFile, now)

	ctx := hookContext{
		cacheFile:     cacheFile,
		statsFile:     statsFile,
		snapFile:      snapFile,
		cacheKey:      in.cacheKey,
		filePath:      in.filePath,
		session:       sessionHash,
		clientName:    clientName,
		unchangedMode: unchangedMode,
		changedMode:   changedMode,
		effectiveTTL:  adaptiveTTL,
		now:           now,
		decay:         decay,
		autoAllow:     autoAllow,
		diffMode:      diffMode,
		hashMode:      hashMode,
	}

	// Fast path: check cache before stat. On a cache hit with no hash validation,
	// skip the os.Stat + isLikelyBinary + maxBytes checks entirely — the file was
	// already validated when first read.
	last, ok := readLastCacheEntry(cacheFile, in.cacheKey)
	if ok && !hashMode {
		ctx.last = last
		ctx.currentMtime = last.Mtime
		ctx.tokens = 0
		ctx.currentHash = ""
		if handleCacheHit(ctx) {
			return nil
		}
	}

	// Slow path: stat the file for cache miss, changed file, or hash validation.
	st, err := os.Stat(in.filePath)
	if err != nil || st.IsDir() {
		debugSkip(cacheDir, "file_stat_failed_or_directory", in.filePath)
		return nil //nolint:nilerr // missing/unreadable files are silently skipped
	}
	if maxBytes > 0 && st.Size() > maxBytes {
		debugSkip(cacheDir, "file_too_large", in.filePath)
		return nil
	}
	if isLikelyBinary(in.filePath) {
		debugSkip(cacheDir, "binary_file", in.filePath)
		return nil
	}
	currentMtime := strconv.FormatInt(st.ModTime().Unix(), 10)
	currentMtimeDisplay := st.ModTime().Format(time.RFC3339)
	fileSize := st.Size()
	if fileSize < 0 {
		fileSize = 0
	}

	// Pre-multiply bound check: reject absurd limits that could overflow or are obviously invalid.
	// 100,000,000 lines is a safe max that guarantees no overflow when multiplied by 50.
	if in.hasRange && in.readLimit > 0 && in.readLimit < 100000000 {
		rangeSize := int64(in.readLimit) * 50
		if rangeSize < fileSize {
			fileSize = rangeSize
		}
	}
	estimatedTokens := ((fileSize / 4) * 170) / 100
	currentHash := ""
	if hashMode {
		currentHash = fileContentHash(in.filePath, hashAlgo)
	}

	unchanged := ok && last.Mtime == currentMtime
	if unchanged && hashMode && last.Hash != "" && currentHash != "" && last.Hash != currentHash {
		unchanged = false
	}
	if unchanged {
		ctx.last = last
		ctx.currentMtime = currentMtime
		ctx.currentMtimeDisplay = currentMtimeDisplay
		ctx.tokens = estimatedTokens
		ctx.currentHash = currentHash
		if handleCacheHit(ctx) {
			return nil
		}
	}

	if ok && diffMode && fileExists(snapFile) {
		diffOutput, diffLines := unifiedDiff(snapFile, in.filePath)
		if strings.TrimSpace(diffOutput) != "" && diffLines <= diffMax {
			_ = appendJSONLine(cacheFile, cacheEntry{
				Path:   in.cacheKey,
				Mtime:  currentMtime,
				Ts:     now,
				Tokens: estimatedTokens,
				Hash:   currentHash,
			})
			_ = copyFile(in.filePath, snapFile, 0o644)

			diffTokens := int64(diffLines * 10)
			tokensSaved := estimatedTokens - diffTokens
			if tokensSaved < 0 {
				tokensSaved = 0
			}
			_ = appendJSONLine(statsFile, map[string]any{
				"ts":           now,
				"path":         in.filePath,
				"tokens_saved": tokensSaved,
				"session":      sessionHash,
				"event":        eventDiff,
			})

			reason := fmt.Sprintf(
				"read-once: %s changed since last read (previous mtime=%s, current mtime=%s). You already have the previous version in context. Here are only the changes:\n\n%s\n\nApply this diff mentally to your cached version of the file.",
				filepath.Base(in.filePath), formatUnixMtime(last.Mtime), currentMtimeDisplay, diffOutput,
			)
			emitHookDecision(changedMode, reason, clientName)
			return nil
		}

		summary := summarizeDiff(snapFile, in.filePath, diffSummaryMaxHunks)
		if summary != "" {
			summaryTokens := int64(len(summary) / 4)
			tokensSaved := estimatedTokens - summaryTokens
			if tokensSaved < 0 {
				tokensSaved = 0
			}
			_ = appendJSONLine(cacheFile, cacheEntry{
				Path:   in.cacheKey,
				Mtime:  currentMtime,
				Ts:     now,
				Tokens: estimatedTokens,
				Hash:   currentHash,
			})
			_ = copyFile(in.filePath, snapFile, 0o644)
			_ = appendJSONLine(statsFile, map[string]any{
				"ts":           now,
				"path":         in.filePath,
				"tokens_saved": tokensSaved,
				"session":      sessionHash,
				"event":        eventDiff,
			})
			reason := fmt.Sprintf(
				"read-once: %s changed since last read (previous mtime=%s, current mtime=%s). Full diff was too large, so here is a compact summary:\n\n%s",
				filepath.Base(in.filePath), formatUnixMtime(last.Mtime), currentMtimeDisplay, summary,
			)
			emitHookDecision(changedMode, reason, clientName)
			return nil
		}
	}

	if ok && changedMode == modeDeny {
		reason := fmt.Sprintf(
			"read-once: %s changed since last read (previous mtime=%s, current mtime=%s); re-read is blocked.",
			filepath.Base(in.filePath), formatUnixMtime(last.Mtime), currentMtimeDisplay,
		)
		emitHookDecision(changedMode, reason, clientName)
		return nil
	}

	_ = appendJSONLine(cacheFile, cacheEntry{
		Path:   in.cacheKey,
		Mtime:  currentMtime,
		Ts:     now,
		Tokens: estimatedTokens,
		Hash:   currentHash,
	})
	if diffMode {
		_ = copyFile(in.filePath, snapFile, 0o644)
	}
	event := eventMiss
	if ok {
		event = eventChanged
	}
	_ = appendJSONLine(statsFile, map[string]any{
		"ts":      now,
		"path":    in.filePath,
		"tokens":  estimatedTokens,
		"session": sessionHash,
		"event":   event,
	})
	return nil
}

func emitHookDecision(mode, reason, client string) {
	if mode == modeAllow {
		return
	}
	if mode == modeDeny {
		// Exit code 2 blocks the tool call and surfaces reason to model.
		// Works across all clients (Claude, Codex).
		_, _ = os.Stderr.WriteString(reason)
		os.Exit(2)
	}
	// warn mode: advisory only, tool call proceeds.
	if client == clientCodex {
		return // Codex has no advisory channel — silent pass-through.
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       modeAllow,
			"permissionDecisionReason": reason,
		},
	})
}

func getMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "deny":
		return modeDeny
	case "allow":
		return modeAllow
	default:
		return modeWarn
	}
}

func debugSkip(cacheDir, reason, detail string) {
	// Optional diagnostics for "why wasn't this command tracked?".
	// Enable with READ_ONCE_DEBUG=1 and inspect $READ_ONCE_HOME/debug.log.
	if !debugHookEnabled(cacheDir) {
		return
	}
	_ = os.MkdirAll(cacheDir, 0o755)
	debugFile := filepath.Join(cacheDir, "debug.log")
	msg := fmt.Sprintf("%s skip=%s", time.Now().Format(time.RFC3339), reason)
	if strings.TrimSpace(detail) != "" {
		msg += " detail=" + strconv.Quote(detail)
	}
	msg += "\n"
	f, err := os.OpenFile(debugFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck // debug log, write error intentionally ignored
	_, _ = f.WriteString(msg)
}

func debugHookEnabled(cacheDir string) bool {
	defaultVal := "0"
	if strings.Contains(filepath.ToSlash(cacheDir), "/.codex/read-once") {
		defaultVal = "1"
	}
	v := strings.ToLower(strings.TrimSpace(getEnv("READ_ONCE_DEBUG", defaultVal)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func formatUnixMtime(raw string) string {
	sec, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || sec <= 0 {
		return "unknown"
	}
	return time.Unix(sec, 0).Format(time.RFC3339)
}
