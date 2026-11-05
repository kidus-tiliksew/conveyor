// Package mergelineagefixture drives the monitor's GitHub source against a
// real dispatcher and store after a task's pull-request head moved: the
// monitor must lineage a Conveyor merge at a recorded head and keep an
// unapproved or stale-approval merge external (req-260811-228be6 AC-4.1,
// AC-4.3; req-delivery-and-forge AC-3.5; component-submission-merge
// "Observed merges"; component-monitor-drift; DEC-64(5)).
package mergelineagefixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
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
	s.d.ReadCommitChecks = dispatch.NoChecksReportedForTest
	s.d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		if views == 1 {
			return githubtrigger.PullRequest{Number: number, URL: pullURL(), State: "open", Mergeable: "MERGEABLE", BaseSHA: "base", HeadSHA: headB, HeadRef: s.task.Branch}, nil
		}
		return githubtrigger.PullRequest{Number: number, URL: pullURL(), State: "closed", Merged: true, BaseSHA: "base", HeadSHA: headB, HeadRef: s.task.Branch, MergeCommitSHA: landed, MergedBy: "conveyor"}, nil
	}
	s.d.RequestMerge = func(context.Context, string, int) error { merges++; return nil }
	if err := s.d.MergeApprovedTaskAutomatically(s.ctx, s.task); err != nil {
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
	s.expectExternal(t, headC, false, core.TaskApproved)
}

// RunStaleApproval: approval at B goes stale toward C with no refresh
// approval. A merge at C (unrecorded) and a merge at B (recorded, but the
// stale approval is ineligible) both stay external without a state change.
// The head-B case is intentionally an eligibility negative: lineage is true,
// but stale approval keeps the merge external.
func RunStaleApproval(t *testing.T, open func(*testing.T) store.Store) {
	t.Helper()
	for _, merged := range []string{headC, headB} {
		t.Run(merged, func(t *testing.T) {
			s := setup(t, open(t))
			if _, err := s.st.MarkTaskApprovalStale(s.ctx, s.task.ID, headB, headC, config.RefreshReviewDelta, "head-changed"); err != nil {
				t.Fatal(err)
			}
			s.expectExternal(t, merged, merged == headB, core.TaskApproved)
		})
	}
}

func (s scenario) expectExternal(t *testing.T, merged string, lineaged bool, state core.TaskState) {
	t.Helper()
	s.d.RequestMerge = func(context.Context, string, int) error {
		t.Fatal("RequestMerge called")
		return nil
	}
	checks := 0
	failedBefore := countKind(t, s, "merge.failed")
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
	if got := countKind(t, s, "merge.failed"); got != failedBefore {
		t.Fatalf("merge at %s wrote %d merge.failed events (was %d)", merged, got-failedBefore, failedBefore)
	}
	task, err := s.st.GetTask(s.ctx, s.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.st.ListEvents(s.ctx, s.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := monitor.RecordedLineage(task, events, repository, githubSlug, s.task.ID, s.task.Branch, number, merged); got != lineaged {
		t.Fatalf("merge at %s RecordedLineage=%t, want %t", merged, got, lineaged)
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
		WorkspaceID: s.ws, Repository: repository, GitHubSlug: githubSlug, Base: "main",
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

// RunCheckStatus proves the outcomes of the check status read before an
// automatic merge on a real dispatcher and store (req-review-gates-evidence
// AC-1.4 to AC-1.8; component-submission-merge, Check status; DEC-108(5) to
// (7)). Each step's read is scripted, and each assertion reads the ledger and
// the next action the store assembles.
func RunCheckStatus(t *testing.T, open func(*testing.T) store.Store) {
	t.Helper()
	// Each scenario makes its own workspace, so the suite shares one store.
	st := open(t)
	passed := githubtrigger.Check{Name: "build", State: githubtrigger.CheckPassed, URL: "https://github.com/acme/app/runs/1"}
	failed := githubtrigger.Check{Name: "e2e", State: githubtrigger.CheckFailed, URL: "https://github.com/acme/app/runs/2"}
	pending := githubtrigger.Check{Name: "deploy", State: githubtrigger.CheckPending, URL: "https://github.com/acme/app/runs/3"}
	other := githubtrigger.Check{Name: "lint", State: githubtrigger.CheckFailed}
	refused := githubtrigger.PermissionError(fmt.Errorf("resource not accessible by integration"))
	transient := githubtrigger.StatusError(502, fmt.Errorf("bad gateway"))

	t.Run("every check passed merges", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, passed)
		c.automatic(t, nil)
		c.requireMerged(t)
		c.requireRequestedStatus(t, "passed")
		c.requireCount(t, store.EventCheckPending, 0)
		c.requireCount(t, store.EventCheckHold, 0)
	})
	t.Run("none reported merges", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil)
		c.automatic(t, nil)
		c.requireMerged(t)
		c.requireRequestedStatus(t, "none_reported")
	})
	t.Run("failed beats pending and repeats append nothing", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, passed, failed, pending)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.requireApprovedUnmerged(t)
		c.requireCount(t, store.EventCheckHold, 1)
		c.requireCount(t, "merge.failed", 0)
		c.requireCount(t, "merge.requested", 0)
		c.requireAction(t, nextactionOperator, nextactionHold, true, "e2e", "deploy")
	})
	t.Run("pending stays with the factory and records once", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, passed, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		first := c.deadline(t)
		c.advance(time.Hour)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		if got := c.deadline(t); !got.Equal(first) {
			t.Fatalf("a later read moved the deadline from %s to %s", first, got)
		}
		if want := c.start.Add(2 * time.Hour); !first.Equal(want) {
			t.Fatalf("deadline = %s, want %s", first, want)
		}
		c.requireCount(t, store.EventCheckPending, 1)
		c.requireAction(t, "factory", "merging", false, "deploy")
	})
	t.Run("read finding nothing pending clears and pending again records anew", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.mergeRefused = true
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		c.answer(nil, passed)
		if err := c.merge(); err == nil {
			t.Fatal("scripted forge refusal did not reach the caller")
		}
		if got := c.current(t); got.CheckStatusDeadline != nil {
			t.Fatalf("deadline stayed after a read with nothing pending: %v", got.CheckStatusDeadline)
		}
		c.advance(time.Hour)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		if want := c.start.Add(3 * time.Hour); !c.deadline(t).Equal(want) {
			t.Fatalf("deadline = %s, want %s", c.deadline(t), want)
		}
		c.requireCount(t, store.EventCheckPending, 2)
	})
	t.Run("a new head starts fresh", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, failed, pending)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		first := c.deadline(t)
		c.advance(time.Hour)
		if _, err := c.st.MarkTaskApprovalStale(c.ctx, c.task.ID, headB, headC, config.RefreshReviewDelta, "head-changed"); err != nil {
			t.Fatal(err)
		}
		if err := c.st.BindTaskApproval(c.ctx, c.task.ID, headC); err != nil {
			t.Fatal(err)
		}
		c.head = headC
		c.requireAction(t, "factory", "merging", false)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		if got := c.deadline(t); got.Equal(first) || !got.Equal(c.start.Add(3*time.Hour)) {
			t.Fatalf("deadline of the new head = %s", got)
		}
		if got := c.current(t); got.CheckStatusDeadlineHead != headC {
			t.Fatalf("deadline head = %q", got.CheckStatusDeadlineHead)
		}
	})
	t.Run("one rerun passing beside another failure stays held", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, failed, other)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.answer(nil, githubtrigger.Check{Name: "e2e", State: githubtrigger.CheckPassed}, other)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.requireApprovedUnmerged(t)
		c.requireCount(t, store.EventCheckHold, 2)
		c.requireCount(t, store.EventCheckHoldCleared, 0)
		c.requireAction(t, nextactionOperator, nextactionHold, true, "lint")
	})
	t.Run("every check passing after a hold merges and clears", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, failed)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.answer(nil, passed)
		c.automatic(t, nil)
		c.requireMerged(t)
		c.requireCount(t, store.EventCheckHoldCleared, 1)
		c.requireCount(t, store.EventCheckOverride, 0)
	})
	t.Run("a rerun left pending returns a failed hold to the factory", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, failed)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.requireAction(t, nextactionOperator, nextactionHold, true, "e2e")
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		c.requireCount(t, store.EventCheckHoldCleared, 1)
		c.requireAction(t, "factory", "merging", false, "deploy")
	})
	t.Run("a transient failed read records the deadline and clears nothing", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(transient)
		c.automatic(t, dispatch.ErrCheckStatusUnread)
		first := c.deadline(t)
		c.advance(time.Hour)
		c.automatic(t, dispatch.ErrCheckStatusUnread)
		if got := c.deadline(t); !got.Equal(first) {
			t.Fatalf("a later failed read moved the deadline to %s", got)
		}
		c.requireCount(t, store.EventCheckPending, 1)
		c.requireApprovedUnmerged(t)
		c.requireAction(t, "factory", "merging", false)
		c.answer(nil, failed)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.answer(transient)
		c.automatic(t, dispatch.ErrCheckStatusUnread)
		c.requireCount(t, store.EventCheckHoldCleared, 0)
		c.requireAction(t, nextactionOperator, nextactionHold, true, "e2e")
	})
	t.Run("a refused read names the operator and the remedy", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(refused)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.requireCount(t, store.EventCheckHold, 1)
		action := c.requireAction(t, nextactionOperator, nextactionHold, false)
		if !strings.Contains(action.Details.CheckRemedy, "workspace settings") || action.Details.CheckHoldReason != "permission" {
			t.Fatalf("details = %+v", action.Details)
		}
		if c.current(t).CheckStatusDeadline == nil {
			t.Fatal("a refused read recorded no deadline")
		}
		c.answer(nil, passed)
		c.automatic(t, nil)
		c.requireMerged(t)
		c.requireCount(t, store.EventCheckHoldCleared, 1)
	})
	t.Run("the operator act reads nothing and records the override", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, failed, pending)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		reads := c.reads
		if err := c.d.MergeApprovedTask(c.operatorCtx, c.current(t), c.operator); err != nil {
			t.Fatal(err)
		}
		if c.reads != reads {
			t.Fatalf("operator act read check status %d times", c.reads-reads)
		}
		c.requireMerged(t)
		override := c.single(t, store.EventCheckOverride)
		for _, want := range []string{c.operatorID, "e2e", "deploy", headB} {
			if !strings.Contains(string(override.Payload), want) {
				t.Fatalf("override %s does not name %s", override.Payload, want)
			}
		}
		requested := c.single(t, "merge.requested")
		if strings.Contains(string(requested.Payload), "check_status") || !strings.Contains(string(requested.Payload), c.operatorID) {
			t.Fatalf("operator merge.requested = %s", requested.Payload)
		}
	})
	t.Run("the operator act on an unheld head records no override", func(t *testing.T) {
		c := newCheckScenario(t, st)
		if err := c.d.MergeApprovedTask(c.operatorCtx, c.current(t), c.operator); err != nil {
			t.Fatal(err)
		}
		c.requireMerged(t)
		if c.reads != 0 {
			t.Fatalf("operator act read check status %d times", c.reads)
		}
		c.requireCount(t, store.EventCheckOverride, 0)
	})
	t.Run("an act that GitHub refuses leaves the hold and records no override", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, failed)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.mergeRefused = true
		if err := c.d.MergeApprovedTask(c.operatorCtx, c.current(t), c.operator); err == nil {
			t.Fatal("scripted forge refusal did not reach the operator")
		}
		c.requireApprovedUnmerged(t)
		c.requireCount(t, store.EventCheckOverride, 0)
		c.requireAction(t, nextactionOperator, nextactionHold, true, "e2e")
	})
	t.Run("a crash after GitHub merged still records the override", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, failed)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.dieAfterMerge = true
		if err := c.d.MergeApprovedTask(c.operatorCtx, c.current(t), c.operator); err == nil {
			t.Fatal("scripted verification failure did not reach the operator")
		}
		c.requireCount(t, store.EventCheckOverride, 0)
		c.dieAfterMerge, c.merged = false, true
		reads := c.reads
		if err := c.merge(); err != nil {
			t.Fatal(err)
		}
		if c.reads != reads {
			t.Fatal("reconciling a merged pull request read check status")
		}
		c.requireMerged(t)
		if override := c.single(t, store.EventCheckOverride); !strings.Contains(string(override.Payload), c.operatorID) {
			t.Fatalf("override = %s", override.Payload)
		}
	})
	t.Run("a merge on GitHub outside Conveyor records no override", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, failed)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.merged = true
		if err := c.merge(); err != nil {
			t.Fatal(err)
		}
		c.requireMerged(t)
		c.requireCount(t, store.EventCheckOverride, 0)
	})
}

