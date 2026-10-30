package storetest

import (
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// largeArgumentBodyBytes exceeds the SingleStore driver's long-data bound for
// the work_orders update, its 64 MiB packet size divided by about 60
// parameters, by a wide margin (component-persistence, Migration runners and
// ledgers).
const largeArgumentBodyBytes = 3 << 20

// runLargeArgumentWrites claims a verify order and a review order whose claim
// pins a governance snapshot holding a 3 MiB System Design body. Every backend
// must accept the claim and return the exact body, so SingleStore never sends
// the snapshot through the long-data command it refuses with error 1706
// (component-persistence; task 261030-fb4d3c).
func runLargeArgumentWrites(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	now := time.Now().UTC().Truncate(time.Microsecond)
	// The line mixes quotes, backslashes, placeholders, control characters,
	// and multi-byte text, so text interpolation must escape every argument.
	line := "Claim pins `authority` with 'quotes', \"doubles\", a \\ backslash, a ? mark,\ta tab, and → é 漢字 text.\n"
	body := strings.Repeat(line, largeArgumentBodyBytes/len(line)+1)
	if len(body) <= largeArgumentBodyBytes {
		t.Fatalf("large design body is %d bytes, want more than %d", len(body), largeArgumentBodyBytes)
	}
	governance := &core.GovernanceSnapshot{
		Designs:   []core.GovernanceDesignContext{{ID: "component-large-argument", Title: "Large argument", Category: "component", Version: 1, Content: body, Governs: []core.GovernedScope{}}},
		Decisions: []core.Decision{},
	}
	requirements := []core.ServedRequirementContext{{ID: "req-large-argument", Title: "Large argument", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Claims shall pin authority of any size."}}}}

	for _, stage := range []core.Stage{core.StageVerify, core.StageReview} {
		t.Run(string(stage), func(t *testing.T) {
			id := core.NewTaskID()
			task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: stage, ReviewedHeadSHA: "submitted-head", CreatedAt: now}
			task.SetupContract.VerifyStage = stage == core.StageVerify
			requireOK(t, st.CreateTask(ctx, task))
			job := core.Job{ID: id + "-" + string(stage) + "-1", TaskID: id, Stage: stage, State: core.JobPending}
			order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: id, Stage: stage, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
			// The stage command creates verify orders; review orders enter
			// through a job and a plain order, as ClaimBlockingProposals does.
			if stage == core.StageVerify {
				_, err := CreateStageWorkOrder(ctx, st, job, order)
				requireOK(t, err)
			} else {
				requireOK(t, st.CreateJob(ctx, job))
				requireOK(t, For(st).CreateWorkOrder(ctx, order))
			}

			claimed, err := For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "large-" + id, ClientToken: "large-token-" + id, Lease: time.Minute, ExecutionTimeout: time.Hour, Requirements: requirements, Governance: governance})
			if err != nil {
				t.Fatalf("%s claim with a %d-byte pinned design body: %v", stage, len(body), err)
			}
			if claimed.State != core.WorkOrderClaimed {
				t.Fatalf("%s claim state=%s, want claimed", stage, claimed.State)
			}
			read, err := st.GetWorkOrder(ctx, job.ID)
			requireOK(t, err)
			if read.GovernanceSnapshot == nil || len(read.GovernanceSnapshot.Designs) != 1 {
				t.Fatalf("%s claimed governance snapshot=%+v, want one pinned design", stage, read.GovernanceSnapshot)
			}
			if got := read.GovernanceSnapshot.Designs[0].Content; got != body {
				t.Fatalf("%s pinned design body has %d bytes, want the exact %d-byte body", stage, len(got), len(body))
			}
			if len(read.ServedRequirementSnapshot) != 1 || read.ServedRequirementSnapshot[0].ID != "req-large-argument" {
				t.Fatalf("%s served requirement snapshot=%+v", stage, read.ServedRequirementSnapshot)
			}
		})
	}
}
