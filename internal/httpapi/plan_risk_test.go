package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// riskTierLine is a line in the retired plan tier format. A plan that still
// holds it is accepted, and the line is plan text (feature-risk-based-review
// RR-13).
const riskTierLine = "Risk tier: medium — The plan changes intake."

// tieredIntakePlan holds the retired tier line; intakeTestPlan holds none.
var tieredIntakePlan = strings.Replace(intakeTestPlan, "## Risks\n- None.", "## Risks\n- None.\n\n"+riskTierLine, 1)

// malformedTierIntakePlan holds a tier line the retired grammar refused.
var malformedTierIntakePlan = strings.Replace(intakeTestPlan, "## Risks\n- None.", "## Risks\n- None.\n\nRisk tier: severe — Not a tier.\n\nRisk tier: low — Twice.", 1)

func (f *sessionIntakeFixture) setRiskMode(mode riskpolicy.Mode) {
	f.cfgMu.Lock()
	defer f.cfgMu.Unlock()
	copied := *f.cfg
	copied.RiskPolicy = &riskpolicy.Settings{Mode: mode}
	f.cfg = &copied
}

// planRiskOrders returns the task's risk orders whose subject is a plan
// version.
func planRiskOrders(t *testing.T, st store.Store, ctx context.Context, taskID string) []core.RiskOrder {
	t.Helper()
	orders, err := st.ListTaskRiskOrders(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var plans []core.RiskOrder
	for _, order := range orders {
		if order.Subject.PlanVersion > 0 {
			plans = append(plans, order)
		}
	}
	return plans
}

// TestIntakePlanRiskOrder covers REST JSON, REST multipart, and MCP intake:
// every plan is accepted with or without a tier line, malformed or repeated
// lines included; a plan filed for approval gets one risk order for plan
// version 1 when the frozen mode is record or enforce; a plan approved in the
// act that files it gets none; an off task gets none; and no intake records a
// rating from plan text (req-risk-based-review AC-4.7, AC-1.1;
// feature-risk-based-review RR-13; component-plan-state invariant 2).
func TestIntakePlanRiskOrder(t *testing.T) {
	f := newSessionIntakeFixture(t)
	token := humanIntakeToken(core.WorkspaceRoleOperator)
	ctx := store.WithWorkspace(t.Context(), "alpha")
	requireOrders := func(t *testing.T, label, taskID string, want int) {
		t.Helper()
		orders := planRiskOrders(t, f.store, ctx, taskID)
		if len(orders) != want {
			t.Fatalf("%s: plan risk orders=%+v, want %d", label, orders, want)
		}
		for _, order := range orders {
			if order.Subject.PlanVersion != core.IntakePlanVersion || order.Order.Stage != core.StageRisk || order.Order.State != core.WorkOrderQueued {
				t.Fatalf("%s: risk order=%+v, want a queued order for plan version 1", label, order)
			}
		}
		if ratings, err := f.store.ListTaskRiskRatings(ctx, taskID); err != nil || len(ratings) != 0 {
			t.Fatalf("%s: intake recorded ratings %+v err=%v", label, ratings, err)
		}
	}
	for _, mode := range []riskpolicy.Mode{riskpolicy.ModeEnforce, riskpolicy.ModeRecord, riskpolicy.ModeOff} {
		f.setRiskMode(mode)
		pending := 1
		if mode == riskpolicy.ModeOff {
			pending = 0
		}
		for _, plan := range []struct{ name, text string }{{"no tier line", intakeTestPlan}, {"tier line", tieredIntakePlan}, {"malformed tier lines", malformedTierIntakePlan}} {
			label := string(mode) + " " + plan.name
			filed := f.postTaskJSON(t, token, intakeFields(map[string]any{"plan": plan.text}))
			if filed.status != http.StatusCreated || filed.task.PlanState != core.PlanAwaitingApproval {
				t.Fatalf("%s (json): status=%d body=%s", label, filed.status, filed.body)
			}
			requireOrders(t, label+" (json)", filed.task.ID, pending)
			multipart := f.postTaskMultipart(t, token, intakeFields(map[string]any{"plan": plan.text}), "")
			if multipart.status != http.StatusCreated || multipart.task.PlanState != core.PlanAwaitingApproval {
				t.Fatalf("%s (multipart): status=%d body=%s", label, multipart.status, multipart.body)
			}
			requireOrders(t, label+" (multipart)", multipart.task.ID, pending)
			approved := f.postTaskJSON(t, token, intakeFields(map[string]any{"plan": plan.text, "plan_approved": true}))
			if approved.status != http.StatusCreated || approved.task.PlanState != core.PlanCompleted {
				t.Fatalf("%s approved: status=%d body=%s", label, approved.status, approved.body)
			}
			requireOrders(t, label+" approved", approved.task.ID, 0)
			mcp := f.callMCP(t, token, "create_task", intakeArgs("mcp-plan-risk-"+string(mode)+"-"+strings.ReplaceAll(plan.name, " ", "-"), map[string]any{"plan": plan.text}))
			if mcp.errText != "" || mcp.task.PlanState != core.PlanAwaitingApproval {
				t.Fatalf("%s (mcp): err=%q task=%+v", label, mcp.errText, mcp.task)
			}
			requireOrders(t, label+" (mcp)", mcp.task.ID, pending)
		}
	}
}

// seedRiskPlanTask files a task at plan needed whose frozen risk mode is mode.
func seedRiskPlanTask(t *testing.T, st store.Store, id string, mode riskpolicy.Mode) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	task := core.Task{ID: id, Workspace: "demo", Repo: "conveyor", Branch: "conveyor/task-" + id, PolicyVersion: 1,
		State: core.TaskQueued, NextStage: core.StageTriage, PlanState: core.PlanNeeded, CreatedAt: time.Now().UTC()}
	task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
}

