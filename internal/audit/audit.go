// Package audit appends a JSONL trail of everything browserd does on the
// user's behalf: tool calls, approval decisions, grants, connections.
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Entry struct {
	Time     time.Time      `json:"ts"`
	Kind     string         `json:"kind"` // tool | approval | grant | connect | kill | error
	Tool     string         `json:"tool,omitempty"`
	Profile  string         `json:"profile,omitempty"`
	Domain   string         `json:"domain,omitempty"`
	Action   string         `json:"action,omitempty"` // permission action group
	Decision string         `json:"decision,omitempty"`
	Detail   map[string]any `json:"detail,omitempty"`
}

type Log struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
	// now is overridable in tests.
	now func() time.Time
}

// Open creates/appends the audit log at dir/audit.jsonl.
func Open(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "audit.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, enc: json.NewEncoder(f), now: time.Now}, nil
}

func (l *Log) Write(e Entry) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Time = l.now().UTC()
	// Best-effort: an audit write failure must not break the tool call, but
	// it is worth a stderr line — handled by the caller checking Err.
	_ = l.enc.Encode(e)
}

func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
