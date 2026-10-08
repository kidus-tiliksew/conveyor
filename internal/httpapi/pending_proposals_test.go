package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// The claim-wait predicate is judged over durable versions. A task ID alone
// never makes a version withhold claims: only the implementation origin does,
// and only while the version is unresolved (req-260810-70ce2f AC-1.1–AC-1.4).
func TestProposalClaimWaitingPredicate(t *testing.T) {
	const task = "task-a"
	for name, tc := range map[string]struct {
		version core.RequirementVersion
		want    bool
	}{
		"implementation origin":            {core.RequirementVersion{Origin: core.RequirementOriginImplementation, OriginTaskID: task}, true},
		"implementation from another task": {core.RequirementVersion{Origin: core.RequirementOriginImplementation, OriginTaskID: "task-b"}, false},
		"task ID without implementation":   {core.RequirementVersion{Origin: core.RequirementOriginOperator, OriginTaskID: task}, false},
		"chat session origin":              {core.RequirementVersion{Origin: core.RequirementOriginChat, OriginSessionID: "session"}, false},
		"drift origin":                     {core.RequirementVersion{Origin: core.RequirementOriginDriftAmendment, OriginDriftID: "drift"}, false},
		"confirmed":                        {core.RequirementVersion{Origin: core.RequirementOriginImplementation, OriginTaskID: task, Confirmed: true}, false},
		"retired or dismissed":             {core.RequirementVersion{Origin: core.RequirementOriginImplementation, OriginTaskID: task, Retired: true}, false},
	} {
		if got := store.RequirementVersionWithholdsClaims(task, tc.version); got != tc.want {
			t.Errorf("requirement %s: got %t want %t", name, got, tc.want)
		}
	}
	for name, tc := range map[string]struct {
		version core.SystemDesignVersion
		want    bool
	}{
		"implementation origin":            {core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: task}, true},
		"implementation from another task": {core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: "task-b"}, false},
		"task ID without implementation":   {core.SystemDesignVersion{Origin: core.SystemDesignOriginOperator, OriginTaskID: task}, false},
		"planning session origin":          {core.SystemDesignVersion{Origin: core.SystemDesignOriginPlanning, OriginSessionID: "session"}, false},
		"confirmed":                        {core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: task, Confirmed: true}, false},
		"dismissed":                        {core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: task, Dismissed: true}, false},
	} {
		if got := store.SystemDesignVersionWithholdsClaims(task, tc.version); got != tc.want {
			t.Errorf("system design %s: got %t want %t", name, got, tc.want)
		}
	}
}

const claimParityTask = "claim-parity"

type claimParityFixture struct {
	ctx context.Context
	st  store.Store
	t   *testing.T
}

