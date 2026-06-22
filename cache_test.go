package main

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestAppendAndReadJSONLine(t *testing.T) {
	tmp := t.TempDir()
	cacheFile := filepath.Join(tmp, "test.jsonl")

	entry1 := cacheEntry{
		Path:   "/a.txt",
		Mtime:  "123",
		Ts:     1000,
		Tokens: 10,
	}

	err := appendJSONLine(cacheFile, entry1)
	if err != nil {
		t.Fatalf("appendJSONLine failed: %v", err)
	}

	last, found := readLastCacheEntry(cacheFile, "/a.txt")
	if !found {
		t.Fatalf("expected to find entry")
	}
	if last.Path != entry1.Path || last.Mtime != entry1.Mtime || last.Ts != entry1.Ts {
		t.Errorf("read mismatch: got %+v, want %+v", last, entry1)
	}

	entry2 := cacheEntry{
		Path:   "/a.txt",
		Mtime:  "456",
		Ts:     2000,
		Tokens: 20,
	}
	_ = appendJSONLine(cacheFile, entry2)

	last, found = readLastCacheEntry(cacheFile, "/a.txt")
	if !found {
		t.Fatalf("expected to find entry")
	}
	if last.Ts != entry2.Ts {
		t.Errorf("expected last entry to be entry2, got %v", last)
	}
}

func TestReadLastCacheEntries(t *testing.T) {
	tmp := t.TempDir()
	cacheFile := filepath.Join(tmp, "test2.jsonl")

	_ = appendJSONLine(cacheFile, cacheEntry{Path: "/a.txt", Ts: 10})
	_ = appendJSONLine(cacheFile, cacheEntry{Path: "/b.txt", Ts: 20})
	_ = appendJSONLine(cacheFile, cacheEntry{Path: "/a.txt", Ts: 30}) // should overwrite /a.txt

	entries, err := readLastCacheEntries(cacheFile)
	if err != nil {
		t.Fatalf("readLastCacheEntries failed: %v", err)
	}

	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(entries))
	}
	if entries["/a.txt"].Ts != 30 {
		t.Errorf("expected /a.txt to have Ts 30, got %d", entries["/a.txt"].Ts)
	}
	if entries["/b.txt"].Ts != 20 {
		t.Errorf("expected /b.txt to have Ts 20, got %d", entries["/b.txt"].Ts)
	}
}

func TestReadLastCacheEntrySkipsEmptyMtime(t *testing.T) {
	tmp := t.TempDir()
	cacheFile := filepath.Join(tmp, "test.jsonl")

	// Entry with empty Mtime (incomplete write).
	_ = appendJSONLine(cacheFile, cacheEntry{Path: "/a.txt", Mtime: "", Ts: 100})

	_, found := readLastCacheEntry(cacheFile, "/a.txt")
	if found {
		t.Fatal("expected cache miss for entry with empty Mtime")
	}
}

func TestReadLastCacheEntrySkipsZeroTs(t *testing.T) {
	tmp := t.TempDir()
	cacheFile := filepath.Join(tmp, "test.jsonl")

	// Entry with Ts=0 (cleared entry).
	_ = appendJSONLine(cacheFile, cacheEntry{Path: "/a.txt", Mtime: "cleared", Ts: 0})

	_, found := readLastCacheEntry(cacheFile, "/a.txt")
	if found {
		t.Fatal("expected cache miss for entry with Ts=0")
	}
}

func TestReadLastCacheEntryMissOnEmptyFile(t *testing.T) {
	tmp := t.TempDir()
	cacheFile := filepath.Join(tmp, "empty.jsonl")

	_, found := readLastCacheEntry(cacheFile, "/a.txt")
	if found {
		t.Fatal("expected cache miss on nonexistent file")
	}
}

