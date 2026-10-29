package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runTaskDetailReads proves the task-detail batch reads on every backend:
// each equals the same backend's single-item reads for its selection and
// never returns another task's or workspace's rows (component-persistence,
// Task-detail batch reads).
func runTaskDetailReads(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	foreign := store.WithWorkspace(ctx, "task-detail-foreign")

	t.Run("CapturesAndSnapshotsFollowTheTasksOrders", func(t *testing.T) {
		task := newAggregateTask(t, x)
		other := newAggregateTask(t, x)
		order := func(task core.Task, index int) core.WorkOrder {
			t.Helper()
			id := task.ID + "-implement-detail-" + string(rune('a'+index))
			requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}))
			created := core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: task.CreatedAt, QueueDeadline: task.CreatedAt.Add(time.Hour), CreatedAt: task.CreatedAt.Add(time.Duration(index) * time.Second)}
			requireOK(t, CreateWorkOrder(ctx, st, created))
			return created
		}
		// ended runs one attempt to a release with a capture; the live
		// snapshot of the last open attempt is the one a read selects.
		ended := func(order core.WorkOrder, session string) core.WorkOrder {
			t.Helper()
			claimed := claimForCapture(t, x, order.ID, session)
			identity := workerCaptureIdentity(claimed.SessionID)
			requireOK(t, st.UpsertWorkOrderActivitySnapshot(ctx, order.ID, identity, "tail "+session))
			_, err := ReleaseWorkerClaim(ctx, st, order.ID, captureWorker, core.WorkOrderRelease{SessionID: claimed.SessionID, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "harness exited", InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond})
			requireOK(t, err)
			_, err = st.RecordWorkOrderAttemptCapture(ctx, order.ID, identity, captureOf(claimed, "", "transcript "+session))
			requireOK(t, err)
			return claimed
		}
		// Only a task's newest order is claimable, and creating one retires
		// its open siblings, so each order is created just before its attempts.
		quiet := order(task, 0)
		second := order(task, 1)
		ended(second, "detail-second-"+second.ID)
		foreignOrder := order(other, 0)
		ended(foreignOrder, "detail-other-"+foreignOrder.ID)
		first := order(task, 2)
		ended(first, "detail-first-a-"+first.ID)
		ended(first, "detail-first-b-"+first.ID)
		live := claimForCapture(t, x, first.ID, "detail-first-c-"+first.ID)
		requireOK(t, st.UpsertWorkOrderActivitySnapshot(ctx, first.ID, workerCaptureIdentity(live.SessionID), "live tail"))

		captures, err := st.ListTaskTranscriptCaptures(ctx, task.ID)
		requireOK(t, err)
		want := map[string][]core.WorkOrderTranscriptCapture{}
		for _, id := range []string{first.ID, second.ID, quiet.ID} {
			single, singleErr := st.ListWorkOrderTranscriptCaptures(ctx, id)
			requireOK(t, singleErr)
			if len(single) > 0 {
				want[id] = single
			}
		}
		if len(want[first.ID]) != 2 || len(want[second.ID]) != 1 || !reflect.DeepEqual(captures, want) {
			t.Fatalf("task captures = %+v, want %+v", captures, want)
		}

		requested := []string{first.ID, second.ID, quiet.ID, foreignOrder.ID, "missing-order", first.ID}
		snapshots, err := st.ListTaskActivitySnapshots(ctx, task.ID, requested)
		requireOK(t, err)
		wantSnapshots := map[string]core.WorkOrderActivitySnapshot{}
		for _, id := range []string{first.ID, second.ID, quiet.ID} {
			snapshot, found, singleErr := st.GetWorkOrderActivitySnapshot(ctx, id)
			requireOK(t, singleErr)
			if found {
				wantSnapshots[id] = snapshot
			}
		}
		if wantSnapshots[first.ID].AttemptID != live.AttemptID || !reflect.DeepEqual(snapshots, wantSnapshots) {
			t.Fatalf("task snapshots = %+v, want %+v", snapshots, wantSnapshots)
		}

		for label, read := range map[string]func() (int, error){
			"empty snapshot selection": func() (int, error) { got, err := st.ListTaskActivitySnapshots(ctx, task.ID, nil); return len(got), err },
			"foreign captures":         func() (int, error) { got, err := st.ListTaskTranscriptCaptures(foreign, task.ID); return len(got), err },
			"foreign snapshots": func() (int, error) {
				got, err := st.ListTaskActivitySnapshots(foreign, task.ID, requested)
				return len(got), err
			},
			"missing task captures": func() (int, error) {
				got, err := st.ListTaskTranscriptCaptures(ctx, "missing-task")
				return len(got), err
			},
		} {
			if n, readErr := read(); readErr != nil || n != 0 {
				t.Fatalf("%s: %d rows, err %v", label, n, readErr)
			}
		}
		if got, err := st.ListTaskActivitySnapshots(ctx, task.ID, nil); err != nil || got == nil {
			t.Fatalf("empty selection = %v, %v; want an empty map", got, err)
		}
	})

	t.Run("UnrelatedHistoryLeavesSelectionsUnchanged", func(t *testing.T) {
		seed := SeedTaskDetailReads(t, st, ctx, "detail-"+strings.ToLower(core.NewTaskID()))
		before := ReadTaskDetailSelections(t, st, ctx, seed)
		GrowUnrelatedTaskDetailHistory(t, st, ctx, seed, "CONFORMANCE-DETAIL-SENTINEL", 3)
		after := ReadTaskDetailSelections(t, st, ctx, seed)
		if before != after || strings.Contains(after, "CONFORMANCE-DETAIL-SENTINEL") {
			t.Fatalf("selections changed with unrelated history:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("DocumentMetadataKeepsBothTiers", func(t *testing.T) {
		suffix := core.NewTaskID()
		requirementVersion := func(content string) core.RequirementVersion {
			return core.RequirementVersion{Content: content, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep detail metadata."}}, Origin: core.RequirementOriginOperator}
		}
		shared, confirmedOnly, unconfirmed, successor := "detail-shared-"+suffix, "detail-req-"+suffix, "detail-draft-"+suffix, "detail-next-"+suffix
		for _, id := range []string{shared, confirmedOnly, successor} {
			_, version, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: "Requirement " + id}, requirementVersion("# "+id))
			requireOK(t, err)
			_, _, err = st.ConfirmRequirementVersion(ctx, id, version.Version)
			requireOK(t, err)
		}
		_, _, err := st.CreateRequirement(ctx, core.Requirement{ID: unconfirmed, Title: "Draft " + unconfirmed}, requirementVersion("# draft"))
		requireOK(t, err)
		for _, id := range []string{shared, successor} {
			design, version, designErr := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: "Design " + id, Category: "Component design"}, core.SystemDesignVersion{Content: designContent(id), Origin: core.SystemDesignOriginOperator})
			requireOK(t, designErr)
			_, _, designErr = st.ConfirmSystemDesignVersion(ctx, design.ID, version.Version)
			requireOK(t, designErr)
		}
		requireOK(t, st.ArchiveRequirement(ctx, confirmedOnly, "operator", []string{successor}))
		requireOK(t, st.ArchiveSystemDesign(ctx, shared, "operator", []string{successor}))

		ids := []string{shared, confirmedOnly, unconfirmed, successor, "detail-missing-" + suffix, shared}
		metadata, err := st.ListDocumentMetadata(ctx, ids)
		requireOK(t, err)
		want := store.NewDocumentMetadataSet()
		for _, id := range ids {
			if requirement, getErr := st.GetRequirement(ctx, id); getErr == nil {
				want.Add(store.DocumentMetadata{ID: id, Tier: store.DocumentHeadRequirement, Title: requirement.Title, CurrentVersion: requirement.CurrentVersion, Archived: requirement.Archived, SupersededBy: requirement.SupersededBy})
			}
			if design, getErr := st.GetSystemDesign(ctx, id); getErr == nil {
				want.Add(store.DocumentMetadata{ID: id, Tier: store.DocumentHeadSystemDesign, Title: design.Title, CurrentVersion: design.CurrentVersion, Archived: design.Archived, SupersededBy: design.SupersededBy})
			}
		}
		if len(want.Requirements) != 4 || len(want.Designs) != 2 || !reflect.DeepEqual(metadata, want) {
			t.Fatalf("metadata = %+v, want %+v", metadata, want)
		}
		if got := metadata.Requirements[unconfirmed]; got.CurrentVersion != 0 || got.SupersededBy == nil {
			t.Fatalf("unconfirmed requirement = %+v", got)
		}
		if got := metadata.Designs[shared]; !got.Archived || !reflect.DeepEqual(got.SupersededBy, []string{successor}) || metadata.Requirements[shared].Archived {
			t.Fatalf("shared ID tiers = %+v / %+v", metadata.Requirements[shared], got)
		}
		for label, selection := range map[string]struct {
			ctxWorkspace bool
			ids          []string
		}{"empty": {true, nil}, "foreign workspace": {false, ids}} {
			readCtx := ctx
			if !selection.ctxWorkspace {
				readCtx = foreign
			}
			got, readErr := st.ListDocumentMetadata(readCtx, selection.ids)
			if readErr != nil || got.Requirements == nil || got.Designs == nil || len(got.Requirements)+len(got.Designs) != 0 {
				t.Fatalf("%s metadata = %+v, %v", label, got, readErr)
			}
		}
	})
}

