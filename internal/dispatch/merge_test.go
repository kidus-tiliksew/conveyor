package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type failingConflictFixStore struct {
	store.Store
	calls int
}

func (s *failingConflictFixStore) CreateConflictFixCommand(context.Context, taskops.TaskLease, store.ConflictFixRequest) (store.ConflictFixResult, error) {
	s.calls++
	return store.ConflictFixResult{}, errors.New("forced conflict-fix order creation failure")
}

func approvedMergeFixture(t *testing.T, githubRepo string) (context.Context, store.Store, core.Task, *Dispatcher) {
	return approvedMergeFixtureWithScope(t, githubRepo, config.RefreshReviewDelta)
}

func approvedMergeFixtureWithScope(t *testing.T, githubRepo, scope string) (context.Context, store.Store, core.Task, *Dispatcher) {
	return approvedMergeFixtureWithScopeAndGate(t, githubRepo, scope, false)
}

func approvedMergeFixtureWithScopeAndGate(t *testing.T, githubRepo, scope string, mergeApproval bool) (context.Context, store.Store, core.Task, *Dispatcher) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
	st := store.NewMemory()
	task := core.Task{ID: "merge-task", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/merge-task", State: core.TaskApproved, MergeApproval: mergeApproval, SetupContract: config.ExecutionSetup{RefreshReview: scope}, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: githubRepo}}}, nil)
	if mergeApproval {
		d.Store = mergeIdentityStore{st}
	}
	return ctx, st, task, d
}

func TestMergeApprovedTaskMergesOnlyAfterAuthoritativeConfirmation(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	views, merges := 0, 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		if views == 1 {
			return githubtrigger.PullRequest{Number: 12, URL: "https://github.com/acme/app/pull/12", State: "open", Mergeable: "MERGEABLE", BaseSHA: "base-sha", HeadSHA: "head-sha"}, nil
		}
		return githubtrigger.PullRequest{Number: 12, URL: "https://github.com/acme/app/pull/12", State: "closed", Merged: true, BaseSHA: "base-sha", HeadSHA: "head-sha"}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error { merges++; return nil }

	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged || merges != 1 || views != 2 {
		t.Fatalf("task=%+v merges=%d views=%d err=%v", current, merges, views, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var requested, confirmed int
	for _, event := range events {
		requested += boolInt(event.Kind == "merge.requested")
		confirmed += boolInt(event.Kind == "merge.confirmed")
		if event.Kind == "merge.requested" || event.Kind == "merge.confirmed" {
			var fields map[string]any
			if err = json.Unmarshal(event.Payload, &fields); err != nil {
				t.Fatal(err)
			}
			if _, exists := fields["forge_author_user_id"]; exists {
				t.Fatalf("workspace merge event carries a user ID: %s", event.Payload)
			}
			var author struct {
				Class  core.ForgeAuthorClass `json:"forge_author_class"`
				UserID string                `json:"forge_author_user_id"`
			}
			if err = json.Unmarshal(event.Payload, &author); err != nil || author.Class != core.ForgeAuthorWorkspace || author.UserID != "" {
				t.Fatalf("automatic merge author=%+v err=%v", author, err)
			}
		}
	}
	if requested != 1 || confirmed != 1 {
		t.Fatalf("events=%+v", events)
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil || len(links) != 1 || links[0].Kind != "merged_range" || links[0].DstID != core.CommitRangeLineageID("acme/app", "base-sha", "head-sha") {
		t.Fatalf("merge lineage=%+v err=%v", links, err)
	}
	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if merges != 1 || views != 2 {
		t.Fatalf("idempotent retry merges=%d views=%d", merges, views)
	}
}

func TestMergeApprovedTaskUsesApprovingOperatorCredentialAndAuditsIdentity(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewDelta, true)
	if err := st.CreateIntervention(ctx, core.Intervention{TaskID: task.ID, Action: core.InterventionApprove, ActorID: store.UserActorID("usr-approver"), ActorRole: core.ActorUser}); err != nil {
		t.Fatal(err)
	}
	views := 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		return githubtrigger.PullRequest{Number: 12, URL: "https://github.com/acme/app/pull/12", State: map[bool]string{false: "open", true: "closed"}[views > 1], Mergeable: "MERGEABLE", Merged: views > 1, BaseSHA: "base", HeadSHA: "head"}, nil
	}
	var usedToken string
	d.RequestMerge = func(_ context.Context, repo string, number int) error {
		token := "workspace-installation-token"
		if repo != "acme/app" || number != 12 {
			t.Fatalf("merge target=%s#%d", repo, number)
		}
		usedToken = token
		return nil
	}
	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if usedToken != "workspace-installation-token" {
		t.Fatalf("used token=%q", usedToken)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"merge.requested", "merge.confirmed"} {
		found := false
		for _, event := range events {
			found = found || (event.Kind == kind && strings.Contains(string(event.Payload), `"forge_author_class":"workspace"`) && strings.Contains(string(event.Payload), `"forge_author_user_id":"usr-approver"`) && !strings.Contains(string(event.Payload), "workspace-installation-token"))
		}
		if !found {
			t.Fatalf("%s attribution missing or unsafe: %+v", kind, events)
		}
	}
}

func TestMergeApprovedTaskVerificationFailuresAuditApprovingOperatorIdentity(t *testing.T) {
	for _, test := range []struct {
		name       string
		reasonCode string
		verify     func() (githubtrigger.PullRequest, error)
	}{
		{
			name:       "verification read fails",
			reasonCode: "merge_verification_failed",
			verify: func() (githubtrigger.PullRequest, error) {
				return githubtrigger.PullRequest{}, errors.New("verification unavailable")
			},
		},
		{
			name:       "merge remains unconfirmed",
			reasonCode: "merge_unconfirmed",
			verify: func() (githubtrigger.PullRequest, error) {
				return githubtrigger.PullRequest{Number: 12, State: "open", Merged: false}, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewDelta, true)
			if err := st.CreateIntervention(ctx, core.Intervention{TaskID: task.ID, Action: core.InterventionApprove, ActorID: store.UserActorID("usr-approver"), ActorRole: core.ActorUser}); err != nil {
				t.Fatal(err)
			}
			views := 0
			d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				views++
				if views == 1 {
					return githubtrigger.PullRequest{Number: 12, URL: "https://github.com/acme/app/pull/12", State: "open", Mergeable: "MERGEABLE"}, nil
				}
				return test.verify()
			}
			d.RequestMerge = func(_ context.Context, _ string, _ int) error {
				token := "workspace-installation-token"
				if token != "workspace-installation-token" {
					t.Fatalf("merge token = %q", token)
				}
				return nil
			}

			if err := d.MergeApprovedTask(ctx, task); err == nil {
				t.Fatal("expected merge verification failure")
			}
			events, err := st.ListEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range events {
				payload := string(event.Payload)
				if event.Kind == "merge.failed" && strings.Contains(payload, `"reason_code":"`+test.reasonCode+`"`) {
					found = strings.Contains(payload, `"forge_author_class":"workspace"`) &&
						strings.Contains(payload, `"forge_author_user_id":"usr-approver"`) &&
						!strings.Contains(payload, "workspace-installation-token")
				}
			}
			if !found {
				t.Fatalf("merge.failed attribution missing or unsafe: %+v", events)
			}
		})
	}
}