func TestReadEvents(t *testing.T) {
	tmp := t.TempDir()
	statsFile := filepath.Join(tmp, "stats.jsonl")

	ev1 := eventEntry{Event: "hit", Path: "/a", Ts: 1}
	_ = appendJSONLine(statsFile, ev1)

	events, err := readEvents(statsFile)
	if err != nil {
		t.Fatalf("readEvents failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Event != "hit" {
		t.Errorf("expected hit, got %s", events[0].Event)
	}
}

func TestAcquireFileLockTimeout(t *testing.T) {
	tmp := t.TempDir()
	lockFile := filepath.Join(tmp, "lock.lock")

	release1 := acquireFileLock(lockFile, time.Second)
	if release1 == nil {
		t.Fatalf("failed to acquire uncontended lock")
	}
	defer release1()

	// Should timeout
	release2 := acquireFileLock(lockFile, 50*time.Millisecond)
	if release2 != nil {
		t.Fatalf("expected timeout acquiring already held lock")
		release2()
	}
}

func TestAcquireFileLockStalePID(t *testing.T) {
	tmp := t.TempDir()
	lockFile := filepath.Join(tmp, "stale.lock")

	// Write a lock file with a PID that doesn't exist (PID 1 is init, always alive
	// on Linux, but we can use a high number that's almost certainly unused).
	_ = os.WriteFile(lockFile, []byte("999999999\n"), 0644)

	// Should detect stale lock and succeed.
	release := acquireFileLock(lockFile, 200*time.Millisecond)
	if release == nil {
		t.Fatal("expected to acquire lock after detecting stale PID")
	}
	release()

	// Verify lock file was cleaned up.
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Error("expected lock file to be removed after acquisition")
	}
}

func TestAcquireFileLockLivePID(t *testing.T) {
	tmp := t.TempDir()
	lockFile := filepath.Join(tmp, "live.lock")

	// Write a lock file with our own PID (we're alive).
	_ = os.WriteFile(lockFile, []byte(strconv.Itoa(syscall.Getpid())+"\n"), 0644)

	// Should timeout because our PID is alive.
	release := acquireFileLock(lockFile, 100*time.Millisecond)
	if release != nil {
		release()
		t.Fatal("expected timeout because lock holder (our PID) is alive")
	}
}

func TestRunCleanup(t *testing.T) {
	tmp := t.TempDir()
	cacheDir := filepath.Join(tmp, "cache")
	snapDir := filepath.Join(tmp, "snaps")
	_ = os.MkdirAll(cacheDir, 0755)
	_ = os.MkdirAll(snapDir, 0755)

	oldFile := filepath.Join(cacheDir, "session-old.jsonl")
	_ = os.WriteFile(oldFile, []byte(""), 0644)
	os.Chtimes(oldFile, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour))

	newFile := filepath.Join(cacheDir, "session-new.jsonl")
	_ = os.WriteFile(newFile, []byte(""), 0644)

	runCleanup(cacheDir, snapDir, time.Now().Unix())

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("expected oldFile to be deleted")
	}
	if _, err := os.Stat(newFile); os.IsNotExist(err) {
		t.Errorf("expected newFile to exist")
	}
}

func TestReadLastCacheEntryMultiplePaths(t *testing.T) {
	tmp := t.TempDir()
	cacheFile := filepath.Join(tmp, "multi.jsonl")

	_ = appendJSONLine(cacheFile, cacheEntry{Path: "/a.txt", Mtime: "1", Ts: 100})
	_ = appendJSONLine(cacheFile, cacheEntry{Path: "/b.txt", Mtime: "2", Ts: 200})
	_ = appendJSONLine(cacheFile, cacheEntry{Path: "/c.txt", Mtime: "3", Ts: 300})

	a, found := readLastCacheEntry(cacheFile, "/a.txt")
	if !found || a.Mtime != "1" {
		t.Errorf("expected /a.txt Mtime=1, got %+v found=%v", a, found)
	}
	b, found := readLastCacheEntry(cacheFile, "/b.txt")
	if !found || b.Mtime != "2" {
		t.Errorf("expected /b.txt Mtime=2, got %+v found=%v", b, found)
	}
	c, found := readLastCacheEntry(cacheFile, "/c.txt")
	if !found || c.Mtime != "3" {
		t.Errorf("expected /c.txt Mtime=3, got %+v found=%v", c, found)
	}
	_, found = readLastCacheEntry(cacheFile, "/d.txt")
	if found {
		t.Error("expected miss for /d.txt")
	}
}

