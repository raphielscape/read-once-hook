package main

import (
	"os"
	"path/filepath"
	"strings"
)

func extractBashReadPath(toolInput map[string]any, cwd string) (string, string) {
	cmd := strings.TrimSpace(asString(toolInput["command"]))
	if cmd == "" {
		return "", "empty_command"
	}
	// Unwrap shell wrappers like bash -lc 'cmd' or zsh -lc 'cmd'.
	cmd = unwrapShellWrapper(cmd)
	// Handle cd dir && cmd chaining — resolve cwd override.
	cmd, cdCwd := extractCdPrefix(cmd)
	if cdCwd != "" {
		switch {
		case filepath.IsAbs(cdCwd):
			cwd = cdCwd
		case strings.TrimSpace(cwd) != "":
			cwd = filepath.Join(cwd, cdCwd)
		default:
			base, _ := os.Getwd()
			cwd = filepath.Join(base, cdCwd)
		}
	}
	segments, ok := splitPipelineCommand(cmd)
	if !ok || len(segments) == 0 {
		return "", "pipeline_parse_failed"
	}
	lastReason := "no_trackable_file_arg"
	for _, seg := range segments {
		path, reason := extractReadPathFromSegment(seg, cwd)
		if path != "" {
			return path, ""
		}
		if reason != "" {
			lastReason = reason
		}
	}
	return "", lastReason
}

func splitPipelineCommand(cmd string) ([]string, bool) {
	return splitBy(cmd, func(r rune) bool { return r == '|' }, true, true, true)
}

func extractReadPathFromSegment(segment, cwd string) (string, string) {
	tokens := splitCommand(segment)
	if len(tokens) < 2 {
		return "", "too_few_tokens"
	}
	verb := filepath.Base(tokens[0])
	if verb == "git" {
		return extractGitFileArg(tokens, cwd)
	}
	if !readers[verb] {
		return "", "unsupported_reader:" + verb
	}
	candidates := collectArgs(tokens[1:])
	return resolveBestCandidate(candidates, cwd)
}

var readers = map[string]bool{
	"cat": true, "sed": true, "head": true, "tail": true,
	"less": true, "more": true, "bat": true, "batcat": true,
	"ccat": true, "nl": true,
	"grep": true, "rg": true, "awk": true,
	"wc": true, "diff": true, "sort": true, "cut": true,
	"tee": true, "file": true, "stat": true,
	"md5sum": true, "sha1sum": true, "sha256sum": true,
	"base64": true, "xxd": true, "strings": true,
	"column": true, "ls": true,
	"eza": true, "exa": true, "tree": true, "du": true,
}

func extractGitFileArg(tokens []string, cwd string) (string, string) {
	if len(tokens) < 3 {
		return "", "too_few_tokens"
	}
	subcmd := tokens[1]
	if !gitFileSubs[subcmd] {
		return "", "unsupported_git_subcommand:" + subcmd
	}
	candidates := collectArgs(tokens[2:])
	for _, tok := range candidates {
		if i := strings.Index(tok, ":"); i > 0 && i < len(tok)-1 {
			candidates = append(candidates, tok[i+1:])
		}
	}
	return resolveBestCandidate(candidates, cwd)
}

var gitFileSubs = map[string]bool{
	"show": true, "diff": true, "log": true, "blame": true,
	"cat-file": true, "ls-tree": true, "grep": true,
}

// collectArgs collects non-flag tokens from args, skipping output redirect targets.
func collectArgs(args []string) []string {
	var out []string
	skipNext := false
	for _, tok := range args {
		if skipNext {
			skipNext = false
			continue
		}
		switch {
		case tok == ">" || tok == ">>" || tok == "|>":
			skipNext = true // skip output redirect target
		case strings.HasPrefix(tok, ">"):
			// inline output redirect like >file — skip
		case tok == "<":
			// input redirect — next token is the file, don't skip it
		case strings.HasPrefix(tok, "-"):
			// flag — skip
		default:
			out = append(out, tok)
		}
	}
	return out
}

func resolveBestCandidate(candidates []string, cwd string) (string, string) {
	for i := len(candidates) - 1; i >= 0; i-- {
		if p, ok := resolveFileToken(candidates[i], cwd); ok {
			return p, ""
		}
	}
	return "", "no_existing_file_in_command"
}

func resolveFileToken(token, cwd string) (string, bool) {
	if strings.TrimSpace(token) == "" {
		return "", false
	}
	pathToken := expandHome(token)
	if !filepath.IsAbs(pathToken) {
		base := cwd
		if strings.TrimSpace(base) == "" {
			base, _ = os.Getwd()
		}
		pathToken = filepath.Join(base, pathToken)
	}
	pathToken = filepath.Clean(pathToken)
	st, err := os.Stat(pathToken)
	if err != nil || st.IsDir() {
		return "", false
	}
	return pathToken, true
}

// unwrapShellWrapper strips bash -lc '...' or zsh -lc '...' wrappers,
// returning the inner command. If the input is not a recognized wrapper,
// it is returned unchanged.
func unwrapShellWrapper(cmd string) string {
	tokens, ok := shellSplit(cmd)
	if !ok {
		return cmd
	}
	if len(tokens) < 3 {
		return cmd
	}
	if tokens[0] != "bash" && tokens[0] != "zsh" {
		return cmd
	}
	for i, tok := range tokens[1:] {
		if tok == "-c" || tok == "-lc" {
			if i+2 < len(tokens) {
				return tokens[i+2]
			}
			return cmd
		}
	}
	return cmd
}

// extractCdPrefix handles "cd dir && cmd" or "cd dir ; cmd" patterns.
// Returns the remaining command after stripping the cd prefix, and the resolved
// cwd from the cd target. If no cd prefix is found, returns the original cmd and "".
func extractCdPrefix(cmd string) (string, string) {
	// Split by " && " or " ; " to find cd prefix.
	for _, sep := range []string{" && ", " ; "} {
		if i := strings.Index(cmd, sep); i > 0 {
			left := strings.TrimSpace(cmd[:i])
			right := strings.TrimSpace(cmd[i+len(sep):])
			// Check if left side is "cd dir" or "cd dir1 dir2".
			if dir, ok := parseCdCommand(left); ok {
				return right, dir
			}
		}
	}
	return cmd, ""
}

// parseCdCommand checks if a command is a "cd dir" command and returns the target dir.
func parseCdCommand(cmd string) (string, bool) {
	tokens := splitCommand(cmd)
	if len(tokens) < 2 {
		return "", false
	}
	if tokens[0] != "cd" {
		return "", false
	}
	// Skip flags and --, find the last operand.
	// After --, everything is an operand (even if it starts with -).
	var dir string
	operands := 0
	afterDoubleDash := false
	for _, tok := range tokens[1:] {
		if tok == "--" {
			afterDoubleDash = true
			continue
		}
		if !afterDoubleDash && strings.HasPrefix(tok, "-") {
			continue // skip flags
		}
		dir = tok
		operands++
	}
	if dir == "" || operands != 1 {
		return "", false
	}
	return dir, true
}