func TestMergeAuthorUsesOperatorIdentityWithoutStoredToken(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewDelta, true)
	if err := st.CreateIntervention(ctx, core.Intervention{TaskID: task.ID, Action: core.InterventionApprove, ActorID: store.UserActorID("usr-approver"), ActorRole: core.ActorUser}); err != nil {
		t.Fatal(err)
	}
	author, message, err := d.mergeAuthor(ctx, task)
	if err != nil || author.Class != core.ForgeAuthorWorkspace || author.UserID != "usr-approver" || message != "Approved-by: Approving Operator <approver@example.com>" {
		t.Fatalf("identity=%+v message=%q error=%v", author, message, err)
	}
}

func TestMergeConfirmedProducesDesignDriftFromAuthoritativePRFiles(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	content := "# Dispatch design\n\n```conveyor:governs\n- repo: app\n  paths:\n    - internal/dispatch/**\n```"
	document, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "DESIGN-dispatch", Title: "Dispatch", Category: "Architecture"}, core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	service := &monitor.Service{Store: st.(monitor.Store), WorkspaceID: "test", Enabled: true, Repositories: map[string]struct{}{"app": {}}}
	d.ObserveDesignMerge = func(ctx context.Context, observation monitor.Observation, taskID string) error {
		_, observeErr := service.ProcessDesignMerge(ctx, observation, taskID)
		return observeErr
	}
	d.ListPullRequestFiles = func(_ context.Context, repo string, number int) ([]string, error) {
		if repo != "acme/app" || number != 12 {
			t.Fatalf("file request repo=%s number=%d", repo, number)
		}
		return []string{"internal/dispatch/dispatch.go"}, nil
	}
	views := 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		return githubtrigger.PullRequest{Number: 12, URL: "https://github.com/acme/app/pull/12", State: map[bool]string{true: "closed", false: "open"}[views > 1], Mergeable: "MERGEABLE", Merged: views > 1, BaseSHA: "landed-base", HeadSHA: "reviewed-pr-head"}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error { return nil }
	if err = d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Drift) != 1 || status.Drift[0].SystemDesignID != document.ID || status.Drift[0].CommitSHA != "reviewed-pr-head" || status.Drift[0].CausalEventID == 0 {
		t.Fatalf("production design drift=%+v", status.Drift)
	}
	if len(status.Observations) != 1 || status.Observations[0].Kind != monitor.LineagedMerge || status.Observations[0].OccurrenceID != "pr:12" || status.Observations[0].CommitSHA != "reviewed-pr-head" || !reflect.DeepEqual(status.Observations[0].ChangedPaths, []string{"internal/dispatch/dispatch.go"}) {
		t.Fatalf("merge observation=%+v", status.Observations)
	}
}

func TestMergeFileFailureIsAuditedAndNonGating(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	d.ObserveDesignMerge = func(context.Context, monitor.Observation, string) error {
		t.Fatal("observation should not run without files")
		return nil
	}
	d.ListPullRequestFiles = func(context.Context, string, int) ([]string, error) { return nil, errors.New("files unavailable") }
	views := 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		return githubtrigger.PullRequest{Number: 12, URL: "https://github.com/acme/app/pull/12", State: map[bool]string{true: "closed", false: "open"}[views > 1], Mergeable: "MERGEABLE", Merged: views > 1, HeadSHA: "head"}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error { return nil }
	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	status, err := st.(monitor.Store).MonitorStatus(ctx, true, time.Now().UTC())
	if err != nil || current.State != core.TaskMerged {
		t.Fatalf("task=%+v status_err=%v", current, err)
	}
	found := false
	for _, activity := range status.Activity {
		found = found || (activity.Kind == "system_design.drift_evaluation_failed" && fmt.Sprint(activity.Payload["pull_request"]) == "12")
	}
	if !found {
		t.Fatalf("failure audit missing: %+v", status.Activity)
	}
}

