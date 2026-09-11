package gitx

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

const DefaultSnapshotMaxBytes int64 = 512 << 20

type SnapshotAPI interface {
	ResolveCommit(context.Context, string, string) (string, error)
	OpenArchive(context.Context, string, string) (io.ReadCloser, error)
	History(context.Context, string, string, string, int) ([]github.SnapshotCommit, error)
	CommitDetail(context.Context, string, string) (github.SnapshotCommit, error)
}

// Manager owns ephemeral session archives; it never executes Git (DEC-32).
// The mutex orders reads and extraction against terminal cleanup.
type Manager struct {
	API      SnapshotAPI
	Root     string
	MaxBytes int64
	mu       sync.Mutex
	closed   map[string]bool
}

func NewManager(api SnapshotAPI, maxBytes int64) *Manager {
	if api == nil {
		api = github.NewSnapshotClient(nil, "")
	}
	if maxBytes == 0 {
		maxBytes = DefaultSnapshotMaxBytes
	}
	temporary := os.TempDir()
	if canonical, err := filepath.EvalSymlinks(temporary); err == nil {
		temporary = canonical
	}
	return &Manager{API: api, MaxBytes: maxBytes, Root: filepath.Join(temporary, "conveyor-planning-snapshots"), closed: map[string]bool{}}
}

type Snapshot struct {
	Repository string
	Revision   string
	slug       string
	sessionKey string
}
type snapshotManifest struct {
	Workspace string
	Session   string
}

func snapshotKey(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}
func (m *Manager) PinSnapshot(ctx context.Context, repoURL, base string) (Snapshot, error) {
	slug := GitHubSlug(repoURL)
	if slug == "" {
		return Snapshot{}, fmt.Errorf("planning requires a GitHub repository")
	}
	sha, err := m.API.ResolveCommit(ctx, slug, base)
	if err != nil {
		return Snapshot{}, err
	}
	if !github.ValidSnapshotSHA(sha) {
		return Snapshot{}, fmt.Errorf("planning revision is not a full commit SHA")
	}
	return Snapshot{Revision: sha, slug: slug}, nil
}
func (m *Manager) root() (string, error) {
	root, err := filepath.Abs(m.Root)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if canonical != root {
		return "", fmt.Errorf("snapshot root must be canonical and contain no symlink")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("snapshot root must be an owner-only directory")
	}
	return root, nil
}
func (m *Manager) OpenSnapshot(ctx context.Context, workspace, session, repoURL, revision string) (Snapshot, error) {
	if workspace == "" || session == "" || !github.ValidSnapshotSHA(revision) {
		return Snapshot{}, fmt.Errorf("planning snapshot requires a workspace, session and full commit SHA")
	}
	slug := GitHubSlug(repoURL)
	if slug == "" {
		return Snapshot{}, fmt.Errorf("planning requires a GitHub repository")
	}
	key := snapshotKey(workspace, session)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed[key] {
		return Snapshot{}, fmt.Errorf("planning session is closed")
	}
	root, err := m.root()
	if err != nil {
		return Snapshot{}, err
	}
	sessionRoot := filepath.Join(root, key)
	if _, statErr := os.Lstat(sessionRoot); os.IsNotExist(statErr) {
		pending, createErr := os.MkdirTemp(root, "session-")
		if createErr != nil {
			return Snapshot{}, createErr
		}
		defer os.RemoveAll(pending)
		manifest, _ := json.Marshal(snapshotManifest{Workspace: workspace, Session: session})
		if err = os.WriteFile(filepath.Join(pending, "session.json"), manifest, 0600); err != nil {
			return Snapshot{}, err
		}
		if err = os.Rename(pending, sessionRoot); err != nil {
			return Snapshot{}, err
		}
	} else if statErr != nil {
		return Snapshot{}, statErr
	}
	if err = rejectSnapshotLinks(root, sessionRoot); err != nil {
		return Snapshot{}, err
	}
	target := filepath.Join(sessionRoot, snapshotKey(slug, revision))
	snapshot := Snapshot{Repository: target, Revision: revision, slug: slug, sessionKey: key}
	if info, statErr := os.Lstat(target); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return Snapshot{}, fmt.Errorf("invalid snapshot directory")
		}
		return snapshot, nil
	} else if !os.IsNotExist(statErr) {
		return Snapshot{}, statErr
	}
	used, err := snapshotSessionBytes(sessionRoot, m.MaxBytes)
	if err != nil {
		return Snapshot{}, err
	}
	stage, err := os.MkdirTemp(sessionRoot, "extract-")
	if err != nil {
		return Snapshot{}, err
	}
	defer os.RemoveAll(stage)
	archive, err := m.API.OpenArchive(ctx, slug, revision)
	if err != nil {
		return Snapshot{}, err
	}
	defer archive.Close()
	if err = extractSnapshot(ctx, archive, stage, m.MaxBytes, used); err != nil {
		return Snapshot{}, err
	}
	if err = os.Rename(stage, target); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}
func (m *Manager) CloseSession(workspace, session string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := snapshotKey(workspace, session)
	if m.closed == nil {
		m.closed = map[string]bool{}
	}
	m.closed[key] = true
	root, err := m.root()
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(root, key))
}

