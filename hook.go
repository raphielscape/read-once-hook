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

func runHookMode(cacheDir string) error {
	if getEnv("READ_ONCE_DISABLED", "0") == "1" {
		return nil
	}

	inputRaw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil
	}
	if len(bytes.TrimSpace(inputRaw)) == 0 {
		return nil
	}

	var in map[string]any
	if err := json.Unmarshal(inputRaw, &in); err != nil {
		return nil
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
		return nil
	}
	toolName = strings.TrimSpace(toolName)
	filePath := ""
	skipReason := ""
	switch toolName {
	case "Read":
		filePath = firstString(
			asString(toolInput["file_path"]),
			asString(toolInput["path"]),
		)
		if filePath == "" {
			skipReason = "read_missing_file_path"
		}
	case "Bash":
		filePath, skipReason = extractBashReadPath(toolInput, asString(in["cwd"]))
	default:
		debugSkip(cacheDir, "unsupported_tool:"+toolName, "")
		return nil
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
		return nil
	}

	cacheKey := filePath
	hasRange := false
	var readOffset, readLimit int
	if toolName == "Read" {
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

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil
	}

	defaultMode := getMode(getEnv("READ_ONCE_MODE", "warn"))
	unchangedMode := getMode(getEnv("READ_ONCE_MODE_UNCHANGED", defaultMode))
	changedMode := getMode(getEnv("READ_ONCE_MODE_CHANGED", defaultMode))
	ttl := int64(getEnvInt("READ_ONCE_TTL", 300))
	diffMode := getEnv("READ_ONCE_DIFF", "0") == "1"
	diffMax := getEnvInt("READ_ONCE_DIFF_MAX", 40)
	diffSummaryMaxHunks := getEnvInt("READ_ONCE_DIFF_SUMMARY_MAX_HUNKS", 12)
	hashMode := getEnv("READ_ONCE_HASH", "0") == "1"
	hashAlgo := strings.ToLower(getEnv("READ_ONCE_HASH_ALGO", "xxhash"))
	maxBytes := int64(getEnvInt("READ_ONCE_MAX_BYTES", 1024*1024))
	decay := int64(getEnvInt("READ_ONCE_DECAY", 60))
	autoAllow := getEnvInt("READ_ONCE_AUTO_ALLOW", 2)
	now := time.Now().Unix()

	if shouldBypassPath(filePath) || !shouldTrackByPolicy(filePath) {
		if shouldBypassPath(filePath) {
			debugSkip(cacheDir, "bypass_path_filter", filePath)
		} else {
			debugSkip(cacheDir, "policy_filter_excluded", filePath)
		}
		return nil
	}

	snapDir := filepath.Join(cacheDir, "snapshots")
	if diffMode && hasRange {
		diffMode = false // Don't do unified diffs for ranged reads against the full file
	}
	if diffMode {
		_ = os.MkdirAll(snapDir, 0o755)
	}

	runCleanup(cacheDir, snapDir, now)

	sessionHash := shortHash(sessionID)
	cacheFile := filepath.Join(cacheDir, "session-"+sessionHash+".jsonl")
	statsFile := filepath.Join(cacheDir, "stats.jsonl")
	pathHash := shortHash(cacheKey)
	snapFile := filepath.Join(snapDir, sessionHash+"-"+pathHash)

	st, err := os.Stat(filePath)
	if err != nil || st.IsDir() {
		debugSkip(cacheDir, "file_stat_failed_or_directory", filePath)
		return nil
	}
	if maxBytes > 0 && st.Size() > maxBytes {
		debugSkip(cacheDir, "file_too_large", filePath)
		return nil
	}
	if isLikelyBinary(filePath) {
		debugSkip(cacheDir, "binary_file", filePath)
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
	if hasRange && readLimit > 0 && readLimit < 100000000 {
		rangeSize := int64(readLimit) * 50
		if rangeSize < fileSize {
			fileSize = rangeSize
		}
	}
	estimatedTokens := ((fileSize / 4) * 170) / 100
	currentHash := ""
	if hashMode {
		currentHash = fileContentHash(filePath, hashAlgo)
	}

	last, ok := readLastCacheEntry(cacheFile, cacheKey)
	unchanged := ok && last.Mtime == currentMtime
	if unchanged && hashMode && last.Hash != "" && currentHash != "" && last.Hash != currentHash {
		unchanged = false
	}
	if unchanged {
		entryAge := now - last.Ts
		if last.Ts <= 0 {
			entryAge = 0
		}
		if entryAge >= ttl {
			_ = appendJSONLine(cacheFile, cacheEntry{
				Path:   cacheKey,
				Mtime:  currentMtime,
				Ts:     now,
				Tokens: estimatedTokens,
				Hash:   currentHash,
			})
			_ = appendJSONLine(statsFile, map[string]any{
				"ts":      now,
				"path":    filePath,
				"tokens":  estimatedTokens,
				"session": sessionHash,
				"event":   "expired",
			})
			if diffMode {
				_ = copyFile(filePath, snapFile, 0o644)
			}
			return nil
		}

		attempts := 1
		if now-last.LastAttemptTs <= decay {
			attempts = last.Attempts + 1
		}

		if autoAllow > 0 && attempts >= autoAllow {
			_ = appendJSONLine(cacheFile, cacheEntry{
				Path:   cacheKey,
				Mtime:  currentMtime,
				Ts:     now,
				Tokens: estimatedTokens,
				Hash:   currentHash,
			})
			_ = appendJSONLine(statsFile, map[string]any{
				"ts":      now,
				"path":    filePath,
				"tokens":  estimatedTokens,
				"session": sessionHash,
				"event":   "auto_allow",
			})
			if diffMode {
				_ = copyFile(filePath, snapFile, 0o644)
			}
			return nil
		}

		_ = appendJSONLine(cacheFile, cacheEntry{
			Path:          cacheKey,
			Mtime:         currentMtime,
			Ts:            last.Ts,
			Tokens:        estimatedTokens,
			Hash:          currentHash,
			LastAttemptTs: now,
			Attempts:      attempts,
		})

		_ = appendJSONLine(statsFile, map[string]any{
			"ts":           now,
			"path":         filePath,
			"tokens_saved": estimatedTokens,
			"session":      sessionHash,
			"event":        "hit",
		})

		minutesAgo := entryAge / 60
		ttlMin := ttl / 60
		reason := fmt.Sprintf(
			"read-once: %s is already in context (read %dm ago, unchanged; mtime=%s). Re-read allowed after %dm.",
			filepath.Base(filePath), minutesAgo, currentMtimeDisplay, ttlMin,
		)
		if autoAllow > 0 {
			reason += fmt.Sprintf(" Attempt %d/%d before auto-allow.", attempts, autoAllow)
		}
		emitHookDecision(unchangedMode, reason, shouldEmitAllowDecision(cacheDir))
		return nil
	}

	if ok && diffMode && fileExists(snapFile) {
		diffOutput, diffLines := unifiedDiff(snapFile, filePath)
		if strings.TrimSpace(diffOutput) != "" && diffLines <= diffMax {
			_ = appendJSONLine(cacheFile, cacheEntry{
				Path:   cacheKey,
				Mtime:  currentMtime,
				Ts:     now,
				Tokens: estimatedTokens,
				Hash:   currentHash,
			})
			_ = copyFile(filePath, snapFile, 0o644)

			diffTokens := int64(diffLines * 10)
			tokensSaved := estimatedTokens - diffTokens
			if tokensSaved < 0 {
				tokensSaved = 0
			}
			_ = appendJSONLine(statsFile, map[string]any{
				"ts":           now,
				"path":         filePath,
				"tokens_saved": tokensSaved,
				"session":      sessionHash,
				"event":        "diff",
			})

			reason := fmt.Sprintf(
				"read-once: %s changed since last read (previous mtime=%s, current mtime=%s). You already have the previous version in context. Here are only the changes:\n\n%s\n\nApply this diff mentally to your cached version of the file.",
				filepath.Base(filePath), formatUnixMtime(last.Mtime), currentMtimeDisplay, diffOutput,
			)
			emitHookDecision(changedMode, reason, shouldEmitAllowDecision(cacheDir))
			return nil
		}

		summary := summarizeDiff(snapFile, filePath, diffSummaryMaxHunks)
		if summary != "" {
			summaryTokens := int64(len(summary) / 4)
			tokensSaved := estimatedTokens - summaryTokens
			if tokensSaved < 0 {
				tokensSaved = 0
			}
			_ = appendJSONLine(cacheFile, cacheEntry{
				Path:   cacheKey,
				Mtime:  currentMtime,
				Ts:     now,
				Tokens: estimatedTokens,
				Hash:   currentHash,
			})
			_ = copyFile(filePath, snapFile, 0o644)
			_ = appendJSONLine(statsFile, map[string]any{
				"ts":           now,
				"path":         filePath,
				"tokens_saved": tokensSaved,
				"session":      sessionHash,
				"event":        "diff",
			})
			reason := fmt.Sprintf(
				"read-once: %s changed since last read (previous mtime=%s, current mtime=%s). Full diff was too large, so here is a compact summary:\n\n%s",
				filepath.Base(filePath), formatUnixMtime(last.Mtime), currentMtimeDisplay, summary,
			)
			emitHookDecision(changedMode, reason, shouldEmitAllowDecision(cacheDir))
			return nil
		}
	}

	if changedMode == "deny" {
		reason := fmt.Sprintf(
			"read-once: %s changed since last read (previous mtime=%s, current mtime=%s); re-read is blocked.",
			filepath.Base(filePath), formatUnixMtime(last.Mtime), currentMtimeDisplay,
		)
		emitHookDecision(changedMode, reason, shouldEmitAllowDecision(cacheDir))
		return nil
	}

	_ = appendJSONLine(cacheFile, cacheEntry{
		Path:   cacheKey,
		Mtime:  currentMtime,
		Ts:     now,
		Tokens: estimatedTokens,
		Hash:   currentHash,
	})
	if diffMode {
		_ = copyFile(filePath, snapFile, 0o644)
	}
	event := "miss"
	if ok {
		event = "changed"
	}
	_ = appendJSONLine(statsFile, map[string]any{
		"ts":      now,
		"path":    filePath,
		"tokens":  estimatedTokens,
		"session": sessionHash,
		"event":   event,
	})
	return nil
}

func emitHookDecision(mode, reason string, emitAllow bool) {
	if mode == "allow" {
		return
	}
	if mode == "deny" {
		// Keep PreToolUse output schema minimal and explicit for Codex:
		// return only hookSpecificOutput fields, no extra top-level keys.
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":            "PreToolUse",
				"permissionDecision":       "deny",
				"permissionDecisionReason": reason,
			},
		})
		return
	}
	if !emitAllow {
		return
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "allow",
			"permissionDecisionReason": reason,
		},
	})
}

func shouldEmitAllowDecision(cacheDir string) bool {
	// Codex currently rejects PreToolUse permissionDecision:"allow".
	// Return no hook output for warn-mode in Codex; deny-mode still emits.
	return !strings.Contains(filepath.ToSlash(cacheDir), "/.codex/read-once")
}

func getMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "deny":
		return "deny"
	case "allow":
		return "allow"
	default:
		return "warn"
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
	defer f.Close()
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
