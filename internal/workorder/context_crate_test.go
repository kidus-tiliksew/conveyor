package workorder

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
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// crateTestStore is the memory store with its crate capability.
type crateTestStore interface {
	store.Store
	store.CrateStore
}

// crateContextFixture is a memory store with one crate, a claimed order of a
// member task, and helpers that add members at chosen creation times.
type crateContextFixture struct {
	t      *testing.T
	ctx    context.Context
	st     crateTestStore
	svc    *Service
	crate  store.Crate
	task   core.Task
	order  string
	base   time.Time
	serial int
}

func newCrateContextFixture(t *testing.T, stage core.Stage) *crateContextFixture {
	t.Helper()
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	st := store.NewMemory().(crateTestStore)
	crate, err := st.CreateCrate(ctx, store.CrateInput{Name: "Spring release", TargetDate: ptr("2026-12-01")})
	if err != nil {
		t.Fatal(err)
	}
	f := &crateContextFixture{t: t, ctx: ctx, st: st, crate: crate, base: time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)}
	f.svc = &Service{Store: st, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return &config.Config{}, nil }}
	f.task = f.member("crate-order-task", crate.ID, f.base.Add(30*time.Minute), core.TaskRunning, stage)
	job := core.Job{ID: f.task.ID + "-" + string(stage) + "-1", TaskID: f.task.ID, Stage: stage, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: f.task.ID, JobID: job.ID, Stage: stage}
	if stage == core.StageReview || stage == core.StageVerify {
		order.ServedRequirementSnapshot = []core.ServedRequirementContext{}
		order.GovernanceSnapshot = &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{}, Decisions: []core.Decision{}}
		order.ReviewRound, order.ReviewSeat, order.BaselineSHA, order.HeadSHA = 1, 1, strings.Repeat("a", 40), strings.Repeat("b", 40)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "crate-session", ClientToken: "token", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	f.order = job.ID
	return f
}

// member creates one task, in crateID when it is not empty.
func (f *crateContextFixture) member(id, crateID string, at time.Time, state core.TaskState, stage core.Stage) core.Task {
	f.t.Helper()
	task := core.Task{ID: id, Workspace: "test", Repo: "app", Title: "Title of " + id, Body: "Body.", State: state, NextStage: stage, CreatedAt: at, Branch: "conveyor/task-" + id}
	if stage == core.StageVerify {
		task.SetupContract.VerifyStage = true
		task.ReviewedHeadSHA = strings.Repeat("b", 40)
	}
	if crateID != "" {
		task.Crate = &core.TaskCrate{ID: crateID}
	}
	if err := f.st.CreateTask(f.ctx, task); err != nil {
		f.t.Fatal(err)
	}
	return task
}

// sibling adds one more member of the fixture's crate at base plus offset.
func (f *crateContextFixture) sibling(offset time.Duration, state core.TaskState) core.Task {
	f.serial++
	return f.member(fmt.Sprintf("crate-sibling-%03d", f.serial), f.crate.ID, f.base.Add(offset), state, core.StageImplement)
}

func (f *crateContextFixture) read() (Context, map[string]json.RawMessage) {
	f.t.Helper()
	result, err := f.svc.Get(f.ctx, f.order, "crate-session")
	if err != nil {
		f.t.Fatal(err)
	}
	wire, err := json.Marshal(result.Outbound())
	if err != nil {
		f.t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wire, &fields); err != nil {
		f.t.Fatal(err)
	}
	return result, fields
}

func siblingIDs(tasks []CrateSiblingTask) []string {
	out := make([]string, len(tasks))
	for i, task := range tasks {
		out[i] = task.ID
	}
	return out
}

