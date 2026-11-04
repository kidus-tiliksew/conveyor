package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

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
	claimed, err := storetest.For(f.st).ClaimWorkOrder(f.ctx, jobID, core.WorkOrderClaim{SessionID: taskID + "-implementer", ClientToken: "token", Lease: time.Minute})
	if err != nil {
		f.t.Fatal(err)
	}
	claimed.State = core.WorkOrderSubmitted
	if err = storetest.For(f.st).UpdateWorkOrder(f.ctx, claimed, core.WorkOrderCmdSubmitForReview); err != nil {
		f.t.Fatal(err)
	}
}

func readNextActionKind(t *testing.T, server *Server, taskID string) nextaction.NextAction {
	t.Helper()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID+"/next-action?workspace_id=demo", nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("next-action status=%d body=%s", response.Code, response.Body.String())
	}
	var action nextaction.NextAction
	if err := json.Unmarshal(response.Body.Bytes(), &action); err != nil {
		t.Fatal(err)
	}
	return action
}

// retiredProposalWaitFields are the task-read fields the retired wait fed.
var retiredProposalWaitFields = []string{"pending_authority", "proposal_claim_waiting", "waiting_proposals", "operator_notes"}

// requireNoProposalWaitFields reads the task detail and activity row and refuses
// any retired wait or dismissal-note field (req-260810-23b69f REQ-3, REQ-5).
func requireNoProposalWaitFields(t *testing.T, server *Server, taskID string) {
	t.Helper()
	detail := httptest.NewRecorder()
	server.Handler().ServeHTTP(detail, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID+"/activity?workspace_id=demo", nil)))
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	var detailBody map[string]json.RawMessage
	if err := json.Unmarshal(detail.Body.Bytes(), &detailBody); err != nil {
		t.Fatal(err)
	}
	activity := httptest.NewRecorder()
	server.Handler().ServeHTTP(activity, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/activity?workspace_id=demo", nil)))
	if activity.Code != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", activity.Code, activity.Body.String())
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(activity.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, field := range retiredProposalWaitFields {
		if _, present := detailBody[field]; present {
			t.Fatalf("task detail still serves %s: %s", field, detail.Body.String())
		}
		for _, row := range rows {
			if _, present := row[field]; present {
				t.Fatalf("activity row still serves %s: %s", field, activity.Body.String())
			}
		}
	}
	for _, row := range rows {
		if strings.Contains(string(row["task"]), `"`+taskID+`"`) {
			return
		}
	}
	t.Fatalf("activity omitted %s: %s", taskID, activity.Body.String())
}

// No proposal shape holds a claim. For each one, the task reads serve no wait
// field, the next action names no document proposal, the order lists as
// claimable, and the claim through the work-order service is admitted, for both
// verify and review (req-260810-23b69f REQ-3; DEC-102(6);
// req-operator-next-action AC-1.3).
func TestVerifyAndReviewClaimsProceedForEveryProposalShape(t *testing.T) {
	cases := map[string]func(f claimParityFixture){
		"implementation System Design": func(f claimParityFixture) {
			document := f.design("design-impl")
			f.proposeDesign(document, core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: claimParityTask})
		},
		"implementation requirement": func(f claimParityFixture) {
			requirement := f.requirement("req-impl")
			f.proposeRequirement(requirement, core.RequirementVersion{Origin: core.RequirementOriginImplementation, OriginTaskID: claimParityTask})
		},
		"decision only": func(f claimParityFixture) {
			if _, err := f.st.ProposeDecision(f.ctx, core.Decision{Statement: "Pending decision", Context: "Signal only", AlternativesRejected: "Holding review", Origin: core.DecisionOriginImplementation, OriginTaskID: claimParityTask}); err != nil {
				f.t.Fatal(err)
			}
		},
		"requirement and decision": func(f claimParityFixture) {
			requirement := f.requirement("req-mixed")
			f.proposeRequirement(requirement, core.RequirementVersion{Origin: core.RequirementOriginImplementation, OriginTaskID: claimParityTask})
			if _, err := f.st.ProposeDecision(f.ctx, core.Decision{Statement: "Mixed decision", Context: "Signal only", AlternativesRejected: "Holding review", Origin: core.DecisionOriginImplementation, OriginTaskID: claimParityTask}); err != nil {
				f.t.Fatal(err)
			}
		},
		"another task's System Design": func(f claimParityFixture) {
			document := f.design("design-other")
			f.proposeDesign(document, core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: "claim-parity-other"})
		},
		"operator requirement": func(f claimParityFixture) {
			f.proposeRequirement(f.requirement("req-operator"), core.RequirementVersion{Origin: core.RequirementOriginOperator})
		},
		"planning session System Design": func(f claimParityFixture) {
			f.proposeDesign(f.design("design-session"), core.SystemDesignVersion{Origin: core.SystemDesignOriginPlanning, OriginSessionID: "planning-session"})
		},
		"task-context suggestion": func(f claimParityFixture) {
			requirement := f.requirement("req-context")
			if _, suppressed, err := f.st.ProposeTaskContext(f.ctx, core.TaskContextProposalInput{
				TaskID: claimParityTask, TargetKind: core.TaskContextProposalRequirement, TargetID: requirement.ID,
				Source: core.TaskContextProposalTriage, Justification: "Context suggestion.",
			}); err != nil || suppressed {
				f.t.Fatalf("suppressed=%t err=%v", suppressed, err)
			}
		},
		"confirmed System Design": func(f claimParityFixture) {
			document := f.design("design-confirmed")
			version := f.proposeDesign(document, core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: claimParityTask})
			if _, _, err := f.st.ConfirmSystemDesignVersion(f.ctx, document.ID, version.Version); err != nil {
				f.t.Fatal(err)
			}
		},
		"dismissed System Design": func(f claimParityFixture) {
			document := f.design("design-dismissed")
			version := f.proposeDesign(document, core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: claimParityTask})
			if _, _, err := f.st.DismissSystemDesignVersion(f.ctx, document.ID, version.Version); err != nil {
				f.t.Fatal(err)
			}
		},
		"dismissed requirement": func(f claimParityFixture) {
			requirement := f.requirement("req-dismissed")
			version := f.proposeRequirement(requirement, core.RequirementVersion{Origin: core.RequirementOriginImplementation, OriginTaskID: claimParityTask})
			if _, _, err := f.st.DismissRequirementVersion(f.ctx, requirement.ID, version.Version); err != nil {
				f.t.Fatal(err)
			}
		},
		"requirement retired by a later confirmation": func(f claimParityFixture) {
			requirement := f.requirement("req-retired")
			f.proposeRequirement(requirement, core.RequirementVersion{Origin: core.RequirementOriginImplementation, OriginTaskID: claimParityTask})
			later := f.proposeRequirement(requirement, core.RequirementVersion{Origin: core.RequirementOriginOperator})
			if _, _, err := f.st.ConfirmRequirementVersion(f.ctx, requirement.ID, later.Version); err != nil {
				f.t.Fatal(err)
			}
		},
		"archived System Design": func(f claimParityFixture) {
			document := f.design("design-archived")
			f.proposeDesign(document, core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: claimParityTask})
			if err := f.st.ArchiveSystemDesign(f.ctx, document.ID, "operator", nil); err != nil {
				f.t.Fatal(err)
			}
		},
		"another workspace's System Design": func(f claimParityFixture) {
			sibling := claimParityFixture{ctx: store.WithWorkspace(store.WithActor(f.t.Context(), store.SystemActor()), "sibling"), st: f.st, t: f.t}
			document := sibling.design("design-sibling")
			sibling.proposeDesign(document, core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: claimParityTask})
		},
	}
	for name, setup := range cases {
		for _, stage := range []core.Stage{core.StageVerify, core.StageReview} {
			t.Run(fmt.Sprintf("%s/%s", name, stage), func(t *testing.T) {
				ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
				st := store.NewMemory()
				// A verify order binds to the task's verify policy and submitted
				// head, so the verify variant carries both (DEC-43).
				const head = "0123456789abcdef0123456789abcdef01234567"
				for _, id := range []string{claimParityTask, "claim-parity-other"} {
					task := core.Task{ID: id, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageImplement, ReviewedHeadSHA: head, CreatedAt: time.Now().UTC()}
					task.SetupContract.VerifyStage = stage == core.StageVerify
					if err := st.CreateTask(ctx, task); err != nil {
						t.Fatal(err)
					}
				}
				fixture := claimParityFixture{ctx: ctx, st: st, t: t}
				fixture.submitImplementation(claimParityTask)
				fixture.submitImplementation("claim-parity-other")
				if err := storetest.ProjectTaskNextStage(ctx, st, claimParityTask, stage); err != nil {
					t.Fatal(err)
				}
				if err := storetest.ProjectTaskNextStage(ctx, st, "claim-parity-other", stage); err != nil {
					t.Fatal(err)
				}
				setup(fixture)
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
				requireNoProposalWaitFields(t, server, claimParityTask)
				if action := readNextActionKind(t, server, claimParityTask); action.Kind == "document_proposal" || len(action.Details.PendingProposals) != 0 {
					t.Fatalf("next action kind=%s pending_proposals=%+v", action.Kind, action.Details.PendingProposals)
				}

				cfg := &config.Config{Routing: config.Routing{Stages: map[string]config.StageRoute{
					string(stage): {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
				}}}
				service := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
				listed, err := service.List(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, order := range listed {
					if order.ID == orderID && !order.Claimable {
						t.Fatalf("listing withheld %s: %+v", orderID, order)
					}
				}
				if _, err := service.Claim(ctx, orderID, core.WorkOrderClaim{SessionID: "service-" + string(stage), ClientToken: "service-token", ClaimantID: "claimant", Lease: time.Minute}); err != nil {
					t.Fatalf("service claim error=%v", err)
				}
			})
		}
	}
}

// A pending proposal leaves the operator inbox and attention counts as they
// were: it stays listed among the decisions with confirm and dismiss, and
// deciding it lowers the count by exactly one, while the origin task's own row
// never folds into it (req-260810-23b69f REQ-1, AC-1.1, AC-4.2;
// req-operator-next-action AC-2.6, AC-2.7).
func TestPendingProposalStaysAnInboxDecisionWithoutHoldingItsTask(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: claimParityTask, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	fixture := claimParityFixture{ctx: ctx, st: st, t: t}
	fixture.submitImplementation(claimParityTask)
	design := fixture.design("design-inbox")
	version := fixture.proposeDesign(design, core.SystemDesignVersion{Origin: core.SystemDesignOriginImplementation, OriginTaskID: claimParityTask})
	server := NewServer(st)
	server.BearerToken, server.Workspace = "operator-token", "demo"
	requireNoProposalWaitFields(t, server, claimParityTask)
	read := func() (inbox struct {
		Items []inboxItem `json:"items"`
	}, attention attentionSummary) {
		t.Helper()
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/next-actions?workspace_id=demo", nil)))
		if response.Code != http.StatusOK {
			t.Fatalf("inbox status=%d body=%s", response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &inbox); err != nil {
			t.Fatal(err)
		}
		pending := httptest.NewRecorder()
		server.Handler().ServeHTTP(pending, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/pending-proposals?workspace_id=demo", nil)))
		var body pendingProposalsResponse
		if err := json.Unmarshal(pending.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return inbox, body.Attention
	}
	inbox, before := read()
	found := false
	for _, item := range inbox.Items {
		if item.Kind == "document_proposal" && item.Proposal != nil && item.Proposal.ID == design.ID && item.Proposal.Version == version.Version && item.Band == "decisions" && item.IfIgnored == "The proposal stays pending." {
			found = true
		}
	}
	if !found || before.PendingProposalCount != 1 {
		t.Fatalf("inbox lost the pending proposal: %+v attention=%+v", inbox.Items, before)
	}
	decisions := 0
	for _, item := range inbox.Items {
		if item.Band == "decisions" {
			decisions++
		}
	}
	if before.OperatorDecisions != decisions || before.Total != decisions || decisions == 0 {
		t.Fatalf("badge %+v differs from the %d inbox decisions", before, decisions)
	}
	if _, _, err := st.DismissSystemDesignVersion(ctx, design.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	_, after := read()
	if after.PendingProposalCount != before.PendingProposalCount-1 || after.OperatorDecisions != before.OperatorDecisions-1 {
		t.Fatalf("attention before=%+v after=%+v", before, after)
	}
}