func TestMergeApprovedTaskReconcilesAlreadyMergedPR(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "reviewed-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "closed", Merged: true, HeadSHA: "reviewed-head"}, nil
	}
	mergeCalled := false
	d.RequestMerge = func(context.Context, string, int) error { mergeCalled = true; return nil }
	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.State != core.TaskMerged || mergeCalled {
		t.Fatalf("task=%+v mergeCalled=%t", current, mergeCalled)
	}
	events, _ := st.ListEvents(ctx, task.ID)
	found := false
	for _, event := range events {
		if event.Kind != "merge.reconciled" {
			continue
		}
		found = true
		var payload struct {
			FactoryReviewValidated bool                  `json:"factory_review_validated"`
			ReviewedHeadSHA        string                `json:"reviewed_head_sha"`
			ApprovedHeadSHA        string                `json:"approved_head_sha"`
			HeadSHA                string                `json:"head_sha"`
			ForgeAuthorClass       core.ForgeAuthorClass `json:"forge_author_class"`
			ForgeAuthorUserID      string                `json:"forge_author_user_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil || !payload.FactoryReviewValidated ||
			payload.ReviewedHeadSHA != "reviewed-head" || payload.ApprovedHeadSHA != "reviewed-head" || payload.HeadSHA != "reviewed-head" ||
			payload.ForgeAuthorClass != "" || payload.ForgeAuthorUserID != "" {
			t.Fatalf("reconciliation provenance=%+v err=%v", payload, err)
		}
		var fields map[string]any
		if err := json.Unmarshal(event.Payload, &fields); err != nil {
			t.Fatal(err)
		}
		if _, exists := fields["forge_author_user_id"]; exists {
			t.Fatalf("workspace reconciliation carries a user ID: %s", event.Payload)
		}
	}
	if !found {
		t.Fatalf("events=%+v", events)
	}
}

func TestMergeApprovedTaskRefusesChangedObservedHead(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "reviewed-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "closed", Merged: true, HeadSHA: "outside-head"}, nil
	}
	if err := d.MergeApprovedTask(ctx, task); err == nil {
		t.Fatal("changed head completed")
	}
	if count, _ := st.CountEvents(ctx, task.ID, "merge.reconciled"); count != 0 {
		t.Fatal("changed head reconciled")
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.State != core.TaskApproved {
		t.Fatalf("state=%s", current.State)
	}
}

func TestReconcileMergeReadinessRecoversAcceptedTaskKnockedOutOfApproved(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
	st := store.NewMemory()
	task := core.Task{ID: "lost-approved", Workspace: "test", Repo: "app", Branch: "conveyor/lost-approved", State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, ApprovedHeadSHA: "accepted-head", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"review_round": 2, "verdict": "approve", "approved_head_sha": "accepted-head"})}); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	views, merges := 0, 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		if views == 1 {
			return githubtrigger.PullRequest{Number: 44, State: "open", Mergeable: "MERGEABLE", HeadSHA: "accepted-head"}, nil
		}
		return githubtrigger.PullRequest{Number: 44, State: "closed", Merged: true, HeadSHA: "accepted-head"}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error { merges++; return nil }
	if reconciled, err := d.ReconcileMergeReadiness(ctx); err != nil || reconciled != 1 {
		t.Fatalf("reconciled=%d err=%v", reconciled, err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged || merges != 1 {
		t.Fatalf("task=%+v merges=%d err=%v", current, merges, err)
	}
	if reconciled, err := d.ReconcileMergeReadiness(ctx); err != nil || reconciled != 0 || merges != 1 {
		t.Fatalf("idempotent reconciled=%d merges=%d err=%v", reconciled, merges, err)
	}
}

func TestMergeApprovedTaskFailuresStayApprovedAndAreAudited(t *testing.T) {
	for _, test := range []struct {
		name       string
		githubRepo string
		view       func(context.Context, string, string) (githubtrigger.PullRequest, error)
		merge      func(context.Context, string, int) error
		reason     string
		category   githubtrigger.ForgeErrorCategory
	}{
		{name: "non GitHub repository", reason: "unsupported_repository"},
		{name: "missing pull request", githubRepo: "acme/app", reason: "missing_pull_request", category: githubtrigger.ForgeStatus, view: func(context.Context, string, string) (githubtrigger.PullRequest, error) {
			return githubtrigger.PullRequest{}, &githubtrigger.Error{Category: githubtrigger.ForgeStatus, Err: githubtrigger.ErrPullRequestNotFound}
		}},
		{name: "forge merge failure", githubRepo: "acme/app", reason: "forge_merge_failed", category: githubtrigger.ForgePermission, view: func(context.Context, string, string) (githubtrigger.PullRequest, error) {
			return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE"}, nil
		}, merge: func(context.Context, string, int) error {
			return &githubtrigger.Error{Category: githubtrigger.ForgePermission, Err: errors.New("branch protection")}
		}},
		{name: "unconfirmed result", githubRepo: "acme/app", reason: "merge_unconfirmed", view: func() func(context.Context, string, string) (githubtrigger.PullRequest, error) {
			calls := 0
			return func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				calls++
				return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", Merged: false}, nil
			}
		}(), merge: func(context.Context, string, int) error { return nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, st, task, d := approvedMergeFixture(t, test.githubRepo)
			if test.view != nil {
				d.ViewPullRequest = test.view
			}
			if test.merge != nil {
				d.RequestMerge = test.merge
			}
			if err := d.MergeApprovedTask(ctx, task); err == nil {
				t.Fatal("expected merge error")
			}
			current, _ := st.GetTask(ctx, task.ID)
			if current.State != core.TaskApproved {
				t.Fatalf("state=%s", current.State)
			}
			events, _ := st.ListEvents(ctx, task.ID)
			last := events[len(events)-1]
			if last.Kind != "merge.failed" || !strings.Contains(string(last.Payload), test.reason) {
				t.Fatalf("last event=%+v", last)
			}
			var payload struct {
				ForgeErrorCategory string `json:"forge_error_category"`
			}
			if err := json.Unmarshal(last.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.ForgeErrorCategory != string(test.category) {
				t.Fatalf("category=%q want=%q payload=%s", payload.ForgeErrorCategory, test.category, last.Payload)
			}
		})
	}
}

func TestMergeApprovedTaskSerializesConcurrentRequests(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	merged, mergeCalls := false, 0
	var forgeMu sync.Mutex
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		forgeMu.Lock()
		defer forgeMu.Unlock()
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", Merged: merged}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error {
		forgeMu.Lock()
		defer forgeMu.Unlock()
		mergeCalls++
		merged = true
		return nil
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- d.MergeApprovedTask(ctx, task)
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.State != core.TaskMerged || mergeCalls != 1 {
		t.Fatalf("state=%s mergeCalls=%d", current.State, mergeCalls)
	}
}

func TestMergeReadinessUnknownIsPendingWithoutMergeFailure(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	calls := 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		calls++
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "UNKNOWN", HeadSHA: "head-1"}, nil
	}
	readiness, err := d.ReadMergeReadiness(ctx, task)
	if err != nil || readiness.State != "UNKNOWN" || calls != 3 {
		t.Fatalf("readiness=%+v calls=%d err=%v", readiness, calls, err)
	}
	events, _ := st.ListEvents(ctx, task.ID)
	for _, event := range events {
		if event.Kind == "merge.failed" {
			t.Fatalf("UNKNOWN recorded merge failure: %+v", event)
		}
	}
}

func TestMergeReadinessSurfacesForgeCategoryWithoutRecordingGetNoise(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{}, &githubtrigger.Error{Category: githubtrigger.ForgeRequest, Err: errors.New("request timed out")}
	}
	_, err := d.ReadMergeReadiness(ctx, task)
	if err == nil || githubtrigger.ErrorCategory(err) != githubtrigger.ForgeRequest || !strings.Contains(err.Error(), "forge_request: request timed out") {
		t.Fatalf("readiness error=%v category=%q", err, githubtrigger.ErrorCategory(err))
	}
	events, listErr := st.ListEvents(ctx, task.ID)
	if listErr != nil {
		t.Fatal(listErr)
	}
	for _, event := range events {
		if event.Kind == "merge.failed" {
			t.Fatalf("GET-driven readiness recorded merge noise: %+v", event)
		}
	}
}

func TestConflictFixDispatchIsIdempotentAndCarriesFrozenContract(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "approved-head"}, nil
	}
	first, err := d.DispatchConflictFix(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.DispatchConflictFix(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || second.ID != first.ID || first.ReasonCode != "merge-conflict" || first.BaselineSHA != "approved-head" {
		t.Fatalf("orders first=%+v second=%+v", first, second)
	}
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	interventions, _ := st.ListInterventions(ctx, task.ID)
	if len(orders) != 1 || len(interventions) != 1 || interventions[0].ActorRole != core.ActorSystem || interventions[0].ReasonCode != "merge-conflict" {
		t.Fatalf("orders=%+v interventions=%+v", orders, interventions)
	}
}

func TestConflictFixTerminalAttemptsPermitOneEpisodeLocalReplacement(t *testing.T) {
	for _, terminalState := range []string{"submitted", "cancelled", "stale", "timed_out", "expired"} {
		t.Run(terminalState, func(t *testing.T) {
			ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
			if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
				t.Fatal(err)
			}
			d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "conflicting-head"}, nil
			}
			first, err := d.DispatchConflictFix(ctx, task)
			if err != nil {
				t.Fatal(err)
			}
			switch terminalState {
			case "submitted":
				first, err = storetest.For(st).ClaimWorkOrder(ctx, first.ID, core.WorkOrderClaim{
					SessionID: "conflict-terminal-session", ClientToken: "conflict-terminal-token",
					Agent: "codex", Model: "operator", Lease: time.Minute, ExecutionTimeout: time.Hour,
				})
				if err != nil {
					t.Fatal(err)
				}
				first.State = core.WorkOrderSubmitted
				if err = storetest.For(st).UpdateWorkOrder(ctx, first, core.WorkOrderCmdSubmitForReview); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				first.State = core.WorkOrderCancelled
				err = storetest.For(st).UpdateWorkOrder(ctx, first, core.WorkOrderCmdCancel)
			case "stale":
				first.State = core.WorkOrderStale
				err = storetest.For(st).UpdateWorkOrder(ctx, first, core.WorkOrderCmdMarkStale)
			case "timed_out":
				first.State = core.WorkOrderTimedOut
				err = storetest.For(st).UpdateWorkOrder(ctx, first, core.WorkOrderCmdTimeout)
			case "expired":
				first.RetrySuppressed = true
				first.LastAttemptOutcome = core.WorkOrderOutcomeExpired
				err = storetest.For(st).UpdateWorkOrder(ctx, first)
			}
			if err != nil {
				t.Fatal(err)
			}
			second, err := d.DispatchConflictFix(ctx, task)
			if err != nil {
				t.Fatal(err)
			}
			orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
			interventions, _ := st.ListInterventions(ctx, task.ID)
			if second.ID == "" || second.ID == first.ID || len(orders) != 2 || len(interventions) != 2 {
				t.Fatalf("first=%+v second=%+v orders=%+v interventions=%+v", first, second, orders, interventions)
			}
		})
	}
}

func TestConflictFixTerminalAttemptBudgetExhaustsOneEpisode(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "conflicting-head"}, nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		order, err := d.DispatchConflictFix(ctx, task)
		if err != nil || order.ID == "" {
			t.Fatalf("dispatch %d order=%+v err=%v", attempt+1, order, err)
		}
		order.RetrySuppressed = true
		order.LastAttemptOutcome = core.WorkOrderOutcomeExpired
		if err = storetest.For(st).UpdateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
	}
	if replacement, err := d.DispatchConflictFix(ctx, task); err != nil || replacement.ID != "" {
		t.Fatalf("exhausted replacement=%+v err=%v", replacement, err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 3 {
		t.Fatalf("orders=%+v err=%v, want three attempts", orders, err)
	}
	if exhausted, _ := st.CountEvents(ctx, task.ID, "merge.conflict_dispatch_exhausted"); exhausted != 1 {
		t.Fatalf("exhaustion events=%d, want 1", exhausted)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil || store.LatestForgeFailure(events) == nil || store.LatestForgeFailure(events).Category != "conflict_dispatch_exhausted" {
		t.Fatalf("needs-operator failure=%+v err=%v", store.LatestForgeFailure(events), err)
	}
}

func TestConflictFixDispatchFailureIsAtomicAndEpisodeBackedOff(t *testing.T) {
	ctx, base, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := base.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	d.Now = func() time.Time { return now }
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "conflicting-head"}, nil
	}
	failing := &failingConflictFixStore{Store: base}
	d.Store = failing

	if _, err := d.DispatchConflictFix(ctx, task); err == nil || !strings.Contains(err.Error(), "forced conflict-fix") {
		t.Fatalf("first dispatch error=%v", err)
	}
	current, _ := base.GetTask(ctx, task.ID)
	orders, _ := base.ListTaskWorkOrders(ctx, task.ID)
	interventions, _ := base.ListInterventions(ctx, task.ID)
	if current.State != core.TaskApproved || len(orders) != 0 || len(interventions) != 0 {
		t.Fatalf("partial conflict dispatch persisted: task=%+v orders=%+v interventions=%+v", current, orders, interventions)
	}
	if dispatched, _ := base.CountEvents(ctx, task.ID, "merge.conflict_fix_dispatched"); dispatched != 0 {
		t.Fatalf("dispatch audit count=%d, want 0", dispatched)
	}
	if _, err := d.DispatchConflictFix(ctx, task); err != nil || failing.calls != 1 {
		t.Fatalf("immediate retry err=%v calls=%d", err, failing.calls)
	}
	now = now.Add(time.Minute)
	if _, err := d.DispatchConflictFix(ctx, task); err == nil || failing.calls != 2 {
		t.Fatalf("one-minute retry err=%v calls=%d", err, failing.calls)
	}
	now = now.Add(2 * time.Minute)
	if _, err := d.DispatchConflictFix(ctx, task); err == nil || failing.calls != 3 {
		t.Fatalf("two-minute retry err=%v calls=%d", err, failing.calls)
	}
	now = now.Add(30 * time.Minute)
	if _, err := d.DispatchConflictFix(ctx, task); err != nil || failing.calls != 3 {
		t.Fatalf("exhausted retry err=%v calls=%d", err, failing.calls)
	}
	failed, _ := base.ListEvents(ctx, task.ID)
	// The durable payloads expose the 1, 2, and 4 minute schedule even though
	// the third failure suppresses automatic retries.
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		var payload struct {
			FailureCount int       `json:"failure_count"`
			NextRetryAt  time.Time `json:"next_retry_at"`
		}
		failureIndex := 0
		for _, event := range failed {
			if event.Kind != "merge.conflict_dispatch_failed" {
				continue
			}
			if failureIndex == i {
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				break
			}
			failureIndex++
		}
		baseTime := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
		if i == 1 {
			baseTime = baseTime.Add(time.Minute)
		} else if i == 2 {
			baseTime = baseTime.Add(3 * time.Minute)
		}
		if payload.FailureCount != i+1 || payload.NextRetryAt.Sub(baseTime) != want {
			t.Fatalf("failure %d payload=%+v delay=%s want=%s", i+1, payload, payload.NextRetryAt.Sub(baseTime), want)
		}
	}
	if exhausted, _ := base.CountEvents(ctx, task.ID, "merge.conflict_dispatch_exhausted"); exhausted != 1 {
		t.Fatalf("exhausted events=%d, want 1", exhausted)
	}
}

func TestConflictFixDispatchFailureBudgetResetsForChangedAndClearedEpisodes(t *testing.T) {
	ctx, base, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := base.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	d.Now = func() time.Time { return now }
	head, readiness := "conflicting-head", "CONFLICTING"
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: readiness, HeadSHA: head}, nil
	}
	failing := &failingConflictFixStore{Store: base}
	d.Store = failing

	for _, advance := range []time.Duration{0, time.Minute, 2 * time.Minute} {
		now = now.Add(advance)
		if _, err := d.DispatchConflictFix(ctx, task); err == nil {
			t.Fatalf("dispatch %d unexpectedly succeeded", failing.calls+1)
		}
	}
	if failing.calls != 3 {
		t.Fatalf("exhausted calls=%d, want 3", failing.calls)
	}

	head = "changed-conflicting-head"
	if _, err := d.DispatchConflictFix(ctx, task); err == nil || failing.calls != 4 {
		t.Fatalf("changed episode err=%v calls=%d", err, failing.calls)
	}
	readiness = "MERGEABLE"
	if _, err := d.ReadMergeReadiness(ctx, task); err != nil {
		t.Fatal(err)
	}
	readiness = "CONFLICTING"
	if _, err := d.DispatchConflictFix(ctx, task); err == nil || failing.calls != 5 {
		t.Fatalf("cleared episode err=%v calls=%d", err, failing.calls)
	}
}

func TestConflictFixInterruptedReviewRecoveryPrecedesHeldDispatch(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetTaskHold(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	interrupted := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1, RetrySuppressed: true, LastAttemptOutcome: core.WorkOrderOutcomeExpired, CreatedAt: time.Now().UTC()}
	if err := storetest.For(st).CreateWorkOrder(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "conflicting-head"}, nil
	}
	for range 3 {
		if _, err := d.DispatchConflictFix(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	interventions, _ := st.ListInterventions(ctx, task.ID)
	current, _ := st.GetTask(ctx, task.ID)
	if len(orders) != 1 || len(interventions) != 0 || current.State != core.TaskApproved {
		t.Fatalf("recovery precedence failed: task=%+v orders=%+v interventions=%+v", current, orders, interventions)
	}
	if blocked, _ := st.CountEvents(ctx, task.ID, "merge.conflict_recovery_blocked"); blocked != 1 {
		t.Fatalf("recovery-blocked events=%d, want 1", blocked)
	}
	interrupted.RetrySuppressed = false
	interrupted.LastAttemptOutcome = ""
	if err := storetest.For(st).UpdateWorkOrder(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DispatchConflictFix(ctx, task); err != nil {
		t.Fatal(err)
	}
	orders, _ = st.ListTaskWorkOrders(ctx, task.ID)
	interventions, _ = st.ListInterventions(ctx, task.ID)
	if len(orders) != 2 || len(interventions) != 1 || orders[1].ReasonCode != "merge-conflict" {
		t.Fatalf("ordinary conflict dispatch did not resume: orders=%+v interventions=%+v", orders, interventions)
	}
}

func TestStaleApprovalDispatchesDeltaRefreshRound(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.Cfg.Routing = config.Routing{Stages: map[string]config.StageRoute{"review": {Model: "reviewer", Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}}}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", HeadSHA: "new-head"}, nil
	}
	if err := d.MergeApprovedTask(ctx, task); err == nil || !strings.Contains(err.Error(), "refresh review dispatched") {
		t.Fatalf("err=%v", err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if !current.ApprovalStale || current.RefreshBaselineSHA != "approved-head" || current.RefreshHeadSHA != "new-head" || len(orders) != 1 || orders[0].ReviewKind != "refresh" || orders[0].ReviewScope != "delta" || orders[0].BaselineSHA != "approved-head" || orders[0].HeadSHA != "new-head" {
		t.Fatalf("task=%+v orders=%+v", current, orders)
	}
}

func TestRepeatedStaleApprovalPollKeepsActiveRefreshSeat(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.Cfg.Routing = config.Routing{Stages: map[string]config.StageRoute{"review": {Model: "reviewer", Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}}}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", HeadSHA: "new-head"}, nil
	}
	if readiness, err := d.ReadMergeReadiness(ctx, task); err != nil || readiness.State != "STALE" {
		t.Fatalf("first poll readiness=%+v err=%v", readiness, err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, orders[0].ID, core.WorkOrderClaim{SessionID: "deliberating-seat", ClientToken: "seat-token", Agent: "codex", Model: "reviewer", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if readiness, err := d.ReadMergeReadiness(ctx, task); err != nil || readiness.State != "STALE" {
		t.Fatalf("second poll readiness=%+v err=%v", readiness, err)
	}
	after, err := st.GetWorkOrder(ctx, claimed.ID)
	if err != nil || after.State != core.WorkOrderClaimed || after.SessionID != claimed.SessionID || after.ReviewRound != claimed.ReviewRound {
		t.Fatalf("active seat changed: before=%+v after=%+v err=%v", claimed, after, err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "approval.stale"); countErr != nil || count != 1 {
		t.Fatalf("approval.stale events=%d err=%v", count, countErr)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "review.refresh_round_created"); countErr != nil || count != 1 {
		t.Fatalf("refresh triggers=%d err=%v", count, countErr)
	}
	orders, _ = st.ListTaskWorkOrders(ctx, task.ID)
	if len(orders) != 1 {
		t.Fatalf("repeated poll created superseding round: %+v", orders)
	}
}

func TestApprovedRefreshBindsOrderHeadAndRestoresMergeReadiness(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewDelta, true)
	if err := st.BindTaskApproval(ctx, task.ID, "head-a"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"head_sha": "head-a"})}); err != nil {
		t.Fatal(err)
	}
	d.Cfg.Routing = config.Routing{Stages: map[string]config.StageRoute{"review": {Model: "reviewer", Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}}}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head-b"}, nil
	}
	if readiness, err := d.ReadMergeReadiness(ctx, task); err != nil || readiness.State != "STALE" {
		t.Fatalf("initial readiness=%+v err=%v", readiness, err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	order := orders[0]
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "refresh-review", ClientToken: "refresh-token", Agent: "codex", Model: "reviewer", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListJobs(ctx, task.ID)
	if err != nil || len(jobs) != 1 || jobs[0].ID != order.JobID {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	job := jobs[0]
	current, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.ApplyExternalReviewPinned(ctx, current, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "head B approved"}, order.ID, claimed.SessionID, "reviewer", []core.ServedRequirementContext{}, emptyGovernanceAuthority(), true); err != nil {
		t.Fatal(err)
	}
	current, err = st.GetTask(ctx, task.ID)
	if err != nil || current.ApprovedHeadSHA != "head-b" || current.ReviewedHeadSHA != "head-b" || current.ApprovalStale || current.RefreshBaselineSHA != "" || current.RefreshHeadSHA != "" || current.RefreshReviewScope != "" {
		t.Fatalf("settled task=%+v err=%v", current, err)
	}
	if readiness, readErr := d.ReadMergeReadiness(ctx, current); readErr != nil || readiness.State != "MERGEABLE" {
		t.Fatalf("settled readiness=%+v err=%v", readiness, readErr)
	}
	orders, err = st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 {
		t.Fatalf("refresh approval created replacement orders=%+v err=%v", orders, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind != "review.completed" {
			continue
		}
		var payload struct {
			ReviewedCommitSHA string `json:"reviewed_commit_sha"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.ReviewedCommitSHA != "head-b" {
			t.Fatalf("review completion did not bind refresh head: %s", event.Payload)
		}
		return
	}
	t.Fatal("review.completed event not found")
}