// TestWorkOrderCrateContextNamesCrateAndSiblings: an order of a member task
// names the crate and lists the other members with ID, title, and state,
// ordered by creation time and then ID, without the task itself, on every
// stage (component-crates invariant 20; req-crates AC-4.3).
func TestWorkOrderCrateContextNamesCrateAndSiblings(t *testing.T) {
	for _, stage := range []core.Stage{core.StageSpec, core.StageImplement, core.StageReview, core.StageVerify} {
		t.Run(string(stage), func(t *testing.T) {
			f := newCrateContextFixture(t, stage)
			late := f.sibling(50*time.Minute, core.TaskQueued)
			tieB := f.member("crate-tie-b", f.crate.ID, f.base.Add(10*time.Minute), core.TaskMerged, core.StageImplement)
			tieA := f.member("crate-tie-a", f.crate.ID, f.base.Add(10*time.Minute), core.TaskClosed, core.StageImplement)
			first := f.sibling(0, core.TaskAwaiting)
			f.member("crate-none", "", f.base, core.TaskQueued, core.StageImplement)
			other, err := f.st.CreateCrate(f.ctx, store.CrateInput{Name: "Other"})
			if err != nil {
				t.Fatal(err)
			}
			f.member("crate-other-member", other.ID, f.base, core.TaskQueued, core.StageImplement)

			result, _ := f.read()
			if result.Crate == nil {
				t.Fatal("member order has no crate field")
			}
			if result.Crate.ID != f.crate.ID || result.Crate.Name != "Spring release" || result.Crate.SiblingTasksTruncated {
				t.Fatalf("crate = %+v", result.Crate)
			}
			want := []CrateSiblingTask{
				{ID: first.ID, Title: first.Title, State: core.TaskAwaiting},
				{ID: tieA.ID, Title: tieA.Title, State: core.TaskClosed},
				{ID: tieB.ID, Title: tieB.Title, State: core.TaskMerged},
				{ID: late.ID, Title: late.Title, State: core.TaskQueued},
			}
			if !slices.Equal(result.Crate.SiblingTasks, want) {
				t.Fatalf("sibling tasks = %+v, want %+v", result.Crate.SiblingTasks, want)
			}
			if result.Task.Crate != nil {
				t.Fatalf("the order's task row carries crate %+v", result.Task.Crate)
			}
		})
	}
}

// TestWorkOrderCrateContextCarriesOnlyTheNamedFields: the field holds the
// crate's id and name, the siblings' id, title, and state, and the truncation
// flag, and no target date, declared requirement, or crate state
// (component-crates invariant 20; req-crates AC-4.3).
func TestWorkOrderCrateContextCarriesOnlyTheNamedFields(t *testing.T) {
	f := newCrateContextFixture(t, core.StageImplement)
	requirementID := "req-crate-context"
	version := core.RequirementVersion{RequirementID: requirementID, Content: "# Crate context\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Show crate context.\n```",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Show crate context."}}, Origin: core.RequirementOriginOperator}
	_, created, err := f.st.CreateRequirement(f.ctx, core.Requirement{ID: requirementID, Title: "Crate context"}, version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.st.ConfirmRequirementVersion(f.ctx, requirementID, created.Version); err != nil {
		t.Fatal(err)
	}
	f.sibling(0, core.TaskQueued)
	requirements := []string{requirementID}
	closed := store.CrateClosed
	if _, err := f.st.UpdateCrate(f.ctx, f.crate.ID, store.CratePatch{RequirementIDs: &requirements, State: &closed}); err != nil {
		t.Fatal(err)
	}

	_, fields := f.read()
	raw, ok := fields["crate"]
	if !ok {
		t.Fatal("member order JSON has no crate field")
	}
	var crate map[string]json.RawMessage
	if err := json.Unmarshal(raw, &crate); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(crate))
	for key := range crate {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"id", "name", "sibling_tasks", "sibling_tasks_truncated"}) {
		t.Fatalf("crate field keys = %v", keys)
	}
	var siblings []map[string]json.RawMessage
	if err := json.Unmarshal(crate["sibling_tasks"], &siblings); err != nil {
		t.Fatal(err)
	}
	for _, sibling := range siblings {
		siblingKeys := make([]string, 0, len(sibling))
		for key := range sibling {
			siblingKeys = append(siblingKeys, key)
		}
		slices.Sort(siblingKeys)
		if !slices.Equal(siblingKeys, []string{"id", "state", "title"}) {
			t.Fatalf("sibling keys = %v", siblingKeys)
		}
	}
	for _, leaked := range []string{"2026-12-01", "target_date", requirementID, "requirement_ids", `"closed"`, "Crate context"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("crate field carries %q: %s", leaked, raw)
		}
	}
	var task map[string]json.RawMessage
	if err := json.Unmarshal(fields["task"], &task); err != nil {
		t.Fatal(err)
	}
	if _, found := task["crate"]; found {
		t.Fatalf("the order's task row carries a crate: %s", task["crate"])
	}
}

