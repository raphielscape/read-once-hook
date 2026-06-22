package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	clientClaude   = "claude"
	clientCodex    = "codex"
	clientOpenCode = "opencode"

	toolRead = "Read"
	toolBash = "Bash"

	eventHit       = "hit"
	eventMiss      = "miss"
	eventDiff      = "diff"
	eventExpired   = "expired"
	eventChanged   = "changed"
	eventAutoAllow = "auto_allow"

	modeAllow = "allow"
	modeDeny  = "deny"
	modeWarn  = "warn"
)

// appConfig holds the resolved paths and identifiers for the detected client.
// All path computation lives in loadAppConfig; the rest of the program treats
// this struct as read-only configuration.
type appConfig struct {
	clientName   string
	cacheDir     string
	statsFile    string
	settingsFile string
	configFile   string
	installedCLI string
	legacyHook   string
	hookCommand  string
	pluginFile   string
}

func main() {
	cmd := "help"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	home, err := os.UserHomeDir()
	if err != nil {
		failf("unable to resolve home directory: %v", err)
	}
	cfg := loadAppConfig(home)
	exe, _ := os.Executable()

	switch cmd {
	case "hook":
		if err := runHookMode(cfg.cacheDir); err != nil {
			failf("%v", err)
		}
	case "mcp":
		if err := runMCP(cfg.cacheDir); err != nil {
			failf("%v", err)
		}
	case "stats", "gain":
		if err := showStats(cfg.statsFile); err != nil {
			failf("%v", err)
		}
	case "clear":
		if err := clearSessions(cfg.cacheDir, cfg.statsFile); err != nil {
			failf("%v", err)
		}
	case "clear-file":
		if len(os.Args) < 4 {
			failf("Usage: read-once clear-file <session_id> <file_path>")
		}
		if err := clearFile(cfg.cacheDir, os.Args[2], os.Args[3]); err != nil {
			failf("%v", err)
		}
	case "install":
		if err := installHook(cfg.clientName, cfg.settingsFile, cfg.cacheDir, exe, cfg.installedCLI, cfg.hookCommand); err != nil {
			failf("%v", err)
		}
	case "optimize":
		if err := optimizeSetup(cfg.clientName, cfg.settingsFile, cfg.hookCommand); err != nil {
			failf("%v", err)
		}
	case "upgrade":
		if err := upgradeHook(exe, cfg.installedCLI); err != nil {
			failf("%v", err)
		}
	case "status":
		if err := showStatus(cfg.clientName, cfg.settingsFile, cfg.installedCLI, cfg.legacyHook, cfg.statsFile); err != nil {
			failf("%v", err)
		}
	case "verify", "check", "test":
		if err := verify(cfg.clientName, cfg.settingsFile, cfg.configFile, cfg.installedCLI, cfg.legacyHook, exe, cfg.hookCommand); err != nil {
			failf("%v", err)
		}
	case "uninstall":
		if err := uninstall(cfg.clientName, cfg.settingsFile); err != nil {
			failf("%v", err)
		}
	case "help", "--help", "-h":
		printHelp()
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		fmt.Println("Run 'read-once help' for usage.")
		os.Exit(1)
	}
}

func loadAppConfig(home string) appConfig {
	client := strings.ToLower(strings.TrimSpace(getEnv("READ_ONCE_CLIENT", "")))
	if client != clientClaude && client != clientCodex && client != clientOpenCode {
		client = detectDefaultClient(home)
	}
	cacheRoot := filepath.Join(home, ".claude")
	settingsFile := filepath.Join(cacheRoot, "settings.json")
	configFile := settingsFile
	switch client {
	case clientCodex:
		cacheRoot = filepath.Join(home, ".codex")
		settingsFile = filepath.Join(cacheRoot, "hooks.json")
		configFile = filepath.Join(cacheRoot, "config.toml")
	case clientOpenCode:
		cacheRoot = filepath.Join(home, ".config", "opencode")
		settingsFile = filepath.Join(cacheRoot, "opencode.json")
		configFile = settingsFile
	}
	cacheDir := filepath.Join(cacheRoot, "read-once")
	cacheDir = getEnv("READ_ONCE_HOME", cacheDir)
	settingsFile = getEnv("READ_ONCE_SETTINGS_FILE", settingsFile)
	configFile = getEnv("READ_ONCE_CONFIG_FILE", configFile)
	hookCommand := getEnv("READ_ONCE_HOOK_COMMAND", expandHome(filepath.ToSlash(filepath.Join(cacheDir, "read-once")))+" hook")
	// installedCLI/hookCommand are the primary runtime paths used by modern installs.
	// legacyHook is kept only for backward compatibility checks/migration in verify.
	return appConfig{
		clientName:   client,
		cacheDir:     cacheDir,
		statsFile:    filepath.Join(cacheDir, "stats.jsonl"),
		settingsFile: settingsFile,
		configFile:   configFile,
		installedCLI: filepath.Join(cacheDir, "read-once"),
		legacyHook:   filepath.Join(cacheDir, "hook.sh"),
		hookCommand:  hookCommand,
		pluginFile:   filepath.Join(filepath.Dir(settingsFile), "plugins", "read-once.js"),
	}
}

func detectDefaultClient(home string) string {
	codexRoot := filepath.Clean(filepath.Join(home, ".codex"))
	opencodeRoot := filepath.Clean(filepath.Join(home, ".config", "opencode"))
	if wd, err := os.Getwd(); err == nil && isWithinDir(wd, codexRoot) {
		return clientCodex
	}
	if wd, err := os.Getwd(); err == nil && isWithinDir(wd, opencodeRoot) {
		return clientOpenCode
	}
	if exe, err := os.Executable(); err == nil && isWithinDir(exe, codexRoot) {
		return clientCodex
	}
	if exe, err := os.Executable(); err == nil && isWithinDir(exe, opencodeRoot) {
		return clientOpenCode
	}
	return clientClaude
}

func failf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
