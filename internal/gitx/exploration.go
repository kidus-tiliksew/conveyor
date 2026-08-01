package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"
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

const (
	defaultSnapshotOutputBytes = 1 << 20
	maxSnapshotStderrBytes     = 32 << 10
	maxPlanningTextLineBytes   = 1 << 20
	gitTruncationMarker        = "\n… output truncated at git boundary; refine the query …\n"
)

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

func (m *Manager) ListSnapshotTree(
	ctx context.Context,
	snapshot Snapshot,
	pathspec string,
	maxBytes int,
) ([]TreeEntry, bool, error) {
	args := []string{"ls-tree", "-r", "-l", "-z", snapshot.Revision}
	if pathspec != "" {
		if err := safeSnapshotPathspec(pathspec); err != nil {
			return nil, false, err
		}
		args = append(args, "--", pathspec)
	}
	output, err := snapshotOutput(ctx, snapshot, maxBytes, args...)
	if err != nil {
		return nil, false, err
	}
	records := output.records(0)
	entries := make([]TreeEntry, 0, len(records))
	for _, record := range records {
		if record == "" {
			continue
		}
		header, path, ok := strings.Cut(record, "\t")
		if !ok {
			if output.truncated {
				continue
			}
			return nil, false, fmt.Errorf("unexpected git ls-tree record")
		}
		fields := strings.Fields(header)
		if len(fields) != 4 || fields[1] != "blob" {
			continue
		}
		size, parseErr := strconv.ParseInt(fields[3], 10, 64)
		if parseErr != nil {
			return nil, false, fmt.Errorf("parse git tree size for %s: %w", path, parseErr)
		}
		entries = append(entries, TreeEntry{Path: path, Size: size})
	}
	return entries, output.truncated, nil
}

func (m *Manager) ReadSnapshotBlob(ctx context.Context, snapshot Snapshot, path string, maxBytes int) ([]byte, error) {
	return m.readSnapshotBlob(ctx, snapshot, path, maxBytes, false)
}

// ReadSnapshotTextBlob rejects a Git-binary prefix before loading the complete
// blob. The size gate still runs first, so a large checked-in binary never
// reaches cat-file's content path at all.
func (m *Manager) ReadSnapshotTextBlob(ctx context.Context, snapshot Snapshot, path string, maxBytes int) ([]byte, error) {
	return m.readSnapshotBlob(ctx, snapshot, path, maxBytes, true)
}

// ReadSnapshotTextLines streams a text blob and retains only the requested
// line window. The blob itself is intentionally not bounded by the rendered
// response cap: pagination is the bound. A finite per-line ceiling prevents a
// pathological blob without newlines from becoming an unbounded allocation.
func (m *Manager) ReadSnapshotTextLines(
	ctx context.Context,
	snapshot Snapshot,
	path string,
	offset int,
	limit int,
) ([]string, int, error) {
	if err := safeSnapshotPath(path); err != nil {
		return nil, 0, err
	}
	object := snapshot.Revision + ":" + path
	metadata, err := snapshotOutput(ctx, snapshot, 128, "cat-file", "-s", object)
	if err != nil {
		return nil, 0, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(metadata.text()), 10, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("parse git blob size for %s: %w", path, err)
	}
	if size > 0 {
		prefix, prefixErr := snapshotBlobPrefix(ctx, snapshot, object, size, 8<<10)
		if prefixErr != nil {
			return nil, 0, prefixErr
		}
		if bytes.IndexByte(prefix, 0) >= 0 {
			return nil, 0, fmt.Errorf("blob %s is binary; read_file supports text blobs only", path)
		}
	}

	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", object)
	cmd.Dir = snapshot.Repository
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, err
	}
	stderr := &limitedBuffer{limit: maxSnapshotStderrBytes}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		return nil, 0, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxPlanningTextLineBytes)
	lines := make([]string, 0, limit)
	total := 0
	for scanner.Scan() {
		total++
		line := scanner.Text()
		if !utf8.ValidString(line) || strings.IndexByte(line, 0) >= 0 {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, 0, fmt.Errorf("blob %s is not valid text; read_file supports text blobs only", path)
		}
		if total >= offset && len(lines) < limit {
			lines = append(lines, line)
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, 0, fmt.Errorf("blob %s contains a line exceeding the %d-byte read_file ceiling: %w",
			path, maxPlanningTextLineBytes, scanErr)
	}
	if err = cmd.Wait(); err != nil {
		return nil, 0, fmt.Errorf("git cat-file blob %s: %w: %s", path, err, stderr.String())
	}
	return lines, total, nil
}