// TaskDetailReadsSeed names one task whose detail selections the durable
// observers read: its orders, with one ended attempt's capture and one live
// snapshot, and one confirmed requirement and System Design.
type TaskDetailReadsSeed struct {
	TaskID      string
	OrderIDs    []string
	DocumentIDs []string
}

// SeedTaskDetailReads creates the observed task and documents in the
// context's workspace, which must already be configured with a "conveyor"
// repository.
func SeedTaskDetailReads(t *testing.T, st store.Store, ctx context.Context, prefix string) TaskDetailReadsSeed {
	t.Helper()
	workspace, _ := store.WorkspaceFromContext(ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{ID: prefix + "-task", Workspace: workspace, Repo: "conveyor", Title: prefix, BaseBranch: "main", Branch: "conveyor/task-" + prefix, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	requireOK(t, st.CreateTask(ctx, task))
	seed := TaskDetailReadsSeed{TaskID: task.ID}
	for i, live := range []bool{false, true} {
		id := fmt.Sprintf("%s-implement-%d", task.ID, i+1)
		requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}))
		requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now.Add(time.Duration(i) * time.Second)}))
		seedTaskDetailAttempt(t, st, ctx, id, "selected transcript "+id, live)
		seed.OrderIDs = append(seed.OrderIDs, id)
	}
	human := store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	requirementID, designID := prefix+"-requirement", prefix+"-design"
	_, version, err := st.CreateRequirement(human, core.Requirement{ID: requirementID, Title: "Detail " + requirementID}, core.RequirementVersion{Content: "# Detail " + requirementID, Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep detail metadata."}}})
	requireOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(human, requirementID, version.Version)
	requireOK(t, err)
	design, designVersion, err := st.CreateSystemDesign(human, core.SystemDesign{ID: designID, Title: "Detail " + designID, Category: "Component design"}, core.SystemDesignVersion{Content: designContent(designID), Origin: core.SystemDesignOriginOperator})
	requireOK(t, err)
	_, _, err = st.ConfirmSystemDesignVersion(human, design.ID, designVersion.Version)
	requireOK(t, err)
	seed.DocumentIDs = []string{requirementID, designID}
	return seed
}

// GrowUnrelatedTaskDetailHistory adds, around a seed, history no selection
// may read: another task's captured attempts, new unconfirmed versions of the
// selected documents, other documents, and artifact blobs, every body
// carrying sentinel and growing with size.
func GrowUnrelatedTaskDetailHistory(t *testing.T, st store.Store, ctx context.Context, seed TaskDetailReadsSeed, sentinel string, size int) {
	t.Helper()
	workspace, _ := store.WorkspaceFromContext(ctx)
	body := strings.Repeat(sentinel+" ", 64*size)
	now := time.Now().UTC().Truncate(time.Microsecond)
	other := core.Task{ID: fmt.Sprintf("%s-unrelated-%d", seed.TaskID, size), Workspace: workspace, Repo: "conveyor", Title: "unrelated", BaseBranch: "main", Branch: fmt.Sprintf("conveyor/task-%s-unrelated-%d", seed.TaskID, size), State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	requireOK(t, st.CreateTask(ctx, other))
	for i := 0; i < size; i++ {
		id := fmt.Sprintf("%s-implement-%d", other.ID, i+1)
		requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: other.ID, Stage: core.StageImplement, State: core.JobPending}))
		requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: other.ID, JobID: id, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now.Add(time.Duration(i) * time.Second)}))
		seedTaskDetailAttempt(t, st, ctx, id, body, false)
	}
	human := store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	for i := 0; i < size; i++ {
		_, err := st.ProposeRequirementVersion(human, core.RequirementVersion{RequirementID: seed.DocumentIDs[0], Content: "# Detail " + seed.DocumentIDs[0] + "\n\n" + body, Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Unrelated " + body[:64]}}})
		requireOK(t, err)
		_, err = st.ProposeSystemDesignVersion(human, core.SystemDesignVersion{DocumentID: seed.DocumentIDs[1], Content: designContent(body), Origin: core.SystemDesignOriginOperator})
		requireOK(t, err)
		id := fmt.Sprintf("%s-unrelated-%d-%d", seed.DocumentIDs[1], size, i)
		_, _, err = st.CreateSystemDesign(human, core.SystemDesign{ID: id, Title: "Unrelated " + id, Category: "Component design"}, core.SystemDesignVersion{Content: designContent(body), Origin: core.SystemDesignOriginOperator})
		requireOK(t, err)
		_, err = st.CreateArtifact(ctx, core.Artifact{Workspace: workspace, Name: fmt.Sprintf("unrelated-%d-%d.txt", size, i), ContentType: "text/plain", Role: core.ArtifactRoleTaskContext, TaskID: other.ID, CreatedAt: now}, []byte(fmt.Sprintf("%d %d %s", size, i, body)))
		requireOK(t, err)
	}
}

// seedTaskDetailAttempt ends one attempt of a queued order with a capture of
// content and, when live, opens another whose snapshot stays current.
func seedTaskDetailAttempt(t *testing.T, st store.Store, ctx context.Context, orderID, content string, live bool) {
	t.Helper()
	claim := func(session string) core.WorkOrder {
		t.Helper()
		claimed, err := ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{WorkerID: captureWorker, ClaimantID: captureWorker, SessionID: session, ClientToken: session, Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		return claimed
	}
	ended := claim("detail-ended-" + orderID)
	identity := workerCaptureIdentity(ended.SessionID)
	_, err := ReleaseWorkerClaim(ctx, st, orderID, captureWorker, core.WorkOrderRelease{SessionID: ended.SessionID, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "harness exited", InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond})
	requireOK(t, err)
	_, err = st.RecordWorkOrderAttemptCapture(ctx, orderID, identity, captureOf(ended, "", content))
	requireOK(t, err)
	if live {
		current := claim("detail-live-" + orderID)
		requireOK(t, st.UpsertWorkOrderActivitySnapshot(ctx, orderID, workerCaptureIdentity(current.SessionID), "live tail "+orderID))
	}
}

// ReadTaskDetailSelections reads the seed's three selections and returns them
// as one canonical JSON document.
func ReadTaskDetailSelections(t *testing.T, st store.Store, ctx context.Context, seed TaskDetailReadsSeed) string {
	t.Helper()
	captures, err := st.ListTaskTranscriptCaptures(ctx, seed.TaskID)
	requireOK(t, err)
	snapshots, err := st.ListTaskActivitySnapshots(ctx, seed.TaskID, seed.OrderIDs)
	requireOK(t, err)
	metadata, err := st.ListDocumentMetadata(ctx, seed.DocumentIDs)
	requireOK(t, err)
	if len(captures) != len(seed.OrderIDs) || len(snapshots) != 1 || len(metadata.Requirements) != 1 || len(metadata.Designs) != 1 {
		t.Fatalf("seed selections: captures=%d snapshots=%d metadata=%+v", len(captures), len(snapshots), metadata)
	}
	out, err := json.Marshal(struct {
		Captures  map[string][]core.WorkOrderTranscriptCapture
		Snapshots map[string]core.WorkOrderActivitySnapshot
		Metadata  store.DocumentMetadataSet
	}{captures, snapshots, metadata})
	requireOK(t, err)
	return string(out)
}
