package jobartifact

import (
	"path/filepath"
	"testing"
)

func TestPathsAreAttemptScopedAndRejectTraversal(t *testing.T) {
	control := t.TempDir()
	path, err := EventLogPath(control, "task-1-implement-2")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "events-task-1-implement-2.jsonl" {
		t.Fatalf("path = %q", path)
	}
	for _, invalid := range []string{"", "../job", "a/b"} {
		if _, err := EventLogPath(control, invalid); err == nil {
			t.Fatalf("accepted invalid job ID %q", invalid)
		}
	}
}
