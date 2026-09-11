package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const snapshotTestSHA = "0123456789012345678901234567890123456789"

func snapshotTestFile(name, status, previous string) map[string]any {
	return map[string]any{"filename": name, "previous_filename": previous, "status": status, "additions": 3, "deletions": 2, "changes": 5}
}
func TestSnapshotCommitDetailCompleteness(t *testing.T) {
	for _, mode := range []string{"small", "second-page", "renamed-deleted-binary", "sha", "duplicate", "malformed-file", "missing-count", "null-files", "cycle", "other-commit", "other-host", "duplicate-next", "malformed-link", "fetch", "page-bytes", "total-bytes", "thirty-pages", "exact-3000"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Header.Get("Authorization") != "Bearer fixture" {
					t.Error("missing auth")
				}
				if r.URL.Query().Get("per_page") != "100" {
					t.Error("unbounded page")
				}
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				sha := snapshotTestSHA
				files := []any{snapshotTestFile(fmt.Sprintf("file-%d", page), "modified", "")}
				next := ""
				extra := ""
				switch mode {
				case "second-page":
					if page == 1 {
						next = "?per_page=100&page=2"
					} else {
						files = []any{snapshotTestFile("target.txt", "modified", "")}
					}
				case "renamed-deleted-binary":
					files = []any{snapshotTestFile("new.txt", "renamed", "old.txt"), snapshotTestFile("deleted", "removed", ""), snapshotTestFile("binary.png", "modified", "")}
				case "sha":
					sha = strings.Repeat("f", 40)
				case "duplicate":
					files = append(files, files[0])
				case "malformed-file":
					files = []any{snapshotTestFile("../escape", "modified", "")}
				case "missing-count":
					f := snapshotTestFile("file", "modified", "")
					delete(f, "additions")
					files = []any{f}
				case "null-files":
					files = nil
				case "cycle":
					next = "?per_page=100&page=1"
				case "other-commit":
					next = "/repos/owner/repo/commits/" + strings.Repeat("f", 40) + "?per_page=100&page=2"
				case "other-host":
					next = "https://attacker.invalid/repos/owner/repo/commits/" + sha + "?per_page=100&page=2"
				case "duplicate-next":
					w.Header().Set("Link", "<?per_page=100&page=2>; rel=\"next\", <?per_page=100&page=2>; rel=\"next\"")
				case "malformed-link":
					w.Header().Set("Link", "bad")
				case "fetch":
					if page == 1 {
						next = "?per_page=100&page=2"
					} else {
						http.Error(w, "failure", 502)
						return
					}
				case "page-bytes":
					extra = strings.Repeat("x", 1<<20)
				case "total-bytes":
					extra = strings.Repeat("x", 950000)
					next = fmt.Sprintf("?per_page=100&page=%d", page+1)
				case "thirty-pages":
					next = fmt.Sprintf("?per_page=100&page=%d", page+1)
				case "exact-3000":
					files = nil
					for i := 0; i < 100; i++ {
						files = append(files, snapshotTestFile(fmt.Sprintf("file-%d-%d", page, i), "modified", ""))
					}
					if page < 30 {
						next = fmt.Sprintf("?per_page=100&page=%d", page+1)
					}
				}
				if next != "" {
					w.Header().Set("Link", "<"+next+">; rel=\"next\"")
				}
				json.NewEncoder(w).Encode(map[string]any{"sha": sha, "files": files, "unused": extra})
			}))
			defer server.Close()
			client := NewSnapshotClient(server.Client(), server.URL)
			detail, err := client.CommitDetail(WithCredential(t.Context(), "fixture", "test"), "owner/repo", snapshotTestSHA)
			success := mode == "small" || mode == "second-page" || mode == "renamed-deleted-binary"
			if success && err != nil {
				t.Fatal(err)
			}
			if !success && (err == nil || (!strings.Contains(err.Error(), "completeness") && !strings.Contains(err.Error(), "limit"))) {
				t.Fatalf("expected explicit incomplete result, got %+v err=%v", detail, err)
			}
			if mode == "second-page" && (requests != 2 || len(detail.Files) != 2 || detail.Files[1].Filename != "target.txt") {
				t.Fatalf("detail=%+v requests=%d", detail, requests)
			}
			if mode == "exact-3000" && (requests != 30 || !strings.Contains(err.Error(), "3000")) {
				t.Fatalf("requests=%d err=%v", requests, err)
			}
			if mode == "total-bytes" && requests != 9 {
				t.Fatalf("total bound requests=%d", requests)
			}
			if requests > 30 {
				t.Fatalf("unbounded requests=%d", requests)
			}
		})
	}
}
func TestSnapshotRejectsRedirectCredentialLeaks(t *testing.T) {
	var leaked bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true; t.Error("unsafe origin was contacted") }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer source.Close()
	client := NewSnapshotClient(source.Client(), source.URL)
	_, err := client.OpenArchive(WithCredential(t.Context(), "fixture", "test"), "owner/repo", snapshotTestSHA)
	if err == nil || leaked {
		t.Fatalf("redirect err=%v leak=%v", err, leaked)
	}
}

