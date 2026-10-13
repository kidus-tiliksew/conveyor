// Package mergelineagefixture drives the monitor's GitHub source against a
// real dispatcher and store after a task's pull-request head moved: the
// monitor must lineage a Conveyor merge at a recorded head and keep an
// unapproved or stale-approval merge external (req-260811-228be6 AC-4.1,
// AC-4.3; req-delivery-and-forge AC-3.5; component-submission-merge
// "Observed merges"; component-monitor-drift; DEC-64(5)).
package mergelineagefixture

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

const (
	repository = "app"
	githubSlug = "acme/app"
	number     = 12
	headA      = "head-a"
	headB      = "head-b"
	headC      = "head-c"
	landed     = "landed-sha"
)

type scenario struct {
	ctx  context.Context
	st   store.Store
	d    *dispatch.Dispatcher
	task core.Task
	ws   string
}

// RunHeadMoved is the regression: PR #12 opened at head A, approval went
// stale, approval moved to head B, and Conveyor merged at B. The monitor
// sees the landed commit with PR #12 at B and must take the lineaged path.
func RunHeadMoved(t *testing.T, open func(*testing.T) store.Store) {
	t.Helper()
	s := setup(t, open(t))
	views, merges := 0, 0
	s.d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		if views == 1 {
			return githubtrigger.PullRequest{Number: number, URL: pullURL(), State: "open", Mergeable: "MERGEABLE", BaseSHA: "base", HeadSHA: headB, HeadRef: s.task.Branch}, nil
		}
		return githubtrigger.PullRequest{Number: number, URL: pullURL(), State: "closed", Merged: true, BaseSHA: "base", HeadSHA: headB, HeadRef: s.task.Branch, MergeCommitSHA: landed, MergedBy: "conveyor"}, nil
	}
	s.d.RequestMerge = func(context.Context, string, int) error { merges++; return nil }
	if err := s.d.MergeApprovedTask(s.ctx, s.task); err != nil {
		t.Fatal(err)
	}
	if merges != 1 || countKind(t, s, "merge.confirmed") != 1 || taskState(t, s) != core.TaskMerged {
		t.Fatalf("Conveyor merge at B did not complete: merges=%d confirmed=%d state=%s", merges, countKind(t, s, "merge.confirmed"), taskState(t, s))
	}
	reconciledBefore, failedBefore := countKind(t, s, "merge.reconciled"), countKind(t, s, "merge.failed")
	checks := 0
	source := s.source(t, headB, &checks)
	for poll := 1; poll <= 2; poll++ {
		observations, err := source.Observations(s.ctx, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, observation := range observations {
			if observation.Kind == monitor.ExternalPRMerge || observation.Kind == monitor.DirectPush || observation.Kind == monitor.Revert {
				t.Fatalf("poll %d: Conveyor merge after head move was classified %s: %+v", poll, observation.Kind, observation)
			}
		}
		if len(observations) != 1 || !observations[0].RecoveryObserved || observations[0].OccurrenceID != "recovery:commit:"+landed {
			t.Fatalf("poll %d: lineaged path did not run: %+v", poll, observations)
		}
	}
	if checks != 2 {
		t.Fatalf("check-runs requests=%d, want one per poll", checks)
	}
	if got := countKind(t, s, "merge.reconciled"); got != reconciledBefore {
		t.Fatalf("merge.reconciled %d -> %d across polls", reconciledBefore, got)
	}
	if got := countKind(t, s, "merge.failed"); got != failedBefore {
		t.Fatalf("merge.failed %d -> %d across polls", failedBefore, got)
	}
}

// RunUnapprovedHead: approval sits at B, GitHub merges PR #12 at C, which no
// event or approval recorded. The merge stays external.
func RunUnapprovedHead(t *testing.T, open func(*testing.T) store.Store) {
	t.Helper()
	s := setup(t, open(t))
	s.expectExternal(t, headC, core.TaskApproved)
}

// RunStaleApproval: approval at B goes stale toward C with no refresh
// approval. A merge at C (unrecorded) and a merge at B (recorded, but the
// stale approval is ineligible) both stay external without a state change.
func RunStaleApproval(t *testing.T, open func(*testing.T) store.Store) {
	t.Helper()
	for _, merged := range []string{headC, headB} {
		t.Run(merged, func(t *testing.T) {
			s := setup(t, open(t))
			if _, err := s.st.MarkTaskApprovalStale(s.ctx, s.task.ID, headB, headC, config.RefreshReviewDelta, "head-changed"); err != nil {
				t.Fatal(err)
			}
			s.expectExternal(t, merged, core.TaskApproved)
		})
	}
}