const (
	nextactionOperator = "operator"
	nextactionHold     = "check_hold"
)

// checkScenario scripts a dispatcher's forge and clock around one approved
// gate-off task.
type checkScenario struct {
	scenario
	head          string
	start, now    time.Time
	reads         int
	readErr       error
	checks        []githubtrigger.Check
	merged        bool
	mergeRefused  bool
	dieAfterMerge bool
	operator      core.Actor
	operatorCtx   context.Context
	operatorID    string
	onRead        func(context.Context) error
	onReadCount   func(int)
	prViews       int
	mergeCalls    int
	mergeable     string
}

func newCheckScenario(t *testing.T, st store.Store) *checkScenario {
	t.Helper()
	if _, durable := st.(store.IdentityProvisioner); !durable {
		if backend, ok := st.(store.Backend); ok {
			st = checkIdentityBackend{backend}
		} else {
			st = checkIdentityStore{st}
		}
	}
	c := &checkScenario{scenario: setup(t, st), head: headB, start: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	c.now = c.start
	c.d.Now = func() time.Time { return c.now }
	c.d.ReadCommitChecks = func(ctx context.Context, _, sha string) (githubtrigger.CheckStatus, error) {
		c.reads++
		if c.onReadCount != nil {
			c.onReadCount(c.reads)
		}
		if c.onRead != nil {
			if err := c.onRead(ctx); err != nil {
				return githubtrigger.CheckStatus{}, err
			}
		}
		if sha != c.head {
			t.Errorf("check-status read at %s, want %s", sha, c.head)
		}
		return githubtrigger.CheckStatus{Checks: c.checks}, c.readErr
	}
	c.mergeable = "MERGEABLE"
	c.d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		c.prViews++
		if c.merged {
			return githubtrigger.PullRequest{Number: number, URL: pullURL(), State: "closed", Merged: true, BaseSHA: "base", HeadSHA: c.head, HeadRef: c.task.Branch, MergeCommitSHA: landed, MergedBy: "operator"}, nil
		}
		return githubtrigger.PullRequest{Number: number, URL: pullURL(), State: "open", Mergeable: c.mergeable, BaseSHA: "base", HeadSHA: c.head, HeadRef: c.task.Branch}, nil
	}
	c.d.RequestMerge = func(context.Context, string, int) error {
		c.mergeCalls++
		if c.mergeRefused {
			return githubtrigger.StatusError(405, fmt.Errorf("merge refused"))
		}
		if c.dieAfterMerge {
			return nil
		}
		c.merged = true
		return nil
	}
	c.operatorID = checkOperatorID
	if identities, durable := st.(store.IdentityProvisioner); durable {
		user, err := identities.ProvisionIdentityUser(c.ctx, "operator-"+core.NewTaskID()+"@example.com", "Check Operator")
		if err != nil {
			t.Fatal(err)
		}
		c.operatorID = user.ID
	}
	c.operator = core.Actor{ID: store.UserActorID(c.operatorID), Role: core.ActorUser}
	c.operatorCtx = store.WithActor(c.ctx, c.operator)
	return c
}

const checkOperatorID = "usr-check-operator"

// checkIdentityBackend is checkIdentityStore for the test backend.
type checkIdentityBackend struct{ store.Backend }

func (checkIdentityBackend) GetCallerIdentity(_ context.Context, userID, _ string) (core.CallerIdentity, error) {
	return core.CallerIdentity{ID: userID, Email: "operator@example.com", DisplayName: "Check Operator"}, nil
}

// checkIdentityStore gives the volatile store the one account the operator's
// act resolves; a durable store provisions a real one.
type checkIdentityStore struct{ store.Store }

func (checkIdentityStore) GetCallerIdentity(_ context.Context, userID, _ string) (core.CallerIdentity, error) {
	return core.CallerIdentity{ID: userID, Email: "operator@example.com", DisplayName: "Check Operator"}, nil
}

func (c *checkScenario) answer(err error, checks ...githubtrigger.Check) {
	c.readErr, c.checks = err, checks
}

func (c *checkScenario) advance(by time.Duration) { c.now = c.now.Add(by) }

func (c *checkScenario) merge() error {
	return c.d.MergeApprovedTaskAutomatically(c.ctx, c.task)
}