// CleanupClosed runs before serving requests. Unknown or unreadable sessions
// are retained on lookup failures; only a confirmed closed/absent row is removed.
func (m *Manager) CleanupClosed(ctx context.Context, isOpen func(context.Context, string, string) (bool, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := m.root()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if strings.HasPrefix(entry.Name(), "session-") {
			if err = os.RemoveAll(dir); err != nil {
				return err
			}
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, "session.json"))
		if readErr != nil {
			return fmt.Errorf("read snapshot session manifest: %w", readErr)
		}
		var manifest snapshotManifest
		if json.Unmarshal(raw, &manifest) != nil || manifest.Workspace == "" || manifest.Session == "" || snapshotKey(manifest.Workspace, manifest.Session) != entry.Name() {
			return fmt.Errorf("invalid snapshot session manifest")
		}
		active, lookupErr := isOpen(ctx, manifest.Workspace, manifest.Session)
		if lookupErr != nil {
			return lookupErr
		}
		if active {
			children, readErr := os.ReadDir(dir)
			if readErr != nil {
				return readErr
			}
			for _, child := range children {
				if strings.HasPrefix(child.Name(), "extract-") {
					if err = os.RemoveAll(filepath.Join(dir, child.Name())); err != nil {
						return err
					}
				}
			}
		}
		if !active {
			if err = os.RemoveAll(dir); err != nil {
				return err
			}
		}
	}
	return nil
}
func extractSnapshot(ctx context.Context, archive io.Reader, root string, limit, existingBytes int64) error {
	if limit <= 0 {
		return fmt.Errorf("snapshot size limit must be positive")
	}
	transfer := &io.LimitedReader{R: archive, N: limit + 1}
	gz, err := gzip.NewReader(transfer)
	if err != nil {
		return fmt.Errorf("open snapshot tarball: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	extracted := existingBytes
	top := ""
	count := 0
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		header, readErr := tr.Next()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if transfer.N <= 0 {
				return fmt.Errorf("snapshot compressed transfer exceeds %d-byte limit", limit)
			}
			return fmt.Errorf("read snapshot archive: %w", readErr)
		}
		count++
		if count > 1000000 {
			return fmt.Errorf("snapshot exceeds 1000000-entry limit")
		}
		name := strings.TrimSuffix(header.Name, "/")
		if name == "" || path.IsAbs(name) || strings.ContainsAny(name, "\x00\\") {
			return fmt.Errorf("unsafe snapshot archive path")
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." || part == "." || part == "" {
				return fmt.Errorf("unsafe snapshot archive traversal")
			}
		}
		first, relative, _ := strings.Cut(name, "/")
		if top == "" {
			top = first
		}
		if first != top {
			return fmt.Errorf("snapshot archive has multiple roots")
		}
		if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsafe snapshot archive entry type %d", header.Typeflag)
		}
		if relative == "" {
			if header.Typeflag != tar.TypeDir {
				return fmt.Errorf("snapshot archive root is not a directory")
			}
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(relative))
		if header.Typeflag == tar.TypeDir {
			if err = os.MkdirAll(target, 0700); err != nil {
				return err
			}
			continue
		}
		if header.Size < 0 || header.Size > limit-extracted {
			return fmt.Errorf("snapshot extracted size exceeds %d-byte limit", limit)
		}
		extracted += header.Size
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		file, openErr := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if openErr != nil {
			return fmt.Errorf("unsafe or duplicate snapshot entry: %w", openErr)
		}
		_, copyErr := io.Copy(file, tr)
		closeErr := file.Close()
		if copyErr != nil {
			if transfer.N <= 0 {
				return fmt.Errorf("snapshot compressed transfer exceeds %d-byte limit", limit)
			}
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	// Consume gzip trailer and trailing members to verify checksum and the
	// transfer cap, without permitting unlimited decompressed archive padding.
	trailing, err := io.Copy(io.Discard, io.LimitReader(gz, limit+1))
	if transfer.N <= 0 {
		return fmt.Errorf("snapshot compressed transfer exceeds %d-byte limit", limit)
	}
	if err != nil {
		return fmt.Errorf("verify snapshot tarball: %w", err)
	}
	if trailing > limit {
		return fmt.Errorf("snapshot decoded padding exceeds %d-byte limit", limit)
	}
	if _, err = io.Copy(io.Discard, transfer); err != nil {
		return err
	}
	if transfer.N <= 0 {
		return fmt.Errorf("snapshot compressed transfer exceeds %d-byte limit", limit)
	}
	if top == "" {
		return fmt.Errorf("snapshot archive is empty")
	}
	return nil
}
func rejectSnapshotLinks(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("snapshot path escapes extracted root")
	}
	current := root
	for _, part := range append([]string{""}, strings.Split(relative, string(filepath.Separator))...) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("snapshot path contains a symlink")
		}
	}
	return nil
}
func (m *Manager) snapshotFile(snapshot Snapshot, name string) (*os.File, error) {
	if err := safeSnapshotPath(name); err != nil {
		return nil, err
	}
	if snapshot.Repository == "" || snapshot.sessionKey == "" || m.closed[snapshot.sessionKey] {
		return nil, fmt.Errorf("planning snapshot is unavailable or closed")
	}
	target := filepath.Join(snapshot.Repository, filepath.FromSlash(name))
	if err := rejectSnapshotLinks(snapshot.Repository, target); err != nil {
		return nil, err
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("snapshot path is not a regular file")
	}
	return file, nil
}

func snapshotSessionBytes(root string, limit int64) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("snapshot session contains a symlink")
		}
		if entry.IsDir() || name == filepath.Join(root, "session.json") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("snapshot session contains a special file")
		}
		if info.Size() > limit-total {
			return fmt.Errorf("snapshot extracted size exceeds %d-byte session limit", limit)
		}
		total += info.Size()
		return nil
	})
	return total, err
}
