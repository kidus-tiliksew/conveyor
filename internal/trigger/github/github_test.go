package github

import (
	"context"
	"errors"
	"reflect"
	"testing"
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
