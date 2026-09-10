package gitx

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type TreeEntry struct {
	Path string
	Size int64
}

const (
	defaultSnapshotOutputBytes = 1 << 20
	maxPlanningTextLineBytes   = 1 << 20
	gitTruncationMarker        = "\n… output truncated at git boundary; refine the query …\n"
)

func safeSnapshotPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("path is required")
	}
	return safeSnapshotPathspec(path)
}
func safeSnapshotPathspec(path string) error {
	if strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\\") {
		return fmt.Errorf("path must be repository-relative")
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return fmt.Errorf("path traversal outside the extracted root is refused")
		}
	}
	return nil
}

// Preserve repository-relative Git pathspecs, including literal, glob,
// case-insensitive, root-relative and exclusion forms.
type snapshotPathspec struct {
	pattern                            string
	literal, glob, ignoreCase, exclude bool
}

func parseSnapshotPathspec(value string) (snapshotPathspec, error) {
	spec := snapshotPathspec{pattern: value}
	if strings.HasPrefix(value, ":(") {
		end := strings.IndexByte(value, ')')
		if end < 0 {
			return spec, fmt.Errorf("invalid repository pathspec")
		}
		for _, flag := range strings.Split(value[2:end], ",") {
			switch flag {
			case "top":
			case "literal":
				spec.literal = true
			case "glob":
				spec.glob = true
			case "icase":
				spec.ignoreCase = true
			case "exclude", "!", "^":
				spec.exclude = true
			default:
				return spec, fmt.Errorf("unsupported repository pathspec magic %q", flag)
			}
		}
		spec.pattern = value[end+1:]
	} else if strings.HasPrefix(value, ":/") {
		spec.pattern = value[2:]
	} else if strings.HasPrefix(value, ":!") || strings.HasPrefix(value, ":^") {
		spec.exclude = true
		spec.pattern = value[2:]
	}
	if spec.literal && spec.glob {
		return spec, fmt.Errorf("pathspec literal and glob are incompatible")
	}
	if err := safeSnapshotPathspec(spec.pattern); err != nil {
		return spec, err
	}
	spec.pattern = strings.TrimSuffix(strings.TrimPrefix(spec.pattern, "./"), "/")
	return spec, nil
}
func snapshotPathMatcher(value string) (func(string) bool, error) {
	spec, err := parseSnapshotPathspec(value)
	if err != nil {
		return nil, err
	}
	pattern := spec.pattern
	var expression strings.Builder
	if spec.ignoreCase {
		expression.WriteString("(?i)")
	}
	expression.WriteString("^")
	if pattern == "" || pattern == "." {
		expression.WriteString(".*")
	} else if spec.literal {
		expression.WriteString(regexp.QuoteMeta(pattern))
	} else {
		for i := 0; i < len(pattern); i++ {
			switch pattern[i] {
			case '*':
				if !spec.glob {
					expression.WriteString(".*")
					continue
				}
				if i+1 < len(pattern) && pattern[i+1] == '*' {
					i++
					if i+1 < len(pattern) && pattern[i+1] == '/' {
						i++
						expression.WriteString("(?:.*/)?")
					} else {
						expression.WriteString(".*")
					}
				} else {
					expression.WriteString("[^/]*")
				}
			case '?':
				if spec.glob {
					expression.WriteString("[^/]")
				} else {
					expression.WriteString(".")
				}
			case '[':
				end := strings.IndexByte(pattern[i+1:], ']')
				if end < 0 {
					expression.WriteString(`\[`)
					continue
				}
				end += i + 1
				group := pattern[i : end+1]
				if strings.HasPrefix(group, "[!") {
					group = "[^" + group[2:]
				}
				expression.WriteString(group)
				i = end
			default:
				expression.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			}
		}
	}
	expression.WriteString("(?:/.*)?$")
	compiled, err := regexp.Compile(expression.String())
	if err != nil {
		return nil, fmt.Errorf("invalid repository pathspec: %w", err)
	}
	return func(name string) bool { return compiled.MatchString(name) != spec.exclude }, nil
}
func snapshotPathMatch(pattern, name string) bool {
	match, err := snapshotPathMatcher(pattern)
	return err == nil && match(name)
}
func (m *Manager) walk(ctx context.Context, snapshot Snapshot, pattern string, visit func(string, fs.DirEntry) error) error {
	match, err := snapshotPathMatcher(pattern)
	if err != nil {
		return err
	}
	if snapshot.Repository == "" || snapshot.sessionKey == "" || m.closed[snapshot.sessionKey] {
		return fmt.Errorf("planning snapshot is unavailable or closed")
	}
	return filepath.WalkDir(snapshot.Repository, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("snapshot contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("snapshot contains a non-regular file")
		}
		name, err := filepath.Rel(snapshot.Repository, file)
		if err != nil {
			return err
		}
		name = filepath.ToSlash(name)
		if !match(name) {
			return nil
		}
		return visit(name, entry)
	})
}
func (m *Manager) ListSnapshotTree(ctx context.Context, snapshot Snapshot, pathspec string, maxBytes int) ([]TreeEntry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	writer := newHeadTailWriter(maxBytes)
	err := m.walk(ctx, snapshot, pathspec, func(name string, entry fs.DirEntry) error {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(writer, "%d\t%s\x00", info.Size(), name)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	out := writer.result()
	var entries []TreeEntry
	for _, record := range out.records(0) {
		size, name, ok := strings.Cut(record, "\t")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			return nil, false, err
		}
		entries = append(entries, TreeEntry{Path: name, Size: n})
	}
	return entries, out.truncated, nil
}
func (m *Manager) ReadSnapshotBlob(ctx context.Context, snapshot Snapshot, path string, maxBytes int) ([]byte, error) {
	return m.readSnapshotBlob(ctx, snapshot, path, maxBytes, false)
}
func (m *Manager) ReadSnapshotTextBlob(ctx context.Context, snapshot Snapshot, path string, maxBytes int) ([]byte, error) {
	return m.readSnapshotBlob(ctx, snapshot, path, maxBytes, true)
}
func (m *Manager) readSnapshotBlob(ctx context.Context, snapshot Snapshot, path string, maxBytes int, textOnly bool) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := m.snapshotFile(snapshot, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if maxBytes <= 0 {
		maxBytes = defaultSnapshotOutputBytes
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > int64(maxBytes) {
		return nil, fmt.Errorf("blob %s is %d bytes; read limit is %d bytes", path, info.Size(), maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("blob %s exceeded its declared size while reading", path)
	}
	if textOnly && bytes.IndexByte(data[:min(len(data), 8<<10)], 0) >= 0 {
		return nil, fmt.Errorf("blob %s is binary; read_file supports text blobs only", path)
	}
	return data, nil
}
func (m *Manager) ReadSnapshotTextLines(ctx context.Context, snapshot Snapshot, path string, offset, limit int) ([]string, int, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if offset < 1 || limit < 1 {
		return nil, 0, false, fmt.Errorf("offset and limit must be positive")
	}
	file, err := m.snapshotFile(snapshot, path)
	if err != nil {
		return nil, 0, false, err
	}
	defer file.Close()
	prefix := make([]byte, 8<<10)
	n, err := file.Read(prefix)
	if err != nil && err != io.EOF {
		return nil, 0, false, err
	}
	if bytes.IndexByte(prefix[:n], 0) >= 0 {
		return nil, 0, false, fmt.Errorf("blob %s is binary; read_file supports text blobs only", path)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, 0, false, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxPlanningTextLineBytes)
	var lines []string
	total := 0
	for scanner.Scan() {
		if err = ctx.Err(); err != nil {
			return nil, 0, false, err
		}
		total++
		line := scanner.Text()
		if !utf8.ValidString(line) || strings.IndexByte(line, 0) >= 0 {
			return nil, 0, false, fmt.Errorf("blob %s is not valid text; read_file supports text blobs only", path)
		}
		if total >= offset && total-offset < limit {
			lines = append(lines, line)
		}
		if total >= offset && total-offset >= limit {
			return lines, total, false, nil
		}
	}
	if err = scanner.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("blob %s contains a line exceeding the %d-byte read_file ceiling: %w", path, maxPlanningTextLineBytes, err)
	}
	return lines, total, true, nil
}

func (m *Manager) GrepSnapshot(ctx context.Context, snapshot Snapshot, pattern, path string, contextLines int, filesOnly, caseInsensitive bool, maxResults, maxBytes int) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if contextLines < 0 || contextLines > 100 {
		return "", false, fmt.Errorf("invalid grep context bound")
	}
	matchLine, err := snapshotGrepPattern(ctx, pattern, caseInsensitive)
	if err != nil {
		return "", false, err
	}
	writer := newHeadTailWriter(maxBytes)
	err = m.walk(ctx, snapshot, path, func(name string, entry fs.DirEntry) error {
		file, err := m.snapshotFile(snapshot, name)
		if err != nil {
			return err
		}
		defer file.Close()
		prefix := make([]byte, 8<<10)
		n, err := file.Read(prefix)
		if err != nil && err != io.EOF {
			return err
		}
		if bytes.IndexByte(prefix[:n], 0) >= 0 {
			return nil
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		scanner := bufio.NewScanner(file)
		maxLine := int(min(m.MaxBytes, int64(int(^uint(0)>>1)-1))) + 1
		scanner.Buffer(make([]byte, 64<<10), maxLine)
		previous := []string{}
		lineNumber, matches, lastPrinted, after := 0, 0, 0, 0
		for scanner.Scan() {
			if err = ctx.Err(); err != nil {
				return err
			}
			lineNumber++
			line := scanner.Text()
			match, matchErr := matchLine(line)
			if matchErr != nil {
				return matchErr
			}
			match = match && (maxResults <= 0 || matches < maxResults)
			if match {
				matches++
				if filesOnly {
					fmt.Fprintf(writer, "%s:%s\n", snapshot.Revision, name)
					return nil
				}
				if contextLines > 0 && lastPrinted > 0 && lineNumber-len(previous) > lastPrinted+1 {
					fmt.Fprintln(writer, "--")
				}
				for i, prior := range previous {
					number := lineNumber - len(previous) + i
					if number > lastPrinted {
						fmt.Fprintf(writer, "%s-%s-%d-%s\n", snapshot.Revision, name, number, prior)
						lastPrinted = number
					}
				}
				fmt.Fprintf(writer, "%s:%s:%d:%s\n", snapshot.Revision, name, lineNumber, line)
				lastPrinted = lineNumber
				after = contextLines
			} else if after > 0 {
				fmt.Fprintf(writer, "%s-%s-%d-%s\n", snapshot.Revision, name, lineNumber, line)
				lastPrinted = lineNumber
				after--
			}
			if contextLines > 0 {
				previous = append(previous, line)
				if len(previous) > contextLines {
					previous = previous[1:]
				}
			}
			if maxResults > 0 && matches >= maxResults && after == 0 {
				break
			}
		}
		if err = scanner.Err(); err != nil {
			return fmt.Errorf("grep line exceeds snapshot size limit: %w", err)
		}
		return nil
	})
	out := writer.result()
	return out.text(), out.truncated, err
}
func (m *Manager) SnapshotHistory(ctx context.Context, snapshot Snapshot, path string, n, maxBytes int) (string, error) {
	if err := safeSnapshotPath(path); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed[snapshot.sessionKey] {
		return "", fmt.Errorf("planning session is closed")
	}
	commits, err := m.API.History(ctx, snapshot.slug, snapshot.Revision, path, n)
	if err != nil {
		return "", err
	}
	if len(commits) == 0 {
		return "", nil
	}
	detail, err := m.API.CommitDetail(ctx, snapshot.slug, commits[0].SHA)
	if err != nil {
		return "", err
	}
	output := newHeadTailWriter(maxBytes)
	for _, commit := range commits {
		message, _, _ := strings.Cut(commit.Commit.Message, "\n")
		fmt.Fprintf(output, "%s %s\n", commit.SHA[:7], message)
	}
	fmt.Fprintf(output, "\nLatest commit context:\ncommit %s\nAuthor: %s\nAuthorDate: %s\nCommit: %s\nCommitDate: %s\n\n%s\n\nGitHub-native rename-aware statistics (additions, deletions, changes):\n", detail.SHA, detail.Commit.Author.Name, detail.Commit.Author.Date, detail.Commit.Committer.Name, detail.Commit.Committer.Date, detail.Commit.Message)
	for _, file := range detail.Files {
		if !snapshotPathMatch(path, file.Filename) && !snapshotPathMatch(path, file.PreviousFilename) {
			continue
		}
		fmt.Fprintf(output, "%s (%s)", file.Filename, file.Status)
		if file.PreviousFilename != "" {
			fmt.Fprintf(output, " from %s", file.PreviousFilename)
		}
		fmt.Fprintf(output, " | +%d -%d %d changes\n", *file.Additions, *file.Deletions, *file.Changes)
	}
	return output.result().text(), nil
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
	if o.maxBytes < len(gitTruncationMarker) {
		return snapshotTextHead([]byte(gitTruncationMarker), o.maxBytes)
	}
	remaining := max(0, o.maxBytes-len(gitTruncationMarker))
	headBytes := min(len(o.head), remaining/2)
	tailBytes := min(len(o.tail), remaining-headBytes)
	return snapshotTextHead(o.head, headBytes) + gitTruncationMarker + snapshotTextTail(o.tail, tailBytes)
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

func snapshotTextHead(data []byte, n int) string {
	data = data[:min(len(data), max(0, n))]
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return string(data)
}
func snapshotTextTail(data []byte, n int) string {
	data = data[len(data)-min(len(data), max(0, n)):]
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[1:]
	}
	return string(data)
}