func (c *checkScenario) current(t *testing.T) core.Task {
	t.Helper()
	task, err := c.st.GetTask(c.ctx, c.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// automatic runs the automatic merge and requires the sentinel it names.
func (c *checkScenario) automatic(t *testing.T, want error) {
	t.Helper()
	err := c.merge()
	if want == nil {
		if err != nil {
			t.Fatalf("automatic merge: %v", err)
		}
		return
	}
	if !errors.Is(err, want) {
		t.Fatalf("automatic merge err = %v, want %v", err, want)
	}
}

func (c *checkScenario) deadline(t *testing.T) time.Time {
	t.Helper()
	task := c.current(t)
	if task.CheckStatusDeadline == nil {
		t.Fatal("no check-status deadline recorded")
	}
	return task.CheckStatusDeadline.UTC()
}

func (c *checkScenario) requireMerged(t *testing.T) {
	t.Helper()
	if got := c.current(t).State; got != core.TaskMerged {
		t.Fatalf("state = %s, want merged", got)
	}
}

func (c *checkScenario) requireApprovedUnmerged(t *testing.T) {
	t.Helper()
	if got := c.current(t).State; got != core.TaskApproved {
		t.Fatalf("state = %s, want approved", got)
	}
	if c.merged {
		t.Fatal("the forge merged a held head")
	}
}

func (c *checkScenario) events(t *testing.T, kind string) []core.Event {
	t.Helper()
	events, err := c.st.ListEvents(c.ctx, c.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []core.Event
	for _, event := range events {
		if event.Kind == kind {
			out = append(out, event)
		}
	}
	return out
}

func (c *checkScenario) requireCount(t *testing.T, kind string, want int) {
	t.Helper()
	if got := len(c.events(t, kind)); got != want {
		t.Fatalf("%s events = %d, want %d", kind, got, want)
	}
}

func (c *checkScenario) single(t *testing.T, kind string) core.Event {
	t.Helper()
	events := c.events(t, kind)
	if len(events) != 1 {
		t.Fatalf("%s events = %d, want 1", kind, len(events))
	}
	return events[0]
}

func (c *checkScenario) requireRequestedStatus(t *testing.T, want string) {
	t.Helper()
	requested := c.single(t, "merge.requested")
	var payload struct {
		CheckStatus string `json:"check_status"`
	}
	if err := json.Unmarshal(requested.Payload, &payload); err != nil || payload.CheckStatus != want {
		t.Fatalf("merge.requested = %s, want check_status %s (%v)", requested.Payload, want, err)
	}
}

func (c *checkScenario) requireAction(t *testing.T, actor, kind string, judgment bool, checks ...string) nextaction.NextAction {
	t.Helper()
	action, err := store.ReadNextAction(c.ctx, c.st, c.task.ID, store.NextActionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if action.Actor != actor || action.Kind != kind || action.Judgment != judgment {
		t.Fatalf("next action = %s/%s judgment=%t, want %s/%s judgment=%t", action.Actor, action.Kind, action.Judgment, actor, kind, judgment)
	}
	named := map[string]bool{}
	for _, check := range action.Details.Checks {
		named[check.Name] = true
	}
	if len(named) != len(checks) {
		t.Fatalf("details.checks = %+v, want %v", action.Details.Checks, checks)
	}
	for _, name := range checks {
		if !named[name] {
			t.Fatalf("details.checks = %+v, want %v", action.Details.Checks, checks)
		}
	}
	return action
}

// RunCheckStatusRetry proves the retry of an unanswered check-status read on a
// real queue runtime (req-review-gates-evidence AC-1.7;
// component-submission-merge invariant 62; component-durable-queue invariant
// 37). Only the job wakes the task: no test here delivers a webhook, starts a
// sweep, runs a timed fallback, or fires a deadline, unless it is proving that
// wake still works. The kind's real delays are recorded and, where a test
// doesn't need to observe a backoff, shortened at the registration.
func RunCheckStatusRetry(t *testing.T, open func(*testing.T) store.Backend) {
	t.Helper()
	st := open(t)
	passed := githubtrigger.Check{Name: "build", State: githubtrigger.CheckPassed}
	failed := githubtrigger.Check{Name: "e2e", State: githubtrigger.CheckFailed}
	pending := githubtrigger.Check{Name: "deploy", State: githubtrigger.CheckPending}
	refused := githubtrigger.PermissionError(fmt.Errorf("resource not accessible by integration"))
	transient := githubtrigger.StatusError(502, fmt.Errorf("bad gateway"))
	limited := &githubtrigger.Error{Category: githubtrigger.ForgeRateLimited, RetryAt: time.Now().Add(time.Hour), Err: fmt.Errorf("rate limited")}

	t.Run("the next read succeeds and the job merges on its first attempt", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.answer(nil, passed)
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.requireMerged(t)
		r.requireFinishedWith(t, logqueue.StateCompleted, 1)
		if r.reads != 2 || len(r.delays) != 0 {
			t.Fatalf("reads=%d delays=%v", r.reads, r.delays)
		}
		r.requireCount(t, "merge.requested", 1)
		r.requireCount(t, "merge.confirmed", 1)
		r.requireCount(t, store.EventCheckPending, 1)
		r.requireCount(t, "dispatch.failed", 0)
		r.requireRequestedStatus(t, "passed")
	})
	t.Run("a failed attempt schedules the next and it merges", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.answerAfter(2, nil, passed)
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.requireMerged(t)
		r.requireFinishedWith(t, logqueue.StateCompleted, 2)
		if r.reads != 3 || !slices.Equal(r.delays, []time.Duration{10 * time.Second}) {
			t.Fatalf("reads=%d delays=%v, want one attempt scheduled at the kind's 10s", r.reads, r.delays)
		}
		r.requireCount(t, "dispatch.failed", 0)
	})
	t.Run("repeated failed reads hold one job", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		if job := r.job(t); job.Generation != 1 || !job.Active() {
			t.Fatalf("job = %+v, want one active generation", job)
		}
		r.requireCount(t, store.EventCheckPending, 1)
	})
	t.Run("the bound completes on the final attempt with no discard, parking, or transition", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		transitions := len(r.events(t, "task.state_changed"))
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.requireFinishedWith(t, logqueue.StateCompleted, queue.DispatchTaskMaxAttempts)
		if r.reads != 1+queue.DispatchTaskMaxAttempts {
			t.Fatalf("reads = %d, want %d", r.reads, 1+queue.DispatchTaskMaxAttempts)
		}
		want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second}
		if !slices.Equal(r.delays, want) {
			t.Fatalf("delays = %v, want %v", r.delays, want)
		}
		r.requireApprovedUnmerged(t)
		r.requireCount(t, "dispatch.failed", 0)
		r.requireCount(t, "merge.failed", 0)
		r.requireCount(t, store.EventCheckPending, 2)
		if got := len(r.events(t, "task.state_changed")); got != transitions {
			t.Fatalf("transitions = %d, want %d", got, transitions)
		}
		deadline := r.deadline(t)
		action := r.requireAction(t, "factory", "merging", false)
		if action.Details.ReasonCode != nextaction.CheckReadRetriesExhausted || !strings.Contains(action.Summary, "retries ended") || action.Details.CheckReadFailure == "" {
			t.Fatalf("next action = %+v", action)
		}
		// A later failed read enqueues the job again, as invariant 62 states, and
		// the task reads as retrying. No deadline moves.
		r.stopRuntime(t, r.running)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		if job := r.job(t); job.Generation != 2 || !job.Active() {
			t.Fatalf("job = %+v, want a second generation", job)
		}
		r.requireCount(t, store.EventCheckPending, 3)
		if got := r.deadline(t); !got.Equal(deadline) {
			t.Fatalf("deadline moved from %s to %s", deadline, got)
		}
		if action := r.requireAction(t, "factory", "merging", false); action.Details.ReasonCode != nextaction.CheckReadFailed {
			t.Fatalf("next action = %+v", action)
		}
		// A delivery still reaches the task, and the queued job then finds nothing.
		r.answer(nil, passed)
		if err := r.d.ApplyChecksDelivery(r.ctx, githubSlug, r.head, nil); err != nil {
			t.Fatal(err)
		}
		r.requireMerged(t)
		reads := r.reads
		r.startRuntime(t)
		r.requireJobFinished(t, 2)
		if r.reads != reads {
			t.Fatalf("a job after the merge read %d times", r.reads-reads)
		}
	})
	t.Run("a sweep after the bound still merges", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.answer(nil, passed)
		if _, err := r.d.ReconcileMergeReadiness(r.ctx); err != nil {
			t.Fatal(err)
		}
		r.requireMerged(t)
	})
	t.Run("another task dispatches while the retry is in backoff", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.realDelays = true
		r.d.Cfg.Routing = config.Routing{Stages: map[string]config.StageRoute{"implement": {Model: "operator", Execution: config.ExecutionMCP, Timeout: time.Hour}}}
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.startRuntime(t)
		backoff := r.waitJob(t, r.task.ID, func(job logqueue.Job) bool { return job.State == logqueue.StateScheduled }, "backoff")
		if delay := backoff.ScheduledAt.Sub(backoff.ClaimedAt); backoff.Attempt != 1 || delay < 10*time.Second || delay > 12*time.Second {
			t.Fatalf("job in backoff = %+v, delay %s, want attempt 1 at the kind's 10s", backoff, delay)
		}
		other := core.Task{ID: "other-" + core.NewTaskID(), Workspace: r.ws, Repo: repository, Title: "Other task", BaseBranch: "main", Branch: "conveyor/other-" + core.NewTaskID(), State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
		if err := r.st.CreateTask(r.ctx, other); err != nil {
			t.Fatal(err)
		}
		args := queue.DispatchTaskArgs{WorkspaceID: r.ws, TaskID: other.ID}
		if _, err := logqueue.Enqueue(r.ctx, r.backend().Log(), r.ws, args.Kind(), other.ID, args, queue.DispatchTaskMaxAttempts, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		r.waitJob(t, other.ID, func(job logqueue.Job) bool { return !job.Active() && job.Generation > 0 }, "the other task's dispatch")
		orders, err := r.st.ListWorkOrdersForTasks(r.ctx, []string{other.ID})
		if err != nil || len(orders) != 1 {
			t.Fatalf("other task's orders = %+v, err = %v", orders, err)
		}
		if job := r.job(t); job.State != logqueue.StateScheduled || job.Attempt != 1 {
			t.Fatalf("job = %+v, want it still in backoff while the other task dispatched", job)
		}
		if r.reads != 2 {
			t.Fatalf("reads = %d, want none during the backoff", r.reads)
		}
	})
	t.Run("a daemon restart during backoff resumes at the scheduled time", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.fixedDelay = 2 * time.Second
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		first := r.startRuntime(t)
		backoff := r.waitJob(t, r.task.ID, func(job logqueue.Job) bool { return job.State == logqueue.StateScheduled }, "backoff")
		r.stopRuntime(t, first)
		r.answer(nil, passed)
		r.d = r.freshDispatcher()
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.requireMerged(t)
		r.requireFinishedWith(t, logqueue.StateCompleted, backoff.Attempt+1)
	})
	t.Run("a job queued across a restart runs on the next runtime", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		if job := r.job(t); job.State != logqueue.StateAvailable {
			t.Fatalf("job = %+v, want it durable and available before any runtime", job)
		}
		r.answer(nil, passed)
		r.d = r.freshDispatcher()
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.requireMerged(t)
	})
	t.Run("a shutdown during a read hands the attempt back and a restart resumes", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		reading := make(chan struct{})
		r.onRead = func(ctx context.Context) error {
			close(reading)
			<-ctx.Done()
			return ctx.Err()
		}
		first := r.startRuntime(t)
		<-reading
		r.stopRuntimeHard(t, first)
		job := r.job(t)
		if job.State == logqueue.StateCompleted || job.State == logqueue.StateDiscarded || job.Attempt != 0 {
			t.Fatalf("job after shutdown = %+v, want the attempt handed back", job)
		}
		r.requireApprovedUnmerged(t)
		r.requireCount(t, "dispatch.failed", 0)
		r.onRead = nil
		r.answer(nil, passed)
		r.d = r.freshDispatcher()
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.requireMerged(t)
	})
	t.Run("a read that answers then fails again opens a new job", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.stopRuntime(t, r.running)
		r.answer(nil, pending)
		r.automatic(t, dispatch.ErrCheckStatusPending)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		if job := r.job(t); job.Generation != 2 || !job.Active() {
			t.Fatalf("job = %+v, want a second generation", job)
		}
	})
	t.Run("a refused read queues no job", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(refused)
		r.automatic(t, dispatch.ErrCheckStatusHeld)
		r.automatic(t, dispatch.ErrCheckStatusHeld)
		if job := r.job(t); job.State != logqueue.StateNone {
			t.Fatalf("job = %+v, want none", job)
		}
		r.requireAction(t, nextactionOperator, nextactionHold, false)
	})
	t.Run("a refusal during the retry ends it", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.answer(refused)
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.requireFinishedWith(t, logqueue.StateCompleted, 1)
		if r.reads != 2 {
			t.Fatalf("reads = %d", r.reads)
		}
		r.requireAction(t, nextactionOperator, nextactionHold, false)
	})
	t.Run("a job firing after a refused read reads nothing", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.answer(refused)
		r.automatic(t, dispatch.ErrCheckStatusHeld)
		reads := r.reads
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		if r.reads != reads {
			t.Fatalf("reads = %d, want none", r.reads-reads)
		}
	})
	t.Run("a rate-limited first read ends the retries at once", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(limited)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		if job := r.job(t); job.State != logqueue.StateNone {
			t.Fatalf("job = %+v, want none", job)
		}
		r.requireCount(t, store.EventCheckPending, 2)
		action := r.requireAction(t, "factory", "merging", false)
		if action.Details.ReasonCode != nextaction.CheckReadRetriesExhausted {
			t.Fatalf("next action = %+v", action)
		}
		// Another rate-limited read neither reopens them nor enqueues.
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.requireCount(t, store.EventCheckPending, 2)
		if job := r.job(t); job.State != logqueue.StateNone {
			t.Fatalf("job = %+v, want none", job)
		}
	})
	t.Run("a rate-limited attempt ends the retries at once", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.answer(limited)
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.requireFinishedWith(t, logqueue.StateCompleted, 1)
		if r.reads != 2 || len(r.delays) != 0 {
			t.Fatalf("reads=%d delays=%v", r.reads, r.delays)
		}
		if action := r.requireAction(t, "factory", "merging", false); action.Details.ReasonCode != nextaction.CheckReadRetriesExhausted {
			t.Fatalf("next action = %+v", action)
		}
	})
	t.Run("a rate-limited read elsewhere ends an active job's retries", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.answer(limited)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		reads := r.reads
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		if r.reads != reads {
			t.Fatalf("reads = %d, want none", r.reads-reads)
		}
	})
	t.Run("a failed check found by the job holds", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.answer(nil, failed)
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.requireFinishedWith(t, logqueue.StateCompleted, 1)
		r.requireAction(t, nextactionOperator, nextactionHold, true, "e2e")
	})
	t.Run("a pending check found by the job stays with the factory", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		first := r.deadline(t)
		r.answer(nil, pending)
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.requireFinishedWith(t, logqueue.StateCompleted, 1)
		action := r.requireAction(t, "factory", "merging", false, "deploy")
		if action.Details.CheckReadFailure != "" || action.Details.ReasonCode != "" {
			t.Fatalf("details = %+v", action.Details)
		}
		r.requireCount(t, store.EventCheckPending, 2)
		if got := r.deadline(t); !got.Equal(first) {
			t.Fatalf("deadline moved from %s to %s", first, got)
		}
	})
	t.Run("a job firing after a later read answered reads nothing", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.answer(nil, pending)
		r.automatic(t, dispatch.ErrCheckStatusPending)
		reads := r.reads
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		if r.reads != reads {
			t.Fatalf("reads = %d, want none", r.reads-reads)
		}
	})
	t.Run("a job firing after the head moved reads nothing", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		reads := r.reads
		if _, err := r.st.MarkTaskApprovalStale(r.ctx, r.task.ID, headB, headC, config.RefreshReviewDelta, "head-changed"); err != nil {
			t.Fatal(err)
		}
		if err := r.st.BindTaskApproval(r.ctx, r.task.ID, headC); err != nil {
			t.Fatal(err)
		}
		r.head = headC
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		if r.reads != reads {
			t.Fatalf("reads = %d, want none", r.reads-reads)
		}
		r.requireApprovedUnmerged(t)
	})
	t.Run("a job firing after the merge calls GitHub for nothing", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		if err := r.d.MergeApprovedTask(r.operatorCtx, r.current(t), r.operator); err != nil {
			t.Fatal(err)
		}
		r.requireMerged(t)
		reads, views := r.reads, r.views
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		if r.reads != reads || r.views != views {
			t.Fatalf("job called GitHub: reads=%d views=%d", r.reads-reads, r.views-views)
		}
		r.requireCount(t, "merge.confirmed", 1)
	})
	t.Run("a job firing after the task closed calls GitHub for nothing", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		if _, err := taskops.New(r.st).Perform(r.ctx, r.task.ID, taskops.Command{Kind: core.TaskCancel, ProjectStages: true}); err != nil {
			t.Fatal(err)
		}
		reads, views := r.reads, r.views
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		if r.reads != reads || r.views != views {
			t.Fatalf("job called GitHub: reads=%d views=%d", r.reads-reads, r.views-views)
		}
	})
	t.Run("a read that takes the task out of approved leaves the job dispatching it", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		// The pull request moved, so the attempt begins a refresh. The transition
		// can't enqueue while this job is active, so the job itself runs the stage.
		// This fixture routes no review, so the real dispatch failure shows the
		// path: it reaches dispatch.failed, which only a non-approved task can.
		r.d.DiscoverDocumentationPolicy = nil
		r.head = headC
		r.realDelays = true
		r.startRuntime(t)
		deadline := time.After(30 * time.Second)
		for len(r.events(t, "dispatch.failed")) == 0 {
			select {
			case <-deadline:
				t.Fatalf("no dispatch.failed; task = %+v, job = %+v", r.current(t), r.job(t))
			case <-time.After(5 * time.Millisecond):
			}
		}
		if got := r.current(t).State; got != core.TaskQueued {
			t.Fatalf("state = %s, want queued after a dispatch failure with attempts left", got)
		}
		r.stopRuntime(t, r.running)
	})
	t.Run("a delivery's failed read arranges the same job", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		// The delivery still reports the failure, so the webhook applier's own
		// retries run. When they run out, the job remains.
		if err := r.d.ApplyChecksDelivery(r.ctx, githubSlug, r.head, nil); !errors.Is(err, dispatch.ErrCheckStatusUnread) {
			t.Fatalf("delivery err = %v", err)
		}
		if job := r.job(t); job.Generation != 1 || !job.Active() {
			t.Fatalf("job = %+v, want one active job", job)
		}
		r.answer(nil, passed)
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.requireMerged(t)
	})
	t.Run("a sweep's failed read arranges the same job", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		if _, err := r.d.ReconcileMergeReadiness(r.ctx); err != nil {
			t.Fatal(err)
		}
		if job := r.job(t); job.Generation != 1 || !job.Active() {
			t.Fatalf("job = %+v, want one active job", job)
		}
		r.answer(nil, passed)
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.requireMerged(t)
	})
}

