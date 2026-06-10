package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestWriteAndAppend(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return fixed }
	l.Write(Entry{Kind: "tool", Tool: "navigate", Profile: "work", Domain: "example.com",
		Detail: map[string]any{"url": "https://example.com"}})
	l.Write(Entry{Kind: "approval", Action: "interact", Domain: "example.com", Decision: "this session"})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: must append, not truncate.
	l2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l2.Write(Entry{Kind: "kill"})
	l2.Close()

	f, err := os.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var entries []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line not valid JSON: %s", sc.Text())
		}
		entries = append(entries, e)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	if entries[0].Tool != "navigate" || !entries[0].Time.Equal(fixed) {
		t.Fatalf("bad first entry: %+v", entries[0])
	}
	if entries[1].Decision != "this session" || entries[2].Kind != "kill" {
		t.Fatalf("bad entries: %+v", entries)
	}
}

func TestNilLogIsSafe(t *testing.T) {
	var l *Log
	l.Write(Entry{Kind: "tool"}) // must not panic
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Write(Entry{Kind: "tool", Tool: "click"})
		}()
	}
	wg.Wait()
	l.Close()
	data, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	sc := bufio.NewScanner(bytes.NewReader(data))
	n := 0
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("interleaved/corrupt line: %q", sc.Text())
		}
		n++
	}
	if n != 50 {
		t.Fatalf("want 50 lines, got %d", n)
	}
}

func TestFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix perms")
	}
	dir := t.TempDir()
	l, _ := Open(dir)
	l.Write(Entry{Kind: "tool"})
	l.Close()
	info, err := os.Stat(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("audit log perms = %v, want 0600", info.Mode().Perm())
	}
}