func (f claimParityFixture) design(id string) core.SystemDesign {
	f.t.Helper()
	document, version, err := f.st.CreateSystemDesign(f.ctx, core.SystemDesign{ID: id, Title: "Design " + id, Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# " + id + "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/" + id + "/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, _, err = f.st.ConfirmSystemDesignVersion(f.ctx, document.ID, version.Version); err != nil {
		f.t.Fatal(err)
	}
	return document
}

func (f claimParityFixture) proposeDesign(document core.SystemDesign, version core.SystemDesignVersion) core.SystemDesignVersion {
	f.t.Helper()
	version.DocumentID = document.ID
	version.Content = fmt.Sprintf("# %s\n\nRevision %d.\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/%s/**\n```", document.ID, time.Now().UnixNano(), document.ID)
	proposed, err := f.st.ProposeSystemDesignVersion(f.ctx, version)
	if err != nil {
		f.t.Fatal(err)
	}
	return proposed
}

func (f claimParityFixture) requirement(id string) core.Requirement {
	f.t.Helper()
	requirement, version, err := f.st.CreateRequirement(f.ctx, core.Requirement{ID: id, Title: "Requirement " + id}, core.RequirementVersion{
		Content: "# " + id, Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep the claim gate exact."}},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, _, err = f.st.ConfirmRequirementVersion(f.ctx, requirement.ID, version.Version); err != nil {
		f.t.Fatal(err)
	}
	return requirement
}

func (f claimParityFixture) proposeRequirement(requirement core.Requirement, version core.RequirementVersion) core.RequirementVersion {
	f.t.Helper()
	version.RequirementID = requirement.ID
	version.Content = fmt.Sprintf("# %s revision %d", requirement.ID, time.Now().UnixNano())
	version.Statements = []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep the claim gate exact."}}
	proposed, err := f.st.ProposeRequirementVersion(f.ctx, version)
	if err != nil {
		f.t.Fatal(err)
	}
	return proposed
}

func (f claimParityFixture) submitImplementation(taskID string) {
	f.t.Helper()
	jobID := taskID + "-implement-1"
	if err := f.st.CreateJob(f.ctx, core.Job{ID: jobID, TaskID: taskID, Stage: core.StageImplement, State: core.JobRunning}); err != nil {
		f.t.Fatal(err)
	}
	if err := storetest.For(f.st).CreateWorkOrder(f.ctx, core.WorkOrder{ID: jobID, TaskID: taskID, JobID: jobID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		f.t.Fatal(err)
	}
	claimed, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, jobID, core.WorkOrderClaim{SessionID: taskID + "-implementer", ClientToken: "token", WorkerID: "worker", Lease: time.Minute})
	if err != nil {
		f.t.Fatal(err)
	}
	claimed.State = core.WorkOrderSubmitted
	if err = storetest.For(f.st).UpdateWorkOrder(f.ctx, claimed, core.WorkOrderCmdSubmitForReview); err != nil {
		f.t.Fatal(err)
	}
}

type claimParityView struct {
	PendingAuthority     bool              `json:"pending_authority"`
	ProposalClaimWaiting bool              `json:"proposal_claim_waiting"`
	WaitingProposals     []waitingProposal `json:"waiting_proposals"`
}

func readClaimParity(t *testing.T, server *Server, taskID string) (claimParityView, claimParityView) {
	t.Helper()
	detail := httptest.NewRecorder()
	server.Handler().ServeHTTP(detail, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID+"/activity?workspace_id=demo", nil)))
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	var detailView claimParityView
	if err := json.Unmarshal(detail.Body.Bytes(), &detailView); err != nil {
		t.Fatal(err)
	}
	activity := httptest.NewRecorder()
	server.Handler().ServeHTTP(activity, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/activity?workspace_id=demo", nil)))
	if activity.Code != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", activity.Code, activity.Body.String())
	}
	var rows []struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
		claimParityView
	}
	if err := json.Unmarshal(activity.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Task.ID == taskID {
			return detailView, row.claimParityView
		}
	}
	t.Fatalf("activity omitted %s: %s", taskID, activity.Body.String())
	return claimParityView{}, claimParityView{}
}

