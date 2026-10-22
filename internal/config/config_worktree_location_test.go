package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exampleWithoutWorktreeLocation is conveyor.example.yaml with its
// worktree_location block removed, so a test controls the location.
func exampleWithoutWorktreeLocation(t *testing.T) []byte {
	t.Helper()
	example, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	skipping := false
	for _, line := range strings.Split(string(example), "\n") {
		if strings.HasPrefix(line, "worktree_location:") {
			skipping = true
			continue
		}
		if skipping && (strings.HasPrefix(line, " ") || line == "") {
			continue
		}
		skipping = false
		kept = append(kept, line)
	}
	if strings.Contains(strings.Join(kept, "\n"), "\nworktree_location:") {
		t.Fatal("example worktree_location block was not removed")
	}
	return []byte(strings.Join(kept, "\n"))
}

func loadWorktreeLocationDocument(t *testing.T, document string) (*Config, error) {
	t.Helper()
	example := exampleWithoutWorktreeLocation(t)
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := os.WriteFile(path, append(example, []byte("\n"+document)...), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestWorktreeLocationLoadsUnconfiguredWithoutDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	loaded, err := loadWorktreeLocationDocument(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if location, ok := loaded.ConfiguredWorktreeLocation(); ok {
		t.Fatalf("location = %+v, want unconfigured", location)
	}
}

func TestWorktreeLocationNormalizesBothModes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, test := range []struct {
		name     string
		document string
		want     WorktreeLocation
	}{
		{"repository default dir", "worktree_location:\n  mode: repository\n", WorktreeLocation{Mode: WorktreeLocationRepository, Dir: "_worktrees"}},
		{"repository custom dir", "worktree_location:\n  mode: repository\n  dir: .trees\n", WorktreeLocation{Mode: WorktreeLocationRepository, Dir: ".trees"}},
		{"machine default root", "worktree_location:\n  mode: machine\n", WorktreeLocation{Mode: WorktreeLocationMachine, Root: filepath.Join(home, ".conveyor", "worktrees")}},
		{"machine home root", "worktree_location:\n  mode: machine\n  root: ~/trees/\n", WorktreeLocation{Mode: WorktreeLocationMachine, Root: filepath.Join(home, "trees")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			loaded, err := loadWorktreeLocationDocument(t, test.document)
			if err != nil {
				t.Fatal(err)
			}
			location, ok := loaded.ConfiguredWorktreeLocation()
			if !ok || location != test.want {
				t.Fatalf("location = %+v, %v; want %+v", location, ok, test.want)
			}
		})
	}
}

func TestWorktreeLocationRefusesInvalidDocuments(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, test := range []struct {
		name, document, want string
	}{
		{"missing mode", "worktree_location:\n  dir: _worktrees\n", "worktree_location.mode is required"},
		{"unknown mode", "worktree_location:\n  mode: home\n", `worktree_location.mode "home"`},
		{"nested dir", "worktree_location:\n  mode: repository\n  dir: a/b\n", "one relative path component"},
		{"parent dir", "worktree_location:\n  mode: repository\n  dir: ..\n", "one relative path component"},
		{"git dir", "worktree_location:\n  mode: repository\n  dir: .git\n", "one relative path component"},
		{"root in repository mode", "worktree_location:\n  mode: repository\n  root: /srv/trees\n", "root applies only to mode machine"},
		{"dir in machine mode", "worktree_location:\n  mode: machine\n  dir: _worktrees\n", "dir applies only to mode repository"},
		{"relative root", "worktree_location:\n  mode: machine\n  root: trees\n", "root must be absolute"},
		{"both keys", "worktree_root: /srv/trees\nworktree_location:\n  mode: machine\n", "not both"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := loadWorktreeLocationDocument(t, test.document); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWorktreeLocationValueSyntaxRoundTrips(t *testing.T) {
	for _, value := range []string{"repository", "repository:.trees", "machine", "machine:/srv/trees"} {
		location, err := ParseWorktreeLocation(value)
		if err != nil {
			t.Fatal(err)
		}
		if location.String() != value {
			t.Fatalf("%q round-tripped to %q", value, location.String())
		}
	}
	if _, err := ParseWorktreeLocation("home"); err == nil || !strings.Contains(err.Error(), "repository, repository:<dir>, machine, or machine:<root>") {
		t.Fatalf("unknown value error = %v", err)
	}
}

func TestWorktreeLocationDirMustBeALiteralGitPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, dir := range []string{"#trees", "!trees", "[trees]", "tr*es", "tr?es", "tr ees", "-trees", "tr\\\\ees", ":trees"} {
		document := "worktree_location:\n  mode: repository\n  dir: \"" + dir + "\"\n"
		if _, err := loadWorktreeLocationDocument(t, document); err == nil || !strings.Contains(err.Error(), "ASCII letters, digits") {
			t.Fatalf("dir %q error = %v", dir, err)
		}
	}
	if _, err := loadWorktreeLocationDocument(t, "worktree_location:\n  mode: repository\n  dir: \"tr\\nees\"\n"); err == nil {
		t.Fatal("dir with a newline loaded")
	}
	for _, dir := range []string{"_worktrees", ".trees", "task-trees", "Trees2"} {
		if _, err := loadWorktreeLocationDocument(t, "worktree_location:\n  mode: repository\n  dir: "+dir+"\n"); err != nil {
			t.Fatalf("dir %q refused: %v", dir, err)
		}
	}
}