type snapshotRoundTrip func(*http.Request) (*http.Response, error)

func (f snapshotRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSnapshotCodeloadDropsAuthorization(t *testing.T) {
	requests := 0
	transport := snapshotRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		h := make(http.Header)
		body := "archive"
		status := 200
		if r.URL.Host == "api.github.com" {
			if r.Header.Get("Authorization") != "Bearer fixture" {
				t.Error("missing API auth")
			}
			h.Set("Location", "https://codeload.github.com/owner/repo/legacy.tar.gz/"+snapshotTestSHA)
			status = 302
			body = ""
		} else if r.URL.Host != "codeload.github.com" || r.Header.Get("Authorization") != "" {
			t.Error("unsafe codeload auth")
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	client := NewSnapshotClient(&http.Client{Transport: transport}, "")
	archive, err := client.OpenArchive(WithCredential(context.Background(), "fixture", "test"), "owner/repo", snapshotTestSHA)
	if err != nil {
		t.Fatal(err)
	}
	archive.Close()
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
}
func TestSnapshotPinAndHistoryValidation(t *testing.T) {
	for _, mode := range []string{"no-content", "missing-app", "uncovered", "malformed", "history-overflow"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if mode == "uncovered" {
					http.Error(w, "missing", 404)
					return
				}
				if mode == "malformed" {
					io.WriteString(w, `{"sha":"main"}`)
					return
				}
				if mode == "history-overflow" {
					json.NewEncoder(w).Encode([]any{map[string]string{"sha": snapshotTestSHA}, map[string]string{"sha": snapshotTestSHA}})
					return
				}
				if r.URL.Path != "/repos/owner/repo/commits/main" {
					t.Errorf("pin fetched content: %s", r.URL.Path)
				}
				json.NewEncoder(w).Encode(map[string]string{"sha": snapshotTestSHA})
			}))
			defer server.Close()
			ctx := t.Context()
			if mode != "missing-app" {
				ctx = WithCredential(ctx, "fixture", "test")
			}
			client := NewSnapshotClient(server.Client(), server.URL)
			if mode == "history-overflow" {
				_, err := client.History(ctx, "owner/repo", snapshotTestSHA, "file", 1)
				if err == nil {
					t.Fatal("overflow accepted")
				}
				return
			}
			sha, err := client.ResolveCommit(ctx, "owner/repo", "main")
			if mode == "no-content" {
				if err != nil || sha != snapshotTestSHA || requests != 1 {
					t.Fatalf("sha=%s requests=%d err=%v", sha, requests, err)
				}
				return
			}
			if err == nil {
				t.Fatal("missing error")
			}
			if mode == "missing-app" && requests != 0 {
				t.Fatal("request without credential")
			}
			if (mode == "missing-app" || mode == "uncovered") && (ErrorCategory(err) != ForgePermission || !strings.Contains(err.Error(), "owner/repo") || !strings.Contains(err.Error(), "workspace settings")) {
				t.Fatalf("permission error=%v", err)
			}
		})
	}
}