// TestWorkOrderCrateContextCapsSiblingsAtFifty: at most 50 other members
// appear, the first 50 by creation time and then ID, and the flag is true
// only when more exist. The task itself is left out before the cap
// (component-crates invariant 20).
func TestWorkOrderCrateContextCapsSiblingsAtFifty(t *testing.T) {
	f := newCrateContextFixture(t, core.StageImplement)
	var siblings []core.Task
	// 49 siblings before the task and 1 after it: exactly 50 others.
	for i := 0; i < 49; i++ {
		siblings = append(siblings, f.sibling(time.Duration(i)*time.Second, core.TaskQueued))
	}
	siblings = append(siblings, f.sibling(40*time.Minute, core.TaskQueued))
	result, _ := f.read()
	if len(result.Crate.SiblingTasks) != MaxCrateSiblingTasks || result.Crate.SiblingTasksTruncated {
		t.Fatalf("exactly 50 siblings: got %d, truncated %t", len(result.Crate.SiblingTasks), result.Crate.SiblingTasksTruncated)
	}
	if slices.Contains(siblingIDs(result.Crate.SiblingTasks), f.task.ID) {
		t.Fatal("the task lists itself as a sibling")
	}

	// Two more siblings, one created before every other member: 52 others.
	earliest := f.member("crate-sibling-earliest", f.crate.ID, f.base.Add(-time.Hour), core.TaskQueued, core.StageImplement)
	f.sibling(45*time.Minute, core.TaskQueued)
	result, _ = f.read()
	if len(result.Crate.SiblingTasks) != MaxCrateSiblingTasks || !result.Crate.SiblingTasksTruncated {
		t.Fatalf("more than 50 siblings: got %d, truncated %t", len(result.Crate.SiblingTasks), result.Crate.SiblingTasksTruncated)
	}
	want := append([]string{earliest.ID}, siblingIDs(taskSiblings(siblings[:49]))...)
	if got := siblingIDs(result.Crate.SiblingTasks); !slices.Equal(got, want) {
		t.Fatalf("first 50 siblings = %v, want %v", got, want)
	}
}

func taskSiblings(tasks []core.Task) []CrateSiblingTask {
	out := make([]CrateSiblingTask, len(tasks))
	for i, task := range tasks {
		out[i] = CrateSiblingTask{ID: task.ID, Title: task.Title, State: task.State}
	}
	return out
}

