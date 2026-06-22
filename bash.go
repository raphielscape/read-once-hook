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
	if strings.ContainsAny(cmd, "&;<>`$()") || strings.Contains(cmd, "||") {
		return "", "unsafe_shell_construct"
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
	return splitBy(cmd, func(r rune) bool { return r == '|' }, true, true)
}

func extractReadPathFromSegment(segment, cwd string) (string, string) {
	tokens := splitCommand(segment)
	if len(tokens) < 2 {
		return "", "too_few_tokens"
	}
	verb := filepath.Base(tokens[0])
	readers := map[string]bool{
		"cat":  true,
		"sed":  true,
		"head": true,
		"tail": true,
		"less": true,
		"more": true,
		"bat":  true,
		"nl":   true,
		"grep": true,
		"rg":   true,
		"awk":  true,
		"git":  true,
	}
	if !readers[verb] {
		return "", "unsupported_reader:" + verb
	}

	candidates := make([]string, 0, len(tokens))
	if verb == "git" {
		if len(tokens) < 3 || tokens[1] != "show" {
			return "", "unsupported_git_subcommand"
		}
		for _, tok := range tokens[2:] {
			if strings.HasPrefix(tok, "-") {
				continue
			}
			candidates = append(candidates, tok)
			// Handle `git show REV:path/to/file` by also trying the suffix path.
			if i := strings.Index(tok, ":"); i > 0 && i < len(tok)-1 {
				candidates = append(candidates, tok[i+1:])
			}
		}
	} else {
		for _, tok := range tokens[1:] {
			if strings.HasPrefix(tok, "-") {
				continue
			}
			candidates = append(candidates, tok)
		}
	}
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
