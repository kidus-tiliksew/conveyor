package gitx

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Snapshot is an immutable commit inside Conveyor's fetch-only bare cache.
// The directory is server-owned and never enters a planning tool argument
// (spec §§8.1, 21.50-21.51).
type Snapshot struct {
	Repository string
	Revision   string
}

type TreeEntry struct {
	Path string
	Size int64
}

// PinSnapshot performs the one allowed repository side effect (the serialized
// cache fetch) and resolves the configured base to a full commit SHA.
func (m *Manager) PinSnapshot(ctx context.Context, repoURL, base string) (Snapshot, error) {
	repository, err := m.EnsureMirror(ctx, repoURL)
	if err != nil {
		return Snapshot{}, err
	}
	ref := "refs/remotes/origin/" + base
	if !refExists(ctx, repository, ref) {
		ref = base
	}
	revision, err := revParse(ctx, repository, ref+"^{commit}")
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Repository: repository, Revision: revision}, nil
}

// OpenSnapshot reopens an already-pinned revision without fetching. A session
// therefore keeps reading the exact stored SHA even when upstream advances.
func (m *Manager) OpenSnapshot(ctx context.Context, repoURL, revision string) (Snapshot, error) {
	repository, err := m.mirrorPath(repoURL)
	if err != nil {
		return Snapshot{}, err
	}
	revision = strings.TrimSpace(revision)
	resolved, err := revParse(ctx, repository, revision+"^{commit}")
	if err != nil {
		return Snapshot{}, err
	}
	if resolved != revision {
		return Snapshot{}, fmt.Errorf("stored planning revision %q is not a full commit SHA", revision)
	}
	return Snapshot{Repository: repository, Revision: revision}, nil
}

func (m *Manager) ListSnapshotTree(ctx context.Context, snapshot Snapshot) ([]TreeEntry, error) {
	output, err := snapshotOutput(ctx, snapshot, "ls-tree", "-r", "-l", "-z", snapshot.Revision)
	if err != nil {
		return nil, err
	}
	records := strings.Split(output, "\x00")
	entries := make([]TreeEntry, 0, len(records))
	for _, record := range records {
		if record == "" {
			continue
		}
		header, path, ok := strings.Cut(record, "\t")
		if !ok {
			return nil, fmt.Errorf("unexpected git ls-tree record")
		}
		fields := strings.Fields(header)
		if len(fields) != 4 || fields[1] != "blob" {
			continue
		}
		size, parseErr := strconv.ParseInt(fields[3], 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("parse git tree size for %s: %w", path, parseErr)
		}
		entries = append(entries, TreeEntry{Path: path, Size: size})
	}
	return entries, nil
}

func (m *Manager) ReadSnapshotBlob(ctx context.Context, snapshot Snapshot, path string) ([]byte, error) {
	if err := safeSnapshotPath(path); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", snapshot.Revision+":"+path)
	cmd.Dir = snapshot.Repository
	output, err := cmd.Output()
	if err != nil {
		return nil, commandFailure("git cat-file blob", err)
	}
	return output, nil
}

func (m *Manager) GrepSnapshot(
	ctx context.Context,
	snapshot Snapshot,
	pattern, path string,
	contextLines int,
	filesOnly, caseInsensitive bool,
) (string, error) {
	args := []string{"grep", "-n", "-I", "--no-color"}
	if filesOnly {
		args = append(args, "-l")
	}
	if contextLines > 0 {
		args = append(args, "-C", strconv.Itoa(contextLines))
	}
	if caseInsensitive {
		args = append(args, "-i")
	}
	args = append(args, "-e", pattern, snapshot.Revision)
	if path != "" {
		if err := safeSnapshotPathspec(path); err != nil {
			return "", err
		}
		args = append(args, "--", path)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = snapshot.Repository
	output, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return "", fmt.Errorf("git grep: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func (m *Manager) SnapshotHistory(ctx context.Context, snapshot Snapshot, path string, n int) (string, error) {
	if err := safeSnapshotPath(path); err != nil {
		return "", err
	}
	logOutput, err := snapshotOutput(ctx, snapshot,
		"log", "--oneline", "--no-decorate", "-n", strconv.Itoa(n), snapshot.Revision, "--", path)
	if err != nil || strings.TrimSpace(logOutput) == "" {
		return logOutput, err
	}
	first := strings.Fields(strings.SplitN(logOutput, "\n", 2)[0])[0]
	stat, err := snapshotOutput(ctx, snapshot,
		"show", "--stat", "--oneline", "--no-renames", "--format=fuller", first, "--", path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(logOutput, "\n") + "\n\nLatest commit context:\n" + stat, nil
}

func snapshotOutput(ctx context.Context, snapshot Snapshot, args ...string) (string, error) {
	if snapshot.Repository == "" || snapshot.Revision == "" {
		return "", fmt.Errorf("planning snapshot is incomplete")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = snapshot.Repository
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func safeSnapshotPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("path is required")
	}
	if strings.ContainsRune(path, '\x00') || strings.HasPrefix(path, "/") {
		return fmt.Errorf("path must be repository-relative")
	}
	return nil
}

func safeSnapshotPathspec(path string) error {
	if strings.ContainsRune(path, '\x00') || strings.HasPrefix(path, "/") {
		return fmt.Errorf("path must be a repository-relative pathspec")
	}
	return nil
}

func commandFailure(operation string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("%s: %w: %s", operation, err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return fmt.Errorf("%s: %w", operation, err)
}