func (m *Manager) readSnapshotBlob(
	ctx context.Context,
	snapshot Snapshot,
	path string,
	maxBytes int,
	textOnly bool,
) ([]byte, error) {
	if err := safeSnapshotPath(path); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = defaultSnapshotOutputBytes
	}
	object := snapshot.Revision + ":" + path
	metadata, err := snapshotOutput(ctx, snapshot, 128, "cat-file", "-s", object)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(metadata.text()), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse git blob size for %s: %w", path, err)
	}
	if size > int64(maxBytes) {
		return nil, fmt.Errorf("blob %s is %d bytes; read limit is %d bytes", path, size, maxBytes)
	}
	if textOnly && size > 0 {
		prefix, prefixErr := snapshotBlobPrefix(ctx, snapshot, object, size, 8<<10)
		if prefixErr != nil {
			return nil, prefixErr
		}
		if bytes.IndexByte(prefix, 0) >= 0 {
			return nil, fmt.Errorf("blob %s is binary; read_file supports text blobs only", path)
		}
	}
	output, err := snapshotOutput(ctx, snapshot, maxBytes, "cat-file", "blob", object)
	if err != nil {
		return nil, err
	}
	if output.truncated {
		return nil, fmt.Errorf("blob %s exceeded its declared size while reading", path)
	}
	return []byte(output.text()), nil
}

func snapshotBlobPrefix(
	ctx context.Context,
	snapshot Snapshot,
	object string,
	size int64,
	maxBytes int,
) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", object)
	cmd.Dir = snapshot.Repository
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &limitedBuffer{limit: maxSnapshotStderrBytes}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	limit := min(int64(maxBytes), size)
	prefix, readErr := io.ReadAll(io.LimitReader(stdout, limit))
	if size > limit && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return nil, fmt.Errorf("git cat-file blob prefix: %w", readErr)
	}
	if size > limit {
		return prefix, nil
	}
	if waitErr != nil {
		return nil, fmt.Errorf("git cat-file blob prefix: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return prefix, nil
}

func (m *Manager) GrepSnapshot(
	ctx context.Context,
	snapshot Snapshot,
	pattern, path string,
	contextLines int,
	filesOnly, caseInsensitive bool,
	maxResults, maxBytes int,
) (string, bool, error) {
	args := []string{"grep", "-n", "-I", "--no-color"}
	if maxResults > 0 {
		args = append(args, "--max-count", strconv.Itoa(maxResults))
	}
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
			return "", false, err
		}
		args = append(args, "--", path)
	}
	output, err := snapshotOutput(ctx, snapshot, maxBytes, args...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return output.text(), output.truncated, nil
}