// The projection agrees with the real claim gates. Each fixture is classified
// by the read model, then the same task's queued verify or review order is
// claimed through the work-order service (its System Design pre-check) and the
// store transaction (the full requirement and System Design gate). A claim is
// refused exactly when the projection says the task is waiting
// (req-260810-70ce2f AC-1.1–AC-1.4; req-260810-23b69f AC-2.1, AC-2.2).
func TestProposalClaimWaitingMatchesVerifyAndReviewClaims(t *testing.T) {
	type expectation struct {
		waiting          []waitingProposal
		pendingAuthority bool
	}
	cases := map[string]func(f claimParityFixture) expectation{
		"implementation System Design": func(f claimParityFixture) expectation {
			document := f.design("design-impl")
			version := f.proposeDesign(document, core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: claimParityTask})
			return expectation{waiting: []waitingProposal{{Tier: "system_design", ID: document.ID, Version: version.Version}}, pendingAuthority: true}
		},
		"implementation requirement": func(f claimParityFixture) expectation {
			requirement := f.requirement("req-impl")
			version := f.proposeRequirement(requirement, core.RequirementVersion{Origin: core.RequirementOriginImplementation, OriginTaskID: claimParityTask})
			return expectation{waiting: []waitingProposal{{Tier: "requirement", ID: requirement.ID, Version: version.Version}}, pendingAuthority: true}
		},
		"decision only": func(f claimParityFixture) expectation {
			if _, err := f.st.ProposeDecision(f.ctx, core.Decision{Statement: "Pending decision", Context: "Signal only", AlternativesRejected: "Holding review", Origin: core.DecisionOriginImplementation, OriginTaskID: claimParityTask}); err != nil {
				f.t.Fatal(err)
			}
			return expectation{pendingAuthority: true}
		},
		"requirement and decision": func(f claimParityFixture) expectation {
			requirement := f.requirement("req-mixed")
			version := f.proposeRequirement(requirement, core.RequirementVersion{Origin: core.RequirementOriginImplementation, OriginTaskID: claimParityTask})
			if _, err := f.st.ProposeDecision(f.ctx, core.Decision{Statement: "Mixed decision", Context: "Signal only", AlternativesRejected: "Holding review", Origin: core.DecisionOriginImplementation, OriginTaskID: claimParityTask}); err != nil {
				f.t.Fatal(err)
			}
			return expectation{waiting: []waitingProposal{{Tier: "requirement", ID: requirement.ID, Version: version.Version}}, pendingAuthority: true}
		},
		"another task's System Design": func(f claimParityFixture) expectation {
			document := f.design("design-other")
			f.proposeDesign(document, core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: "claim-parity-other"})
			return expectation{}
		},
		"operator requirement": func(f claimParityFixture) expectation {
			f.proposeRequirement(f.requirement("req-operator"), core.RequirementVersion{Origin: core.RequirementOriginOperator})
			return expectation{}
		},
		"planning session System Design": func(f claimParityFixture) expectation {
			f.proposeDesign(f.design("design-session"), core.SystemDesignVersion{Origin: core.SystemDesignOriginPlanning, OriginSessionID: "planning-session"})
			return expectation{}
		},
		"task-context suggestion": func(f claimParityFixture) expectation {
			requirement := f.requirement("req-context")
			if _, suppressed, err := f.st.ProposeTaskContext(f.ctx, core.TaskContextProposalInput{
				TaskID: claimParityTask, TargetKind: core.TaskContextProposalRequirement, TargetID: requirement.ID,
				Source: core.TaskContextProposalTriage, Justification: "Context suggestion.",
			}); err != nil || suppressed {
				f.t.Fatalf("suppressed=%t err=%v", suppressed, err)
			}
			// Task-context suggestions keep their existing attention signal and
			// never withhold a claim.
			return expectation{pendingAuthority: true}
		},
		"confirmed System Design": func(f claimParityFixture) expectation {
			document := f.design("design-confirmed")
			version := f.proposeDesign(document, core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: claimParityTask})
			if _, _, err := f.st.ConfirmSystemDesignVersion(f.ctx, document.ID, version.Version); err != nil {
				f.t.Fatal(err)
			}
			return expectation{}
		},
		"dismissed System Design": func(f claimParityFixture) expectation {
			document := f.design("design-dismissed")
			version := f.proposeDesign(document, core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: claimParityTask})
			if _, _, err := f.st.DismissSystemDesignVersion(f.ctx, document.ID, version.Version); err != nil {
				f.t.Fatal(err)
			}
			return expectation{}
		},
		"dismissed requirement": func(f claimParityFixture) expectation {
			requirement := f.requirement("req-dismissed")
			version := f.proposeRequirement(requirement, core.RequirementVersion{Origin: core.RequirementOriginImplementation, OriginTaskID: claimParityTask})
			if _, _, err := f.st.DismissRequirementVersion(f.ctx, requirement.ID, version.Version); err != nil {
				f.t.Fatal(err)
			}
			return expectation{}
		},
		"requirement retired by a later confirmation": func(f claimParityFixture) expectation {
			requirement := f.requirement("req-retired")
			f.proposeRequirement(requirement, core.RequirementVersion{Origin: core.RequirementOriginImplementation, OriginTaskID: claimParityTask})
			later := f.proposeRequirement(requirement, core.RequirementVersion{Origin: core.RequirementOriginOperator})
			if _, _, err := f.st.ConfirmRequirementVersion(f.ctx, requirement.ID, later.Version); err != nil {
				f.t.Fatal(err)
			}
			return expectation{}
		},
		"another workspace's System Design": func(f claimParityFixture) expectation {
			sibling := claimParityFixture{ctx: store.WithWorkspace(f.t.Context(), "sibling"), st: f.st, t: f.t}
			document := sibling.design("design-sibling")
			sibling.proposeDesign(document, core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: claimParityTask})
			return expectation{}
		},
	}
	for name, setup := range cases {
		for _, stage := range []core.Stage{core.StageVerify, core.StageReview} {
			t.Run(fmt.Sprintf("%s/%s", name, stage), func(t *testing.T) {
				ctx := store.WithWorkspace(t.Context(), "demo")
				st := store.NewMemory()
				// A verify order binds to the task's verify policy and submitted
				// head, so the verify variant carries both (DEC-43).
				const head = "0123456789abcdef0123456789abcdef01234567"
				for _, id := range []string{claimParityTask, "claim-parity-other"} {
					task := core.Task{ID: id, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: stage, ReviewedHeadSHA: head, CreatedAt: time.Now().UTC()}
					task.SetupContract.VerifyStage = stage == core.StageVerify
					if err := st.CreateTask(ctx, task); err != nil {
						t.Fatal(err)
					}
				}
				fixture := claimParityFixture{ctx: ctx, st: st, t: t}
				fixture.submitImplementation(claimParityTask)
				fixture.submitImplementation("claim-parity-other")
				want := setup(fixture)
				orderID := fmt.Sprintf("%s-%s-1", claimParityTask, stage)
				if err := st.CreateJob(ctx, core.Job{ID: orderID, TaskID: claimParityTask, Stage: stage, State: core.JobPending}); err != nil {
					t.Fatal(err)
				}
				downstream := core.WorkOrder{ID: orderID, TaskID: claimParityTask, JobID: orderID, Stage: stage}
				if stage == core.StageVerify {
					downstream.HeadSHA = head
				}
				if err := storetest.For(st).CreateWorkOrder(ctx, downstream); err != nil {
					t.Fatal(err)
				}

				server := NewServer(st)
				server.BearerToken, server.Workspace = "operator-token", "demo"
				detail, row := readClaimParity(t, server, claimParityTask)
				waiting := len(want.waiting) > 0
				if detail.ProposalClaimWaiting != waiting || row.ProposalClaimWaiting != waiting {
					t.Fatalf("waiting detail=%t activity=%t want %t", detail.ProposalClaimWaiting, row.ProposalClaimWaiting, waiting)
				}
				if fmt.Sprint(detail.WaitingProposals) != fmt.Sprint(want.waiting) {
					t.Fatalf("waiting proposals=%+v want %+v", detail.WaitingProposals, want.waiting)
				}
				if detail.PendingAuthority != want.pendingAuthority || row.PendingAuthority != want.pendingAuthority {
					t.Fatalf("pending authority detail=%t activity=%t want %t", detail.PendingAuthority, row.PendingAuthority, want.pendingAuthority)
				}

				cfg := &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{
					string(stage): {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
				}}}
				service := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
				_, serviceErr := service.Claim(ctx, orderID, core.WorkOrderClaim{SessionID: "service-" + string(stage), ClientToken: "service-token", ClaimantID: "claimant", Lease: time.Minute})
				_, storeErr := storetest.For(st).ClaimWorkOrder(ctx, orderID, core.WorkOrderClaim{SessionID: "store-" + string(stage), ClientToken: "store-token", Lease: time.Minute})
				if waiting {
					if storeErr == nil || !strings.Contains(storeErr.Error(), "waiting on task-authored") {
						t.Fatalf("store claim error=%v; projection said waiting", storeErr)
					}
					if serviceErr == nil || !strings.Contains(serviceErr.Error(), "waiting on") {
						t.Fatalf("service claim error=%v; projection said waiting", serviceErr)
					}
					return
				}
				// The service claims first; the store's own claim then meets a live
				// lease rather than a proposal wait, which is still not a wait.
				if serviceErr != nil {
					t.Fatalf("service claim error=%v; projection said not waiting", serviceErr)
				}
				if storeErr != nil && strings.Contains(storeErr.Error(), "waiting on") {
					t.Fatalf("store claim error=%v; projection said not waiting", storeErr)
				}
			})
		}
	}
}

