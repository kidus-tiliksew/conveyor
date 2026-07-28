package github

import (
	"context"
	"errors"
	"fmt"
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

func TestPublishIssueCreatesOneMarkedIssue(t *testing.T) {
	var calls [][]string
	prepared := false
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "api" {
			return []byte(`[[]]`), nil
		}
		return []byte("https://github.com/acme/api/issues/42\n"), nil
	}
	result, err := publishIssue(context.Background(), IssuePublication{
		Repo: "acme/api", TaskID: "task-1", Title: "Add lifecycle",
		ApprovedSpec: "## Intent\nShip it.\n\n## Acceptance criteria\n- It works.", SpecVersion: 3,
		AllowCreate: true,
		BeforeCreate: func(context.Context) error {
			prepared = true
			return nil
		},
	}, run)
	if err != nil {
		t.Fatal(err)
	}
	if result.Number != 42 || result.Reused || len(calls) != 2 || !prepared {
		t.Fatalf("result=%+v calls=%v", result, calls)
	}
	if got := strings.Join(calls[0], " "); got != "api --paginate --slurp repos/acme/api/issues?state=all&sort=created&direction=asc&per_page=100" {
		t.Fatalf("lookup args=%s", got)
	}
	if got := strings.Join(calls[1], " "); !strings.Contains(got, "<!-- conveyor:task=task-1 -->") || !strings.Contains(got, "Approved spec version: `3`") {
		t.Fatalf("create args=%v", calls[1])
	}
}

func TestPublishIssueRetryFindsMarkerAndUpdatesInsteadOfDuplicating(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch len(calls) {
		case 1:
			return []byte(`[[{"number":42,"url":"https://github.com/acme/api/issues/42","body":"<!-- conveyor:task=task-1 --> old"}]]`), nil
		case 2:
			return []byte(`{"number":42,"url":"https://github.com/acme/api/issues/42","body":"Original context.\n\n<!-- conveyor:task=task-1 --> old"}`), nil
		default:
			return nil, nil
		}
	}
	result, err := publishIssue(context.Background(), IssuePublication{Repo: "acme/api", TaskID: "task-1", Title: "Title", ApprovedSpec: "## Intent\nNew", SpecVersion: 2}, run)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reused || result.Number != 42 || len(calls) != 3 || calls[2][1] != "edit" {
		t.Fatalf("result=%+v calls=%v", result, calls)
	}
	joined := strings.Join(calls[2], " ")
	if !strings.Contains(joined, "Original context.") || strings.Count(joined, "<!-- conveyor:task=task-1 -->") != 1 {
		t.Fatalf("updated body=%s", joined)
	}
}

func TestPublishIssueLostAcknowledgementNeverCreatesTwiceWhileMarkerConverges(t *testing.T) {
	lookupCalls := 0
	createCalls := 0
	prepared := false
	run := func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case args[0] == "api":
			lookupCalls++
			if lookupCalls <= 2 {
				return []byte(`[[]]`), nil
			}
			return []byte(`[[{"number":42,"body":"<!-- conveyor:task=task-1 -->"}]]`), nil
		case args[0] == "issue" && args[1] == "create":
			createCalls++
			return nil, errors.New("connection reset after GitHub accepted create")
		case args[0] == "issue" && args[1] == "view":
			return []byte(`{"number":42,"url":"https://github.com/acme/api/issues/42","body":"<!-- conveyor:task=task-1 -->"}`), nil
		case args[0] == "issue" && args[1] == "edit":
			return nil, nil
		default:
			t.Fatalf("unexpected call: %v", args)
			return nil, nil
		}
	}
	publication := IssuePublication{
		Repo: "acme/api", TaskID: "task-1", Title: "Title", ApprovedSpec: "approved", SpecVersion: 1,
		AllowCreate: true,
		BeforeCreate: func(context.Context) error {
			prepared = true
			return nil
		},
	}
	if _, err := publishIssue(context.Background(), publication, run); err == nil || !prepared {
		t.Fatalf("first publication err=%v prepared=%t", err, prepared)
	}

	publication.AllowCreate = false
	if _, err := publishIssue(context.Background(), publication, run); !errors.Is(err, ErrIssueReconciliationPending) {
		t.Fatalf("first recovery err=%v", err)
	}
	if createCalls != 1 {
		t.Fatalf("create calls after missed recovery lookup=%d, want 1", createCalls)
	}

	result, err := publishIssue(context.Background(), publication, run)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reused || result.Number != 42 || createCalls != 1 {
		t.Fatalf("result=%+v createCalls=%d", result, createCalls)
	}
}

