package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type verifyState struct {
	Issues int
	Checks int
	Passed int
}

func (v *verifyState) pass(msg string) {
	v.Checks++
	v.Passed++
	fmt.Printf("  [ok]   %s\n", msg)
}

func (v *verifyState) fail(msg, fix string) {
	v.Checks++
	v.Issues++
	fmt.Printf("  [FAIL] %s\n", msg)
	if fix != "" {
		fmt.Printf("         Fix: %s\n", fix)
	}
}

func (v *verifyState) warn(msg string) {
	v.Checks++
	fmt.Printf("  [warn] %s\n", msg)
}

func showStats(statsFile string) error {
	entries, err := readEvents(statsFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("No read-once data yet. Stats appear after your first session with the hook installed.")
			return nil
		}
		return err
	}

	var hits, diffs, misses, changed, expired int64
	var tokensSaved, tokensAllowed int64
	sessions := map[string]struct{}{}
	hitFiles := map[string]int64{}

	for _, e := range entries {
		switch e.Event {
		case "hit":
			hits++
			tokensSaved += e.TokensSaved
			if e.Path != "" {
				hitFiles[filepath.Base(e.Path)]++
			}
		case "diff":
			diffs++
			tokensSaved += e.TokensSaved
		case "miss":
			misses++
			tokensAllowed += e.Tokens
		case "changed":
			changed++
			tokensAllowed += e.Tokens
		case "expired":
			expired++
			tokensAllowed += e.Tokens
		}
		if e.Session != "" {
			sessions[e.Session] = struct{}{}
		}
	}

	totalReads := hits + diffs + misses + changed + expired
	if totalReads == 0 {
		fmt.Println("No reads tracked yet.")
		return nil
	}

	tokensTotal := tokensAllowed + tokensSaved
	savingsPct := int64(0)
	if tokensTotal > 0 {
		savingsPct = (tokensSaved * 100) / tokensTotal
	}
	ttl := getEnvInt("READ_ONCE_TTL", 300)
	ttlMin := ttl / 60

	fmt.Println("read-once - file read deduplication")
	fmt.Println()
	fmt.Printf("  Total file reads:    %d\n", totalReads)
	fmt.Printf("  Cache hits:          %d (blocked re-reads)\n", hits)
	if diffs > 0 {
		fmt.Printf("  Diff hits:           %d (changed files - sent diff only)\n", diffs)
	}
	fmt.Printf("  First reads:         %d\n", misses)
	fmt.Printf("  Changed files:       %d (full re-read after modification)\n", changed)
	fmt.Printf("  TTL expired:         %d (re-read after %dm - compaction safety)\n", expired, ttlMin)
	fmt.Println()
	fmt.Printf("  Tokens saved:        ~%d\n", tokensSaved)
	fmt.Printf("  Read token total:    ~%d\n", tokensTotal)
	fmt.Printf("  Savings:             %d%%\n", savingsPct)
	if tokensSaved > 0 {
		fmt.Printf("  Est. cost saved:     $%.4f (Sonnet) / $%.4f (Opus)\n", float64(tokensSaved)*3/1_000_000, float64(tokensSaved)*15/1_000_000)
	}
	fmt.Println()
	if hits > 0 && len(hitFiles) > 0 {
		fmt.Println("  Top re-read files:")
		type kv struct {
			name  string
			count int64
		}
		var list []kv
		for k, v := range hitFiles {
			list = append(list, kv{name: k, count: v})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].count == list[j].count {
				return list[i].name < list[j].name
			}
			return list[i].count > list[j].count
		})
		max := 5
		if len(list) < max {
			max = len(list)
		}
		for i := 0; i < max; i++ {
			fmt.Printf("    %dx  %s\n", list[i].count, list[i].name)
		}
		fmt.Println()
	}
	fmt.Printf("  Sessions tracked:    %d\n", len(sessions))
	fmt.Printf("  Cache TTL:           %d minutes (READ_ONCE_TTL=%ds)\n", ttlMin, ttl)
	return nil
}