// retryScenario adds the real queue runtime to a check scenario. By default the
// registration records each delay the kind asks for and schedules it at once, so
// a test runs every attempt without waiting; realDelays and fixedDelay leave the
// job in its backoff to be observed.
type retryScenario struct {
	*checkScenario
	delays     []time.Duration
	onDelay    func()
	realDelays bool
	fixedDelay time.Duration
	views      int
	view       func(context.Context, string, string) (githubtrigger.PullRequest, error)
	shutdown   *dispatch.ShutdownMarker
	running    *logqueue.Runtime
	laterAfter int
	laterErr   error
	later      []githubtrigger.Check
}

func newRetryScenario(t *testing.T, st store.Backend) *retryScenario {
	t.Helper()
	r := &retryScenario{checkScenario: newCheckScenario(t, st), shutdown: &dispatch.ShutdownMarker{}}
	r.instrument(r.d)
	t.Cleanup(func() {
		if r.running != nil {
			r.stopRuntime(t, r.running)
		}
	})
	return r
}

// answerAfter changes the scripted answer once the read count passes reads.
func (r *retryScenario) answerAfter(reads int, err error, checks ...githubtrigger.Check) {
	r.laterAfter, r.laterErr, r.later = reads, err, checks
}

// freshDispatcher builds what a restarted daemon has, over the same forge.
func (r *retryScenario) freshDispatcher() *dispatch.Dispatcher {
	d := dispatch.New(r.st, r.d.Cfg, nil)
	d.Now, d.ReadCommitChecks, d.RequestMerge = r.d.Now, r.d.ReadCommitChecks, r.d.RequestMerge
	r.instrument(d)
	return d
}

