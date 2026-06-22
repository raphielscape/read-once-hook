package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type eventEntry struct {
	Event       string `json:"event"`
	Path        string `json:"path"`
	Tokens      int64  `json:"tokens"`
	TokensSaved int64  `json:"tokens_saved"`
	Session     string `json:"session"`
	Ts          int64  `json:"ts"`
}

type cacheEntry struct {
	Path          string `json:"path"`
	Mtime         string `json:"mtime"`
	Ts            int64  `json:"ts"`
	Tokens        int64  `json:"tokens"`
	Hash          string `json:"hash,omitempty"`
	LastAttemptTs int64  `json:"last_attempt_ts,omitempty"`
	Attempts      int    `json:"attempts,omitempty"`
}

func readEvents(path string) ([]eventEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file, close error is meaningless

	var out []eventEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e eventEntry
		if err := json.Unmarshal([]byte(line), &e); err == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

func appendJSONLine(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	release := acquireFileLock(path+".lock", 2*time.Second)
	if release == nil {
		// Lock timed out under concurrent hook invocations (e.g. parallel Read tool calls).
		// Skip the write rather than proceed without the lock and risk interleaved JSONL lines.
		// Under-counted stats are preferable to a corrupted append-only log.
		return errors.New("lock timeout")
	}
	defer release()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // write error already checked above
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// readLastCacheEntry performs a full O(n) sequential scan of the session cache file to find
// the most recent entry for filePath. Each hook invocation is a fresh process, so there is
// no cheaper persistent alternative without a daemon. In practice this is bounded: the session
// cache is scoped to a single TTL window (default 20 min) and entries are append-only, so the
// file size grows at roughly one line per unique file read. For typical sessions (<500 unique
// reads) the scan completes in microseconds. Known limitation: very large sessions with
// thousands of unique reads will degrade linearly.
// scanJSONL calls fn for each valid JSONL line in cacheFile.
func scanJSONL(cacheFile string, fn func(cacheEntry)) {
	f, err := os.Open(cacheFile)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck // read-only file, close error is meaningless
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var c cacheEntry
		if json.Unmarshal([]byte(line), &c) == nil {
			fn(c)
		}
	}
}

// readLastCacheEntries returns the most recent cache entry per path.
func readLastCacheEntries(cacheFile string) (map[string]cacheEntry, error) {
	if _, err := os.Stat(cacheFile); err != nil {
		return nil, err
	}
	entries := make(map[string]cacheEntry)
	scanJSONL(cacheFile, func(c cacheEntry) {
		entries[c.Path] = c
	})
	return entries, nil
}

// readLastCacheEntry performs a full O(n) sequential scan of the session cache file to find
// the most recent entry for filePath. Each hook invocation is a fresh process, so there is
// no cheaper persistent alternative without a daemon. In practice this is bounded: the session
// cache is scoped to a single TTL window (default 20 min) and entries are append-only, so the
// file size grows at roughly one line per unique file read. For typical sessions (<500 unique
// reads) the scan completes in microseconds. Known limitation: very large sessions with
// thousands of unique reads will degrade linearly.
func readLastCacheEntry(cacheFile, filePath string) (cacheEntry, bool) {
	var last cacheEntry
	found := false
	scanJSONL(cacheFile, func(c cacheEntry) {
		if c.Path == filePath {
			last = c
			found = true
		}
	})
	return last, found
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

func acquireFileLock(lockPath string, timeout time.Duration) func() {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }
		}
		if !errors.Is(err, os.ErrExist) || time.Now().After(deadline) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runCleanup(cacheDir, snapDir string, now int64) {
	marker := filepath.Join(cacheDir, ".last-cleanup")
	last := int64(0)
	if b, err := os.ReadFile(marker); err == nil {
		n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		last = n
	}
	if now-last <= 3600 {
		return
	}
	removeOlderThan(cacheDir, "session-*.jsonl", 24*time.Hour)
	removeOlderThan(snapDir, "*", 24*time.Hour)
	// NOTE: stale *.lock files are NOT cleaned here. runCleanup is called from within a live
	// hook invocation that may itself be holding locks. An mtime-based age check cannot safely
	// distinguish a stale lock (from a SIGKILL'd process) from a slow-but-alive holder, so
	// auto-removal here risks corrupting JSONL under concurrent writes. Stale locks are
	// instead cleaned by clearSessions (user-invoked 'read-once clear'), which has no
	// concurrency risk.
	_ = os.WriteFile(marker, []byte(strconv.FormatInt(now, 10)+"\n"), 0o644)
}

func removeOlderThan(dir, pattern string, maxAge time.Duration) {
	if !fileExists(dir) {
		return
	}
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, path := range matches {
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			continue
		}
		if st.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}
