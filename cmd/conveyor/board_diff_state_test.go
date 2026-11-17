package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestBoardDiffSnapshot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	want := boardDiffState{Version: 1, Server: "https://example.com:123", Workspace: "default", Position: now, SavedAt: now, Recent: map[string]time.Time{"a": now}, Tasks: map[string]boardDiffTask{"a": {Title: "A", State: "running", Stage: "implement", EnteredAt: &now, PullRequest: 42, Decision: "Approve"}}}
	if err := saveBoardDiffState(want.Server, "", want); err != nil {
		t.Fatal(err)
	}
	path, err := boardDiffStatePath(want.Server, "")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(filepath.Dir(path))) != "example.com_123" {
		t.Fatalf("path = %s", path)
	}
	for p, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("mode of %s: %v %v", p, info, err)
		}
	}
	got, reason, err := loadBoardDiffState(want.Server, "", now)
	if err != nil || reason != "" || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v reason %q err %v", got, reason, err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := boardDiffStateWriter.rename
	boardDiffStateWriter.rename = func(string, string) error { return errors.New("rename failed") }
	defer func() { boardDiffStateWriter.rename = old }()
	want.Tasks["b"] = boardDiffTask{Title: "B"}
	if err := saveBoardDiffState(want.Server, "", want); err == nil {
		t.Fatal("expected failed replace")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatalf("old state changed: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary file left: %v %v", entries, err)
	}
}

func TestBoardDiffSnapshotUnusable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	server := "https://example.com"
	path, _ := boardDiffStatePath(server, "ws")
	if _, reason, err := loadBoardDiffState(server, "ws", now); err != nil || reason == "" {
		t.Fatalf("missing: %q %v", reason, err)
	}
	for _, tc := range []struct{ name, data string }{
		{"corrupt", "{"}, {"version", `{"version":2}`},
		{"server", `{"version":1,"server":"https://other","workspace":"ws"}`},
		{"workspace", `{"version":1,"server":"https://example.com","workspace":"other"}`},
		{"position", `{"version":1,"server":"https://example.com","workspace":"ws"}`},
		{"old", `{"version":1,"server":"https://example.com","workspace":"ws","saved_at":"2025-12-01T00:00:00Z","position":"2025-12-01T00:00:00Z"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, reason, err := loadBoardDiffState(server, "ws", now); err != nil || reason == "" {
				t.Fatalf("reason %q err %v", reason, err)
			}
		})
	}
}