func TestNonAdvancingRefreshBindingParksEpisodeWithoutReplacementRound(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewDelta, true)
	if err := st.BindTaskApproval(ctx, task.ID, "head-a"); err != nil {
		t.Fatal(err)
	}
	d.Cfg.Routing = config.Routing{Stages: map[string]config.StageRoute{"review": {Model: "reviewer", Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}}}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", HeadSHA: "head-b"}, nil
	}
	if readiness, err := d.ReadMergeReadiness(ctx, task); err != nil || readiness.State != "STALE" {
		t.Fatalf("initial readiness=%+v err=%v", readiness, err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	order := orders[0]
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "regressed-refresh", ClientToken: "regressed-token", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	decision := core.ReviewDecision{
		TaskID: task.ID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ClaimSession: claimed.SessionID,
		ReviewRound: order.ReviewRound, ReviewSeat: order.ReviewSeat, ReviewKind: order.ReviewKind, ReviewScope: order.ReviewScope,
		BaselineSHA: order.BaselineSHA, HeadSHA: order.HeadSHA, ReviewedCommitSHA: "head-a",
		Verdict: "approve", ReasonCode: "approved", Summary: "forced stale binding", PolicyVersion: 1, MergeApproval: true,
	}
	if err = storetest.For(st).AcceptReviewDecision(ctx, decision); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || !current.ApprovalStale || current.ApprovedHeadSHA != "head-a" || current.RefreshBaselineSHA != "head-a" || current.RefreshHeadSHA != "head-b" || current.RefreshReviewScope != config.RefreshReviewDelta {
		t.Fatalf("parked task=%+v err=%v", current, err)
	}
	for range 2 {
		if readiness, readErr := d.ReadMergeReadiness(ctx, current); readErr != nil || readiness.State != "STALE" {
			t.Fatalf("parked readiness=%+v err=%v", readiness, readErr)
		}
	}
	orders, err = st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 {
		t.Fatalf("non-advancing binding created replacement orders=%+v err=%v", orders, err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "review.refresh_binding_not_advanced"); countErr != nil || count != 1 {
		t.Fatalf("non-advancing events=%d err=%v", count, countErr)
	}
}

func TestCleanStaleApprovalCanSkipRefreshUnderFrozenNone(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", HeadSHA: "clean-head"}, nil
	}
	if err := d.MergeApprovedTask(ctx, task); err == nil {
		t.Fatal("expected fail-closed stale response")
	}
	current, _ := st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if current.ApprovalStale || current.ApprovedHeadSHA != "clean-head" || current.State != core.TaskApproved || len(orders) != 0 {
		t.Fatalf("task=%+v orders=%+v", current, orders)
	}
}

func TestConflictResolutionForcesDeltaWhenFrozenPolicyIsNone(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.Cfg.Routing = config.Routing{Stages: map[string]config.StageRoute{"review": {Model: "reviewer", Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}}}
	current, _ := st.GetTask(ctx, task.ID)
	if err := d.beginRefreshLocked(ctx, current, "resolved-head", "merge-conflict", true); err != nil {
		t.Fatal(err)
	}
	current, _ = st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if !current.ApprovalStale || current.RefreshReviewScope != config.RefreshReviewDelta || len(orders) != 1 || orders[0].ReviewScope != config.RefreshReviewDelta {
		t.Fatalf("task=%+v orders=%+v", current, orders)
	}
}

func TestGateOffConflictingMergeAutomaticallyDispatchesFix(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "approved-head"}, nil
	}
	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if len(orders) != 1 || orders[0].ReasonCode != "merge-conflict" {
		t.Fatalf("orders=%+v", orders)
	}
}