// TestAttachPlanRiskOrder covers POST /v1/tasks/{id}/plan: every plan is
// accepted with or without a tier line in every mode; the attached version
// gets one risk order under record and enforce; and the plan reads serve no
// tier from the plan text (req-risk-based-review AC-4.7;
// feature-risk-based-review RR-13; DEC-134(2); component-plan-state invariant
// 5).
func TestAttachPlanRiskOrder(t *testing.T) {
	_, st, handler := policyHTTPFixture(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	tiered := strings.Replace(httpPlan, "## Risks\n- None.", "## Risks\n- None.\n\nRisk tier: high — Attach changes the plan.", 1)
	malformed := strings.Replace(httpPlan, "## Risks\n- None.", "## Risks\n- None.\n\n- Risk tier: high — Listed.", 1)
	for _, tc := range []struct {
		task   string
		mode   riskpolicy.Mode
		plan   string
		orders int
	}{
		{"risk-enforce-plain", riskpolicy.ModeEnforce, httpPlan, 1},
		{"risk-enforce-tiered", riskpolicy.ModeEnforce, tiered, 1},
		{"risk-enforce-malformed", riskpolicy.ModeEnforce, malformed, 1},
		{"risk-record-plain", riskpolicy.ModeRecord, httpPlan, 1},
		{"risk-record-malformed", riskpolicy.ModeRecord, malformed, 1},
		{"risk-off-tiered", riskpolicy.ModeOff, tiered, 0},
	} {
		seedRiskPlanTask(t, st, tc.task, tc.mode)
		response := planActsRequest(t, handler, http.MethodPost, "/v1/tasks/"+tc.task+"/plan",
			planActsJSON(t, map[string]any{"plan": tc.plan, "reason": "operator plan", "request_id": "attach-" + tc.task}))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.task, response.Code, response.Body)
		}
		orders := planRiskOrders(t, st, ctx, tc.task)
		if len(orders) != tc.orders {
			t.Fatalf("%s: plan risk orders=%+v, want %d", tc.task, orders, tc.orders)
		}
		if tc.orders == 1 && (orders[0].Subject.PlanVersion != 1 || orders[0].Order.State != core.WorkOrderQueued) {
			t.Fatalf("%s: risk order=%+v, want a queued order for the attached version", tc.task, orders[0])
		}
		for _, path := range []string{"/v1/tasks/" + tc.task + "/activity", "/v1/tasks/" + tc.task + "/spec"} {
			read := planActsRequest(t, handler, http.MethodGet, path, "")
			if read.Code != http.StatusOK {
				t.Fatalf("GET %s: %d %s", path, read.Code, read.Body)
			}
			body := read.Body.String()
			if strings.HasSuffix(path, "/activity") {
				var detail struct {
					Spec json.RawMessage `json:"spec"`
				}
				if err := json.Unmarshal(read.Body.Bytes(), &detail); err != nil || len(detail.Spec) == 0 {
					t.Fatalf("GET %s: spec missing: %s", path, body)
				}
				body = string(detail.Spec)
			}
			if strings.Contains(body, `"risk_tier"`) || strings.Contains(body, `"risk_reason"`) {
				t.Fatalf("GET %s served a plan rating from plan text: %s", path, body)
			}
		}
	}
}