func clearFile(cacheDir, sessionID, filePath string) error {
	sessionHash := shortHash(sessionID)
	cacheFile := filepath.Join(cacheDir, "session-"+sessionHash+".jsonl")

	if !fileExists(cacheFile) {
		return nil // Nothing to clear
	}

	absPath, err := filepath.Abs(filePath)
	if err == nil {
		filePath = filepath.Clean(absPath)
	}

	entries, err := readLastCacheEntries(cacheFile)
	if err != nil {
		return err
	}

	clearedCount := 0
	for key := range entries {
		// Clear base path and any ranged variants (e.g., path:0:100)
		if key == filePath || strings.HasPrefix(key, filePath+":") {
			err := appendJSONLine(cacheFile, cacheEntry{
				Path:   key,
				Mtime:  "cleared",
				Ts:     0,
				Tokens: 0,
				Hash:   "",
			})
			if err != nil {
				if err.Error() == "lock timeout" {
					return errors.New("failed to acquire cache lock, try again")
				}
				return err
			}
			clearedCount++
		}
	}

	if clearedCount == 0 {
		return nil
	}
	return nil
}

func readLastCacheEntries(cacheFile string) (map[string]cacheEntry, error) {
	f, err := os.Open(cacheFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	entries := make(map[string]cacheEntry)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c cacheEntry
		if json.Unmarshal([]byte(line), &c) == nil {
			entries[c.Path] = c
		}
	}
	return entries, sc.Err()
}

