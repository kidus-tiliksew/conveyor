package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// riskTierLine is a well-formed plan risk tier line (req-risk-based-review
// REQ-4).
const riskTierLine = "Risk tier: medium — The plan changes intake."

// tieredIntakePlan states a tier; intakeTestPlan states none.
var tieredIntakePlan = strings.Replace(intakeTestPlan, "## Risks\n- None.", "## Risks\n- None.\n\n"+riskTierLine, 1)

func (f *sessionIntakeFixture) setRiskMode(mode riskpolicy.Mode) {
	f.cfgMu.Lock()
	defer f.cfgMu.Unlock()
	copied := *f.cfg
	copied.RiskPolicy = &riskpolicy.Settings{Mode: mode}
	f.cfg = &copied
}

// TestIntakePlanRiskTier covers REST JSON, REST multipart, a keyed multipart
// retry, and MCP intake: under enforce a plan with no tier is refused with
// nothing recorded, before the idempotency lookup; a malformed tier line is
// refused in every mode; record and off accept a plan with no tier; and an
// approved intake plan's stated tier is recorded as a plan rating
// (req-risk-based-review AC-4.2, AC-4.3, AC-4.5; feature-risk-based-review
// RR-4; component-plan-state invariant 2).
func TestIntakePlanRiskTier(t *testing.T) {
	f := newSessionIntakeFixture(t)
	token := humanIntakeToken(core.WorkspaceRoleOperator)
	ctx := store.WithWorkspace(t.Context(), "alpha")
	seed := f.postTaskMultipart(t, token, intakeFields(nil), "plan-risk-key")
	if seed.status != http.StatusCreated {
		t.Fatalf("seed status=%d body=%s", seed.status, seed.body)
	}
	malformed := strings.Replace(intakeTestPlan, "## Risks\n- None.", "## Risks\n- None.\n\nRisk tier: severe — Not a tier.", 1)
	refuse := func(t *testing.T, label string, fields map[string]any, want string) {
		t.Helper()
		for _, surface := range []string{"json", "multipart", "multipart retry", "mcp"} {
			tasks, titles, notified := f.taskCount(t), f.titles.Load(), f.notified.Load()
			switch surface {
			case "mcp":
				result := f.callMCP(t, token, "create_task", intakeArgs("mcp-plan-risk", map[string]any{"plan": fields["plan"]}))
				if !strings.HasPrefix(result.errText, invalidPlanIntake+": ") || !strings.Contains(result.errText, want) {
					t.Fatalf("%s (mcp): err=%q", label, result.errText)
				}
			default:
				var result restIntakeResult
				switch surface {
				case "json":
					result = f.postTaskJSON(t, token, intakeFields(fields))
				case "multipart":
					result = f.postTaskMultipart(t, token, intakeFields(fields), "")
				default:
					result = f.postTaskMultipart(t, token, intakeFields(fields), "plan-risk-key")
				}
				if result.status != http.StatusBadRequest || result.code() != invalidPlanIntake || !strings.Contains(result.body, want) {
					t.Fatalf("%s (%s): status=%d body=%s", label, surface, result.status, result.body)
				}
			}
			f.requireNoSideEffects(t, label+" ("+surface+")", tasks, titles, notified)
		}
	}

	f.setRiskMode(riskpolicy.ModeEnforce)
	refuse(t, "enforce without a tier", map[string]any{"plan": intakeTestPlan}, "plan_risk_tier_missing")
	refuse(t, "enforce approved without a tier", map[string]any{"plan": intakeTestPlan, "plan_approved": true}, "plan_risk_tier_missing")
	refuse(t, "enforce malformed tier", map[string]any{"plan": malformed}, "must be low, medium, or high")
	enforced := f.postTaskJSON(t, token, intakeFields(map[string]any{"plan": tieredIntakePlan}))
	if enforced.status != http.StatusCreated || enforced.task.PlanState != core.PlanAwaitingApproval {
		t.Fatalf("enforce with a tier: status=%d body=%s", enforced.status, enforced.body)
	}
	if ratings, err := f.store.ListTaskRiskRatings(ctx, enforced.task.ID); err != nil || len(ratings) != 0 {
		t.Fatalf("an unapproved intake plan was rated: %+v err=%v", ratings, err)
	}
	approved := f.postTaskJSON(t, token, intakeFields(map[string]any{"plan": tieredIntakePlan, "plan_approved": true}))
	if approved.status != http.StatusCreated || approved.task.PlanState != core.PlanCompleted {
		t.Fatalf("enforce approved with a tier: status=%d body=%s", approved.status, approved.body)
	}
	ratings, err := f.store.ListTaskRiskRatings(ctx, approved.task.ID)
	if err != nil || len(ratings) != 1 || ratings[0].Source != core.RiskSourcePlan || ratings[0].Tier != riskpolicy.TierMedium ||
		ratings[0].Reason != "The plan changes intake." || ratings[0].PlanVersion != core.IntakePlanVersion {
		t.Fatalf("approved intake plan ratings=%+v err=%v", ratings, err)
	}
	stored, err := f.store.GetTask(ctx, approved.task.ID)
	if err != nil || stored.PlanState != core.PlanCompleted {
		t.Fatalf("rated task plan state=%s err=%v", stored.PlanState, err)
	}

	f.setRiskMode(riskpolicy.ModeRecord)
	refuse(t, "record malformed tier", map[string]any{"plan": malformed}, "must be low, medium, or high")
	recorded := f.postTaskJSON(t, token, intakeFields(map[string]any{"plan": intakeTestPlan, "plan_approved": true}))
	if recorded.status != http.StatusCreated || recorded.task.PlanState != core.PlanCompleted {
		t.Fatalf("record without a tier: status=%d body=%s", recorded.status, recorded.body)
	}
	if ratings, err := f.store.ListTaskRiskRatings(ctx, recorded.task.ID); err != nil || len(ratings) != 0 {
		t.Fatalf("a plan with no tier was rated: %+v err=%v", ratings, err)
	}
	if mcp := f.callMCP(t, token, "create_task", intakeArgs("mcp-plan-risk-record", map[string]any{"plan": intakeTestPlan})); mcp.errText != "" {
		t.Fatalf("record MCP intake without a tier: %q", mcp.errText)
	}

	f.setRiskMode(riskpolicy.ModeOff)
	refuse(t, "off malformed tier", map[string]any{"plan": malformed}, "must be low, medium, or high")
	off := f.postTaskJSON(t, token, intakeFields(map[string]any{"plan": tieredIntakePlan, "plan_approved": true}))
	if off.status != http.StatusCreated {
		t.Fatalf("off with a tier: status=%d body=%s", off.status, off.body)
	}
	if ratings, err := f.store.ListTaskRiskRatings(ctx, off.task.ID); err != nil || len(ratings) != 0 {
		t.Fatalf("an off task was rated: %+v err=%v", ratings, err)
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

// TestAttachPlanRiskTier covers POST /v1/tasks/{id}/plan: under the task's
// frozen enforce mode a plan with no tier answers 400 and leaves the plan
// versions, plan state, and events as they were; a malformed tier line is
// refused in every mode; record accepts a plan with no tier; and the task
// detail read serves the attached version's own tier and reason
// (req-risk-based-review AC-4.2, AC-4.4, AC-4.5; feature-risk-based-review
// RR-4; component-plan-state invariant 5).
func TestAttachPlanRiskTier(t *testing.T) {
	_, st, handler := policyHTTPFixture(t)
	seedRiskPlanTask(t, st, "risk-enforce", riskpolicy.ModeEnforce)
	seedRiskPlanTask(t, st, "risk-record", riskpolicy.ModeRecord)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	tiered := strings.Replace(httpPlan, "## Risks\n- None.", "## Risks\n- None.\n\nRisk tier: high — Attach changes the plan.", 1)
	malformed := strings.Replace(httpPlan, "## Risks\n- None.", "## Risks\n- None.\n\n- Risk tier: high — Listed.", 1)
	attach := func(task, plan, requestID string) (int, string) {
		response := planActsRequest(t, handler, http.MethodPost, "/v1/tasks/"+task+"/plan",
			planActsJSON(t, map[string]any{"plan": plan, "reason": "operator plan", "request_id": requestID}))
		return response.Code, response.Body.String()
	}
	before, err := st.ListEvents(ctx, "risk-enforce")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, task, plan, requestID, want string }{
		{"enforce without a tier", "risk-enforce", httpPlan, "risk-attach-1", "plan_risk_tier_missing"},
		{"enforce malformed tier", "risk-enforce", malformed, "risk-attach-2", "list marker"},
		{"record malformed tier", "risk-record", malformed, "risk-attach-3", "list marker"},
	} {
		code, body := attach(tc.task, tc.plan, tc.requestID)
		if code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
			t.Fatalf("%s: %d %s", tc.name, code, body)
		}
	}
	if _, ok, err := st.GetLatestSpecVersion(ctx, "risk-enforce"); err != nil || ok {
		t.Fatalf("a refused attach recorded a plan version: ok=%t err=%v", ok, err)
	}
	after, err := st.ListEvents(ctx, "risk-enforce")
	if err != nil || len(after) != len(before) {
		t.Fatalf("a refused attach appended events: %d -> %d err=%v", len(before), len(after), err)
	}
	if task, err := st.GetTask(ctx, "risk-enforce"); err != nil || task.PlanState != core.PlanNeeded {
		t.Fatalf("a refused attach moved the plan state: %+v err=%v", task.PlanState, err)
	}
	// The refused request ID records no replay: the corrected plan under the
	// same ID is recorded.
	if code, body := attach("risk-enforce", tiered, "risk-attach-1"); code != http.StatusOK {
		t.Fatalf("enforce with a tier: %d %s", code, body)
	}
	if code, body := attach("risk-record", httpPlan, "risk-attach-4"); code != http.StatusOK {
		t.Fatalf("record without a tier: %d %s", code, body)
	}

	for _, tc := range []struct {
		task   string
		tier   riskpolicy.Tier
		reason string
	}{{"risk-enforce", riskpolicy.TierHigh, "Attach changes the plan."}, {"risk-record", "", ""}} {
		for _, path := range []string{"/v1/tasks/" + tc.task + "/activity", "/v1/tasks/" + tc.task + "/spec"} {
			response := planActsRequest(t, handler, http.MethodGet, path, "")
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s: %d %s", path, response.Code, response.Body)
			}
			var spec core.SpecVersion
			if strings.HasSuffix(path, "/activity") {
				var detail struct {
					Spec *core.SpecVersion `json:"spec"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil || detail.Spec == nil {
					t.Fatalf("GET %s: spec missing: %s", path, response.Body)
				}
				spec = *detail.Spec
			} else if err := json.Unmarshal(response.Body.Bytes(), &spec); err != nil {
				t.Fatal(err)
			}
			if spec.RiskTier != tc.tier || spec.RiskReason != tc.reason {
				t.Fatalf("GET %s: plan tier=%q reason=%q, want %q %q", path, spec.RiskTier, spec.RiskReason, tc.tier, tc.reason)
			}
			if tc.tier == "" && strings.Contains(response.Body.String(), `"risk_tier"`) {
				t.Fatalf("GET %s served a tier for a plan that states none: %s", path, response.Body)
			}
		}
	}
}