func TestPublishIssueReusesExplicitSourceIssue(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[1] == "view" {
			return []byte(`{"number":7,"url":"https://github.com/acme/api/issues/7","body":"Customer report"}`), nil
		}
		return nil, nil
	}
	result, err := publishIssue(context.Background(), IssuePublication{Repo: "acme/api", TaskID: "task-2", ApprovedSpec: "approved", SpecVersion: 1, SourceIssueNumber: 7}, run)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reused || result.Number != 7 || len(calls) != 2 || calls[0][1] != "view" || calls[1][1] != "edit" {
		t.Fatalf("result=%+v calls=%v", result, calls)
	}
}

func TestPullRequestForBranchReturnsAuthoritativeMergeState(t *testing.T) {
	pr, err := pullRequestForBranch(context.Background(), "acme/api", "conveyor/task-1", func(_ context.Context, args ...string) ([]byte, error) {
		if got := strings.Join(args, " "); got != "pr view conveyor/task-1 --repo acme/api --json number,url,state,mergedAt,mergeable,headRefOid" {
			t.Fatalf("args = %s", got)
		}
		return []byte(`{"number":12,"url":"https://github.com/acme/api/pull/12","state":"CLOSED","mergedAt":"2026-07-15T10:00:00Z","mergeable":"UNKNOWN","headRefOid":"abc123"}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 12 || !pr.Merged || pr.State != "closed" || pr.Mergeable != "UNKNOWN" || pr.HeadSHA != "abc123" {
		t.Fatalf("pull request = %+v", pr)
	}
}

func TestPullRequestForBranchClassifiesMissingPR(t *testing.T) {
	_, err := pullRequestForBranch(context.Background(), "acme/api", "conveyor/missing", func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("no pull requests found for branch conveyor/missing")
	})
	if !errors.Is(err, ErrPullRequestNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestMergePullRequestUsesNormalGitHubMerge(t *testing.T) {
	var got string
	err := mergePullRequest(context.Background(), "acme/api", 12, func(_ context.Context, args ...string) ([]byte, error) {
		got = strings.Join(args, " ")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "pr merge 12 --repo acme/api --merge" {
		t.Fatalf("args = %s", got)
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
	if len(gitCalls) != 1 || len(ghCalls) != 3 || ghCalls[0][1] != "list" || ghCalls[1][1] != "view" || ghCalls[2][1] != "edit" {
		t.Fatalf("git calls=%v gh calls=%v", gitCalls, ghCalls)
	}
}

func TestReconcilePullRequestBodyPreservesHumanContextAndReplacesFactoryBlock(t *testing.T) {
	existing := "Human context.\n\n<!-- conveyor:task-link -->\nConveyor task `old`\n\nCloses #1"
	updated := reconcilePullRequestBody(existing, "<!-- conveyor:task-link -->\nConveyor task `new`\n\nCloses #42")
	if !strings.Contains(updated, "Human context.") || !strings.Contains(updated, "Closes #42") || strings.Contains(updated, "Closes #1") || strings.Count(updated, pullRequestLifecycleMarker) != 1 {
		t.Fatalf("updated=%q", updated)
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

func TestPublishReviewCreatesSuccessfulStatusAndFactoryComment(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch len(calls) {
		case 1:
			return []byte(`{"number":7,"url":"https://github.com/acme/api/pull/7","headRefOid":"abc123"}`), nil
		case 2:
			return []byte(`{"statuses":[]}`), nil
		case 3:
			return []byte(`{"id":41}`), nil
		case 4:
			return []byte(`[[]]`), nil
		default:
			return []byte(`{"id":51}`), nil
		}
	}
	result, err := publishReview(context.Background(), ReviewPublication{
		Repo: "acme/api", Branch: "conveyor/task-1", TaskID: "task-1",
		ReviewWorkOrderID: "review-1", Verdict: "approve", ReasonCode: "approved",
		Summary: "All criteria pass.", ReviewerModel: "gpt", ReviewerSession: "distinct",
		SameModelAsImplementer: "false",
	}, run)
	if err != nil {
		t.Fatal(err)
	}
	if result.CheckRunID != 0 || result.CommentID != 51 || result.ReviewedCommitSHA != "abc123" {
		t.Fatalf("result = %+v", result)
	}
	joined := strings.Join(calls[2], " ")
	if !strings.Contains(joined, "POST repos/acme/api/statuses/abc123") || !strings.Contains(joined, "state=success") || !strings.Contains(joined, "context="+ReviewStatusContext) || !strings.Contains(joined, "target_url=https://github.com/acme/api/pull/7") {
		t.Fatalf("status args = %v", calls[2])
	}
	if !strings.Contains(strings.Join(calls[4], " "), "<!-- conveyor:review-publication task=task-1 -->") {
		t.Fatalf("comment args = %v", calls[4])
	}
}

func TestPublishReviewRetryUpdatesAggregateStatusAndStickyComment(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch len(calls) {
		case 1:
			return []byte(`{"number":7,"headRefOid":"def456"}`), nil
		case 2:
			return []byte(`{"statuses":[{"state":"pending","context":"Conveyor / Code review","description":"Waiting for the remaining independent review verdicts"}]}`), nil
		case 3:
			return []byte(`{"id":42}`), nil
		case 4:
			return []byte(`[[{"id":52,"body":"<!-- conveyor:review-publication task=task-1 --> old"}]]`), nil
		default:
			return []byte(`{"id":52}`), nil
		}
	}
	_, err := publishReview(context.Background(), ReviewPublication{
		Repo: "acme/api", Branch: "conveyor/task-1", TaskID: "task-1",
		ReviewWorkOrderID: "review-2", Verdict: "changes_requested", ReasonCode: "tests",
		Summary: "Needs work.", Feedback: "Add the missing test.", ReviewedCommitSHA: "def456",
		ReviewerModel: "gpt", ReviewerSession: "distinct", SameModelAsImplementer: "true",
		ReviewRound: 2, ReviewSeat: 1, RequiredModel: "gpt", RequiredEffort: "high", ModelEnforcement: "worker-pinned",
		History: []ReviewHistoryItem{
			{WorkOrderID: "review-1", Round: 1, Seat: 1, Verdict: "changes_requested", ReasonCode: "tests", Feedback: "Add the missing test.", ResolutionState: "resolved"},
			{WorkOrderID: "review-2", Round: 2, Seat: 1, Verdict: "changes_requested", ReasonCode: "tests", Feedback: "Add another test.", ResolutionState: "unresolved"},
		},
		BounceHistory: []string{"bounce 1: tests"},
		StatusState:   "failure",
	}, run)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls[2], " "); !strings.Contains(got, "POST repos/acme/api/statuses/def456") || !strings.Contains(got, "state=failure") {
		t.Fatalf("status retry args = %v", calls[2])
	}
	if got := strings.Join(calls[4], " "); !strings.Contains(got, "PATCH repos/acme/api/issues/comments/52") || !strings.Contains(got, "bounce 1: tests") || !strings.Contains(got, "resolved") || !strings.Contains(got, "unresolved") || !strings.Contains(got, "round 2, seat 1") || !strings.Contains(got, "Required effort: `high`") {
		t.Fatalf("comment retry args = %v", calls[4])
	}
}

func TestPublishReviewLaterApprovalResolvesHistoryInSameComment(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch len(calls) {
		case 1, 6:
			return []byte(`{"number":7,"url":"https://github.com/acme/api/pull/7","headRefOid":"abc123"}`), nil
		case 2:
			return []byte(`{"statuses":[]}`), nil
		case 3, 8:
			return []byte(`{"id":41}`), nil
		case 4:
			return []byte(`[[]]`), nil
		case 5:
			return []byte(`{"id":52}`), nil
		case 7:
			return []byte(`{"statuses":[{"state":"failure","context":"Conveyor / Code review","description":"Independent review requested changes","target_url":"https://github.com/acme/api/pull/7"}]}`), nil
		case 9:
			return []byte(`[[{"id":52,"body":"<!-- conveyor:review-publication task=task-1 --> unresolved"}]]`), nil
		case 10:
			return []byte(`{"id":52}`), nil
		default:
			return nil, fmt.Errorf("unexpected call: %v", args)
		}
	}
	firstHistory := []ReviewHistoryItem{{
		WorkOrderID: "review-1", Round: 1, Seat: 1, Verdict: "changes_requested",
		ReasonCode: "tests", Feedback: "Add coverage.", ResolutionState: "unresolved",
	}}
	first, err := publishReview(context.Background(), ReviewPublication{
		Repo: "acme/api", Branch: "conveyor/task-1", TaskID: "task-1",
		ReviewWorkOrderID: "review-1", Verdict: "changes_requested", ReasonCode: "tests",
		Feedback: "Add coverage.", ReviewRound: 1, ReviewSeat: 1, History: firstHistory,
		StatusState: "failure",
	}, run)
	if err != nil || first.CommentID != 52 {
		t.Fatalf("requested-changes result=%+v err=%v", first, err)
	}
	if got := strings.Join(calls[4], " "); !strings.Contains(got, "unresolved") {
		t.Fatalf("first comment did not render unresolved history: %v", calls[4])
	}

	second, err := publishReview(context.Background(), ReviewPublication{
		Repo: "acme/api", Branch: "conveyor/task-1", TaskID: "task-1",
		ReviewWorkOrderID: "review-2", Verdict: "approve", ReasonCode: "approved",
		Summary: "All criteria pass.", ReviewRound: 2, ReviewSeat: 1,
		History: []ReviewHistoryItem{
			{WorkOrderID: "review-1", Round: 1, Seat: 1, Verdict: "changes_requested", ReasonCode: "tests", Feedback: "Add coverage.", ResolutionState: "resolved"},
			{WorkOrderID: "review-2", Round: 2, Seat: 1, Verdict: "approve", ReasonCode: "approved", ResolutionState: "accepted"},
		},
		StatusState: "success",
	}, run)
	if err != nil || second.CommentID != 52 {
		t.Fatalf("approval result=%+v err=%v", second, err)
	}
	if got := strings.Join(calls[9], " "); !strings.Contains(got, "PATCH repos/acme/api/issues/comments/52") ||
		!strings.Contains(got, "resolved") || !strings.Contains(got, "review-2") {
		t.Fatalf("later approval did not update the existing resolved history: %v", calls[9])
	}
}

func TestPublishReviewDoesNotDuplicateMatchingCommitStatus(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch len(calls) {
		case 1:
			return []byte(`{"number":7,"url":"https://github.com/acme/api/pull/7","headRefOid":"abc123"}`), nil
		case 2:
			return []byte(`{"statuses":[{"state":"pending","context":"Conveyor / Code review","description":"Waiting for the remaining independent review verdicts","target_url":"https://github.com/acme/api/pull/7"}]}`), nil
		case 3:
			return []byte(`[[{"id":51,"body":"<!-- conveyor:review-publication task=task-1 --> old"}]]`), nil
		case 4:
			return []byte(`{"id":51}`), nil
		default:
			return nil, fmt.Errorf("unexpected call: %v", args)
		}
	}
	_, err := publishReview(context.Background(), ReviewPublication{
		Repo: "acme/api", Branch: "conveyor/task-1", TaskID: "task-1",
		ReviewWorkOrderID: "review-1", Verdict: "approve", StatusState: "pending",
	}, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 || !strings.Contains(strings.Join(calls[3], " "), "PATCH repos/acme/api/issues/comments/51") {
		t.Fatalf("matching status did not preserve aggregate comment upsert: %v", calls)
	}
}

func TestFactoryMarkedFeedbackIsNeverHuman(t *testing.T) {
	if humanFeedback("conveyor-service", "<!-- conveyor:review-publication task=task-1 -->\nresult") {
		t.Fatal("factory-marked feedback was accepted as human")
	}
	if !humanFeedback("alice", "Please address this.") {
		t.Fatal("human feedback was suppressed")
	}
}

func TestReviewPublicationBodyOmitsUnsetLegacyEffort(t *testing.T) {
	body := reviewPublicationBody(ReviewPublication{TaskID: "task-1", ReviewWorkOrderID: "review-1", Verdict: "approve", ReviewRound: 1, ReviewSeat: 1, RequiredModel: "gpt", ModelEnforcement: "worker-pinned"})
	if strings.Contains(body, "Required effort") {
		t.Fatalf("legacy publication invented effort: %s", body)
	}
}