func TestConflictingMergeGateRetriesRecordOneBlockedEvent(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewNone, true)
	if err := st.CreateIntervention(ctx, core.Intervention{TaskID: task.ID, Action: core.InterventionApprove, ActorID: store.UserActorID("usr-approver"), ActorRole: core.ActorUser}); err != nil {
		t.Fatal(err)
	}
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "conflicting-head"}, nil
	}
	if readiness, err := d.ReadMergeReadiness(ctx, task); err != nil || readiness.State != "CONFLICTING" {
		t.Fatalf("readiness=%+v err=%v", readiness, err)
	}
	for range 2 {
		if err := d.MergeApprovedTask(ctx, task); err == nil || !strings.Contains(err.Error(), "merge conflicts") {
			t.Fatalf("err=%v", err)
		}
	}
	if blocked, _ := st.CountEvents(ctx, task.ID, "merge.blocked"); blocked != 1 {
		t.Fatalf("merge.blocked events=%d, want 1", blocked)
	}
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if len(orders) != 0 {
		t.Fatalf("human-gated merge retries created orders=%+v", orders)
	}
}

func TestChangedHeadConflictingReadinessKeepsGateBlockedWithoutRefresh(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewNone, true)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "conflicting-head"}, nil
	}
	readiness, err := d.ReadMergeReadiness(ctx, task)
	if err != nil || readiness.State != "CONFLICTING" {
		t.Fatalf("readiness=%+v err=%v", readiness, err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if current.ApprovalStale || current.State != core.TaskApproved || len(orders) != 0 {
		t.Fatalf("task=%+v orders=%+v", current, orders)
	}
	if _, err := d.ReadMergeReadiness(ctx, task); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := st.CountEvents(ctx, task.ID, "merge.blocked"); blocked != 1 {
		t.Fatalf("merge.blocked events=%d, want 1", blocked)
	}
	orders, _ = st.ListTaskWorkOrders(ctx, task.ID)
	if len(orders) != 0 {
		t.Fatalf("repeated human-gated readiness created orders=%+v", orders)
	}
}

func TestChangedHeadConflictingAutoMergeDispatchesFixBeforeRefresh(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "conflicting-head"}, nil
	}
	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if current.ApprovalStale || current.State != core.TaskQueued || current.NextStage != core.StageImplement || len(orders) != 1 || orders[0].ReasonCode != "merge-conflict" || orders[0].BaselineSHA != "approved-head" {
		t.Fatalf("task=%+v orders=%+v", current, orders)
	}
}