func TestReadLastCacheEntryLastWins(t *testing.T) {
	tmp := t.TempDir()
	cacheFile := filepath.Join(tmp, "lastwins.jsonl")

	_ = appendJSONLine(cacheFile, cacheEntry{Path: "/a.txt", Mtime: "1", Ts: 100, Tokens: 10})
	_ = appendJSONLine(cacheFile, cacheEntry{Path: "/a.txt", Mtime: "2", Ts: 200, Tokens: 20})
	_ = appendJSONLine(cacheFile, cacheEntry{Path: "/a.txt", Mtime: "3", Ts: 300, Tokens: 30})

	last, found := readLastCacheEntry(cacheFile, "/a.txt")
	if !found {
		t.Fatal("expected to find entry")
	}
	if last.Ts != 300 || last.Tokens != 30 || last.Mtime != "3" {
		t.Errorf("expected last entry, got %+v", last)
	}
}

func TestReadLastCacheEntryPartialLine(t *testing.T) {
	tmp := t.TempDir()
	cacheFile := filepath.Join(tmp, "partial.jsonl")

	// Write a valid line, then a partial line, then another valid line.
	_ = appendJSONLine(cacheFile, cacheEntry{Path: "/a.txt", Mtime: "1", Ts: 100})
	f, _ := os.OpenFile(cacheFile, os.O_APPEND|os.O_WRONLY, 0644)
	f.WriteString("{\"path\":\"/b.txt\",\"mtime\":\"2\",\"ts\":\n") // partial
	f.Close()
	_ = appendJSONLine(cacheFile, cacheEntry{Path: "/c.txt", Mtime: "3", Ts: 300})

	// Should find a.txt and c.txt, skip partial b.txt.
	a, found := readLastCacheEntry(cacheFile, "/a.txt")
	if !found || a.Mtime != "1" {
		t.Errorf("expected /a.txt, got %+v found=%v", a, found)
	}
	_, found = readLastCacheEntry(cacheFile, "/b.txt")
	if found {
		t.Error("expected miss for partial /b.txt entry")
	}
	c, found := readLastCacheEntry(cacheFile, "/c.txt")
	if !found || c.Mtime != "3" {
		t.Errorf("expected /c.txt, got %+v found=%v", c, found)
	}
}

func TestAppendJSONLineCreatesFile(t *testing.T) {
	tmp := t.TempDir()
	cacheFile := filepath.Join(tmp, "new.jsonl")

	err := appendJSONLine(cacheFile, cacheEntry{Path: "/a.txt", Mtime: "1", Ts: 100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(cacheFile); os.IsNotExist(err) {
		t.Error("expected file to be created")
	}
}

func TestAcquireFileLockRelease(t *testing.T) {
	tmp := t.TempDir()
	lockFile := filepath.Join(tmp, "release.lock")

	release := acquireFileLock(lockFile, time.Second)
	if release == nil {
		t.Fatal("failed to acquire lock")
	}

	// Lock file should exist.
	if _, err := os.Stat(lockFile); os.IsNotExist(err) {
		t.Error("expected lock file to exist while held")
	}

	release()

	// Lock file should be removed.
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Error("expected lock file to be removed after release")
	}
}

func TestShortHash(t *testing.T) {
	h1 := shortHash("hello")
	h2 := shortHash("hello")
	h3 := shortHash("world")
	if h1 != h2 {
		t.Errorf("same input should produce same hash: %q != %q", h1, h2)
	}
	if h1 == h3 {
		t.Errorf("different inputs should produce different hashes: %q == %q", h1, h3)
	}
	if len(h1) != 16 {
		t.Errorf("expected 16 char hash, got %d", len(h1))
	}
}