func (r *retryScenario) instrument(d *dispatch.Dispatcher) {
	d.DisableMemoryQueueForTest()
	if r.view == nil {
		r.view = d.ViewPullRequest
	}
	d.ViewPullRequest = func(ctx context.Context, repo, branch string) (githubtrigger.PullRequest, error) {
		r.views++
		return r.view(ctx, repo, branch)
	}
	r.onReadCount = func(reads int) {
		if r.laterAfter > 0 && reads > r.laterAfter {
			r.readErr, r.checks = r.laterErr, r.later
		}
	}
}

func (r *retryScenario) backend() store.Backend {
	if identity, wrapped := r.st.(checkIdentityBackend); wrapped {
		return identity.Backend
	}
	return r.st.(store.Backend)
}

func (r *retryScenario) startRuntime(t *testing.T) *logqueue.Runtime {
	t.Helper()
	backend := r.backend()
	runtime := logqueue.NewRuntime(backend.Log(), logqueue.Options{
		Workspaces: []string{r.ws}, RescueStuckAfter: time.Hour, WorkerID: "check-retry", Changes: backend.Changes(), Logf: t.Logf,
	})
	for _, registration := range r.d.Registrations(r.shutdown) {
		if registration.Kind != (queue.DispatchTaskArgs{}).Kind() {
			continue
		}
		kindDelay := registration.RetryDelay
		registration.RetryDelay = func(attempt int) time.Duration {
			delay := kindDelay(attempt)
			r.delays = append(r.delays, delay)
			if r.onDelay != nil {
				r.onDelay()
			}
			switch {
			case r.realDelays:
				return delay
			case r.fixedDelay > 0:
				return r.fixedDelay
			}
			return 0
		}
		runtime.Register(registration)
	}
	if err := runtime.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	r.running = runtime
	return runtime
}

func (r *retryScenario) stopRuntime(t *testing.T, runtime *logqueue.Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := runtime.Stop(ctx); err != nil {
		t.Error(err)
	}
	if r.running == runtime {
		r.running = nil
	}
}

// stopRuntimeHard is the daemon's hard shutdown: the marker first, then the
// cancelled handler context.
func (r *retryScenario) stopRuntimeHard(t *testing.T, runtime *logqueue.Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dispatch.NewMarkedRuntime(runtime, r.shutdown).StopAndCancel(ctx); err != nil {
		t.Error(err)
	}
	r.running = nil
	r.shutdown = &dispatch.ShutdownMarker{}
}