func TestChangedHeadConflictingReadinessAutoDispatchesFixBeforeRefresh(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "conflicting-head"}, nil
	}
	readiness, err := d.ReadMergeReadiness(ctx, task)
	if err != nil || readiness.State != "CONFLICTING" {
		t.Fatalf("readiness=%+v err=%v", readiness, err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if current.ApprovalStale || current.State != core.TaskQueued || current.NextStage != core.StageImplement || len(orders) != 1 || orders[0].ReasonCode != "merge-conflict" || orders[0].BaselineSHA != "approved-head" {
		t.Fatalf("task=%+v orders=%+v", current, orders)
	}
	if _, err := d.ReadMergeReadiness(ctx, task); err != nil {
		t.Fatal(err)
	}
	orders, _ = st.ListTaskWorkOrders(ctx, task.ID)
	if blocked, _ := st.CountEvents(ctx, task.ID, "merge.blocked"); blocked != 1 || len(orders) != 1 {
		t.Fatalf("merge.blocked events=%d orders=%+v", blocked, orders)
	}
	if dispatched, _ := st.CountEvents(ctx, task.ID, "merge.conflict_fix_dispatched"); dispatched != 1 {
		t.Fatalf("merge.conflict_fix_dispatched events=%d, want 1", dispatched)
	}
}

func TestResolvedReadinessAllowsNewConflictEpisode(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewNone, true)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	mergeable := "CONFLICTING"
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: mergeable, HeadSHA: "approved-head"}, nil
	}
	if _, err := d.ReadMergeReadiness(ctx, task); err != nil {
		t.Fatal(err)
	}
	mergeable = "MERGEABLE"
	if readiness, err := d.ReadMergeReadiness(ctx, task); err != nil || readiness.State != "MERGEABLE" {
		t.Fatalf("resolved readiness=%+v err=%v", readiness, err)
	}
	mergeable = "CONFLICTING"
	if _, err := d.ReadMergeReadiness(ctx, task); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := st.CountEvents(ctx, task.ID, "merge.blocked"); blocked != 2 {
		t.Fatalf("merge.blocked events=%d, want 2", blocked)
	}
	if cleared, _ := st.CountEvents(ctx, task.ID, "merge.conflict_cleared"); cleared != 1 {
		t.Fatalf("merge.conflict_cleared events=%d, want 1", cleared)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// Regression for the live once-per-poll demotion loop (task 260807-0e2bc1,
// 44 identical approval.stale events): a refresh already engaged for the
// same head pair and scope must be a no-op on re-observation — re-marking
// re-ran the recover transition every minute, demoting a task whose claimed
// refresh seat was mid-deliberation, so no completed verdict could land.
func TestEngagedRefreshIsNotRemarkedForUnchangedHeadPair(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewDelta)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.Cfg.Routing = config.Routing{Stages: map[string]config.StageRoute{"review": {Model: "reviewer", Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}}}
	current, _ := st.GetTask(ctx, task.ID)
	if err := d.beginRefreshLocked(ctx, current, "new-head", "approval-stale", false); err != nil {
		t.Fatal(err)
	}
	staleCount := func() int {
		n, _ := st.CountEvents(ctx, task.ID, "approval.stale")
		return n
	}
	if staleCount() != 1 {
		t.Fatalf("first engagement should emit exactly one approval.stale, got %d", staleCount())
	}
	// Re-observation of the identical pair: silent no-op — no event, no
	// transition, no additional round.
	for i := 0; i < 3; i++ {
		current, _ = st.GetTask(ctx, task.ID)
		if err := d.beginRefreshLocked(ctx, current, "new-head", "approval-stale", false); err != nil {
			t.Fatal(err)
		}
	}
	current, _ = st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if staleCount() != 1 || len(orders) != 1 {
		t.Fatalf("unchanged pair re-marked: stale=%d orders=%d", staleCount(), len(orders))
	}
	// A genuinely new head re-engages.
	if err := d.beginRefreshLocked(ctx, current, "newer-head", "approval-stale", false); err != nil {
		t.Fatal(err)
	}
	if staleCount() != 2 {
		t.Fatalf("changed pair should re-engage, stale=%d", staleCount())
	}
}

