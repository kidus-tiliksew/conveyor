package storetest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// ReduceReadEgressClockOrders is the consumer fold of a full snapshot read
// onto the lifecycle order ListTaskWorkOrders serves. Snapshot bodies are
// cleared; the digest and every other WorkOrder field stay.
func ReduceReadEgressClockOrders(orders []core.WorkOrder) []core.WorkOrder {
	out := make([]core.WorkOrder, 0, len(orders))
	for _, order := range orders {
		order.GovernanceSnapshot = nil
		order.ServedRequirementSnapshot = nil
		out = append(out, order)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TaskID != out[j].TaskID {
			return out[i].TaskID < out[j].TaskID
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ReadEgressRiskBatch is one selected task's ratings and orders, the fold
// ProjectTaskRisks reads from the per-task APIs and from the batch APIs.
type ReadEgressRiskBatch struct {
	TaskID  string            `json:"task_id"`
	Ratings []core.RiskRating `json:"ratings"`
	Orders  []core.RiskOrder  `json:"orders"`
}

// ReduceReadEgressRiskBatch canonicalizes the selected tasks' ratings and
// orders by task ID so a membership or payload difference still changes the
// digest and empty slices stay non-nil.
func ReduceReadEgressRiskBatch(ids []string, ratings map[string][]core.RiskRating, orders map[string][]core.RiskOrder) []ReadEgressRiskBatch {
	out := make([]ReadEgressRiskBatch, 0, len(ids))
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for _, id := range sorted {
		row := ReadEgressRiskBatch{TaskID: id, Ratings: ratings[id], Orders: orders[id]}
		if row.Ratings == nil {
			row.Ratings = []core.RiskRating{}
		}
		if row.Orders == nil {
			row.Orders = []core.RiskOrder{}
		}
		out = append(out, row)
	}
	return out
}

func clockPinGovernance(pinBytes int) core.GovernanceSnapshot {
	if pinBytes < 1 {
		pinBytes = 1
	}
	return core.GovernanceSnapshot{
		Designs: []core.GovernanceDesignContext{{
			ID: "component-clock-pins", Title: "Clock pins", Category: "component", Version: 1,
			Content: strings.Repeat("c", pinBytes), Governs: []core.GovernedScope{},
		}},
		Decisions:              []core.Decision{},
		PendingDesignProposals: []core.PendingSystemDesignProposal{},
	}
}

func clockPinServed() []core.ServedRequirementContext {
	return []core.ServedRequirementContext{{ID: "req-clock-pins", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep the pin"}}}}
}

func seedClockStage(t testing.TB, ctx context.Context, st store.Store, name string, stage core.Stage) (core.Task, core.WorkOrder) {
	t.Helper()
	ws, _ := store.WorkspaceFromContext(ctx)
	id := "egress-clock-" + name + "-" + core.NewTaskID()
	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{ID: id, Workspace: ws, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id,
		State: core.TaskRunning, NextStage: stage, CreatedAt: now}
	if stage == core.StageVerify {
		task.ReviewedHeadSHA = "submitted-head"
		task.SetupContract.VerifyStage = true
	}
	scopedFixtureOK(t, st.CreateTask(ctx, task))
	job := core.Job{ID: id + "-" + string(stage) + "-1", TaskID: id, Stage: stage, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: stage, State: core.WorkOrderQueued,
		HeadSHA: task.ReviewedHeadSHA, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	created, err := CreateStageWorkOrder(ctx, st, job, order)
	scopedFixtureOK(t, err)
	if !created {
		t.Fatal("clock stage was not created")
	}
	return task, order
}

func claimClockOrder(t testing.TB, ctx context.Context, st store.Store, order core.WorkOrder, name string, lease, execution time.Duration, pinBytes int) core.WorkOrder {
	t.Helper()
	governance := clockPinGovernance(pinBytes)
	claimed, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{
		ClaimantID: core.TaskRunClaimantID("worker"), SessionID: "clock-" + name, ClientToken: "clock-" + name,
		Lease: lease, ExecutionTimeout: execution, Requirements: clockPinServed(), Governance: &governance,
	})
	scopedFixtureOK(t, err)
	if claimed.GovernanceSnapshotDigest == "" || claimed.ServedRequirementSnapshot == nil {
		t.Fatal("clock claim lost its pin bodies")
	}
	return claimed
}

func tickClockTask(t testing.TB, ctx context.Context, st store.Store, taskID string, at time.Time) {
	t.Helper()
	_, err := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdTimeout, func(lease taskops.TaskLease) (int, error) {
		return st.ApplyWorkOrderClock(ctx, lease, taskID, at)
	})
	scopedFixtureOK(t, err)
}

// SeedReadEgressClockLifecycle writes one selected verify order in each clock
// state the order clock owns — claimed no-op, queue-blocked, claimed with a
// past lease, claimed with a past execution deadline, and stored legacy stale
// — plus unrelated large-pin claimed orders. seedStale is the existing
// integration UPDATE that stores the retired stale value; no public writer
// writes it. pinBytes sizes the governance body; noise is the unrelated count.
func SeedReadEgressClockLifecycle(t testing.TB, ctx context.Context, st store.Store, noise, pinBytes int, seedStale func(testing.TB, context.Context, string)) ([]string, map[string]int) {
	t.Helper()
	if seedStale == nil {
		t.Fatal("legacy stale seed is required")
	}
	if pinBytes < 1 {
		pinBytes = foldGovernanceBytes
	}
	dep, _ := seedClockStage(t, ctx, st, "dep", core.StageImplement)

	_, noop := seedClockStage(t, ctx, st, "noop", core.StageVerify)
	claimClockOrder(t, ctx, st, noop, "noop", time.Hour, time.Hour, pinBytes)

	blockedTask, blocked := seedClockStage(t, ctx, st, "blocked", core.StageVerify)
	_, err := st.AddTaskDependency(ctx, store.DependencyAdditionRequest{
		TaskID: blockedTask.ID, DependsOnTaskID: dep.ID, Reason: "clock block", RequestID: core.NewTaskID(),
	})
	scopedFixtureOK(t, err)
	tickClockTask(t, ctx, st, blocked.TaskID, time.Now().UTC())

	_, lease := seedClockStage(t, ctx, st, "lease", core.StageVerify)
	claimClockOrder(t, ctx, st, lease, "lease", time.Nanosecond, time.Hour, pinBytes)

	_, execution := seedClockStage(t, ctx, st, "execution", core.StageVerify)
	claimClockOrder(t, ctx, st, execution, "execution", time.Hour, time.Nanosecond, pinBytes)

	_, legacy := seedClockStage(t, ctx, st, "legacy", core.StageVerify)
	claimedLegacy := claimClockOrder(t, ctx, st, legacy, "legacy", time.Hour, time.Hour, pinBytes)
	seedStale(t, ctx, claimedLegacy.ID)

	for i := range noise {
		_, extra := seedClockStage(t, ctx, st, fmt.Sprintf("noise-%03d", i), core.StageVerify)
		claimClockOrder(t, ctx, st, extra, fmt.Sprintf("noise-%03d", i), time.Hour, time.Hour, pinBytes)
	}
	selected := []string{noop.TaskID, blocked.TaskID, lease.TaskID, execution.TaskID, legacy.TaskID}
	return selected, map[string]int{
		"selected_tasks": 5, "noop_orders": 1, "blocked_orders": 1, "lease_orders": 1,
		"execution_orders": 1, "legacy_orders": 1, "noise_orders": noise, "pin_bytes": pinBytes,
	}
}

func seedRiskTask(t testing.TB, ctx context.Context, st store.Store, name string) core.Task {
	t.Helper()
	ws, _ := store.WorkspaceFromContext(ctx)
	id := "egress-risk-" + name + "-" + core.NewTaskID()
	task := core.Task{ID: id, Workspace: ws, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id,
		State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: &riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()}}
	scopedFixtureOK(t, st.CreateTask(ctx, task))
	return task
}

func seedRiskHistory(t testing.TB, ctx context.Context, st store.Store, taskID string, history, reasonBytes int) {
	t.Helper()
	reason := strings.Repeat("risk rating prose. ", reasonBytes/19+1)
	for i := range history {
		head := fmt.Sprintf("head-%s-%03d", taskID, i)
		_, _, err := st.RecordRiskRating(ctx, core.RiskRating{
			TaskID: taskID, Key: core.PathRiskKey(head), Source: core.RiskSourcePathRule,
			Tier: riskpolicy.TierMedium, HeadSHA: head, BaseSHA: "base-" + head,
			DeclarationState: riskpolicy.DeclarationAbsent, Reason: reason,
		})
		scopedFixtureOK(t, err)
	}
}

// SeedReadEgressRiskBatch writes several selected record-mode tasks with a
// long path-rating history and unrelated tasks with the same history. The
// selected IDs are the only ones the measurement asks for. history is the
// ratings per task; noise is the unrelated task count; reasonBytes pads each
// rating reason.
func SeedReadEgressRiskBatch(t testing.TB, ctx context.Context, st store.Store, selected, history, noise, reasonBytes int) ([]string, map[string]int) {
	t.Helper()
	if selected < 1 {
		selected = 1
	}
	if history < 1 {
		history = 1
	}
	ids := make([]string, 0, selected)
	for i := range selected {
		task := seedRiskTask(t, ctx, st, fmt.Sprintf("selected-%03d", i))
		seedRiskHistory(t, ctx, st, task.ID, history, reasonBytes)
		ids = append(ids, task.ID)
	}
	for i := range noise {
		task := seedRiskTask(t, ctx, st, fmt.Sprintf("noise-%03d", i))
		seedRiskHistory(t, ctx, st, task.ID, history, reasonBytes)
	}
	return ids, map[string]int{
		"selected_tasks": selected, "history_ratings": history, "noise_tasks": noise, "reason_bytes": reasonBytes,
	}
}