func (r *retryScenario) jobOf(t *testing.T, taskID string) logqueue.Job {
	t.Helper()
	job, err := logqueue.Load(r.ctx, r.backend().Log(), r.ws, logqueue.StreamFor(queue.DispatchTaskArgs{}.Kind(), taskID))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func (r *retryScenario) job(t *testing.T) logqueue.Job {
	t.Helper()
	return r.jobOf(t, r.task.ID)
}

func (r *retryScenario) waitJob(t *testing.T, taskID string, matches func(logqueue.Job) bool, what string) logqueue.Job {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		job := r.jobOf(t, taskID)
		if matches(job) {
			return job
		}
		select {
		case <-deadline:
			t.Fatalf("wait for %s: job = %+v", what, job)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// requireJobFinished waits for the generation's job to leave the active states.
func (r *retryScenario) requireJobFinished(t *testing.T, generation int) {
	t.Helper()
	r.waitJob(t, r.task.ID, func(job logqueue.Job) bool { return job.Generation == generation && !job.Active() }, "the job to finish")
}

func (r *retryScenario) requireFinishedWith(t *testing.T, state logqueue.State, attempt int) {
	t.Helper()
	if job := r.job(t); job.State != state || job.Attempt != attempt {
		t.Fatalf("job = %+v, want state %v at attempt %d", job, state, attempt)
	}
}

// RunCheckStatusDeadline proves the check-status deadline pass on a real
// dispatcher and store (req-review-gates-evidence AC-1.5, AC-1.9;
// req-quiet-idle-runtime AC-3.9; component-submission-merge invariants 60 to
// 62). The clock is the scenario's, each step's read is scripted, and each
// assertion reads the GitHub calls, the ledger, and the assembled next action.
func RunCheckStatusDeadline(t *testing.T, open func(*testing.T) store.Store) {
	t.Helper()
	st := open(t)
	passed := githubtrigger.Check{Name: "build", State: githubtrigger.CheckPassed, URL: "https://github.com/acme/app/runs/1"}
	failed := githubtrigger.Check{Name: "e2e", State: githubtrigger.CheckFailed, URL: "https://github.com/acme/app/runs/2"}
	pending := githubtrigger.Check{Name: "deploy", State: githubtrigger.CheckPending, URL: "https://github.com/acme/app/runs/3"}
	refused := githubtrigger.PermissionError(fmt.Errorf("resource not accessible by integration"))
	transient := githubtrigger.StatusError(502, fmt.Errorf("bad gateway"))

	t.Run("a pass before the deadline reads nothing and a later read does not move it", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		first := c.deadline(t)
		if want := c.start.Add(dispatch.CheckStatusDeadlineWindow); !first.Equal(want) {
			t.Fatalf("deadline = %s, want %s", first, want)
		}
		c.advance(time.Hour)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		if got := c.deadline(t); !got.Equal(first) {
			t.Fatalf("a later read moved the deadline to %s", got)
		}
		reads := c.reads
		c.advance(time.Hour - time.Second)
		c.fireDeadline(t)
		if c.reads != reads || !c.deadline(t).Equal(first) {
			t.Fatalf("reads = %d, want %d; deadline = %s", c.reads, reads, c.deadline(t))
		}
		c.requireAction(t, "factory", "merging", false, "deploy")
	})
	t.Run("a deadline that stays stuck causes exactly one GitHub read", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, passed, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		reads, views := c.reads, c.prViews
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		if c.reads != reads+1 || c.prViews != views || c.mergeCalls != 0 {
			t.Fatalf("reads=%d views=%d merges=%d, want exactly the deadline's read after reads=%d views=%d", c.reads, c.prViews, c.mergeCalls, reads, views)
		}
		c.requireApprovedUnmerged(t)
		c.requireDeadlineFired(t, c.head)
		hold := c.singleHold(t, nextaction.CheckHoldStuck)
		if len(hold.Checks) != 1 || hold.Checks[0] != (nextaction.CheckRef{Name: pending.Name, State: pending.State, URL: pending.URL}) || hold.Failure != "" {
			t.Fatalf("hold = %+v, want the stuck check", hold)
		}
		action := c.requireAction(t, nextactionOperator, nextactionHold, true, "deploy")
		if action.Details.CheckHoldReason != nextaction.CheckHoldStuck || action.Details.Checks[0].URL != pending.URL || len(action.Choices) == 0 || action.Choices[0].ID != "merge" {
			t.Fatalf("next action = %+v", action)
		}
		// The fired deadline stops being due: no later fire reads, however late.
		c.fireDeadline(t)
		c.advance(48 * time.Hour)
		c.fireDeadline(t)
		if c.reads != reads+1 || c.prViews != views {
			t.Fatalf("a later fire read GitHub: reads=%d views=%d", c.reads, c.prViews)
		}
		c.requireCount(t, store.EventCheckHold, 1)
		c.requireCount(t, store.EventCheckPending, 1)
		c.requireDeadlineFired(t, c.head)
	})
	t.Run("a deadline read finding every check passed merges", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		reads := c.reads
		c.answer(nil, passed)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		c.requireMerged(t)
		if c.reads != reads+1 || c.mergeCalls != 1 {
			t.Fatalf("reads=%d merges=%d, want one read after %d and one merge", c.reads, c.mergeCalls, reads)
		}
		c.requireRequestedStatus(t, "passed")
		c.requireCount(t, store.EventCheckHold, 0)
		c.requireCount(t, store.EventCheckOverride, 0)
	})
	t.Run("a deadline read finding none reported merges", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		reads := c.reads
		c.answer(nil)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		c.requireMerged(t)
		c.requireRequestedStatus(t, "none_reported")
		if c.reads != reads+1 {
			t.Fatalf("reads = %d, want %d", c.reads, reads+1)
		}
	})
	t.Run("a failed deadline read is stuck and names the failure and what the last read found pending", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		c.answer(transient)
		c.automatic(t, dispatch.ErrCheckStatusUnread)
		reads, views := c.reads, c.prViews
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		if c.reads != reads+1 || c.prViews != views || c.mergeCalls != 0 {
			t.Fatalf("reads=%d views=%d merges=%d after reads=%d views=%d", c.reads, c.prViews, c.mergeCalls, reads, views)
		}
		c.requireApprovedUnmerged(t)
		c.requireDeadlineFired(t, c.head)
		hold := c.singleHold(t, nextaction.CheckHoldStuck)
		if hold.Failure == "" || !strings.Contains(hold.Failure, "read failed") {
			t.Fatalf("hold = %+v, want the read failure", hold)
		}
		action := c.requireAction(t, nextactionOperator, nextactionHold, true, "deploy")
		if action.Details.CheckReadFailure == "" || !strings.Contains(action.Summary, "could not read") {
			t.Fatalf("next action = %+v", action)
		}
		c.fireDeadline(t)
		if c.reads != reads+1 {
			t.Fatalf("a later fire read GitHub: reads = %d", c.reads)
		}
	})
	t.Run("a failed deadline read with no earlier answer names only the failure", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(transient)
		c.automatic(t, dispatch.ErrCheckStatusUnread)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		if hold := c.singleHold(t, nextaction.CheckHoldStuck); len(hold.Checks) != 0 || hold.Failure == "" {
			t.Fatalf("hold = %+v", hold)
		}
		c.requireAction(t, nextactionOperator, nextactionHold, true)
		c.requireDeadlineFired(t, c.head)
	})
	t.Run("a refused deadline read stays a permission hold", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		reads := c.reads
		c.answer(refused)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		if c.reads != reads+1 {
			t.Fatalf("reads = %d, want %d", c.reads, reads+1)
		}
		c.requireDeadlineFired(t, c.head)
		hold := c.singleHold(t, nextaction.CheckHoldPermission)
		if hold.Remedy != dispatch.CheckReadRemedy {
			t.Fatalf("hold = %+v, want the workspace-settings remedy", hold)
		}
		action := c.requireAction(t, nextactionOperator, nextactionHold, false, "deploy")
		if action.Details.CheckHoldReason != nextaction.CheckHoldPermission || action.Details.CheckRemedy == "" {
			t.Fatalf("next action = %+v", action)
		}
		c.fireDeadline(t)
		if c.reads != reads+1 {
			t.Fatalf("a later fire read GitHub: reads = %d", c.reads)
		}
	})
	t.Run("a deadline falling due on a refused read's hold appends nothing new", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(refused)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		reads := c.reads
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		if c.reads != reads+1 {
			t.Fatalf("reads = %d, want %d", c.reads, reads+1)
		}
		c.singleHold(t, nextaction.CheckHoldPermission)
		c.requireDeadlineFired(t, c.head)
		c.requireAction(t, nextactionOperator, nextactionHold, false)
	})
	t.Run("a failed check at the deadline applies the failed hold and records no new deadline", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		c.answer(nil, failed, pending)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		c.requireApprovedUnmerged(t)
		c.requireDeadlineFired(t, c.head)
		c.singleHold(t, nextaction.CheckHoldFailed)
		c.requireAction(t, nextactionOperator, nextactionHold, true, "e2e", "deploy")
	})
	t.Run("a stuck hold stays through pending and failed reads and records no new deadline", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		c.advance(time.Hour)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.requireDeadlineFired(t, c.head)
		c.singleHold(t, nextaction.CheckHoldStuck)
		c.answer(transient)
		c.automatic(t, dispatch.ErrCheckStatusUnread)
		c.requireDeadlineFired(t, c.head)
		c.singleHold(t, nextaction.CheckHoldStuck)
		c.requireCount(t, store.EventCheckHoldCleared, 0)
		c.requireCount(t, store.EventCheckPending, 1)
		c.requireAction(t, nextactionOperator, nextactionHold, true, "deploy")
	})
	t.Run("a later answered read replaces the failure a stuck hold named", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		c.answer(transient)
		c.automatic(t, dispatch.ErrCheckStatusUnread)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.requireCount(t, store.EventCheckHold, 2)
		if action := c.requireAction(t, nextactionOperator, nextactionHold, true, "deploy"); action.Details.CheckReadFailure != "" || strings.Contains(action.Summary, "could not read") {
			t.Fatalf("next action = %+v, want the answered read", action)
		}
	})
	t.Run("a later read finding no check pending clears a stuck hold and merges", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		c.answer(nil, passed)
		c.automatic(t, nil)
		c.requireMerged(t)
		c.requireCount(t, store.EventCheckHoldCleared, 1)
		c.requireCount(t, store.EventCheckOverride, 0)
	})
	t.Run("a later failed check replaces a stuck hold and a rerun left pending records a new deadline", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		c.answer(nil, failed)
		c.automatic(t, dispatch.ErrCheckStatusHeld)
		c.requireAction(t, nextactionOperator, nextactionHold, true, "e2e")
		c.advance(time.Hour)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		if want := c.now.Add(dispatch.CheckStatusDeadlineWindow); !c.deadline(t).Equal(want) {
			t.Fatalf("deadline = %s, want %s", c.deadline(t), want)
		}
		c.requireAction(t, "factory", "merging", false, "deploy")
	})
	t.Run("a check going pending again after a read cleared the stuck hold records a new deadline", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		c.mergeRefused = true
		c.answer(nil, passed)
		if err := c.merge(); err == nil {
			t.Fatal("scripted forge refusal did not reach the caller")
		}
		c.requireDeadlineFired(t, c.head)
		c.requireCount(t, store.EventCheckHoldCleared, 1)
		c.advance(time.Hour)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		if want := c.now.Add(dispatch.CheckStatusDeadlineWindow); !c.deadline(t).Equal(want) {
			t.Fatalf("deadline = %s, want %s", c.deadline(t), want)
		}
		reads := c.reads
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		if c.reads != reads+1 {
			t.Fatalf("reads = %d, want %d", c.reads, reads+1)
		}
		c.requireCount(t, store.EventCheckHold, 2)
		c.requireAction(t, nextactionOperator, nextactionHold, true, "deploy")
	})
	t.Run("a new head starts fresh and an older head's deadline reads and holds nothing", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		if _, err := c.st.MarkTaskApprovalStale(c.ctx, c.task.ID, headB, headC, config.RefreshReviewDelta, "head-changed"); err != nil {
			t.Fatal(err)
		}
		if err := c.st.BindTaskApproval(c.ctx, c.task.ID, headC); err != nil {
			t.Fatal(err)
		}
		c.head = headC
		reads := c.reads
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		if c.reads != reads {
			t.Fatalf("the older head's deadline read GitHub: reads = %d, want %d", c.reads, reads)
		}
		c.requireDeadlineFired(t, headB)
		c.requireCount(t, store.EventCheckHold, 0)
		c.requireAction(t, "factory", "merging", false)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		if got := c.current(t); got.CheckStatusDeadlineHead != headC || !got.CheckStatusDeadline.Equal(c.now.Add(dispatch.CheckStatusDeadlineWindow)) {
			t.Fatalf("new head deadline = %v at %q", got.CheckStatusDeadline, got.CheckStatusDeadlineHead)
		}
	})
	t.Run("a stale approval's deadline reads nothing", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		if _, err := c.st.MarkTaskApprovalStale(c.ctx, c.task.ID, headB, headC, config.RefreshReviewDelta, "head-changed"); err != nil {
			t.Fatal(err)
		}
		reads := c.reads
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		if c.reads != reads {
			t.Fatalf("reads = %d, want %d", c.reads, reads)
		}
		c.requireCount(t, store.EventCheckHold, 0)
		if got := c.current(t); got.CheckStatusDeadline != nil {
			t.Fatalf("deadline = %v", got.CheckStatusDeadline)
		}
	})
	t.Run("the operator's merge over a stuck hold reads nothing and records the override", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.fireDeadline(t)
		reads := c.reads
		if err := c.d.MergeApprovedTask(c.operatorCtx, c.current(t), c.operator); err != nil {
			t.Fatal(err)
		}
		c.requireMerged(t)
		if c.reads != reads {
			t.Fatalf("the merge act read check status: reads = %d, want %d", c.reads, reads)
		}
		override := c.single(t, store.EventCheckOverride)
		for _, want := range []string{c.operatorID, nextaction.CheckHoldStuck, pending.Name} {
			if !strings.Contains(string(override.Payload), want) {
				t.Fatalf("override = %s, want %s", override.Payload, want)
			}
		}
	})
	t.Run("a merge failure a retry can fix keeps the deadline due", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		first := c.deadline(t)
		c.answer(nil, passed)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.mergeable = "UNKNOWN"
		if err := c.d.ApplyDueCheckStatusDeadlines(c.ctx); !errors.Is(err, dispatch.ErrMergeReadinessPending) {
			t.Fatalf("err = %v, want the alarm to retry", err)
		}
		if got := c.deadline(t); !got.Equal(first) {
			t.Fatalf("deadline = %s, want it still due at %s", got, first)
		}
		c.mergeable = "MERGEABLE"
		c.fireDeadline(t)
		c.requireMerged(t)
		c.requireRequestedStatus(t, "passed")
	})
	t.Run("a merge GitHub refuses ends the deadline with the failure recorded", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		c.answer(nil, passed)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		c.mergeRefused = true
		c.fireDeadline(t)
		c.requireDeadlineFired(t, c.head)
		c.requireCount(t, "merge.failed", 1)
		c.requireCount(t, store.EventCheckHold, 0)
		reads := c.reads
		c.fireDeadline(t)
		if c.reads != reads {
			t.Fatalf("a later fire read GitHub: reads = %d", c.reads)
		}
	})
	t.Run("dying before the deadline clears reads again and duplicates no hold", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		reads := c.reads
		c.advance(dispatch.CheckStatusDeadlineWindow)
		real := c.d.Store
		c.d.Store = clearRefusingStore{real}
		if err := c.d.ApplyDueCheckStatusDeadlines(c.ctx); !errors.Is(err, errClearRefused) {
			t.Fatalf("err = %v", err)
		}
		c.d.Store = real
		c.singleHold(t, nextaction.CheckHoldStuck)
		if got := c.current(t); got.CheckStatusDeadline == nil {
			t.Fatal("the deadline cleared with its clear refused")
		}
		c.fireDeadline(t)
		if c.reads != reads+2 {
			t.Fatalf("reads = %d, want %d", c.reads, reads+2)
		}
		c.singleHold(t, nextaction.CheckHoldStuck)
		c.requireDeadlineFired(t, c.head)
	})
	t.Run("a cancelled read leaves the deadline due and records nothing", func(t *testing.T) {
		c := newCheckScenario(t, st)
		c.answer(nil, pending)
		c.automatic(t, dispatch.ErrCheckStatusPending)
		c.advance(dispatch.CheckStatusDeadlineWindow)
		ctx, cancel := context.WithCancel(c.ctx)
		c.onRead = func(context.Context) error {
			cancel()
			return context.Canceled
		}
		if err := c.d.ApplyDueCheckStatusDeadlines(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		c.onRead = nil
		c.requireCount(t, store.EventCheckHold, 0)
		if got := c.current(t); got.CheckStatusDeadline == nil {
			t.Fatal("a cancelled fire cleared the deadline")
		}
	})
}