// TestPlanApprovalWaitsForRiskOrder covers the review route's approve act at
// the plan approval gate: under enforce, while the version's risk order is
// open, it answers 409 plan_risk_rating_open naming the order and records no
// intervention, no approval, and no plan-state move; once the order is
// cancelled, approval succeeds. Under record it never waits
// (req-risk-based-review AC-4.8, AC-1.6; req-task-plan-state AC-1.3;
// feature-risk-based-review RR-13).
func TestPlanApprovalWaitsForRiskOrder(t *testing.T) {
	for _, mode := range []riskpolicy.Mode{riskpolicy.ModeEnforce, riskpolicy.ModeRecord} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := core.Task{ID: "plan-risk-approve-" + string(mode), Workspace: "demo", Repo: "conveyor", PolicyVersion: 1,
				State: core.TaskRunning, NextStage: core.StageSpec, PlanState: core.PlanNeeded, CreatedAt: time.Now()}
			task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-spec", TaskID: task.ID, Stage: core.StageSpec, State: core.JobDone}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{
				Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true,
				PlanState:   &taskops.PlanStateMove{To: core.PlanAwaitingApproval, Cause: core.PlanCausePlanSubmitted},
				PlanVersion: &core.SpecVersion{Content: httpPlan, Acceptance: []byte(`[]`), Decomposition: []byte(`[]`)},
			}); err != nil {
				t.Fatal(err)
			}
			orders := planRiskOrders(t, st, ctx, task.ID)
			if len(orders) != 1 {
				t.Fatalf("plan risk orders=%+v, want one", orders)
			}
			server := NewServer(st)
			server.Workspace, server.BearerToken = "demo", "token"
			server.OnIntervention = dispatch.New(st, &config.Config{Workspace: "demo"}, nil).HandleIntervention
			approve := func() *httptest.ResponseRecorder {
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, authenticatedSpecApprovalRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/review", `{"action":"approve","reason_code":"plan-approved"}`))
				return response
			}
			if mode == riskpolicy.ModeEnforce {
				refused := approve()
				var body struct {
					Error           string `json:"error"`
					Message         string `json:"message"`
					RiskWorkOrderID string `json:"risk_work_order_id"`
					PlanVersion     int    `json:"plan_version"`
				}
				if refused.Code != http.StatusConflict || json.Unmarshal(refused.Body.Bytes(), &body) != nil ||
					body.Error != store.PlanRiskRatingOpenCode || body.RiskWorkOrderID != orders[0].Order.ID || body.PlanVersion != 1 || body.Message == "" {
					t.Fatalf("refused approve: %d %s", refused.Code, refused.Body)
				}
				current, err := st.GetTask(ctx, task.ID)
				if err != nil || current.PlanState != core.PlanAwaitingApproval || current.GateKind != core.GateKindPlanApproval {
					t.Fatalf("refused approve moved the task: %+v err=%v", current, err)
				}
				if interventions, err := st.ListInterventions(ctx, task.ID); err != nil || len(interventions) != 0 {
					t.Fatalf("refused approve recorded interventions %+v err=%v", interventions, err)
				}
				if _, approved, err := st.GetApprovedSpecVersion(ctx, task.ID); err != nil || approved {
					t.Fatalf("refused approve approved the plan: %t err=%v", approved, err)
				}
				if _, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdCancel, func(lease taskops.TaskLease) (core.WorkOrder, error) {
					order, _, cancelErr := st.CancelRiskOrderCommand(ctx, lease, store.RiskOrderCancelRequest{WorkOrderID: orders[0].Order.ID, Reason: "operator ended the wait"})
					return order, cancelErr
				}); err != nil {
					t.Fatal(err)
				}
			}
			accepted := approve()
			if accepted.Code != http.StatusAccepted {
				t.Fatalf("approve: %d %s", accepted.Code, accepted.Body)
			}
			current, err := st.GetTask(ctx, task.ID)
			if err != nil || current.PlanState != core.PlanCompleted {
				t.Fatalf("approved task=%+v err=%v", current, err)
			}
			after := planRiskOrders(t, st, ctx, task.ID)
			if mode == riskpolicy.ModeRecord && after[0].Order.State != core.WorkOrderQueued {
				t.Fatalf("record approval ended the risk order: %+v", after[0])
			}
		})
	}
}
