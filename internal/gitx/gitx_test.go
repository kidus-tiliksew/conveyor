package gitx

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

const testSHA = "0123456789012345678901234567890123456789"

func testArchive(t *testing.T, files map[string][]byte, extra ...*tar.Header) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "root/" + name, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	for _, header := range extra {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func snapshotFixture(t *testing.T, archive []byte) (*Manager, context.Context, *atomic.Int32) {
	t.Helper()
	downloads := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer snapshot-fixture" {
			http.Error(w, "auth", 401)
			return
		}
		switch r.URL.Path {
		case "/repos/owner/repo/commits/main":
			json.NewEncoder(w).Encode(map[string]string{"sha": testSHA})
		case "/repos/owner/repo/tarball/" + testSHA:
			downloads.Add(1)
			http.Redirect(w, r, "/download", http.StatusFound)
		case "/download":
			w.Write(archive)
		case "/repos/owner/repo/commits":
			json.NewEncoder(w).Encode([]any{map[string]any{"sha": testSHA, "commit": map[string]string{"message": "initial"}}})
		case "/repos/owner/repo/commits/" + testSHA:
			json.NewEncoder(w).Encode(map[string]any{"sha": testSHA, "commit": map[string]string{"message": "initial"}, "files": []any{map[string]any{"filename": "internal/eligibility.go", "status": "added", "additions": 3, "deletions": 0, "changes": 3}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	manager := NewManager(github.NewSnapshotClient(server.Client(), server.URL), 0)
	manager.Root = filepath.Join(t.TempDir(), "snapshots")
	return manager, github.WithCredential(t.Context(), "snapshot-fixture", "workspace demo GitHub App"), downloads
}
func openFixture(t *testing.T, m *Manager, ctx context.Context) Snapshot {
	t.Helper()
	pin, err := m.PinSnapshot(ctx, "https://github.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.OpenSnapshot(ctx, "demo", "session", "https://github.com/owner/repo", pin.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
func TestPlanningSnapshotPlumbingStaysPinnedAndReadOnly(t *testing.T) {
	content := "package internal\n\nfunc eligible() bool { return true }\n"
	m, ctx, downloads := snapshotFixture(t, testArchive(t, map[string][]byte{"internal/eligibility.go": []byte(content)}))
	pin, err := m.PinSnapshot(ctx, "https://github.com/owner/repo", "main")
	if err != nil || pin.Revision != testSHA || downloads.Load() != 0 {
		t.Fatalf("pin=%+v downloads=%d err=%v", pin, downloads.Load(), err)
	}
	snapshot := openFixture(t, m, ctx)
	entries, truncated, err := m.ListSnapshotTree(ctx, snapshot, "internal", 1<<20)
	if err != nil || truncated || len(entries) != 1 || entries[0].Path != "internal/eligibility.go" {
		t.Fatalf("tree=%+v err=%v", entries, err)
	}
	blob, err := m.ReadSnapshotBlob(ctx, snapshot, "internal/eligibility.go", 1<<20)
	if err != nil || string(blob) != content {
		t.Fatalf("blob=%q err=%v", blob, err)
	}
	matches, _, err := m.GrepSnapshot(ctx, snapshot, "eligible", "internal", 0, false, false, 200, 1<<20)
	if err != nil || !strings.Contains(matches, "eligibility.go:3:") {
		t.Fatalf("grep=%q err=%v", matches, err)
	}
	history, err := m.SnapshotHistory(ctx, snapshot, "internal/eligibility.go", 20, 1<<20)
	if err != nil || !strings.Contains(history, "Latest commit context:") || !strings.Contains(history, "rename-aware") || !strings.Contains(history, "+3 -0") {
		t.Fatalf("history=%q err=%v", history, err)
	}
	reopened, err := m.OpenSnapshot(ctx, "demo", "session", "https://github.com/owner/repo", testSHA)
	if err != nil || reopened.Repository != snapshot.Repository || downloads.Load() != 1 {
		t.Fatalf("reopen err=%v downloads=%d", err, downloads.Load())
	}
	for _, path := range []string{"../outside", "/etc/passwd", "internal/../../outside"} {
		if _, err = m.ReadSnapshotBlob(ctx, snapshot, path, 100); err == nil {
			t.Fatalf("accepted traversal %q", path)
		}
	}
	if err = os.Symlink("/etc/passwd", filepath.Join(snapshot.Repository, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err = m.ReadSnapshotBlob(ctx, snapshot, "escape", 1<<20); err == nil {
		t.Fatal("read symlink")
	}
}
func TestPlanningSnapshotPlumbingBoundsLargeSearchAndRejectsOversizedBlob(t *testing.T) {
	m, ctx, _ := snapshotFixture(t, testArchive(t, map[string][]byte{
		"large.txt": []byte(strings.Repeat("match match match\n", 1000)), "large.bin": bytes.Repeat([]byte{0, 255}, 50000),
		"single-line.txt": []byte(strings.Repeat("x", maxPlanningTextLineBytes+1)), "paginated-tail.txt": []byte("one\ntwo\nthree\n"),
	}))
	snapshot := openFixture(t, m, ctx)
	matches, truncated, err := m.GrepSnapshot(ctx, snapshot, ".", "large.txt", 0, false, false, 50, 256)
	if err != nil || !truncated || len(matches) > 256 || !strings.Contains(matches, gitTruncationMarker) {
		t.Fatalf("grep len=%d trunc=%v err=%v", len(matches), truncated, err)
	}
	if _, err = m.ReadSnapshotBlob(ctx, snapshot, "large.bin", 256); err == nil || !strings.Contains(err.Error(), "read limit") {
		t.Fatalf("size err=%v", err)
	}
	if _, err = m.ReadSnapshotTextBlob(ctx, snapshot, "large.bin", 128<<10); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary err=%v", err)
	}
	if _, _, _, err = m.ReadSnapshotTextLines(ctx, snapshot, "single-line.txt", 1, 10); err == nil {
		t.Fatal("unbounded line")
	}
	page, total, complete, err := m.ReadSnapshotTextLines(ctx, snapshot, "paginated-tail.txt", 1, 2)
	if err != nil || len(page) != 2 || total != 3 || complete {
		t.Fatalf("page=%v total=%d complete=%v err=%v", page, total, complete, err)
	}
	if _, _, err = m.GrepSnapshot(ctx, snapshot, "[", "large.txt", 0, false, false, 50, 256); err == nil {
		t.Fatal("invalid regexp")
	}
}
func TestSnapshotRejectsUnsafeArchivesAndBothSizeCaps(t *testing.T) {
	random := make([]byte, 4096)
	rand.New(rand.NewSource(1)).Read(random)
	tests := []struct {
		name    string
		archive []byte
		limit   int64
		want    string
	}{
		{"traversal", testArchive(t, nil, &tar.Header{Name: "root/../escape", Typeflag: tar.TypeDir}), 1 << 20, "traversal"},
		{"absolute", testArchive(t, nil, &tar.Header{Name: "/escape", Typeflag: tar.TypeDir}), 1 << 20, "unsafe"},
		{"symlink", testArchive(t, nil, &tar.Header{Name: "root/link", Typeflag: tar.TypeSymlink, Linkname: "/etc"}), 1 << 20, "entry type"},
		{"hardlink", testArchive(t, nil, &tar.Header{Name: "root/link", Typeflag: tar.TypeLink, Linkname: "root/file"}), 1 << 20, "entry type"},
		{"device", testArchive(t, nil, &tar.Header{Name: "root/device", Typeflag: tar.TypeChar}), 1 << 20, "entry type"},
		{"extracted", testArchive(t, map[string][]byte{"large": bytes.Repeat([]byte("a"), 4096)}), 1024, "extracted size"},
		{"compressed", testArchive(t, map[string][]byte{"random": random}), 4096, "compressed transfer"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m, ctx, _ := snapshotFixture(t, test.archive)
			m.MaxBytes = test.limit
			_, err := m.OpenSnapshot(ctx, "demo", "session", "https://github.com/owner/repo", testSHA)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v want=%s", err, test.want)
			}
			matches, _ := filepath.Glob(filepath.Join(m.Root, "*", "extract-*"))
			if len(matches) != 0 {
				t.Fatalf("leaked extraction: %v", matches)
			}
		})
	}
}
func TestSnapshotConcurrentReuseTerminalCleanupAndRestart(t *testing.T) {
	m, ctx, downloads := snapshotFixture(t, testArchive(t, map[string][]byte{"file": []byte("data")}))
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.OpenSnapshot(ctx, "demo", "session", "https://github.com/owner/repo", testSHA)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if downloads.Load() != 1 {
		t.Fatalf("downloads=%d", downloads.Load())
	}
	snapshot := openFixture(t, m, ctx)
	restarted := NewManager(m.API, 0)
	restarted.Root = m.Root
	if err := restarted.CleanupClosed(ctx, func(context.Context, string, string) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snapshot.Repository); err != nil {
		t.Fatal(err)
	}
	if err := restarted.CleanupClosed(ctx, func(context.Context, string, string) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snapshot.Repository); !os.IsNotExist(err) {
		t.Fatalf("orphan remains: %v", err)
	}
	snapshot = openFixture(t, m, ctx)
	if err := m.CloseSession("demo", "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snapshot.Repository); !os.IsNotExist(err) {
		t.Fatalf("closed snapshot remains: %v", err)
	}
	if _, err := m.ReadSnapshotBlob(ctx, snapshot, "file", 100); err == nil {
		t.Fatal("read closed snapshot")
	}
	if _, err := m.OpenSnapshot(ctx, "demo", "session", "https://github.com/owner/repo", testSHA); err == nil {
		t.Fatal("reopened closed session")
	}
}
func TestSnapshotPermissionWithoutCredential(t *testing.T) {
	m, _, _ := snapshotFixture(t, nil)
	_, err := m.PinSnapshot(t.Context(), "https://github.com/owner/repo", "main")
	if github.ErrorCategory(err) != github.ForgePermission || !strings.Contains(fmt.Sprint(err), "workspace settings") {
		t.Fatalf("err=%v", err)
	}
}

func TestSnapshotGrepPreservesBasicRegularExpressions(t *testing.T) {
	for _, test := range []struct {
		pattern, line string
		match         bool
	}{
		{`func eligible()`, "func eligible()", true},
		{`\(foo\)\1`, "foofoo", true},
		{`\(foo\)\1`, "foobar", false},
		{`a\+`, "aaa", true},
		{`a+`, "aaa", false},
		{`foo\|bar`, "bar", true},
		{"foo\nbar", "bar", true},
		{`\<foo\>`, "foo", true},
		{`\<foo\>`, "foobar", false},
		{`[[:alpha:]]\{3\}`, "abc", true},
	} {
		t.Run(test.pattern, func(t *testing.T) {
			match, err := snapshotGrepPattern(t.Context(), test.pattern, false)
			if err != nil {
				t.Fatal(err)
			}
			got, err := match(test.line)
			if err != nil || got != test.match {
				t.Fatalf("match=%v want=%v err=%v", got, test.match, err)
			}
		})
	}
}

func TestSnapshotHistoryFiltersBothRenamePathsAfterCompleteAcquisition(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("missing auth")
		}
		commit := map[string]any{"sha": testSHA, "commit": map[string]string{"message": "rename and remove"}}
		if r.URL.Path == "/repos/owner/repo/commits" {
			json.NewEncoder(w).Encode([]any{commit})
			return
		}
		requests++
		file := map[string]any{"filename": "unrelated", "status": "modified", "additions": 1, "deletions": 1, "changes": 2}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", "<?per_page=100&page=2>; rel=\"next\"")
			commit["files"] = []any{file}
		} else {
			commit["files"] = []any{
				map[string]any{"filename": "new/file.txt", "previous_filename": "old/file.txt", "status": "renamed", "additions": 3, "deletions": 2, "changes": 5},
				map[string]any{"filename": "old/deleted.txt", "status": "removed", "additions": 0, "deletions": 10, "changes": 10},
				map[string]any{"filename": "old/binary.png", "status": "modified", "additions": 0, "deletions": 0, "changes": 0},
			}
		}
		json.NewEncoder(w).Encode(commit)
	}))
	defer server.Close()
	m := NewManager(github.NewSnapshotClient(server.Client(), server.URL), 0)
	snapshot := Snapshot{Revision: testSHA, slug: "owner/repo", sessionKey: "fixture"}
	for _, path := range []string{"old", "new"} {
		output, err := m.SnapshotHistory(github.WithCredential(t.Context(), "fixture", "test"), snapshot, path, 20, 1<<20)
		if err != nil || !strings.Contains(output, "new/file.txt (renamed) from old/file.txt | +3 -2 5 changes") || strings.Contains(output, "unrelated") {
			t.Fatalf("output=%s err=%v", output, err)
		}
		if path == "old" && (!strings.Contains(output, "deleted.txt (removed) | +0 -10") || !strings.Contains(output, "binary.png (modified) | +0 -0 0 changes") || strings.Contains(output, "bytes")) {
			t.Fatalf("dishonest file metadata: %s", output)
		}
	}
	if requests != 4 {
		t.Fatalf("detail requests=%d", requests)
	}
}