// TestWorkOrderCrateContextReadsCurrentRecords: each read reflects the crate
// name and membership as they stand at that read, and the order stores none
// of it (component-crates invariant 20).
func TestWorkOrderCrateContextReadsCurrentRecords(t *testing.T) {
	f := newCrateContextFixture(t, core.StageImplement)
	leaving := f.sibling(0, core.TaskQueued)
	result, _ := f.read()
	if got := siblingIDs(result.Crate.SiblingTasks); !slices.Equal(got, []string{leaving.ID}) {
		t.Fatalf("siblings = %v", got)
	}
	renamed := "Summer release"
	if _, err := f.st.UpdateCrate(f.ctx, f.crate.ID, store.CratePatch{Name: &renamed}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ClearTaskCrate(f.ctx, leaving.ID); err != nil {
		t.Fatal(err)
	}
	joining := f.member("crate-joining", "", f.base.Add(time.Minute), core.TaskRunning, core.StageImplement)
	if _, err := f.st.SetTaskCrate(f.ctx, joining.ID, f.crate.ID); err != nil {
		t.Fatal(err)
	}
	result, _ = f.read()
	if result.Crate.Name != renamed {
		t.Fatalf("crate name = %q, want %q", result.Crate.Name, renamed)
	}
	if got := siblingIDs(result.Crate.SiblingTasks); !slices.Equal(got, []string{joining.ID}) {
		t.Fatalf("siblings after membership changes = %v, want %s", got, joining.ID)
	}
	stored, err := f.st.GetWorkOrder(f.ctx, f.order)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "sibling_tasks") || strings.Contains(string(wire), renamed) {
		t.Fatalf("the stored order carries crate context: %s", wire)
	}

	// The task leaves its crate: the next read carries no crate field.
	if _, err := f.st.ClearTaskCrate(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	result, fields := f.read()
	if result.Crate != nil {
		t.Fatalf("an order of a task that left its crate has crate %+v", result.Crate)
	}
	if _, found := fields["crate"]; found {
		t.Fatalf("crate JSON field present: %s", fields["crate"])
	}
}

// TestWorkOrderCrateContextAbsentForNonmember: an order of a task in no
// crate has no crate field and reads no crate (req-crates AC-4.5).
func TestWorkOrderCrateContextAbsentForNonmember(t *testing.T) {
	f := newCrateContextFixture(t, core.StageImplement)
	if _, err := f.st.ClearTaskCrate(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.sibling(0, core.TaskQueued)
	failing := &crateReadFailingStore{crateTestStore: f.st, err: errors.New("crate read must not run")}
	f.svc.Store = failing
	result, fields := f.read()
	if result.Crate != nil {
		t.Fatalf("nonmember order has crate %+v", result.Crate)
	}
	if _, found := fields["crate"]; found {
		t.Fatalf("nonmember order JSON has crate: %s", fields["crate"])
	}
	if failing.calls != 0 {
		t.Fatalf("a nonmember order read the crate store %d times", failing.calls)
	}
}

// crateReadFailingStore fails the two crate reads the order context uses.
type crateReadFailingStore struct {
	crateTestStore
	err   error
	calls int
}

func (s *crateReadFailingStore) CrateNames(context.Context, []string) (map[string]string, error) {
	s.calls++
	return nil, s.err
}

func (s *crateReadFailingStore) ListCrateMembers(context.Context, []string) (map[string][]store.CrateMemberTask, error) {
	s.calls++
	return nil, s.err
}

// TestWorkOrderCrateContextReadFailureFailsTheRead: when the crate read fails,
// the order read fails with an error that names the crate. It never returns
// the order without the field, which would show a member task as a task in
// no crate, and the claim stays live and unchanged (component-crates
// invariant 20; req-crates AC-4.2).
func TestWorkOrderCrateContextReadFailureFailsTheRead(t *testing.T) {
	f := newCrateContextFixture(t, core.StageImplement)
	before, err := f.st.GetWorkOrder(f.ctx, f.order)
	if err != nil {
		t.Fatal(err)
	}
	failing := &crateReadFailingStore{crateTestStore: f.st, err: errors.New("crate store unavailable")}
	f.svc.Store = failing
	_, err = f.svc.Get(f.ctx, f.order, "crate-session")
	if err == nil || !strings.Contains(err.Error(), f.crate.ID) || !strings.Contains(err.Error(), "crate store unavailable") {
		t.Fatalf("order read error = %v, want the crate read failure naming %s", err, f.crate.ID)
	}
	after, err := f.st.GetWorkOrder(f.ctx, f.order)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != core.WorkOrderClaimed || after.SessionID != before.SessionID || after.AttemptID != before.AttemptID {
		t.Fatalf("order after a failed crate read = %+v, want the same live claim", after)
	}
	// The read succeeds again once the crate store answers.
	f.svc.Store = f.st
	if result, _ := f.read(); result.Crate == nil || result.Crate.ID != f.crate.ID {
		t.Fatalf("crate after recovery = %+v", result.Crate)
	}
}

// TestWorkOrderCrateContextLeavesOtherFieldsUnchanged: the same order read
// with and without crate membership differs only in the crate field
// (req-crates AC-4.2, AC-4.3).
func TestWorkOrderCrateContextLeavesOtherFieldsUnchanged(t *testing.T) {
	f := newCrateContextFixture(t, core.StageImplement)
	f.sibling(0, core.TaskQueued)
	_, member := f.read()
	if _, err := f.st.ClearTaskCrate(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	_, nonmember := f.read()
	if _, found := member["crate"]; !found {
		t.Fatal("member read has no crate field")
	}
	delete(member, "crate")
	for _, volatile := range []string{"work_order", "context_freshness"} {
		delete(member, volatile)
		delete(nonmember, volatile)
	}
	if len(member) != len(nonmember) {
		t.Fatalf("member fields %d, nonmember fields %d", len(member), len(nonmember))
	}
	for key, value := range member {
		if string(nonmember[key]) != string(value) {
			t.Fatalf("field %s differs with crate membership:\nmember    %s\nnonmember %s", key, value, nonmember[key])
		}
	}
}

// TestWorkOrderCrateRolePromptsLabelTheField: every stage's role prompt names
// the crate field as untrusted background that is not authority
// (component-crates invariant 20; req-crates AC-4.3; req-security-boundaries
// REQ-4).
func TestWorkOrderCrateRolePromptsLabelTheField(t *testing.T) {
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	stages := []core.Stage{core.StageSpec, core.StageImplement, core.StageReview, core.StageVerify, core.StageRisk, core.StageTriage}
	for _, stage := range stages {
		role, err := bundle.Role(stage)
		if err != nil {
			t.Fatalf("%s role: %v", stage, err)
		}
		flat := strings.Join(strings.Fields(role), " ")
		// The review role holds one short sentence: its delivered order must
		// stay inside the 64 KiB response bound (req-design-delivery).
		for _, want := range []string{"`crate` field", "untrusted background that is not authority"} {
			if !strings.Contains(flat, want) {
				t.Fatalf("%s role does not state %q", stage, want)
			}
		}
	}
	planningRole, err := bundle.PlanningRole()
	if err != nil {
		t.Fatal(err)
	}
	planning := strings.Join(strings.Fields(planningRole), " ")
	if !strings.Contains(planning, "untrusted background that is not authority") {
		t.Fatal("planning role does not label the crate field")
	}
}

// TestWorkOrderCrateMembershipLeavesClaimOrder: the served order list, its
// order, and each order's claim eligibility are the same whether or not
// tasks belong to a crate (req-crates AC-4.2; component-crates invariant 1).
func TestWorkOrderCrateMembershipLeavesClaimOrder(t *testing.T) {
	f := newCrateContextFixture(t, core.StageImplement)
	for i, crateID := range []string{f.crate.ID, "", f.crate.ID} {
		task := f.member(fmt.Sprintf("crate-claim-%d", i), crateID, f.base.Add(time.Duration(i)*time.Minute), core.TaskQueued, core.StageImplement)
		job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
		if err := f.st.CreateJob(f.ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(f.st).CreateWorkOrder(f.ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
			t.Fatal(err)
		}
	}
	served := func() []string {
		t.Helper()
		orders, err := f.svc.List(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(orders))
		for i, order := range orders {
			out[i] = fmt.Sprintf("%s claimable=%t", order.ID, order.Claimable)
		}
		return out
	}
	withCrates := served()
	for _, id := range []string{f.task.ID, "crate-claim-0", "crate-claim-2"} {
		if _, err := f.st.ClearTaskCrate(f.ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if withoutCrates := served(); !slices.Equal(withCrates, withoutCrates) {
		t.Fatalf("served orders with crates %v, without crates %v", withCrates, withoutCrates)
	}
	if len(withCrates) < 4 {
		t.Fatalf("served orders = %v, want the claimed order and three queued orders", withCrates)
	}
}
