package storetest

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// RunRiskPolicyFreezeConformance proves on every backend that a workspace
// risk policy saves and reads back with its full table, that a task filed
// without an enabled policy stores the contract bytes it stored before risk
// review existed, and that a task's frozen risk policy survives a later
// workspace edit unchanged (req-risk-based-review REQ-1, AC-1.1, AC-1.3,
// AC-1.5; feature-risk-based-review RR-1).
func RunRiskPolicyFreezeConformance(t *testing.T, x Fixture) {
	st, ctx, workspace := x.Backend, x.Context, x.Workspace
	saveRisk := func(t *testing.T, settings *riskpolicy.Settings) config.UpdateReceipt {
		t.Helper()
		current, err := st.WorkspaceConfig(ctx)
		requireOK(t, err)
		next, err := st.RuntimeConfig(ctx, x.Config)
		requireOK(t, err)
		next.RiskPolicy = settings
		receipt, err := st.UpdateWorkspaceConfig(ctx, current.Version, next)
		requireOK(t, err)
		return receipt
	}
	fileTask := func(t *testing.T) core.Task {
		t.Helper()
		cfg, err := st.RuntimeConfig(ctx, x.Config)
		requireOK(t, err)
		task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskQueued, NextStage: core.StageTriage, SetupContract: cfg.FreezePolicy(), CreatedAt: time.Now().UTC()}
		task.Branch = "conveyor/task-" + task.ID
		requireOK(t, st.CreateTask(ctx, task))
		stored, err := st.GetTask(ctx, task.ID)
		requireOK(t, err)
		return stored
	}
	contractJSON := func(t *testing.T, contract config.PolicyContract) string {
		t.Helper()
		data, err := json.Marshal(contract)
		requireOK(t, err)
		return string(data)
	}

	t.Run("OffStoresPreRiskContract", func(t *testing.T) {
		withoutRisk := fileTask(t)
		if withoutRisk.SetupContract.Risk != nil {
			t.Fatalf("task filed without a risk policy froze %+v", withoutRisk.SetupContract.Risk)
		}
		saveRisk(t, &riskpolicy.Settings{Mode: riskpolicy.ModeOff})
		withOff := fileTask(t)
		if withOff.SetupContract.Risk != nil {
			t.Fatalf("task filed with mode off froze %+v", withOff.SetupContract.Risk)
		}
		got, want := contractJSON(t, withOff.SetupContract), contractJSON(t, withoutRisk.SetupContract)
		if got != want || json.Valid([]byte(got)) && containsRiskKey(got) {
			t.Fatalf("mode off stored %s, want the pre-risk contract %s", got, want)
		}
	})

	t.Run("WorkspaceEditLeavesFrozenPolicy", func(t *testing.T) {
		receipt := saveRisk(t, &riskpolicy.Settings{Mode: riskpolicy.ModeEnforce})
		if !slices.Contains(receipt.Sections, "risk_policy") {
			t.Fatalf("receipt sections=%v, want risk_policy", receipt.Sections)
		}
		saved, err := st.WorkspaceConfig(ctx)
		requireOK(t, err)
		policy, err := saved.Document.RiskPolicy.Resolve()
		if err != nil || policy != (riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()}) {
			t.Fatalf("saved risk policy=%+v err=%v, want enforce with the default table", policy, err)
		}
		filed := fileTask(t)
		frozen := filed.SetupContract.Risk
		if frozen == nil || *frozen != policy {
			t.Fatalf("filed task froze %+v, want %+v", frozen, policy)
		}
		before := contractJSON(t, filed.SetupContract)

		raised := 3
		saveRisk(t, &riskpolicy.Settings{Mode: riskpolicy.ModeRecord, Tiers: &riskpolicy.SettingsTable{
			Low:    &riskpolicy.SettingsRow{AgentReviews: &raised, HumanApprovals: &raised},
			Medium: &riskpolicy.SettingsRow{AgentReviews: &raised, HumanApprovals: &raised},
			High:   &riskpolicy.SettingsRow{AgentReviews: &raised, HumanApprovals: &raised},
		}})
		after, err := st.GetTask(ctx, filed.ID)
		requireOK(t, err)
		if got := contractJSON(t, after.SetupContract); got != before || after.SetupContract.Risk == nil || *after.SetupContract.Risk != policy {
			t.Fatalf("workspace edit changed the filed task's contract:\n got %s\nwant %s", got, before)
		}
		runtime, err := st.RuntimeConfig(ctx, x.Config)
		requireOK(t, err)
		projected, err := runtime.WithPolicy(after.SetupContract).RiskPolicyValue()
		if err != nil || projected == nil || *projected != policy {
			t.Fatalf("task projection read %+v, %v; want the frozen policy", projected, err)
		}
		later := fileTask(t)
		if later.SetupContract.Risk == nil || later.SetupContract.Risk.Mode != riskpolicy.ModeRecord || later.SetupContract.Risk.Tiers.High.AgentReviews != 3 {
			t.Fatalf("a task filed after the edit froze %+v", later.SetupContract.Risk)
		}

		saveRisk(t, nil)
		cleared, err := st.WorkspaceConfig(ctx)
		requireOK(t, err)
		if cleared.Document.RiskPolicy != nil {
			t.Fatalf("removed risk policy read back as %+v", cleared.Document.RiskPolicy)
		}
		if again, err := st.GetTask(ctx, filed.ID); err != nil || contractJSON(t, again.SetupContract) != before {
			t.Fatalf("removing the workspace policy changed the filed task: %v", err)
		}
	})
}

func containsRiskKey(encoded string) bool {
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(encoded), &members); err != nil {
		return false
	}
	_, ok := members["risk"]
	return ok
}