func TestReconcileObservedPullRequestApprovalLineageAndIdempotency(t *testing.T) {
	for _, scenario := range []string{"approved", "unapproved", "wrong-repository", "wrong-slug", "wrong-pr", "changed-head", "missing-lineage", "stale-approval"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := core.Task{ID: "observed", Workspace: "demo", Repo: "app", BaseBranch: "main", Branch: "conveyor/task-observed", State: core.TaskApproved, ReviewedHeadSHA: "head", ApprovedHeadSHA: "head", CreatedAt: time.Now()}
			if scenario == "unapproved" {
				task.State = core.TaskRunning
			}
			if scenario == "stale-approval" {
				task.ApprovalStale = true
			}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if scenario != "missing-lineage" {
				if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 12, "head_sha": "head"})}); err != nil {
					t.Fatal(err)
				}
			}
			d := New(st, &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "app", GitHub: "org/app"}}}, nil)
			pr := githubtrigger.PullRequest{Number: 12, URL: "https://github.com/org/app/pull/12", Merged: true, HeadSHA: "head", HeadRef: task.Branch, MergeCommitSHA: "landed", MergedBy: "github-operator"}
			repository, slug := "app", "org/app"
			switch scenario {
			case "wrong-repository":
				repository = "other"
			case "wrong-slug":
				slug = "else/app"
			case "wrong-pr":
				pr.Number = 13
			case "changed-head":
				pr.HeadSHA = "moved"
			}
			for range 2 {
				accepted, err := d.ReconcileObservedPullRequest(ctx, repository, slug, task.ID, pr)
				if err != nil || accepted != (scenario == "approved") {
					t.Fatalf("accepted=%t err=%v", accepted, err)
				}
			}
			current, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			count, err := st.CountEvents(ctx, task.ID, "merge.reconciled")
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "approved" {
				if current.State != task.State || count != 0 {
					t.Fatalf("ineligible task changed: state=%s events=%d", current.State, count)
				}
				return
			}
			if current.State != core.TaskMerged || count != 1 {
				t.Fatalf("state=%s events=%d", current.State, count)
			}
			events, _ := st.ListEvents(ctx, task.ID)
			for _, event := range events {
				if event.Kind != "merge.reconciled" {
					continue
				}
				var payload map[string]any
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if payload["merged_by"] != "github-operator" || payload["merge_commit_sha"] != "landed" {
					t.Fatalf("actor provenance=%v", payload)
				}
			}
		})
	}
}