// A task outside its claim window is not waiting even with a qualifying
// proposal: nothing has been submitted for verification or review yet.
func TestProposalClaimWaitingRequiresSubmittedWork(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: claimParityTask, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	fixture := claimParityFixture{ctx: ctx, st: st, t: t}
	fixture.proposeDesign(fixture.design("design-early"), core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: claimParityTask})
	server := NewServer(st)
	server.BearerToken, server.Workspace = "operator-token", "demo"
	detail, row := readClaimParity(t, server, claimParityTask)
	if detail.ProposalClaimWaiting || row.ProposalClaimWaiting || len(detail.WaitingProposals) != 0 || detail.PendingAuthority {
		t.Fatalf("detail=%+v activity=%+v", detail, row)
	}
}

type versionReadStore struct {
	store.Store
	reads          int
	requirementErr error
	resolveDesign  bool
}

func (s *versionReadStore) GetRequirementVersion(ctx context.Context, id string, version int) (core.RequirementVersion, error) {
	s.reads++
	if s.requirementErr != nil {
		return core.RequirementVersion{}, s.requirementErr
	}
	return s.Store.GetRequirementVersion(ctx, id, version)
}

func (s *versionReadStore) GetSystemDesignVersion(ctx context.Context, id string, version int) (core.SystemDesignVersion, error) {
	s.reads++
	item, err := s.Store.GetSystemDesignVersion(ctx, id, version)
	if err == nil && s.resolveDesign {
		// The operator dismissed the version between the queue read and
		// this re-read; its current state decides eligibility.
		item.Dismissed = true
	}
	return item, err
}