// RunCheckStatusDeadlineRetry proves the deadline beside the job retry of an
// unanswered read on the real queue runtime (component-submission-merge
// invariants 61 and 62; component-durable-queue invariant 37). The retry delay
// callback is where the job sits in its backoff, so each deadline fires exactly
// there, and the job's next attempt then finds nothing due.
func RunCheckStatusDeadlineRetry(t *testing.T, open func(*testing.T) store.Backend) {
	t.Helper()
	st := open(t)
	passed := githubtrigger.Check{Name: "build", State: githubtrigger.CheckPassed}
	pending := githubtrigger.Check{Name: "deploy", State: githubtrigger.CheckPending}
	refused := githubtrigger.PermissionError(fmt.Errorf("resource not accessible by integration"))
	transient := githubtrigger.StatusError(502, fmt.Errorf("bad gateway"))

	// fireInBackoff fails the job's first attempt, fires the deadline during its
	// backoff with the read answer sets, and waits for the job to finish.
	fireInBackoff := func(t *testing.T, r *retryScenario, answer func()) {
		t.Helper()
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		fired := false
		r.onDelay = func() {
			if fired {
				return
			}
			fired = true
			answer()
			r.advance(dispatch.CheckStatusDeadlineWindow)
			if err := r.d.ApplyDueCheckStatusDeadlines(r.ctx); err != nil {
				t.Errorf("deadline during the backoff: %v", err)
			}
		}
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		if !fired {
			t.Fatal("the job finished with no backoff")
		}
	}
	// One read arranged the job, one is its first attempt, one is the deadline's.
	const readsThroughDeadline = 3

	t.Run("a deadline read that fails during the backoff is one read and the job reads no more", func(t *testing.T) {
		r := newRetryScenario(t, st)
		fireInBackoff(t, r, func() {})
		r.requireFinishedWith(t, logqueue.StateCompleted, 2)
		if r.reads != readsThroughDeadline {
			t.Fatalf("reads = %d, want %d", r.reads, readsThroughDeadline)
		}
		r.requireApprovedUnmerged(t)
		r.requireDeadlineFired(t, r.head)
		if hold := r.singleHold(t, nextaction.CheckHoldStuck); hold.Failure == "" {
			t.Fatalf("hold = %+v", hold)
		}
		r.requireCount(t, store.EventCheckPending, 1)
		r.requireCount(t, "dispatch.failed", 0)
		action := r.requireAction(t, nextactionOperator, nextactionHold, true)
		if action.Details.CheckReadFailure == "" || action.Details.ReasonCode != nextaction.CheckHoldStuck {
			t.Fatalf("next action = %+v", action)
		}
	})
	t.Run("a deadline read finding a check pending during the backoff holds once", func(t *testing.T) {
		r := newRetryScenario(t, st)
		fireInBackoff(t, r, func() { r.answer(nil, pending) })
		r.requireFinishedWith(t, logqueue.StateCompleted, 2)
		if r.reads != readsThroughDeadline {
			t.Fatalf("reads = %d, want %d", r.reads, readsThroughDeadline)
		}
		if hold := r.singleHold(t, nextaction.CheckHoldStuck); len(hold.Checks) != 1 || hold.Failure != "" {
			t.Fatalf("hold = %+v", hold)
		}
		r.requireAction(t, nextactionOperator, nextactionHold, true, "deploy")
	})
	t.Run("a deadline read that passes during the backoff merges and the job reads no more", func(t *testing.T) {
		r := newRetryScenario(t, st)
		fireInBackoff(t, r, func() { r.answer(nil, passed) })
		if r.reads != readsThroughDeadline {
			t.Fatalf("reads = %d, want %d", r.reads, readsThroughDeadline)
		}
		r.requireMerged(t)
		r.requireCount(t, "merge.requested", 1)
		r.requireRequestedStatus(t, "passed")
		r.requireCount(t, store.EventCheckHold, 0)
	})
	t.Run("a refused deadline read during the backoff stays a permission hold", func(t *testing.T) {
		r := newRetryScenario(t, st)
		fireInBackoff(t, r, func() { r.answer(refused) })
		r.requireFinishedWith(t, logqueue.StateCompleted, 2)
		if r.reads != readsThroughDeadline {
			t.Fatalf("reads = %d, want %d", r.reads, readsThroughDeadline)
		}
		r.singleHold(t, nextaction.CheckHoldPermission)
		r.requireAction(t, nextactionOperator, nextactionHold, false)
	})
	t.Run("a deadline after the retries ended still reads once and arms no job", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		r.startRuntime(t)
		r.requireJobFinished(t, 1)
		r.stopRuntime(t, r.running)
		reads := r.reads
		r.advance(dispatch.CheckStatusDeadlineWindow)
		if err := r.d.ApplyDueCheckStatusDeadlines(r.ctx); err != nil {
			t.Fatal(err)
		}
		if r.reads != reads+1 {
			t.Fatalf("reads = %d, want %d", r.reads, reads+1)
		}
		if job := r.job(t); job.Generation != 1 || job.Active() {
			t.Fatalf("job = %+v, want the deadline to arm none", job)
		}
		r.singleHold(t, nextaction.CheckHoldStuck)
	})
	t.Run("a deadline's failed read arms no job when none is active", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(nil, pending)
		r.automatic(t, dispatch.ErrCheckStatusPending)
		r.answer(transient)
		r.advance(dispatch.CheckStatusDeadlineWindow)
		if err := r.d.ApplyDueCheckStatusDeadlines(r.ctx); err != nil {
			t.Fatal(err)
		}
		if job := r.job(t); job.State != logqueue.StateNone {
			t.Fatalf("job = %+v, want none", job)
		}
		r.singleHold(t, nextaction.CheckHoldStuck)
	})
	t.Run("a later delivery's failed read during a stuck hold arranges no job", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(nil, pending)
		r.automatic(t, dispatch.ErrCheckStatusPending)
		r.advance(dispatch.CheckStatusDeadlineWindow)
		if err := r.d.ApplyDueCheckStatusDeadlines(r.ctx); err != nil {
			t.Fatal(err)
		}
		r.answer(transient)
		r.automatic(t, dispatch.ErrCheckStatusUnread)
		if job := r.job(t); job.State != logqueue.StateNone {
			t.Fatalf("job = %+v, want none", job)
		}
		r.requireDeadlineFired(t, r.head)
		r.singleHold(t, nextaction.CheckHoldStuck)
	})
	t.Run("the alarm applies a recorded deadline on its own", func(t *testing.T) {
		r := newRetryScenario(t, st)
		r.answer(nil, pending)
		r.d.Now = func() time.Time { return time.Now().UTC() }
		runtime := r.startAlarm(t)
		// The event raises the notice that moves the sleeping alarm, as a real
		// read's does, with a deadline the test can wait for.
		deadline := time.Now().UTC().Add(250 * time.Millisecond)
		recorded, err := r.st.RecordCheckStatusDeadline(r.ctx, r.task.ID, r.head, deadline, core.Event{TaskID: r.task.ID, Kind: store.EventCheckPending, Payload: core.JSONPayload(map[string]any{
			"workspace": r.ws, "task_id": r.task.ID, "head_sha": r.head, "reason": "pending", "checks": []githubtrigger.Check{pending}, "deadline": deadline,
		})})
		if err != nil || !recorded {
			t.Fatalf("record=%t err=%v", recorded, err)
		}
		// AC-2.4 gives the alarm five seconds.
		for wait := time.After(5 * time.Second); len(r.events(t, store.EventCheckHold)) == 0; {
			select {
			case <-wait:
				t.Fatalf("no hold within five seconds: reads = %d", r.reads)
			case <-time.After(5 * time.Millisecond):
			}
		}
		r.stopRuntime(t, runtime)
		if r.reads != 1 {
			t.Fatalf("reads = %d, want the deadline's one", r.reads)
		}
		r.singleHold(t, nextaction.CheckHoldStuck)
		r.requireDeadlineFired(t, r.head)
	})
}

