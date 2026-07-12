package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMarkIssueDispatchedMovesReadyLabel(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	}
	if err := markIssueDispatched(context.Background(), "acme/api", 42, "task-1", run); err != nil {
		t.Fatal(err)
	}

	wantEdit := []string{
		"issue", "edit", "42", "--repo", "acme/api",
		"--remove-label", ReadyLabel, "--add-label", DispatchedLabel,
	}
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3: %v", len(calls), calls)
	}
	if !reflect.DeepEqual(calls[1], wantEdit) {
		t.Fatalf("edit args = %v, want %v", calls[1], wantEdit)
	}
}

func TestMarkIssueDispatchedDoesNotEditWhenLabelSetupFails(t *testing.T) {
	calls := 0
	run := func(_ context.Context, _ ...string) ([]byte, error) {
		calls++
		return nil, errors.New("denied")
	}
	if err := markIssueDispatched(context.Background(), "acme/api", 42, "task-1", run); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestMarkIssueDispatchedIgnoresProvenanceCommentFailure(t *testing.T) {
	calls := 0
	run := func(_ context.Context, _ ...string) ([]byte, error) {
		calls++
		if calls == 3 {
			return nil, errors.New("comments disabled")
		}
		return nil, nil
	}
	if err := markIssueDispatched(context.Background(), "acme/api", 42, "task-1", run); err != nil {
		t.Fatalf("comment failure suppressed dispatch: %v", err)
	}
}

func TestOpenPRReusesExistingPRAfterRedispatch(t *testing.T) {
	var gitCalls, ghCalls [][]string
	runGit := func(_ context.Context, _ string, name string, args ...string) error {
		gitCalls = append(gitCalls, append([]string{name}, args...))
		return nil
	}
	runGH := func(_ context.Context, args ...string) ([]byte, error) {
		ghCalls = append(ghCalls, append([]string(nil), args...))
		return []byte("https://github.com/acme/api/pull/7\n"), nil
	}
	url, err := openPR(context.Background(), "/work", "acme/api", "conveyor/task-1", "main", "title", "body", runGit, runGH)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/acme/api/pull/7" {
		t.Fatalf("url = %q", url)
	}
	if len(gitCalls) != 1 || len(ghCalls) != 1 || ghCalls[0][1] != "list" {
		t.Fatalf("git calls=%v gh calls=%v", gitCalls, ghCalls)
	}
}

func TestOpenPRCreatesWhenTaskBranchHasNoOpenPR(t *testing.T) {
	var calls int
	runGit := func(context.Context, string, string, ...string) error { return nil }
	runGH := func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if args[1] == "list" {
			return nil, nil
		}
		return []byte("https://github.com/acme/api/pull/8\n"), nil
	}
	url, err := openPR(context.Background(), "/work", "acme/api", "conveyor/task-2", "main", "title", "body", runGit, runGH)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/acme/api/pull/8" || calls != 2 {
		t.Fatalf("url=%q calls=%d", url, calls)
	}
}

func TestListReviewFeedbackIncludesReviewBodiesAndInlineComments(t *testing.T) {
	calls := 0
	var apiArgs []string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		switch calls {
		case 1:
			return []byte(`{"number":8,"state":"OPEN","mergedAt":null}`), nil
		case 2:
			if !reflect.DeepEqual(args[len(args)-2:], []string{"-f", "after=review-cursor-1"}) {
				t.Fatalf("GraphQL args lack review cursor: %v", args)
			}
			return []byte(`{"data":{"repository":{"pullRequest":{"reviews":{"nodes":[{"id":"R1","body":"Please add a test.","state":"COMMENTED","submittedAt":"2026-07-12T07:00:01Z","author":{"login":"alice"}},{"id":"R2","body":"ignored","state":"COMMENTED","submittedAt":"2026-07-12T07:00:01Z","author":{"login":"ci[bot]"}}],"pageInfo":{"hasNextPage":false,"endCursor":"review-cursor-2"}}}}}}`), nil
		}
		apiArgs = append([]string(nil), args...)
		return []byte(`[[{"id":91,"body":"Handle nil here.","created_at":"2026-07-12T07:00:02Z","user":{"login":"bob"}},{"id":90,"body":"old inline","created_at":"2026-07-12T06:59:00Z","user":{"login":"eve"}}]]`), nil
	}
	since := time.Date(2026, 7, 12, 7, 0, 0, 0, time.UTC)
	page, err := listReviewFeedback(context.Background(), "acme/api", "conveyor/task-2", ReviewCursor{Since: since, ReviewAfter: "review-cursor-1"}, run)
	if err != nil {
		t.Fatal(err)
	}
	if page.State != "open" || page.Cursor.ReviewAfter != "review-cursor-2" || len(page.Feedback) != 2 || page.Feedback[0].ID != "review:R1" || page.Feedback[1].ID != "comment:91" || page.Feedback[1].PR != 8 {
		t.Fatalf("page=%+v", page)
	}
	if len(apiArgs) < 2 || !strings.Contains(apiArgs[1], "since=2026-07-12T07%3A00%3A00Z") {
		t.Fatalf("inline API args lack cursor: %v", apiArgs)
	}
}

func TestListReviewFeedbackStopsBeforeFetchingMergedPRComments(t *testing.T) {
	calls := 0
	run := func(_ context.Context, _ ...string) ([]byte, error) {
		calls++
		return []byte(`{"number":8,"state":"CLOSED","mergedAt":"2026-07-12T07:00:00Z","reviews":[]}`), nil
	}
	page, err := listReviewFeedback(context.Background(), "acme/api", "conveyor/task-2", ReviewCursor{}, run)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || page.State != "merged" || len(page.Feedback) != 0 {
		t.Fatalf("calls=%d page=%+v", calls, page)
	}
}