func (m *Manager) SnapshotHistory(ctx context.Context, snapshot Snapshot, path string, n, maxBytes int) (string, error) {
	if err := safeSnapshotPath(path); err != nil {
		return "", err
	}
	logResult, err := snapshotOutput(ctx, snapshot, maxBytes/2,
		"log", "--oneline", "--no-decorate", "-n", strconv.Itoa(n), snapshot.Revision, "--", path)
	logOutput := logResult.text()
	if err != nil || strings.TrimSpace(logOutput) == "" {
		return logOutput, err
	}
	fields := strings.Fields(strings.SplitN(logOutput, "\n", 2)[0])
	if len(fields) == 0 {
		return logOutput, nil
	}
	first := fields[0]
	statResult, err := snapshotOutput(ctx, snapshot, maxBytes/2,
		"show", "--stat", "--oneline", "--no-renames", "--format=fuller", first, "--", path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(logOutput, "\n") + "\n\nLatest commit context:\n" + statResult.text(), nil
}

type boundedCommandOutput struct {
	head      []byte
	tail      []byte
	truncated bool
	maxBytes  int
}

func (o boundedCommandOutput) text() string {
	if !o.truncated {
		return string(append(append([]byte(nil), o.head...), o.tail...))
	}
	remaining := max(0, o.maxBytes-len(gitTruncationMarker))
	headBytes := min(len(o.head), remaining/2)
	tailBytes := min(len(o.tail), remaining-headBytes)
	return string(o.head[:headBytes]) + gitTruncationMarker + string(o.tail[len(o.tail)-tailBytes:])
}

func (o boundedCommandOutput) records(separator byte) []string {
	if !o.truncated {
		return strings.Split(o.text(), string(separator))
	}
	left := strings.Split(string(o.head), string(separator))
	right := strings.Split(string(o.tail), string(separator))
	if len(left) > 0 {
		left = left[:len(left)-1]
	}
	if len(right) > 0 {
		right = right[1:]
	}
	return append(left, right...)
}

type headTailWriter struct {
	limit   int
	total   int64
	head    []byte
	tail    []byte
	tailPos int
	filled  int
}

func newHeadTailWriter(limit int) *headTailWriter {
	if limit <= 0 {
		limit = defaultSnapshotOutputBytes
	}
	return &headTailWriter{limit: limit, tail: make([]byte, max(1, limit/2))}
}

func (w *headTailWriter) Write(p []byte) (int, error) {
	written := len(p)
	w.total += int64(written)
	headLimit := w.limit - len(w.tail)
	if len(w.head) < headLimit {
		take := min(len(p), headLimit-len(w.head))
		w.head = append(w.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return written, nil
	}
	if len(p) >= len(w.tail) {
		copy(w.tail, p[len(p)-len(w.tail):])
		w.tailPos, w.filled = 0, len(w.tail)
		return written, nil
	}
	first := min(len(p), len(w.tail)-w.tailPos)
	copy(w.tail[w.tailPos:], p[:first])
	copy(w.tail, p[first:])
	w.tailPos = (w.tailPos + len(p)) % len(w.tail)
	w.filled = min(len(w.tail), w.filled+len(p))
	return written, nil
}

func (w *headTailWriter) result() boundedCommandOutput {
	if w.total <= int64(w.limit) {
		return boundedCommandOutput{head: w.head, tail: append([]byte(nil), w.tail[:w.filled]...), maxBytes: w.limit}
	}
	tail := make([]byte, w.filled)
	first := copy(tail, w.tail[w.tailPos:w.filled])
	copy(tail[first:], w.tail[:w.tailPos])
	return boundedCommandOutput{head: w.head, tail: tail, truncated: true, maxBytes: w.limit}
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if remaining := w.limit - w.Len(); remaining > 0 {
		_, _ = w.Buffer.Write(p[:min(len(p), remaining)])
	}
	return written, nil
}

func snapshotOutput(ctx context.Context, snapshot Snapshot, maxBytes int, args ...string) (boundedCommandOutput, error) {
	if snapshot.Repository == "" || snapshot.Revision == "" {
		return boundedCommandOutput{}, fmt.Errorf("planning snapshot is incomplete")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = snapshot.Repository
	stdout := newHeadTailWriter(maxBytes)
	stderr := &limitedBuffer{limit: maxSnapshotStderrBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if err != nil {
		return boundedCommandOutput{}, fmt.Errorf(
			"git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.result(), nil
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