func TestReconcileObservedPullRequestUsesCurrentAssignedBranch(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "observed", Workspace: "demo", Repo: "app", BaseBranch: "main", Branch: "feature/from-ide", State: core.TaskApproved, ReviewedHeadSHA: "head", ApprovedHeadSHA: "head", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 9, "head_sha": "head"})}); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "app", GitHub: "org/app"}}}, nil)
	pr := githubtrigger.PullRequest{Number: 9, URL: "https://github.com/org/app/pull/9", Merged: true, HeadSHA: "head", HeadRef: "feature/from-ide", MergeCommitSHA: "landed", MergedBy: "github-operator"}
	accepted, err := d.ReconcileObservedPullRequest(ctx, "app", "org/app", task.ID, pr)
	if err != nil || !accepted {
		t.Fatalf("custom branch accepted=%t err=%v", accepted, err)
	}
	retired := githubtrigger.PullRequest{Number: 9, URL: pr.URL, Merged: true, HeadSHA: "head", HeadRef: "conveyor/task-observed", MergeCommitSHA: "landed", MergedBy: "github-operator"}
	accepted, err = d.ReconcileObservedPullRequest(ctx, "app", "org/app", task.ID, retired)
	if err != nil || accepted {
		t.Fatalf("retired default name after attach-away accepted=%t err=%v", accepted, err)
	}
}

// conflictFixDeferralFixture reproduces the incident shape: an accepted
// round-1 approval, an unresolved conflict episode, and a merge-conflict
// implement order while GitHub already reports the pushed fix as mergeable.
func conflictFixDeferralFixture(t *testing.T, taskState core.TaskState, claim bool) (context.Context, store.Store, core.Task, *Dispatcher, *int) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
	st := store.NewMemory()
	task := core.Task{ID: "conflict-deferral", Workspace: "test", Repo: "app", Branch: "conveyor/conflict-deferral", BaseBranch: "main", State: taskState, NextStage: core.StageImplement, PolicyVersion: 1, ApprovedHeadSHA: "approved-head", ReviewedHeadSHA: "approved-head", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, event := range []core.Event{
		{TaskID: task.ID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"review_round": 1, "verdict": "approve", "approved_head_sha": "approved-head"})},
		{TaskID: task.ID, Kind: "merge.blocked", Payload: core.JSONPayload(map[string]any{"workspace": "test", "task_id": task.ID, "reason_code": "merge-conflict", "approved_head": "approved-head", "new_head": "conflict-head"})},
	} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	job := core.Job{ID: task.ID + "-implement-2", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, ReasonCode: "merge-conflict", BaselineSHA: "approved-head"}); err != nil {
		t.Fatal(err)
	}
	if claim {
		if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "fixer", ClientToken: "token", Lease: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP}}}}, nil)
	d.DisableMemoryQueueForTest()
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 1098, State: "open", Mergeable: "MERGEABLE", HeadSHA: "fix-head", URL: "https://github.com/acme/app/pull/1098"}, nil
	}
	merges := 0
	d.RequestMerge = func(context.Context, string, int) error { merges++; return nil }
	return ctx, st, current, d, &merges
}

func eventKinds(t *testing.T, st store.Store, ctx context.Context, taskID string) []string {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func TestReconcileMergeReadinessDefersWhileConflictFixOrderIsActive(t *testing.T) {
	for _, test := range []struct {
		name  string
		claim bool
	}{{name: "queued", claim: false}, {name: "claimed", claim: true}} {
		t.Run(test.name, func(t *testing.T) {
			ctx, st, task, d, merges := conflictFixDeferralFixture(t, core.TaskRunning, test.claim)
			before := eventKinds(t, st, ctx, task.ID)
			ordersBefore, err := st.ListTaskWorkOrders(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			for poll := 0; poll < 3; poll++ {
				if reconciled, err := d.ReconcileMergeReadiness(ctx); err != nil || reconciled != 0 {
					t.Fatalf("poll %d reconciled=%d err=%v", poll, reconciled, err)
				}
			}
			if after := eventKinds(t, st, ctx, task.ID); !reflect.DeepEqual(before, after) {
				t.Fatalf("deferred sweep appended events: before=%v after=%v", before, after)
			}
			ordersAfter, err := st.ListTaskWorkOrders(ctx, task.ID)
			if err != nil || len(ordersAfter) != len(ordersBefore) {
				t.Fatalf("orders before=%d after=%d err=%v", len(ordersBefore), len(ordersAfter), err)
			}
			current, err := st.GetTask(ctx, task.ID)
			if err != nil || current.State != task.State || current.ApprovalStale || current.ApprovedHeadSHA != "approved-head" || *merges != 0 {
				t.Fatalf("task=%+v merges=%d err=%v", current, *merges, err)
			}
			if err = d.MergeApprovedTask(ctx, current); !errors.Is(err, ErrConflictFixPending) || *merges != 0 {
				t.Fatalf("direct merge err=%v merges=%d", err, *merges)
			}
			if after := eventKinds(t, st, ctx, task.ID); !reflect.DeepEqual(before, after) {
				t.Fatalf("deferred merge appended events: before=%v after=%v", before, after)
			}
		})
	}
}

func TestMergeReadinessResumesWhenConflictFixOrderIsRetrySuppressed(t *testing.T) {
	ctx, st, task, d, _ := conflictFixDeferralFixture(t, core.TaskRunning, false)
	order, err := st.GetWorkOrder(ctx, task.ID+"-implement-2")
	if err != nil {
		t.Fatal(err)
	}
	order.RetrySuppressed = true
	if err = storetest.For(st).UpdateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	if reconciled, err := d.ReconcileMergeReadiness(ctx); err != nil || reconciled != 1 {
		t.Fatalf("reconciled=%d err=%v", reconciled, err)
	}
	kinds := strings.Join(eventKinds(t, st, ctx, task.ID), ",")
	if !strings.Contains(kinds, "merge.conflict_cleared") || !strings.Contains(kinds, "approval.stale") {
		t.Fatalf("suppressed order must not defer the existing readiness path: %s", kinds)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || !current.ApprovalStale || current.RefreshHeadSHA != "fix-head" {
		t.Fatalf("task=%+v err=%v", current, err)
	}
}

func TestReadMergeReadinessReportsPendingWhileConflictFixOrderIsActive(t *testing.T) {
	ctx, st, task, d, merges := conflictFixDeferralFixture(t, core.TaskApproved, true)
	before := eventKinds(t, st, ctx, task.ID)
	for poll := 0; poll < 2; poll++ {
		readiness, err := d.ReadMergeReadiness(ctx, task)
		if err != nil || readiness.State != "UNKNOWN" || readiness.HeadSHA != "fix-head" || readiness.Number != 1098 {
			t.Fatalf("poll %d readiness=%+v err=%v", poll, readiness, err)
		}
	}
	if after := eventKinds(t, st, ctx, task.ID); !reflect.DeepEqual(before, after) || *merges != 0 {
		t.Fatalf("readiness mutated history: before=%v after=%v merges=%d", before, after, *merges)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskApproved || current.ApprovalStale {
		t.Fatalf("task=%+v err=%v", current, err)
	}
}
