package main

import (
	"os"
	"path/filepath"
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