func (s scenario) expectExternal(t *testing.T, merged string, state core.TaskState) {
	t.Helper()
	s.d.RequestMerge = func(context.Context, string, int) error {
		t.Fatal("RequestMerge called")
		return nil
	}
	checks := 0
	observations, err := s.source(t, merged, &checks).Observations(s.ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Kind != monitor.ExternalPRMerge || observations[0].OccurrenceID != fmt.Sprintf("pr:%d", number) || checks != 0 {
		t.Fatalf("merge at %s observations=%+v checks=%d, want one external_pr_merge", merged, observations, checks)
	}
	if got := countKind(t, s, "merge.reconciled"); got != 0 {
		t.Fatalf("merge at %s wrote %d merge.reconciled", merged, got)
	}
	if got := taskState(t, s); got != state {
		t.Fatalf("merge at %s moved task to %s", merged, got)
	}
}

// setup records PR #12 opened at A, approval at A gone stale toward B, then
// the refresh approval bound at B (ApprovedHeadSHA=B, ApprovalStale=false).
func setup(t *testing.T, st store.Store) scenario {
	t.Helper()
	root := store.WithActor(t.Context(), store.SystemActor())
	workspace := "merge-lineage-" + core.NewTaskID()
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: repository, Base: "main", GitHub: githubSlug}}}
	if control, durable := st.(store.WorkspaceControlStore); durable {
		if _, err := control.CreateWorkspace(store.WithActor(root, store.Actor{ID: "test", Role: core.ActorHuman}), workspace, workspace, cfg); err != nil {
			t.Fatal(err)
		}
	}
	ctx := store.WithWorkspace(root, workspace)
	id := "lineage-" + core.NewTaskID()
	task := core.Task{
		ID: id, Workspace: workspace, Repo: repository, Title: "Merge lineage " + id,
		BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskApproved, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.BindTaskApproval(ctx, id, headA); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: id, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{
		"repository": githubSlug, "number": number, "head_sha": headA,
	})}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkTaskApprovalStale(ctx, id, headA, headB, config.RefreshReviewDelta, "head-changed"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindTaskApproval(ctx, id, headB); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, id)
	if err != nil || current.ApprovedHeadSHA != headB || current.ApprovalStale || countKindIn(t, st, ctx, id, "approval.stale") != 1 {
		t.Fatalf("setup task=%+v err=%v", current, err)
	}
	return scenario{ctx: ctx, st: st, d: dispatch.New(st, cfg, nil), task: current, ws: workspace}
}

// source mirrors the cmd/conveyord wiring: ReconcileMerged is the
// dispatcher's ReconcileObservedPullRequest.
func (s scenario) source(t *testing.T, mergedHead string, checks *int) monitor.GitHubSource {
	pull := fmt.Sprintf(`{"number":%d,"html_url":%q,"merged_at":"2026-10-13T12:00:00Z","merged_by":{"login":"operator"},"merge_commit_sha":%q,"head":{"ref":%q,"sha":%q}}`,
		number, pullURL(), landed, s.task.Branch, mergedHead)
	return monitor.GitHubSource{
		WorkspaceID: s.ws, Repository: repository, GitHubSlug: githubSlug,
		ResolveTask: func(_ context.Context, headRef string, _ int) (string, bool, error) {
			return s.task.ID, headRef == s.task.Branch, nil
		},
		ReconcileMerged: func(callCtx context.Context, taskID string, pr githubtrigger.PullRequest) (bool, error) {
			return s.d.ReconcileObservedPullRequest(callCtx, repository, githubSlug, taskID, pr)
		},
		Run: func(_ context.Context, args ...string) ([]byte, error) {
			request := strings.Join(args, " ")
			switch {
			case strings.Contains(request, "/commits -f"):
				return fmt.Appendf(nil, `[{"sha":%q,"html_url":"https://github.com/acme/app/commit/%s","parents":[{"sha":"main-before"},{"sha":%q}],"commit":{"message":"merged","committer":{"date":"2026-10-13T12:00:00Z"}}}]`, landed, landed, mergedHead), nil
			case strings.Contains(request, fmt.Sprintf("/pulls/%d", number)):
				return []byte(pull), nil
			case strings.Contains(request, "/pulls"):
				return []byte("[" + pull + "]"), nil
			case strings.Contains(request, "/check-runs"):
				*checks++
				return []byte(`{"check_runs":[{"id":1,"name":"ci","conclusion":"success","run_attempt":1}]}`), nil
			case strings.Contains(request, "compare/main-before..."+landed):
				return []byte(`{"files":[{"filename":"internal/app/main.go"}]}`), nil
			default:
				return nil, fmt.Errorf("unexpected args %v", args)
			}
		},
	}
}

func pullURL() string { return fmt.Sprintf("https://github.com/%s/pull/%d", githubSlug, number) }

func taskState(t *testing.T, s scenario) core.TaskState {
	t.Helper()
	task, err := s.st.GetTask(s.ctx, s.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return task.State
}

func countKind(t *testing.T, s scenario, kind string) int {
	t.Helper()
	return countKindIn(t, s.st, s.ctx, s.task.ID, kind)
}

func countKindIn(t *testing.T, st store.Store, ctx context.Context, taskID, kind string) int {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}
