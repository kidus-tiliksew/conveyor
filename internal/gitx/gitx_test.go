package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanningSnapshotPlumbingStaysPinnedAndReadOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	mustRun(t, "", "git", "init", "-b", "main", origin)
	mustRun(t, origin, "git", "config", "user.email", "test@example.com")
	mustRun(t, origin, "git", "config", "user.name", "test")
	if err := os.MkdirAll(filepath.Join(origin, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "internal", "eligibility.go"), []byte("package internal\n\nfunc eligible() bool { return true }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "initial eligibility")

	manager := NewManager(filepath.Join(tmp, "cache"), "")
	snapshot, err := manager.PinSnapshot(ctx, "file://"+origin, "main")
	if err != nil {
		t.Fatal(err)
	}
	entries, truncated, err := manager.ListSnapshotTree(ctx, snapshot, "", defaultSnapshotOutputBytes)
	if err != nil || truncated || len(entries) != 1 || entries[0].Path != "internal/eligibility.go" {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	content, err := manager.ReadSnapshotBlob(ctx, snapshot, "internal/eligibility.go", defaultSnapshotOutputBytes)
	if err != nil || !strings.Contains(string(content), "return true") {
		t.Fatalf("content=%q err=%v", content, err)
	}
	matches, _, err := manager.GrepSnapshot(ctx, snapshot, "eligible", "internal", 0, false, false, 200, defaultSnapshotOutputBytes)
	if err != nil || !strings.Contains(matches, "eligibility.go:3:") {
		t.Fatalf("matches=%q err=%v", matches, err)
	}
	history, err := manager.SnapshotHistory(ctx, snapshot, "internal/eligibility.go", 20, defaultSnapshotOutputBytes)
	if err != nil || !strings.Contains(history, "initial eligibility") || !strings.Contains(history, "Latest commit context") {
		t.Fatalf("history=%q err=%v", history, err)
	}

	if err := os.WriteFile(filepath.Join(origin, "internal", "eligibility.go"), []byte("package internal\n\nfunc eligible() bool { return false }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "advance main")
	reopened, err := manager.OpenSnapshot(ctx, "file://"+origin, snapshot.Revision)
	if err != nil {
		t.Fatal(err)
	}
	content, err = manager.ReadSnapshotBlob(ctx, reopened, "internal/eligibility.go", defaultSnapshotOutputBytes)
	if err != nil || !strings.Contains(string(content), "return true") || strings.Contains(string(content), "return false") {
		t.Fatalf("pinned content changed: %q err=%v", content, err)
	}
}

func TestPlanningSnapshotPlumbingBoundsLargeSearchAndRejectsOversizedBlob(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	mustRun(t, "", "git", "init", "-b", "main", origin)
	mustRun(t, origin, "git", "config", "user.email", "test@example.com")
	mustRun(t, origin, "git", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(origin, "large.txt"),
		[]byte(strings.Repeat("match bounded exploration output\n", 20_000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "large.bin"), []byte(strings.Repeat("\x00\xff", 32_768)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "single-line.txt"), []byte(strings.Repeat("x", maxPlanningTextLineBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "paginated-tail.txt"),
		[]byte("one\ntwo\nthree\n"+strings.Repeat("x", maxPlanningTextLineBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "large planning fixtures")
	manager := NewManager(filepath.Join(tmp, "cache"), "")
	snapshot, err := manager.PinSnapshot(ctx, "file://"+origin, "main")
	if err != nil {
		t.Fatal(err)
	}
	const outputLimit = 512
	matches, truncated, err := manager.GrepSnapshot(ctx, snapshot, ".", "large.txt", 0, false, false, 50, outputLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) > outputLimit || !truncated || !strings.Contains(matches, "truncated at git boundary") {
		t.Fatalf("bounded grep returned %d bytes:\n%s", len(matches), matches)
	}
	if _, err = manager.ReadSnapshotBlob(ctx, snapshot, "large.bin", outputLimit); err == nil ||
		!strings.Contains(err.Error(), "read limit") {
		t.Fatalf("oversized binary read error=%v", err)
	}
	if _, err = manager.ReadSnapshotTextBlob(ctx, snapshot, "large.bin", 128<<10); err == nil ||
		!strings.Contains(err.Error(), "supports text blobs only") {
		t.Fatalf("bounded binary-prefix read error=%v", err)
	}
	if _, _, _, err = manager.ReadSnapshotTextLines(ctx, snapshot, "single-line.txt", 1, 10); err == nil ||
		!strings.Contains(err.Error(), "line exceeding") {
		t.Fatalf("pathological text line error=%v", err)
	}
	page, scanned, complete, err := manager.ReadSnapshotTextLines(ctx, snapshot, "paginated-tail.txt", 1, 2)
	if err != nil || strings.Join(page, ",") != "one,two" || scanned != 3 || complete {
		t.Fatalf("bounded page=%v scanned=%d complete=%t err=%v", page, scanned, complete, err)
	}
	if _, _, err = manager.GrepSnapshot(ctx, snapshot, "[", "large.txt", 0, false, false, 50, outputLimit); err == nil {
		t.Fatal("invalid git grep pattern unexpectedly succeeded")
	}
}

func mustRun(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

func TestPlanningMirrorFetchPreservesLocalRefs(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	mustRun(t, root, "git", "init", "-b", "main", origin)
	mustRun(t, origin, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "base")
	manager := NewManager(filepath.Join(root, "cache"), "")
	mirror, err := manager.EnsureMirror(ctx, "file://"+origin)
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, mirror, "git", "update-ref", "refs/heads/preserved", "refs/remotes/origin/main")
	mustRun(t, origin, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "advance")
	if _, err = manager.EnsureMirror(ctx, "file://"+origin); err != nil {
		t.Fatal(err)
	}
	if !refExists(ctx, mirror, "refs/heads/preserved") {
		t.Fatal("planning fetch pruned a local ref")
	}
	old, _ := revParse(ctx, mirror, "refs/heads/preserved")
	current, _ := revParse(ctx, mirror, "refs/remotes/origin/main")
	if old == current {
		t.Fatal("planning mirror did not fetch updated origin")
	}
}