func TestSnapshotPathspecsAndRenderedCaps(t *testing.T) {
	for _, test := range []struct {
		pattern, name string
		want          bool
	}{
		{"*.go", "internal/file.go", true}, {"internal", "internal/file.go", true}, {":(glob)*.go", "internal/file.go", false},
		{":(glob)**/*.go", "file.go", true}, {":(glob)**/*.go", "internal/file.go", true},
		{":(literal)*.go", "file.go", false}, {":(literal)*.go", "*.go", true},
		{":(icase)README.md", "readme.md", true}, {":/internal", "internal/file.go", true}, {":!internal", "internal/file.go", false},
	} {
		match, err := snapshotPathMatcher(test.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if match(test.name) != test.want {
			t.Fatalf("%s against %s", test.pattern, test.name)
		}
	}
	for limit := 1; limit < 200; limit++ {
		writer := newHeadTailWriter(limit)
		writer.Write([]byte(strings.Repeat("世é界", 100)))
		result := writer.result().text()
		if len(result) > limit || !utf8.ValidString(result) {
			t.Fatalf("cap=%d len=%d invalid=%v", limit, len(result), !utf8.ValidString(result))
		}
	}
}

func TestSnapshotSizeCapCoversAllSessionRepositories(t *testing.T) {
	archive := testArchive(t, map[string][]byte{"file": bytes.Repeat([]byte("x"), 160)})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(archive) }))
	defer server.Close()
	m := NewManager(github.NewSnapshotClient(server.Client(), server.URL), 256)
	m.Root = filepath.Join(t.TempDir(), "snapshots")
	ctx := github.WithCredential(t.Context(), "fixture", "test")
	first, err := m.OpenSnapshot(ctx, "demo", "session", "https://github.com/owner/first", testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.OpenSnapshot(ctx, "demo", "session", "https://github.com/owner/second", testSHA); err == nil || !strings.Contains(err.Error(), "256-byte limit") {
		t.Fatalf("combined session cap err=%v", err)
	}
	if data, readErr := m.ReadSnapshotBlob(ctx, first, "file", 256); readErr != nil || len(data) != 160 {
		t.Fatalf("first repository damaged: %v", readErr)
	}
	if _, err = m.OpenSnapshot(ctx, "demo", "another-session", "https://github.com/owner/second", testSHA); err != nil {
		t.Fatalf("unrelated session charged for first session: %v", err)
	}
}
