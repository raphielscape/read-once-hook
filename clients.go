package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

//go:embed templates/opencode_plugin.js.tmpl
var openCodePluginTmpl string

//go:embed templates/opencode_tool.js.tmpl
var openCodeToolTmpl string

// pluginTemplateData is the data contract for opencode_plugin.js.tmpl.
// Binary must be a pre-quoted JS string literal (use strconv.Quote).
// EnvBlock is the raw JS snippet placed inside the Bun.spawn options object.
type pluginTemplateData struct {
	Binary   string // e.g. `"/home/user/.config/opencode/read-once/read-once"`
	EnvBlock string // e.g. `env: process.env,` or the optimized multi-line block
}

func installOpenCodeHook(cacheDir, sourceExe, installedCLI string) error {
	settingsFile := filepath.Join(filepath.Dir(cacheDir), "opencode.json")
	if err := os.MkdirAll(filepath.Dir(settingsFile), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(settingsFile); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(settingsFile, []byte("{}\n"), 0o644); err != nil {
			return err
		}
		fmt.Printf("No opencode config found at %s. Created default config.\n", settingsFile)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	if err := copyFile(sourceExe, installedCLI, 0o755); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}
	pluginDir := filepath.Join(filepath.Dir(settingsFile), "plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return err
	}
	pluginFile := filepath.Join(pluginDir, "read-once.js")
	// OpenCode defaults to the optimized profile to reduce read churn and token cost.
	pluginBody, err := renderOpenCodePlugin(installedCLI, true)
	if err != nil {
		return fmt.Errorf("render plugin template: %w", err)
	}
	if err := writeFileAtomic(pluginFile, []byte(pluginBody), 0o644); err != nil {
		return err
	}

	toolDir := filepath.Join(filepath.Dir(settingsFile), "tools")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		return err
	}
	toolFile := filepath.Join(toolDir, "readOnceClearCache.js")
	toolBody, err := renderOpenCodeTool(installedCLI, true)
	if err != nil {
		return fmt.Errorf("render tool template: %w", err)
	}
	if err := writeFileAtomic(toolFile, []byte(toolBody), 0o644); err != nil {
		return err
	}

	fmt.Println("read-once hook installed for opencode.")
	fmt.Printf("Binary: %s\n", installedCLI)
	fmt.Printf("Plugin: %s\n", pluginFile)
	fmt.Printf("Tool:   %s\n", toolFile)
	return nil
}

func optimizeOpenCodePlugin(settingsFile, hookCommand string) error {
	pluginFile := filepath.Join(filepath.Dir(settingsFile), "plugins", "read-once.js")
	if !fileExists(pluginFile) {
		return fmt.Errorf("opencode plugin not found at %s. Run: read-once install", pluginFile)
	}
	spec := parseCommandSpec(hookCommand)
	if len(spec.argv) == 0 {
		return errors.New("hook command is empty")
	}
	binary := expandHome(spec.argv[0])
	if !fileExists(binary) {
		return fmt.Errorf("hook binary not found at %s", binary)
	}
	pluginBody, err := renderOpenCodePlugin(binary, true)
	if err != nil {
		return fmt.Errorf("render plugin template: %w", err)
	}
	if err := writeFileAtomic(pluginFile, []byte(pluginBody), 0o644); err != nil {
		return err
	}

	toolDir := filepath.Join(filepath.Dir(settingsFile), "tools")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		return err
	}
	toolFile := filepath.Join(toolDir, "readOnceClearCache.js")
	toolBody, err := renderOpenCodeTool(binary, true)
	if err != nil {
		return fmt.Errorf("render tool template: %w", err)
	}
	if err := writeFileAtomic(toolFile, []byte(toolBody), 0o644); err != nil {
		return err
	}

	fmt.Println("Optimized read-once OpenCode plugin configuration applied.")
	fmt.Printf("Plugin: %s\n", pluginFile)
	fmt.Printf("Tool:   %s\n", toolFile)
	return nil
}

// renderOpenCodePlugin renders the OpenCode JS plugin from the embedded template.
// binary is the absolute path to the read-once binary on the target machine.
// optimized=true injects the recommended env vars that maximise token savings
// (unchanged-reads denied, diff mode enabled, hash validation on).
func renderOpenCodePlugin(binary string, optimized bool) (string, error) {
	envBlock := "env: process.env,"
	if optimized {
		// OpenCode's tool.execute.before hook returns Promise<void>; its return value is never
		// captured by Plugin.trigger. The only way to affect tool execution is to throw (deny)
		// or mutate output.args. There is no advisory channel for "allow" decisions — warn mode
		// emits allow+reason JSON which the plugin silently discards, making warn identical to
		// allow (zero token savings). Default unchanged-reads to deny so deduplication actually
		// fires. Changed files are left on allow so the agent always sees fresh content.
		envBlock = `env: {
      ...process.env,
      READ_ONCE_CLIENT: "opencode",
      READ_ONCE_MODE: "deny",
      READ_ONCE_MODE_UNCHANGED: "deny",
      READ_ONCE_MODE_CHANGED: "allow",
      READ_ONCE_DIFF: "1",
      READ_ONCE_DIFF_MAX: "80",
      READ_ONCE_DIFF_SUMMARY_MAX_HUNKS: "16",
      READ_ONCE_HASH: "1",
      READ_ONCE_HASH_ALGO: "xxhash",
      READ_ONCE_MAX_BYTES: "524288",
    },`
	}
	tmpl, err := template.New("opencode_plugin").Parse(openCodePluginTmpl)
	if err != nil {
		return "", fmt.Errorf("parse plugin template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, pluginTemplateData{
		Binary:   strconv.Quote(binary),
		EnvBlock: envBlock,
	}); err != nil {
		return "", fmt.Errorf("execute plugin template: %w", err)
	}
	return buf.String(), nil
}

// renderOpenCodeTool renders the OpenCode JS tool from the embedded template.
func renderOpenCodeTool(binary string, optimized bool) (string, error) {
	envBlock := "env: process.env,"
	if optimized {
		envBlock = `env: {
      ...process.env,
      READ_ONCE_CLIENT: "opencode",
      READ_ONCE_MODE: "deny",
      READ_ONCE_MODE_UNCHANGED: "deny",
      READ_ONCE_MODE_CHANGED: "allow",
      READ_ONCE_DIFF: "1",
      READ_ONCE_DIFF_MAX: "80",
      READ_ONCE_DIFF_SUMMARY_MAX_HUNKS: "16",
      READ_ONCE_HASH: "1",
      READ_ONCE_HASH_ALGO: "xxhash",
      READ_ONCE_MAX_BYTES: "524288",
    },`
	}
	tmpl, err := template.New("opencode_tool").Parse(openCodeToolTmpl)
	if err != nil {
		return "", fmt.Errorf("parse tool template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, pluginTemplateData{
		Binary:   strconv.Quote(binary),
		EnvBlock: envBlock,
	}); err != nil {
		return "", fmt.Errorf("execute tool template: %w", err)
	}
	return buf.String(), nil
}

func isWithinDir(path, dir string) bool {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(dir) == "" {
		return false
	}
	cleanPath := filepath.Clean(path)
	cleanDir := filepath.Clean(dir)
	rel, err := filepath.Rel(cleanDir, cleanPath)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