// startAlarm starts the runtime with the deadline alarm and its order clock,
// the way the daemon does.
func (r *retryScenario) startAlarm(t *testing.T) *logqueue.Runtime {
	t.Helper()
	backend := r.backend()
	runtime := logqueue.NewRuntime(backend.Log(), logqueue.Options{
		Workspaces: []string{r.ws}, RescueStuckAfter: time.Hour, WorkerID: "check-alarm", Changes: backend.Changes(), Deadlines: dispatch.StoreDeadlines(backend), Logf: t.Logf,
	})
	for _, registration := range r.d.Registrations(r.shutdown) {
		runtime.Register(registration)
	}
	if err := runtime.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	r.running = runtime
	return runtime
}

func (c *checkScenario) fireDeadline(t *testing.T) {
	t.Helper()
	if err := c.d.ApplyDueCheckStatusDeadlines(c.ctx); err != nil {
		t.Fatal(err)
	}
}

// requireDeadlineFired requires what a fired deadline leaves: no deadline, and
// its head kept (component-submission-merge invariant 61).
func (c *checkScenario) requireDeadlineFired(t *testing.T, head string) {
	t.Helper()
	got := c.current(t)
	if got.CheckStatusDeadline != nil || got.CheckStatusDeadlineHead != head {
		t.Fatalf("deadline = %v at %q, want none at %q", got.CheckStatusDeadline, got.CheckStatusDeadlineHead, head)
	}
}

type recordedHold struct {
	Reason  string                `json:"reason"`
	Failure string                `json:"read_failure"`
	Remedy  string                `json:"remedy"`
	Checks  []nextaction.CheckRef `json:"checks"`
}

// singleHold requires exactly one merge.check_hold, with this reason.
func (c *checkScenario) singleHold(t *testing.T, reason string) recordedHold {
	t.Helper()
	var hold recordedHold
	event := c.single(t, store.EventCheckHold)
	if err := json.Unmarshal(event.Payload, &hold); err != nil || hold.Reason != reason {
		t.Fatalf("merge.check_hold = %s, want reason %s (%v)", event.Payload, reason, err)
	}
	return hold
}

var errClearRefused = errors.New("process died before the clear")

// clearRefusingStore is the process that died after the hold and before the
// deadline cleared.
type clearRefusingStore struct{ store.Store }

func (clearRefusingStore) ClearCheckStatusDeadline(context.Context, string) (bool, error) {
	return false, errClearRefused
}
