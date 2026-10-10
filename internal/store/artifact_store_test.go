package store

import (
	"context"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
)

func TestMemoryArtifactsAreContentAddressedAndLinked(t *testing.T) {
	t.Parallel()
	ctx := WithActor(context.Background(), SystemActor())
	st := NewMemory()
	if err := st.CreateTask(ctx, core.Task{ID: "task"}); err != nil {
		t.Fatal(err)
	}
	artifact := core.Artifact{Name: "brief.txt", ContentType: "text/plain", TaskID: "task"}
	created, err := st.CreateArtifact(ctx, artifact, []byte("brief"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := st.CreateArtifact(ctx, artifact, []byte("brief"))
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != again.ID {
		t.Fatalf("dedupe failed")
	}
	if created.Role != core.ArtifactRoleTaskContext || !created.Role.ModelInputEligible() {
		t.Fatalf("default artifact role = %q", created.Role)
	}
	_, content, err := st.GetArtifact(ctx, created.ID)
	if err != nil || string(content) != "brief" {
		t.Fatalf("content=%q err=%v", content, err)
	}
}

func TestMemoryVerificationEvidenceEnforcesRoleMediaLimitsAndOwnership(t *testing.T) {
	t.Parallel()
	st := NewMemory()
	ctx := WithWorkspace(WithActor(context.Background(), SystemActor()), "demo")
	for _, id := range []string{"task-a", "task-b"} {
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: "demo"}); err != nil {
			t.Fatal(err)
		}
	}
	evidence, err := st.CreateArtifact(ctx, core.Artifact{
		Name: "proof.png", ContentType: "IMAGE/PNG; charset=binary",
		Role: core.ArtifactRoleVerificationEvidence, TaskID: "task-a",
	}, testimage.PNG("proof"))
	if err != nil || evidence.ContentType != "image/png" || !evidence.EligibleVerificationEvidence() {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
	for _, invalid := range []core.Artifact{
		{Name: "wrong.gif", ContentType: "image/gif", Role: core.ArtifactRoleVerificationEvidence, TaskID: "task-a"},
		{Name: "unattached.png", ContentType: "image/png", Role: core.ArtifactRoleVerificationEvidence},
		{Name: "missing.png", ContentType: "image/png", Role: core.ArtifactRoleVerificationEvidence, TaskID: "missing"},
	} {
		if _, err = st.CreateArtifact(ctx, invalid, []byte("bytes")); err == nil {
			t.Fatalf("accepted invalid evidence %+v", invalid)
		}
	}
	if _, err = st.CreateArtifact(ctx, core.Artifact{
		Workspace: "other", Name: "cross.png", ContentType: "image/png",
		Role: core.ArtifactRoleVerificationEvidence, TaskID: "task-a",
	}, []byte("bytes")); err == nil {
		t.Fatal("accepted cross-workspace evidence")
	}
}

func TestMemoryArtifactsScopeIdenticalContentByWorkspace(t *testing.T) {
	t.Parallel()
	st := NewMemory()
	ctxA := WithWorkspace(WithActor(context.Background(), SystemActor()), "workspace-a")
	ctxB := WithWorkspace(WithActor(context.Background(), SystemActor()), "workspace-b")
	if err := st.CreateTask(ctxA, core.Task{ID: "task-a", Workspace: "workspace-a"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctxB, core.Task{ID: "task-b", Workspace: "workspace-b"}); err != nil {
		t.Fatal(err)
	}

	content := []byte("shared content")
	artifactA, err := st.CreateArtifact(ctxA, core.Artifact{Name: "a.txt", ContentType: "text/plain", TaskID: "task-a"}, content)
	if err != nil {
		t.Fatal(err)
	}
	artifactAAgain, err := st.CreateArtifact(ctxA, core.Artifact{Name: "a.txt", ContentType: "text/plain", TaskID: "task-a"}, content)
	if err != nil {
		t.Fatal(err)
	}
	artifactB, err := st.CreateArtifact(ctxB, core.Artifact{Name: "b.txt", ContentType: "text/plain", TaskID: "task-b"}, content)
	if err != nil {
		t.Fatal(err)
	}
	if artifactA.ID != artifactAAgain.ID || artifactA.ID != artifactB.ID {
		t.Fatalf("content-addressed IDs differ: %q, %q, %q", artifactA.ID, artifactAAgain.ID, artifactB.ID)
	}

	for _, test := range []struct {
		name      string
		ctx       context.Context
		artifact  core.Artifact
		taskID    string
		workspace string
	}{
		{name: "workspace A", ctx: ctxA, artifact: artifactA, taskID: "task-a", workspace: "workspace-a"},
		{name: "workspace B", ctx: ctxB, artifact: artifactB, taskID: "task-b", workspace: "workspace-b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved, got, err := st.GetArtifact(test.ctx, test.artifact.ID)
			if err != nil || resolved.Workspace != test.workspace || resolved.TaskID != test.taskID || string(got) != string(content) {
				t.Fatalf("resolved=%+v content=%q err=%v", resolved, got, err)
			}
			listed, err := st.ListArtifacts(test.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 || listed[0].Workspace != test.workspace || listed[0].TaskID != test.taskID {
				t.Fatalf("workspace artifact list leaked another workspace: %+v", listed)
			}
		})
	}

	if _, _, err := st.GetArtifact(WithActor(context.Background(), SystemActor()), artifactA.ID); err == nil {
		t.Fatal("unscoped read of cross-workspace digest was not rejected as ambiguous")
	}
}
