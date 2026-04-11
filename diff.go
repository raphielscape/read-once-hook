package main

import (
	"bufio"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

func unifiedDiff(oldPath, newPath string) (string, int) {
	cmd := exec.Command("diff", "-u", oldPath, newPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return "", 0
		}
	}
	diff := string(out)
	if strings.TrimSpace(diff) == "" {
		return "", 0
	}
	lines := 0
	sc := bufio.NewScanner(strings.NewReader(diff))
	for sc.Scan() {
		lines++
	}
	return diff, lines
}

func summarizeDiff(oldPath, newPath string, maxHunks int) string {
	cmd := exec.Command("diff", "-U0", oldPath, newPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return ""
		}
	}
	diff := string(out)
	if strings.TrimSpace(diff) == "" {
		return ""
	}
	lines := strings.Split(diff, "\n")
	hunks := make([]string, 0, maxHunks)
	added := 0
	removed := 0
	for _, ln := range lines {
		if strings.HasPrefix(ln, "@@") && len(hunks) < maxHunks {
			hunks = append(hunks, ln)
		}
		if strings.HasPrefix(ln, "+") && !strings.HasPrefix(ln, "+++") {
			added++
		}
		if strings.HasPrefix(ln, "-") && !strings.HasPrefix(ln, "---") {
			removed++
		}
	}
	if len(hunks) == 0 && added == 0 && removed == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Summary: %d hunks, +%d / -%d lines.\n", countDiffHunks(lines), added, removed)
	if len(hunks) > 0 {
		b.WriteString("Top hunks:\n")
		for _, h := range hunks {
			b.WriteString("  - ")
			b.WriteString(h)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSpace(b.String())
}

func countDiffHunks(lines []string) int {
	c := 0
	for _, ln := range lines {
		if strings.HasPrefix(ln, "@@") {
			c++
		}
	}
	return c
}
