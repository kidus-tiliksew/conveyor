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

// runTaskDetailReads proves the task-detail document metadata read on every
// backend: it equals the same backend's single-item reads for its selection
// and never returns another workspace's rows (component-persistence,
// Task-detail batch reads).
func runTaskDetailReads(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	foreign := store.WithWorkspace(ctx, "task-detail-foreign")

	t.Run("UnrelatedHistoryLeavesSelectionsUnchanged", func(t *testing.T) {
		seed := SeedTaskDetailReads(t, st, ctx, "detail-"+strings.ToLower(core.NewTaskID()))
		before := ReadTaskDetailSelections(t, st, ctx, seed)
		GrowUnrelatedTaskDetailHistory(t, st, ctx, seed, "CONFORMANCE-DETAIL-SENTINEL", 3)
		after := ReadTaskDetailSelections(t, st, ctx, seed)
		if before != after || strings.Contains(after, "CONFORMANCE-DETAIL-SENTINEL") {
			t.Fatalf("selections changed with unrelated history:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("SelectedGrowthFollowsTheOutputBounds", func(t *testing.T) {
		seed := SeedTaskDetailReads(t, st, ctx, "detail-grow-"+strings.ToLower(core.NewTaskID()))
		count := func() int {
			t.Helper()
			metadata, err := st.ListDocumentMetadata(ctx, seed.DocumentIDs)
			requireOK(t, err)
			return len(metadata.Requirements) + len(metadata.Designs)
		}
		documents := count()
		GrowSelectedTaskDetail(t, st, ctx, &seed, 3)
		if grown := count(); grown != documents+3 {
			t.Fatalf("selected growth: documents %d->%d", documents, grown)
		}
		ReadTaskDetailSelections(t, st, ctx, seed)
	})

	t.Run("SelectionsFailWithoutPartialResults", func(t *testing.T) {
		seed := SeedTaskDetailReads(t, st, ctx, "detail-fail-"+strings.ToLower(core.NewTaskID()))
		done, cancel := context.WithCancel(ctx)
		cancel()
		if metadata, err := st.ListDocumentMetadata(done, seed.DocumentIDs); err == nil || metadata.Requirements != nil || metadata.Designs != nil {
			t.Fatalf("metadata with a done context = %+v, %v; want no result and an error", metadata, err)
		}
		// The live context still reads every selection.
		ReadTaskDetailSelections(t, st, ctx, seed)
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

// TaskDetailReadsSeed names one task whose detail selection the durable
// observers read: its orders and one confirmed requirement and System Design.
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
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("%s-implement-%d", task.ID, i+1)
		requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}))
		requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now.Add(time.Duration(i) * time.Second)}))
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
// may read: another task's orders, new unconfirmed versions of the
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

// ReadTaskDetailSelections reads the seed's document selection and returns it
// as one canonical JSON document.
func ReadTaskDetailSelections(t *testing.T, st store.Store, ctx context.Context, seed TaskDetailReadsSeed) string {
	t.Helper()
	metadata, err := st.ListDocumentMetadata(ctx, seed.DocumentIDs)
	requireOK(t, err)
	if len(metadata.Requirements)+len(metadata.Designs) != len(seed.DocumentIDs) {
		t.Fatalf("seed selection: metadata=%+v", metadata)
	}
	out, err := json.Marshal(struct {
		Metadata store.DocumentMetadataSet
	}{metadata})
	requireOK(t, err)
	return string(out)
}

// GrowSelectedTaskDetail adds n confirmed requirements to the requested
// metadata, so the selection returns n more metadata rows.
func GrowSelectedTaskDetail(t *testing.T, st store.Store, ctx context.Context, seed *TaskDetailReadsSeed, n int) {
	t.Helper()
	human := store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-grown-%d", seed.DocumentIDs[0], len(seed.DocumentIDs))
		_, version, err := st.CreateRequirement(human, core.Requirement{ID: id, Title: "Detail " + id}, core.RequirementVersion{Content: "# Detail " + id, Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep grown metadata."}}})
		requireOK(t, err)
		_, _, err = st.ConfirmRequirementVersion(human, id, version.Version)
		requireOK(t, err)
		seed.DocumentIDs = append(seed.DocumentIDs, id)
	}
}
