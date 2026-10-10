package storetest

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
)

// RunFeatureRetirementConformance proves that removing the features entity
// (task 261007-9d50e0; component-persistence, component-artifacts) leaves
// task and artifact behavior intact on every backend: task create, read, list,
// and update; every artifact read path with each remaining owner (task,
// requirement, planning session) and the workspace-unattached case, which is
// the shape a retired feature attachment takes after migrations 142/0018;
// duplicate-upload metadata; role distinctions; workspace isolation; and an
// unattached upload kept out of task context and task evidence.
func RunFeatureRetirementConformance(t *testing.T, x Fixture) {
	t.Helper()
	st, ctx := x.Backend, x.Context
	now := time.Now().UTC()

	task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", Title: "Retired features", BaseBranch: "main",
		State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	requireOK(t, st.CreateTask(ctx, task))
	read, err := st.GetTask(ctx, task.ID)
	requireOK(t, err)
	if read.ID != task.ID || read.Title != task.Title || read.Workspace != x.Workspace {
		t.Fatalf("task read=%+v", read)
	}
	held, err := st.SetTaskHold(ctx, task.ID, true)
	requireOK(t, err)
	if !held.Hold {
		t.Fatalf("task update did not persist: %+v", held)
	}
	tasks, err := st.ListTasks(ctx)
	requireOK(t, err)
	listed := false
	for _, candidate := range tasks {
		if candidate.ID == task.ID {
			listed = candidate.Hold
		}
	}
	if !listed {
		t.Fatalf("updated task missing from list: %+v", tasks)
	}
	encoded, err := json.Marshal(held)
	requireOK(t, err)
	if bytes.Contains(encoded, []byte(`"feature_id"`)) {
		t.Fatalf("task wire shape still carries a feature field: %s", encoded)
	}

	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Retirement planning"})
	requireOK(t, err)
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Retirement requirement"}, core.RequirementVersion{
		Content: "# Retirement requirement", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep attachments readable."}},
	})
	requireOK(t, err)

	taskContext, err := st.CreateArtifact(ctx, core.Artifact{Name: "task.txt", ContentType: "text/plain", TaskID: task.ID}, []byte("task context "+task.ID))
	requireOK(t, err)
	planning, err := st.CreateArtifact(ctx, core.Artifact{Name: "planning.txt", ContentType: "text/plain", PlanningSessionID: session.ID}, []byte("planning context "+session.ID))
	requireOK(t, err)
	unattached, err := st.CreateArtifact(ctx, core.Artifact{Name: "unattached.png", ContentType: "image/png"}, testimage.PNG("unattached-"+task.ID))
	requireOK(t, err)
	if unattached.TaskID != "" || unattached.RequirementID != "" || unattached.PlanningSessionID != "" {
		t.Fatalf("unattached upload gained an owner: %+v", unattached)
	}

	// One content address, three ownership links: a task context link, a
	// requirement link whose duplicate upload keeps the persisted metadata,
	// and a distinct verification-evidence role on the same task.
	shared := testimage.PNG("shared-" + task.ID)
	first, err := st.CreateArtifact(ctx, core.Artifact{Name: "first.png", ContentType: "image/png", TaskID: task.ID}, shared)
	requireOK(t, err)
	duplicate, err := st.CreateArtifact(ctx, core.Artifact{Name: "second.png", ContentType: "image/png", RequirementID: requirement.ID}, shared)
	requireOK(t, err)
	if duplicate.ID != first.ID || duplicate.Name != "first.png" || duplicate.SizeBytes != first.SizeBytes || !duplicate.CreatedAt.Equal(first.CreatedAt) || duplicate.RequirementID != requirement.ID {
		t.Fatalf("duplicate upload relabeled persisted metadata: first=%+v duplicate=%+v", first, duplicate)
	}
	evidence, err := st.CreateArtifact(ctx, core.Artifact{Name: "proof.png", ContentType: "image/png", Role: core.ArtifactRoleVerificationEvidence, TaskID: task.ID}, shared)
	requireOK(t, err)
	if evidence.ID != first.ID || evidence.Role != core.ArtifactRoleVerificationEvidence || !evidence.EligibleVerificationEvidence() {
		t.Fatalf("evidence role link=%+v", evidence)
	}

	type ownership struct {
		id, task, requirement, session string
		role                           core.ArtifactRole
	}
	key := func(a core.Artifact) ownership {
		role := a.Role
		if role == "" {
			role = core.ArtifactRoleTaskContext
		}
		return ownership{a.ID, a.TaskID, a.RequirementID, a.PlanningSessionID, role}
	}
	want := map[ownership]bool{
		{taskContext.ID, task.ID, "", "", core.ArtifactRoleTaskContext}:    true,
		{planning.ID, "", "", session.ID, core.ArtifactRoleTaskContext}:    true,
		{unattached.ID, "", "", "", core.ArtifactRoleTaskContext}:          true,
		{first.ID, task.ID, "", "", core.ArtifactRoleTaskContext}:          true,
		{first.ID, "", requirement.ID, "", core.ArtifactRoleTaskContext}:   true,
		{first.ID, task.ID, "", "", core.ArtifactRoleVerificationEvidence}: true,
	}
	listedArtifacts, err := st.ListArtifacts(ctx)
	requireOK(t, err)
	got := map[ownership]bool{}
	for _, artifact := range listedArtifacts {
		got[key(artifact)] = true
		if artifact.ID == unattached.ID && artifact.EligibleVerificationEvidence() {
			t.Fatalf("unattached upload is eligible task evidence: %+v", artifact)
		}
		encoded, err := json.Marshal(artifact)
		requireOK(t, err)
		if bytes.Contains(encoded, []byte(`"feature_id"`)) {
			t.Fatalf("artifact wire shape still carries a feature field: %s", encoded)
		}
	}
	for link := range want {
		if !got[link] {
			t.Fatalf("artifact list missing ownership link %+v in %+v", link, listedArtifacts)
		}
	}
	for id, wantOwner := range map[string]ownership{
		taskContext.ID: {taskContext.ID, task.ID, "", "", core.ArtifactRoleTaskContext},
		planning.ID:    {planning.ID, "", "", session.ID, core.ArtifactRoleTaskContext},
		unattached.ID:  {unattached.ID, "", "", "", core.ArtifactRoleTaskContext},
	} {
		artifact, content, err := st.GetArtifact(ctx, id)
		requireOK(t, err)
		if key(artifact) != wantOwner || len(content) == 0 || int64(len(content)) != artifact.SizeBytes {
			t.Fatalf("artifact read=%+v bytes=%d want=%+v", artifact, len(content), wantOwner)
		}
	}
	if artifact, content, err := st.GetArtifact(ctx, first.ID); err != nil || !bytes.Equal(content, shared) || artifact.Name != "first.png" {
		t.Fatalf("shared artifact read=%+v err=%v", artifact, err)
	}
	if artifact, _, err := st.GetArtifactForPlanningSession(ctx, planning.ID, session.ID); err != nil || artifact.PlanningSessionID != session.ID {
		t.Fatalf("planning-session read=%+v err=%v", artifact, err)
	}

	// Lineage selection reaches each owner's links and never the detached
	// upload, which has no task, requirement, or planning-session owner.
	lineage, err := st.ListArtifactsForLineage(ctx, []core.LineageNode{
		{Type: core.LineageTask, ID: task.ID},
		{Type: core.LineageRequirement, ID: requirement.ID},
		{Type: core.LineagePlanningSession, ID: session.ID},
		{Type: core.LineageEvidence, ID: first.ID},
		{Type: core.LineageEvidence, ID: unattached.ID},
	})
	requireOK(t, err)
	selected := map[ownership]bool{}
	for _, artifact := range lineage {
		if artifact.ID == unattached.ID {
			t.Fatalf("detached upload entered lineage selection: %+v", artifact)
		}
		selected[key(artifact)] = true
	}
	for link := range want {
		if link.id == unattached.ID {
			continue
		}
		if !selected[link] {
			t.Fatalf("lineage selection missing %+v in %+v", link, lineage)
		}
	}
	taskOnly, err := st.ListArtifactsForLineage(ctx, []core.LineageNode{{Type: core.LineageTask, ID: task.ID}})
	requireOK(t, err)
	for _, artifact := range taskOnly {
		if artifact.TaskID != task.ID {
			t.Fatalf("task context selected a non-task link: %+v", artifact)
		}
	}

	// Another workspace reads none of these artifacts.
	foreign := x.Workspace + "-retired-features"
	foreignCtx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), foreign)
	if _, err = st.BootstrapWorkspaceConfig(foreignCtx, &config.Config{Workspace: foreign, Repos: x.Config.Repos}); err != nil {
		t.Fatal(err)
	}
	owned := map[string]bool{taskContext.ID: true, planning.ID: true, unattached.ID: true, first.ID: true}
	for id := range owned {
		if _, _, err := st.GetArtifact(foreignCtx, id); err == nil {
			t.Fatalf("foreign workspace read artifact %s", id)
		}
	}
	foreignList, err := st.ListArtifacts(foreignCtx)
	requireOK(t, err)
	for _, artifact := range foreignList {
		if owned[artifact.ID] {
			t.Fatalf("foreign workspace listed artifact %+v", artifact)
		}
	}
}
