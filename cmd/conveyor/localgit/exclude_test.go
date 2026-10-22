package localgit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func excludeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@example.test"}, {"config", "user.name", "t"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func readExclude(t *testing.T, primary string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(primary, ".git", "info", "exclude"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func TestEnsureExcludedAppendsOnceAndNeverDuplicates(t *testing.T) {
	primary := excludeFixture(t)
	ctx := context.Background()
	appended, err := EnsureExcluded(ctx, primary, "_worktrees")
	if err != nil || !appended {
		t.Fatalf("first EnsureExcluded = %v, %v", appended, err)
	}
	appended, err = EnsureExcluded(ctx, primary, "_worktrees")
	if err != nil || appended {
		t.Fatalf("second EnsureExcluded = %v, %v; want no append", appended, err)
	}
	if count := strings.Count(readExclude(t, primary), "_worktrees/\n"); count != 1 {
		t.Fatalf("exclude entry count = %d in %q", count, readExclude(t, primary))
	}
	if err := os.MkdirAll(filepath.Join(primary, "_worktrees", "repo-task-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primary, "_worktrees", "repo-task-1", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if only, err := UntrackedOnlyUnder(ctx, primary, "_worktrees"); err != nil || only {
		t.Fatalf("excluded container still reads as untracked: %v, %v", only, err)
	}
}

func TestEnsureExcludedLeavesAnAlreadyIgnoredDirectoryAlone(t *testing.T) {
	primary := excludeFixture(t)
	if err := os.WriteFile(filepath.Join(primary, ".gitignore"), []byte("_worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appended, err := EnsureExcluded(context.Background(), primary, "_worktrees")
	if err != nil || appended {
		t.Fatalf("EnsureExcluded = %v, %v; want no append", appended, err)
	}
	if strings.Contains(readExclude(t, primary), "_worktrees") {
		t.Fatalf("exclude edited although .gitignore already ignores the dir: %q", readExclude(t, primary))
	}
	if data, _ := os.ReadFile(filepath.Join(primary, ".gitignore")); string(data) != "_worktrees/\n" {
		t.Fatalf(".gitignore changed: %q", data)
	}
}

func TestEnsureExcludedRefusesATrackedDirectory(t *testing.T) {
	primary := excludeFixture(t)
	if err := os.MkdirAll(filepath.Join(primary, "_worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primary, "_worktrees", "tracked.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "_worktrees/tracked.txt")
	cmd.Dir = primary
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	_, err := EnsureExcluded(context.Background(), primary, "_worktrees")
	if err == nil || !strings.Contains(err.Error(), "holds tracked files") {
		t.Fatalf("EnsureExcluded error = %v", err)
	}
	if strings.Contains(readExclude(t, primary), "_worktrees") {
		t.Fatalf("exclude edited for a tracked dir: %q", readExclude(t, primary))
	}
}

func TestUntrackedOnlyUnderNamesOnlyAnUnignoredContainer(t *testing.T) {
	primary := excludeFixture(t)
	ctx := context.Background()
	if only, err := UntrackedOnlyUnder(ctx, primary, "_worktrees"); err != nil || only {
		t.Fatalf("clean checkout = %v, %v", only, err)
	}
	if err := os.MkdirAll(filepath.Join(primary, "_worktrees", "repo-task-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primary, "_worktrees", "repo-task-1", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if only, err := UntrackedOnlyUnder(ctx, primary, "_worktrees"); err != nil || !only {
		t.Fatalf("container-only status = %v, %v", only, err)
	}
	if err := os.WriteFile(filepath.Join(primary, "other.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if only, err := UntrackedOnlyUnder(ctx, primary, "_worktrees"); err != nil || only {
		t.Fatalf("mixed status = %v, %v", only, err)
	}
}

func TestEnsureExcludedRefusesNonLiteralDirectories(t *testing.T) {
	primary := excludeFixture(t)
	for _, dir := range []string{"#trees", "!trees", "[trees]", "tr*es", "tr?es", ":(glob)trees", "tr\nees", "../trees", "-trees"} {
		if _, err := EnsureExcluded(context.Background(), primary, dir); err == nil || !strings.Contains(err.Error(), "literal path") {
			t.Fatalf("dir %q error = %v", dir, err)
		}
	}
	if exclude := readExclude(t, primary); strings.Contains(exclude, "trees") {
		t.Fatalf("exclude edited for a non-literal dir: %q", exclude)
	}
	if !LiteralGitPath("_worktrees") || !LiteralGitPath("nested/.trees") {
		t.Fatal("literal paths refused")
	}
}

func TestTrackedUnderReadsTheDirectoryLiterally(t *testing.T) {
	primary := excludeFixture(t)
	if err := os.MkdirAll(filepath.Join(primary, "tracked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primary, "tracked", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "tracked/file")
	cmd.Dir = primary
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	// As a glob pathspec, "track*" would match tracked/file.
	if tracked, err := TrackedUnder(context.Background(), primary, "track*"); err != nil || tracked {
		t.Fatalf("TrackedUnder(track*) = %v, %v", tracked, err)
	}
	if tracked, err := TrackedUnder(context.Background(), primary, "tracked"); err != nil || !tracked {
		t.Fatalf("TrackedUnder(tracked) = %v, %v", tracked, err)
	}
}
