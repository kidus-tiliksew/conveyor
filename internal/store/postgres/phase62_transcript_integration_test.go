package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// On finalize a planning session archives its transcript as an artifact linked
// to what it produced, so the rationale behind a requirement becomes lineage
// instead of evaporating in a chat window (spec §9, §21.46 change 2; AC-2).
// The transcript rides the ordinary artifact machinery — content-addressed,
// immutable, and attached to exactly one owner.

func newPhase62IntegrationStore(t *testing.T) (*Store, context.Context, string) {
	t.Helper()
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	workspace := "phase62-transcript-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{
		Workspace: workspace,
		Repos:     []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}},
	}); err != nil {
		t.Fatal(err)
	}
	return st, ctx, workspace
}

func TestPhase62TranscriptArchivesOntoProducedRequirementIntegration(t *testing.T) {
	st, ctx, _ := newPhase62IntegrationStore(t)

	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{
		ID: "session-" + core.NewTaskID(), Title: "Nightly reconciliation",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []core.PlanningMessage{
		{SessionID: session.ID, Role: core.PlanningMessageUser, Content: "Payments drift overnight."},
		{SessionID: session.ID, Role: core.PlanningMessageAssistant, Content: "Proposing a reconciliation requirement."},
	} {
		if _, err = st.AppendPlanningMessage(ctx, message); err != nil {
			t.Fatal(err)
		}
	}

	requirement, first, err := st.CreateRequirement(ctx, core.Requirement{
		ID: "req-" + core.NewTaskID(), Title: "Nightly Reconciliation",
	}, core.RequirementVersion{
		Content: "Payments must reconcile nightly.",
		Statements: []core.RequirementStatement{
			{ID: "REQ-1", Statement: "Every payment reconciles within 24 hours."},
		},
		Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Confirmed {
		t.Error("a chat-produced first version is confirmed; it must stay pending")
	}

	// The transcript attaches to the requirement, not to a task: this session
	// produced intent, not work.
	transcript := []byte("user: Payments drift overnight.\nassistant: Proposing a reconciliation requirement.\n")
	artifact, err := st.CreateArtifact(ctx, core.Artifact{
		Name: session.ID + "-transcript.txt", ContentType: "text/plain",
		Role: core.ArtifactRoleGeneratedAudit, RequirementID: requirement.ID,
	}, transcript)
	if err != nil {
		t.Fatalf("archive transcript onto requirement: %v", err)
	}
	if artifact.RequirementID != requirement.ID {
		t.Errorf("artifact requirement = %q, want %q", artifact.RequirementID, requirement.ID)
	}

	finalized, err := st.FinalizePlanningSession(ctx, store.PlanningFinalizeRequest{
		SessionID: session.ID, RequirementID: requirement.ID,
		TranscriptArtifactID: artifact.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Status != core.PlanningSessionFinalized {
		t.Errorf("session status = %q", finalized.Status)
	}
	if finalized.ProducedRequirementID != requirement.ID {
		t.Errorf("produced requirement = %q", finalized.ProducedRequirementID)
	}
	if finalized.TranscriptArtifactID != artifact.ID {
		t.Errorf("transcript artifact = %q", finalized.TranscriptArtifactID)
	}
	if finalized.ProducedTaskID != "" {
		t.Errorf("requirement-producing session also claims task %q", finalized.ProducedTaskID)
	}

	// The archived transcript is readable back through the ordinary artifact
	// machinery, byte-for-byte.
	stored, content, err := st.GetArtifact(ctx, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(transcript) {
		t.Errorf("transcript content = %q, want %q", content, transcript)
	}
	if stored.RequirementID != requirement.ID {
		t.Errorf("stored artifact requirement = %q", stored.RequirementID)
	}
	// A transcript is audit, never model input (spec §21.4).
	if stored.Role.ModelInputEligible() {
		t.Error("transcript artifact is model-input eligible; it is generated audit")
	}

	// Finalizing granted no approval authority: the requirement is still
	// pending an operator confirmation (spec §13.1).
	pending, err := st.GetRequirement(ctx, requirement.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.CurrentVersion != 0 {
		t.Errorf("finalizing a session confirmed version %d; confirmation is a separate operator act", pending.CurrentVersion)
	}
}

// An artifact never claims two owners, so a transcript cannot be attached to a
// requirement and a task at once (spec §21.46 change 5).
func TestPhase62TranscriptRejectsTwoOwnersIntegration(t *testing.T) {
	st, ctx, workspaceName := newPhase62IntegrationStore(t)

	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{
		ID: "req-" + core.NewTaskID(), Title: "Exclusive Owner",
	}, core.RequirementVersion{
		Content:    "Only one owner.",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "One owner."}},
		Origin:     core.RequirementOriginFeatureMigration,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskID := core.NewTaskID()
	if err = st.CreateTask(ctx, core.Task{
		ID: taskID, Workspace: workspaceName, Repo: "conveyor", Branch: "conveyor/task-" + taskID,
		State: core.TaskQueued, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = st.CreateArtifact(ctx, core.Artifact{
		Name: "ambiguous.txt", ContentType: "text/plain",
		Role: core.ArtifactRoleGeneratedAudit,
		// Two owners at once.
		TaskID: taskID, RequirementID: requirement.ID,
	}, []byte("ambiguous")); err == nil {
		t.Fatal("an artifact claimed both a task and a requirement")
	} else if !strings.Contains(err.Error(), "one of a task, feature, or requirement") {
		t.Errorf("unexpected rejection: %v", err)
	}

	// A requirement that does not exist in this workspace is not a valid owner.
	if _, err = st.CreateArtifact(ctx, core.Artifact{
		Name: "orphan.txt", ContentType: "text/plain",
		Role: core.ArtifactRoleGeneratedAudit, RequirementID: "req-does-not-exist",
	}, []byte("orphan")); err == nil {
		t.Error("an artifact attached to a nonexistent requirement")
	}
}
