package localgit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnsureExcluded prepares an in-repository worktree container before the
// primary-cleanliness check, because nested worktrees read as untracked files
// in the primary checkout unless their directory is ignored. It refuses a dir
// that holds tracked files and appends one "<dir>/" line to the repository's
// info/exclude only when Git does not already ignore the dir; it never edits a
// tracked .gitignore (component-git-delivery). It reports whether it appended.
func EnsureExcluded(ctx context.Context, primary, dir string) (bool, error) {
	if !LiteralGitPath(dir) {
		return false, fmt.Errorf("in-repository worktree directory %q must use only ASCII letters, digits, '.', '_', and '-' in each path component, so Git reads it as a literal path", dir)
	}
	tracked, err := TrackedUnder(ctx, primary, dir)
	if err != nil {
		return false, err
	}
	if tracked {
		return false, fmt.Errorf("in-repository worktree directory %s holds tracked files; choose another worktree_location dir", dir)
	}
	ignored, err := Ignored(ctx, primary, dir)
	if err != nil || ignored {
		return false, err
	}
	excludePath, err := commandOutput(ctx, primary, "git", "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	if err != nil {
		return false, err
	}
	excludePath = strings.TrimSpace(excludePath)
	if err = os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(excludePath), err)
	}
	existing, err := os.ReadFile(excludePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", excludePath, err)
	}
	line := dir + "/\n"
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		line = "\n" + line
	}
	file, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", excludePath, err)
	}
	_, err = file.WriteString(line)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, fmt.Errorf("append %s: %w", excludePath, err)
	}
	return true, nil
}

// LiteralGitPath reports whether every component of a slash-separated relative
// path uses only ASCII letters, digits, '.', '_', and '-' and none starts with
// '-' or is '.' or '..'. Such a path written to info/exclude is never a
// comment, negation, or glob, and is never pathspec magic (component-git-delivery).
func LiteralGitPath(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") {
		return false
	}
	for _, component := range strings.Split(path, "/") {
		if component == "" || component == "." || component == ".." || strings.HasPrefix(component, "-") {
			return false
		}
		for _, character := range component {
			if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-') {
				return false
			}
		}
	}
	return true
}

// TrackedUnder reports whether the index tracks any path under dir, reading
// dir as a literal pathspec.
func TrackedUnder(ctx context.Context, primary, dir string) (bool, error) {
	out, err := commandOutput(ctx, primary, "git", "--literal-pathspecs", "ls-files", "-z", "--", dir)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// Ignored reports whether Git's ignore rules, including info/exclude, ignore
// dir as a directory.
func Ignored(ctx context.Context, primary, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "check-ignore", "-q", "--", dir+"/")
	cmd.Dir = primary
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git check-ignore %s/: %w: %s", dir, err, out)
}

// UntrackedOnlyUnder reports whether the checkout's status is non-empty and
// every entry is an untracked path under dir, which means only an unignored
// in-repository worktree container makes the checkout look dirty.
func UntrackedOnlyUnder(ctx context.Context, checkout, dir string) (bool, error) {
	out, err := commandOutput(ctx, checkout, "git", "status", "--porcelain", "--untracked-files=normal", "-z")
	if err != nil {
		return false, err
	}
	entries := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	if len(entries) == 0 || entries[0] == "" {
		return false, nil
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry, "?? ") || !strings.HasPrefix(strings.TrimPrefix(entry, "?? "), dir+"/") {
			return false, nil
		}
	}
	return true, nil
}