func clearSessions(cacheDir, statsFile string) error {
	matches, err := filepath.Glob(filepath.Join(cacheDir, "session-*.jsonl"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
	// Remove any stale lock files. A lock file left by a SIGKILL'd hook process persists
	// indefinitely and causes all subsequent writes to that path to be skipped on timeout.
	// clearSessions is user-invoked so there is no concurrency concern here.
	locks, _ := filepath.Glob(filepath.Join(cacheDir, "*.lock"))
	for _, l := range locks {
		_ = os.Remove(l)
	}
	fmt.Println("Session cache cleared. Stats preserved.")
	fmt.Printf("To clear stats too: rm %s\n", statsFile)
	return nil
}

func installHook(clientName, settingsFile, cacheDir, sourceExe, installedCLI, hookCommand string) error {
	if clientName == "opencode" {
		return installOpenCodeHook(cacheDir, sourceExe, installedCLI)
	}
	if err := os.MkdirAll(filepath.Dir(settingsFile), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(settingsFile); errors.Is(err, os.ErrNotExist) {
		fmt.Printf("No settings file found at %s. Creating one.\n", settingsFile)
		if err := os.WriteFile(settingsFile, []byte("{}\n"), 0o644); err != nil {
			return err
		}
	}

	raw, err := os.ReadFile(settingsFile)
	if err != nil {
		return err
	}
	settings, err := parseJSONMap(raw)
	if err != nil {
		return fmt.Errorf("settings.json is invalid JSON: %w", err)
	}
	matcher := "Read"
	if clientName == "codex" {
		matcher = "Bash"
	}
	if hasReadOnceHookForTool(settings, matcher) {
		if !fileExists(installedCLI) {
			if err := os.MkdirAll(cacheDir, 0o755); err != nil {
				return err
			}
			if err := copyFile(sourceExe, installedCLI, 0o755); err != nil {
				return fmt.Errorf("copy binary: %w", err)
			}
			fmt.Println("read-once hook already configured; installed missing binary.")
			fmt.Printf("Binary: %s\n", installedCLI)
			return nil
		}
		fmt.Println("read-once hook is already installed.")
		return nil
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	if err := copyFile(sourceExe, installedCLI, 0o755); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}

	hooksVal, ok := settings["hooks"]
	if !ok {
		hooksVal = map[string]any{}
		settings["hooks"] = hooksVal
	}
	hooksMap, ok := hooksVal.(map[string]any)
	if !ok {
		hooksMap = map[string]any{}
		settings["hooks"] = hooksMap
	}

	pre, ok := hooksMap["PreToolUse"]
	if !ok {
		pre = []any{}
		hooksMap["PreToolUse"] = pre
	}
	preSlice, ok := pre.([]any)
	if !ok {
		preSlice = []any{}
	}
	preSlice = append(preSlice, map[string]any{
		"matcher": matcher,
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": hookCommand,
			},
		},
	})
	hooksMap["PreToolUse"] = preSlice

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(settingsFile, append(out, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Println("read-once hook installed.")
	fmt.Printf("Binary: %s\n\n", installedCLI)
	fmt.Printf("Matcher: %s\n", matcher)
	fmt.Printf("Config:  %s\n\n", settingsFile)
	fmt.Println("Sessions will now track and deduplicate file reads.")
	fmt.Println("The hook is installed at a stable path - you can move or delete the source repo.")
	return nil
}

func upgradeHook(sourceExe, installedCLI string) error {
	if _, err := os.Stat(installedCLI); errors.Is(err, os.ErrNotExist) {
		fmt.Println("Hook not installed yet. Run: read-once install")
		return nil
	}
	if err := copyFile(sourceExe, installedCLI, 0o755); err != nil {
		return err
	}
	fmt.Println("Hook upgraded to latest version.")
	return nil
}

func optimizeSetup(clientName, settingsFile, hookCommand string) error {
	if clientName == "opencode" {
		return optimizeOpenCodePlugin(settingsFile, hookCommand)
	}
	raw, err := os.ReadFile(settingsFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s not found. Run: read-once install", settingsFile)
		}
		return err
	}
	settings, err := parseJSONMap(raw)
	if err != nil {
		return fmt.Errorf("settings.json is invalid JSON: %w", err)
	}
	hooksMap, ok := settings["hooks"].(map[string]any)
	if !ok {
		return errors.New("no hooks configured. Run: read-once install")
	}
	pre, ok := hooksMap["PreToolUse"].([]any)
	if !ok || len(pre) == 0 {
		return errors.New("no PreToolUse hooks configured. Run: read-once install")
	}

	optimal := optimalHookCommand(hookCommand)
	targetMatcher := "Read"
	if clientName == "codex" {
		targetMatcher = "Bash"
	}
	updated := 0
	for i, item := range pre {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		matcher, _ := m["matcher"].(string)
		if !matcherMatchesTool(matcher, targetMatcher) {
			continue
		}
		hs, ok := m["hooks"].([]any)
		if !ok || len(hs) == 0 {
			continue
		}
		for j, h := range hs {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			cmd, _ := hm["command"].(string)
			if cmd == "" || !strings.Contains(cmd, "read-once") {
				continue
			}
			hm["command"] = optimal
			hs[j] = hm
			updated++
		}
		m["hooks"] = hs
		pre[i] = m
	}
	if updated == 0 {
		return fmt.Errorf("no read-once %s matcher found. Run: read-once install", targetMatcher)
	}

	hooksMap["PreToolUse"] = pre
	settings["hooks"] = hooksMap
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(settingsFile, append(out, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println("Optimized read-once hook configuration applied.")
	fmt.Printf("Command: %s\n", optimal)
	return nil
}

func showStatus(clientName, settingsFile, installedCLI, legacyHook, statsFile string) error {
	fmt.Println("read-once status")
	fmt.Println()
	fmt.Printf("  Client:        %s\n", clientName)

	if _, err := os.Stat(installedCLI); err == nil {
		fmt.Printf("  Hook binary:   %s (exists)\n", installedCLI)
	} else if _, err := os.Stat(legacyHook); err == nil {
		fmt.Printf("  Hook file:     %s (legacy install)\n", legacyHook)
	} else {
		fmt.Println("  Hook:          NOT INSTALLED - run: read-once install")
	}

	if clientName == "opencode" {
		pluginFile := filepath.Join(filepath.Dir(settingsFile), "plugins", "read-once.js")
		if fileExists(pluginFile) {
			fmt.Printf("  Plugin:        Configured in %s\n", pluginFile)
		} else {
			fmt.Println("  Plugin:        NOT configured - run: read-once install")
		}
		toolFile := filepath.Join(filepath.Dir(settingsFile), "tools", "readOnceClearCache.js")
		if fileExists(toolFile) {
			fmt.Printf("  Tool:          Configured in %s\n", toolFile)
		} else {
			fmt.Println("  Tool:          NOT configured - run: read-once install")
		}
	} else {
		raw, _ := os.ReadFile(settingsFile)
		if len(raw) > 0 && strings.Contains(string(raw), "read-once") {
			fmt.Printf("  Settings:      Configured in %s\n", settingsFile)
		} else {
			fmt.Println("  Settings:      NOT configured - run: read-once install")
		}
	}

	entries, err := readEvents(statsFile)
	if err == nil {
		var hits int64
		for _, e := range entries {
			if e.Event == "hit" {
				hits++
			}
		}
		fmt.Printf("  Data:          %d events, %d hits\n", len(entries), hits)
	} else {
		fmt.Println("  Data:          No data yet")
	}

	ttl := getEnvInt("READ_ONCE_TTL", 300)
	fmt.Printf("  TTL:           %ds (%dm)\n", ttl, ttl/60)
	fmt.Printf("  Disabled:      %s\n", getEnv("READ_ONCE_DISABLED", "0"))
	return nil
}

func verify(clientName, settingsFile, configFile, installedCLI, legacyHook, sourceExe, fallbackHookCommand string) error {
	v := &verifyState{}

	fmt.Println("read-once verify")
	fmt.Println()

	fmt.Println("Dependencies:")
	if ver := strings.TrimSpace(runOutput("go", "version")); ver != "" {
		v.pass(fmt.Sprintf("go runtime available (%s)", ver))
	} else {
		v.warn("go runtime version unavailable")
	}

	if _, err := exec.LookPath("diff"); err == nil {
		v.pass("diff found (needed for diff mode)")
	} else {
		v.warn("diff not found (diff mode will be unavailable)")
	}
	fmt.Println()

	fmt.Println("Installation:")
	if st, err := os.Stat(installedCLI); err == nil {
		v.pass(fmt.Sprintf("Hook binary exists at %s", installedCLI))
		if st.Mode()&0o111 != 0 {
			v.pass("Hook binary is executable")
		} else {
			v.fail("Hook binary is not executable", fmt.Sprintf("chmod +x %s", installedCLI))
		}
		if sourceExe != installedCLI {
			if same, err := filesEqual(sourceExe, installedCLI); err == nil && same {
				v.pass("Installed binary matches source (up to date)")
			} else if err == nil {
				v.warn("Installed binary differs from source (run: read-once upgrade)")
			}
		}
	} else if st, err := os.Stat(legacyHook); err == nil {
		v.warn(fmt.Sprintf("Legacy hook.sh install detected at %s", legacyHook))
		if st.Mode()&0o111 != 0 {
			v.pass("Legacy hook is executable")
		} else {
			v.fail("Legacy hook is not executable", fmt.Sprintf("chmod +x %s", legacyHook))
		}
	} else {
		v.fail(fmt.Sprintf("Hook binary not found at %s", installedCLI), "read-once install")
	}

	raw, err := os.ReadFile(settingsFile)
	targetMatcher := "Read"
	if clientName == "codex" || clientName == "opencode" {
		targetMatcher = "Bash"
	}
	if err == nil {
		v.pass(fmt.Sprintf("%s exists", settingsFile))
		if clientName == "opencode" {
			if _, parseErr := parseJSONMap(raw); parseErr == nil {
				v.pass("opencode config file is valid JSON")
			} else {
				v.fail(fmt.Sprintf("%s is invalid JSON", settingsFile), fmt.Sprintf("Check for syntax errors: jq . %s", settingsFile))
			}
			pluginFile := filepath.Join(filepath.Dir(settingsFile), "plugins", "read-once.js")
			if b, readErr := os.ReadFile(pluginFile); readErr == nil {
				v.pass(fmt.Sprintf("opencode plugin exists at %s", pluginFile))
				if strings.Contains(string(b), "read-once") {
					v.pass("opencode plugin references read-once binary")
				} else {
					v.warn("opencode plugin does not appear to reference read-once binary")
				}
			} else {
				v.fail(fmt.Sprintf("opencode plugin missing: %s", pluginFile), "read-once install")
			}
			toolFile := filepath.Join(filepath.Dir(settingsFile), "tools", "readOnceClearCache.js")
			if b, readErr := os.ReadFile(toolFile); readErr == nil {
				v.pass(fmt.Sprintf("opencode tool exists at %s", toolFile))
				if strings.Contains(string(b), "read-once") {
					v.pass("opencode tool references read-once binary")
				} else {
					v.warn("opencode tool does not appear to reference read-once binary")
				}
			} else {
				v.warn(fmt.Sprintf("opencode tool missing: %s (run read-once install)", toolFile))
			}
		} else if strings.HasSuffix(strings.ToLower(strings.TrimSpace(settingsFile)), ".json") {
			settings, parseErr := parseJSONMap(raw)
			if parseErr == nil {
				v.pass("hooks settings file is valid JSON")
				hookCmd := readMatcherReadOnceCommand(settings, targetMatcher)
				if hookCmd != "" {
					v.pass(fmt.Sprintf("PreToolUse %s matcher configured", targetMatcher))
					spec := parseCommandSpec(hookCmd)
					if len(spec.argv) == 0 {
						v.fail(fmt.Sprintf("Hook command is empty: %s", hookCmd), "read-once install")
					} else if resolved, ok := resolveExecutable(spec.argv[0]); ok {
						v.pass(fmt.Sprintf("Hook command path resolves (%s)", resolved))
					} else {
						v.fail(fmt.Sprintf("Hook command path does not exist: %s", hookCmd), "read-once install")
					}
				} else {
					v.fail(fmt.Sprintf("No PreToolUse %s matcher in settings.json", targetMatcher), "read-once install")
				}
			} else {
				v.fail(fmt.Sprintf("%s is invalid JSON", settingsFile), fmt.Sprintf("Check for syntax errors: jq . %s", settingsFile))
			}
		} else {
			v.warn(fmt.Sprintf("non-JSON settings file detected (%s) - skipping automatic Read-matcher validation", settingsFile))
		}
	} else {
		v.warn(fmt.Sprintf("settings file not found (%s) - install is optional if your runtime wires hooks externally", settingsFile))
	}
	if clientName == "codex" && configFile != "" {
		if cfgRaw, cfgErr := os.ReadFile(configFile); cfgErr == nil {
			if codexHooksEnabled(string(cfgRaw)) {
				v.pass(fmt.Sprintf("codex hooks feature enabled in %s", configFile))
			} else {
				v.warn(fmt.Sprintf("codex hooks feature may be disabled in %s (set features.codex_hooks = true)", configFile))
			}
		}
	}
	fmt.Println()

	fmt.Println("Dry-run test:")
	testHookCmd := readMatcherReadOnceCommand(parseJSONMapSafe(raw), targetMatcher)
	if strings.TrimSpace(testHookCmd) == "" {
		testHookCmd = readAnyReadOnceCommand(parseJSONMapSafe(raw))
	}
	if strings.TrimSpace(testHookCmd) == "" {
		if fileExists(installedCLI) {
			testHookCmd = fallbackHookCommand
		} else if fileExists(legacyHook) {
			testHookCmd = legacyHook
		}
	}
	if strings.TrimSpace(testHookCmd) != "" {
		if err := runDryRun(v, testHookCmd, clientName); err != nil {
			v.fail("Dry-run could not complete", err.Error())
		}
	} else {
		v.warn("Skipping dry-run (no hook command found)")
	}
	fmt.Println()

	fmt.Println("Configuration:")
	mode := getMode(getEnv("READ_ONCE_MODE", "warn"))
	modeUnchanged := getMode(getEnv("READ_ONCE_MODE_UNCHANGED", mode))
	modeChanged := getMode(getEnv("READ_ONCE_MODE_CHANGED", mode))
	ttl := getEnvInt("READ_ONCE_TTL", 300)
	diff := getEnv("READ_ONCE_DIFF", "0")
	diffMax := getEnvInt("READ_ONCE_DIFF_MAX", 40)
	diffSummaryMaxHunks := getEnvInt("READ_ONCE_DIFF_SUMMARY_MAX_HUNKS", 12)
	hashMode := getEnv("READ_ONCE_HASH", "0")
	hashAlgo := strings.ToLower(getEnv("READ_ONCE_HASH_ALGO", "xxhash"))
	maxBytes := getEnvInt("READ_ONCE_MAX_BYTES", 1024*1024)
	include := getEnv("READ_ONCE_INCLUDE", "")
	exclude := getEnv("READ_ONCE_EXCLUDE", "")
	disabled := getEnv("READ_ONCE_DISABLED", "0")
	debugDefault := "0"
	if clientName == "codex" {
		debugDefault = "1"
	}
	debug := getEnv("READ_ONCE_DEBUG", debugDefault)
	fmt.Printf("  Mode:     %s (READ_ONCE_MODE)\n", mode)
	fmt.Printf("  Unchanged:%s (READ_ONCE_MODE_UNCHANGED)\n", modeUnchanged)
	fmt.Printf("  Changed:  %s (READ_ONCE_MODE_CHANGED)\n", modeChanged)
	fmt.Printf("  TTL:      %ds (%dm) (READ_ONCE_TTL)\n", ttl, ttl/60)
	fmt.Printf("  Diff:     %s (READ_ONCE_DIFF)\n", diff)
	fmt.Printf("  Diff max: %d lines (READ_ONCE_DIFF_MAX)\n", diffMax)
	fmt.Printf("  Diff sum: %d hunks (READ_ONCE_DIFF_SUMMARY_MAX_HUNKS)\n", diffSummaryMaxHunks)
	fmt.Printf("  Hash:     %s (READ_ONCE_HASH)\n", hashMode)
	fmt.Printf("  Hash alg: %s (READ_ONCE_HASH_ALGO)\n", hashAlgo)
	fmt.Printf("  Max size: %d bytes (READ_ONCE_MAX_BYTES)\n", maxBytes)
	fmt.Printf("  Include:  %s (READ_ONCE_INCLUDE)\n", defaultIfEmpty(include, "<none>"))
	fmt.Printf("  Exclude:  %s (READ_ONCE_EXCLUDE)\n", defaultIfEmpty(exclude, "<none>"))
	fmt.Printf("  Disabled: %s (READ_ONCE_DISABLED)\n", disabled)
	fmt.Printf("  Debug:    %s (READ_ONCE_DEBUG)\n", debug)
	fmt.Println()

	if v.Issues == 0 {
		fmt.Printf("%d/%d checks passed. read-once is ready.\n", v.Passed, v.Checks)
		return nil
	}
	fmt.Printf("%d/%d checks passed, %d issue(s) found.\n", v.Passed, v.Checks, v.Issues)
	fmt.Println("Fix the issues above, then run 'read-once verify' again.")
	return errors.New("verification failed")
}

func runDryRun(v *verifyState, hookCommand, clientName string) error {
	tmp, err := os.MkdirTemp("", "read-once-verify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	testFile := filepath.Join(tmp, "verify-test-file.txt")
	if err := os.WriteFile(testFile, []byte("read-once verify test content\n"), 0o644); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("verify-%d", time.Now().UnixNano())))
	sid := "verify-" + hex.EncodeToString(sum[:8])
	input := map[string]any{
		"tool_name": "Read",
		"tool_input": map[string]any{
			"file_path": testFile,
		},
		"session_id": sid,
		"cwd":        tmp,
	}
	if clientName == "codex" {
		input["tool_name"] = "Bash"
		input["tool_input"] = map[string]any{
			"command": "cat " + testFile,
		}
	}
	inputRaw, _ := json.Marshal(input)

	out1, code1 := runConfiguredHook(hookCommand, tmp, inputRaw)
	if code1 == 0 && strings.TrimSpace(out1) == "" {
		v.pass("First read: allowed (no output = pass-through)")
	} else if code1 == 0 {
		v.warn("First read: unexpected output (expected empty for first read)")
	} else {
		v.fail(fmt.Sprintf("First read: hook exited with code %d", code1), "Check hook command configuration")
	}

	out2, code2 := runConfiguredHook(hookCommand, tmp, inputRaw)
	if code2 == 0 && strings.TrimSpace(out2) != "" {
		var data map[string]any
		if json.Unmarshal([]byte(out2), &data) == nil {
			v.pass("Second read: produced valid JSON response")
			mode := "unknown"
			if hs, ok := data["hookSpecificOutput"].(map[string]any); ok {
				if p, ok := hs["permissionDecision"].(string); ok && (p == "allow" || p == "deny") {
					mode = p
				}
			}
			if mode != "unknown" {
				v.pass(fmt.Sprintf("Second read: correctly detected re-read (mode: %s)", mode))
			} else {
				v.warn("Second read: output format unexpected")
			}
		} else {
			v.fail("Second read: output is not valid JSON", "Check hook command output formatting")
		}
	} else if code2 == 0 {
		v.pass("Second read: no output (pass-through/strict-runtime compatibility)")
	} else {
		v.fail(fmt.Sprintf("Second read: hook exited with code %d", code2), "Check hook command execution errors")
	}
	return nil
}

func runConfiguredHook(command, home string, input []byte) (string, int) {
	spec := parseCommandSpec(command)
	argv := spec.argv
	if len(argv) == 0 {
		return "", 1
	}
	argv[0] = expandHome(argv[0])
	cmd := exec.Command(argv[0], argv[1:]...)
	env := append(os.Environ(), "HOME="+home)
	for k, v := range spec.env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(input)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if err == nil {
		return out.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.String(), exitErr.ExitCode()
	}
	return out.String(), 1
}

func uninstall(clientName, settingsFile string) error {
	if clientName == "opencode" {
		pluginFile := filepath.Join(filepath.Dir(settingsFile), "plugins", "read-once.js")
		if err := os.Remove(pluginFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		toolFile := filepath.Join(filepath.Dir(settingsFile), "tools", "readOnceClearCache.js")
		if err := os.Remove(toolFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		fmt.Println("read-once plugin and tool removed from opencode directories.")
		return nil
	}
	raw, err := os.ReadFile(settingsFile)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("No settings file found.")
		return nil
	}
	if err != nil {
		return err
	}
	settings, err := parseJSONMap(raw)
	if err != nil {
		return err
	}

	hooksVal, ok := settings["hooks"].(map[string]any)
	if !ok {
		fmt.Println("read-once hook removed from settings.")
		return nil
	}
	pre, ok := hooksVal["PreToolUse"].([]any)
	if !ok {
		fmt.Println("read-once hook removed from settings.")
		return nil
	}

	var filtered []any
	for _, item := range pre {
		m, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		if hs, ok := m["hooks"].([]any); ok && len(hs) > 0 {
			keptHooks := make([]any, 0, len(hs))
			for _, h := range hs {
				hm, ok := h.(map[string]any)
				if !ok {
					keptHooks = append(keptHooks, h)
					continue
				}
				cmd, _ := hm["command"].(string)
				if strings.Contains(cmd, "read-once") {
					continue
				}
				keptHooks = append(keptHooks, h)
			}
			if len(keptHooks) == 0 {
				continue
			}
			m["hooks"] = keptHooks
			filtered = append(filtered, m)
			continue
		}
		if _, ok := m["hooks"]; !ok {
			filtered = append(filtered, item)
		}
	}
	hooksVal["PreToolUse"] = filtered

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(settingsFile, append(out, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println("read-once hook removed from settings.")
	if clientName == "codex" {
		fmt.Println("If needed, keep features.codex_hooks = true in ~/.codex/config.toml for other hooks.")
	}
	return nil
}

func printHelp() {
	fmt.Println("read-once - Stop repeated file reads in agent sessions")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  read-once stats       Show token savings")
	fmt.Println("  read-once gain        Same as stats (RTK-style)")
	fmt.Println("  read-once status      Quick health check")
	fmt.Println("  read-once verify      Full diagnostic with dry-run test")
	fmt.Println("  read-once clear       Clear session cache")
	fmt.Println("  read-once install     Install hook (Claude/Codex hooks JSON, OpenCode plugin)")
	fmt.Println("  read-once optimize    Apply recommended high-performance hook settings")
	fmt.Println("  read-once upgrade     Update hook to latest version")
	fmt.Println("  read-once uninstall   Remove hook")
	fmt.Println()
	fmt.Println("How it works:")
	fmt.Println("  A pre-tool hook intercepts Read calls. When an agent tries to")
	fmt.Println("  re-read a file it already read this session (and the file hasn't")
	fmt.Println("  changed), the hook blocks the read and tells the agent the content")
	fmt.Println("  is already in context. Saves ~2000+ tokens per prevented re-read.")
	fmt.Println()
	fmt.Println("Compaction safety:")
	fmt.Println("  Cache entries expire after READ_ONCE_TTL seconds (default: 300 = 5m).")
	fmt.Println("  After expiry, re-reads are allowed because the agent may have compacted")
	fmt.Println("  the context window and lost the earlier content.")
	fmt.Println()
	fmt.Println("Config (environment variables):")
	fmt.Println("  READ_ONCE_MODE=warn     'warn' (default) allows read with advisory.")
	fmt.Println("                          'deny' blocks reads entirely (maximum savings).")
	fmt.Println("  READ_ONCE_MODE_UNCHANGED Override mode for unchanged-file re-reads.")
	fmt.Println("  READ_ONCE_MODE_CHANGED   Override mode for changed-file handling.")
	fmt.Println("  READ_ONCE_TTL=300       Cache TTL in seconds (default: 300)")
	fmt.Println("  READ_ONCE_DIFF=1        Return inline diff/summary for changed files")
	fmt.Println("  READ_ONCE_DIFF_MAX=40   Max diff lines before switching to summary")
	fmt.Println("  READ_ONCE_HASH=1        Validate unchanged reads by content hash")
	fmt.Println("  READ_ONCE_HASH_ALGO=xxhash  Hash algorithm: xxhash (default) or sha256")
	fmt.Println("  READ_ONCE_MAX_BYTES=1048576  Skip very large files")
	fmt.Println("  READ_ONCE_INCLUDE=...   Optional include patterns (glob/re:regex)")
	fmt.Println("  READ_ONCE_EXCLUDE=...   Optional exclude patterns (glob/re:regex)")
	fmt.Println("  READ_ONCE_DISABLED=1    Disable the hook entirely")
	fmt.Println("  READ_ONCE_DEBUG=1       Log skipped tracking reasons to $READ_ONCE_HOME/debug.log")
	fmt.Println("  READ_ONCE_CLIENT=claude|codex|opencode  Select defaults for path/layout")
	fmt.Println("  READ_ONCE_HOME=...      Override cache/binary directory")
	fmt.Println("  READ_ONCE_SETTINGS_FILE=...  Override settings file path")
	fmt.Println("  READ_ONCE_HOOK_COMMAND=...   Override hook command string")
}