// Version reads stay bounded to candidates authored by tasks in their claim
// window, a version resolved during the read is judged on its re-read state,
// and a storage failure is a read error, never "not waiting".
func TestProposalClaimWaitingReadsAreBoundedAndTruthful(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	base := store.NewMemory()
	for _, id := range []string{claimParityTask, "claim-parity-idle"} {
		if err := base.CreateTask(ctx, core.Task{ID: id, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	fixture := claimParityFixture{ctx: ctx, st: base, t: t}
	fixture.submitImplementation(claimParityTask)
	design := fixture.design("design-bounded")
	designVersion := fixture.proposeDesign(design, core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: claimParityTask})
	requirement := fixture.requirement("req-bounded")
	fixture.proposeRequirement(requirement, core.RequirementVersion{Origin: core.RequirementOriginImplementation, OriginTaskID: claimParityTask})
	// The idle task has submitted nothing, so its proposals cost no reads.
	for index := range 5 {
		fixture.proposeDesign(fixture.design(fmt.Sprintf("design-idle-%d", index)), core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: "claim-parity-idle"})
	}
	if _, err := base.ProposeDecision(ctx, core.Decision{Statement: "No read", Context: "Decisions are never candidates", AlternativesRejected: "Reading them", Origin: core.DecisionOriginImplementation, OriginTaskID: claimParityTask}); err != nil {
		t.Fatal(err)
	}

	counting := &versionReadStore{Store: base}
	server := NewServer(counting)
	server.BearerToken, server.Workspace = "operator-token", "demo"
	detail, _ := readClaimParity(t, server, claimParityTask)
	if counting.reads != 4 { // two candidates on the detail read, two on the activity read
		t.Fatalf("version reads=%d", counting.reads)
	}
	if !detail.ProposalClaimWaiting || len(detail.WaitingProposals) != 2 {
		t.Fatalf("detail=%+v", detail)
	}

	counting.resolveDesign = true
	detail, row := readClaimParity(t, server, claimParityTask)
	if !detail.ProposalClaimWaiting || !row.ProposalClaimWaiting || len(detail.WaitingProposals) != 1 || detail.WaitingProposals[0].Tier != "requirement" {
		t.Fatalf("resolved design still counted: detail=%+v activity=%+v (design %s v%d)", detail, row, design.ID, designVersion.Version)
	}

	counting.requirementErr = errors.New("version read failed")
	for _, path := range []string{"/v1/tasks/" + claimParityTask + "/activity", "/v1/activity", "/v1/reviews"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, path+"?workspace_id=demo", nil)))
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	counting.requirementErr = fmt.Errorf("%w: requirement removed", store.ErrNotFound)
	detail, _ = readClaimParity(t, server, claimParityTask)
	if detail.ProposalClaimWaiting || len(detail.WaitingProposals) != 0 {
		t.Fatalf("removed version still counted: %+v", detail)
	}
}
