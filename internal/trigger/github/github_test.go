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
