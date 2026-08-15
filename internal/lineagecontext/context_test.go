package lineagecontext

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type contextRecordCountingStore struct {
	store.Store
	contextBatches   int
	taskPointReads   int
	requirementReads int
	versionReads     int
}

func (s *contextRecordCountingStore) ListLineageContextRecords(ctx context.Context, nodes []core.LineageNode) (store.LineageContextRecords, error) {
	s.contextBatches++
	return s.Store.ListLineageContextRecords(ctx, nodes)
}

func (s *contextRecordCountingStore) GetTask(ctx context.Context, id string) (core.Task, error) {
	s.taskPointReads++
	return s.Store.GetTask(ctx, id)
}

func (s *contextRecordCountingStore) GetRequirement(ctx context.Context, id string) (core.Requirement, error) {
	s.requirementReads++
	return s.Store.GetRequirement(ctx, id)
}

func (s *contextRecordCountingStore) GetRequirementVersion(ctx context.Context, id string, version int) (core.RequirementVersion, error) {
	s.versionReads++
	return s.Store.GetRequirementVersion(ctx, id, version)
}

func TestAssembleBatchesGraphContextRecords(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	base := store.NewMemory()
	task := core.Task{ID: "batched-context", Workspace: "demo", Repo: "conveyor", Title: "Completed task", State: core.TaskMerged, CreatedAt: time.Now().UTC()}
	if err := base.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	requirement, version, err := base.CreateRequirement(ctx, core.Requirement{ID: "req-batched", Title: "Batched authority"}, core.RequirementVersion{
		Content: "Load graph context in bounded batches.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Batch graph context."}}, Origin: core.RequirementOriginChat, OriginSessionID: "planning-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	human := store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	if _, _, err = base.ConfirmRequirementVersion(human, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = base.ProposeRequirementServes(ctx, task.ID, requirement.ID, core.RequirementServesPlanning, false); err != nil {
		t.Fatal(err)
	}
	if _, err = base.ConfirmRequirementServes(human, task.ID, requirement.ID); err != nil {
		t.Fatal(err)
	}
	counted := &contextRecordCountingStore{Store: base}
	result, err := Assemble(ctx, counted, nil, []core.LineageNode{{Type: core.LineageTask, ID: task.ID}, {Type: core.LineageRequirement, ID: requirement.ID}}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if counted.contextBatches != 1 || counted.taskPointReads != 0 || counted.requirementReads != 0 || counted.versionReads != 0 {
		t.Fatalf("context reads batches=%d task_points=%d requirements=%d versions=%d", counted.contextBatches, counted.taskPointReads, counted.requirementReads, counted.versionReads)
	}
	var requirementContent string
	for _, item := range result.Items {
		if item.Node.ID == requirement.ID {
			requirementContent = item.Content
		}
	}
	if !strings.Contains(requirementContent, "confirmed v1") {
		t.Fatalf("batched context items=%+v", result.Items)
	}
}

func TestRenderUntrustedContainsTripleBacktickContent(t *testing.T) {
	rendered := RenderUntrusted(Result{Items: []Item{{
		Node:            core.LineageNode{Type: core.LineageRequirement, ID: "req-fenced"},
		SelectionReason: "served_requirement",
		Content:         "before\n```conveyor:requirements\n- id: REQ-1\n```\nafter",
	}}})
	if !strings.Contains(rendered, "````text\nbefore\n```conveyor:requirements") ||
		!strings.Contains(rendered, "```\nafter\n````") {
		t.Fatalf("nested requirement fence escaped outer boundary:\n%s", rendered)
	}
}

func TestRenderUntrustedDerivesFenceAndFramesOrigin(t *testing.T) {
	rendered := RenderUntrusted(Result{Items: []Item{{
		Node: core.LineageNode{Type: core.LineageTask, ID: "task"}, SelectionReason: "task_local_artifact",
		Origin: "source", Content: "conveyor-spec contains ```` runs",
	}}})
	if !strings.Contains(rendered, "origin source") || !strings.Contains(rendered, "`````text\n") || !strings.Contains(rendered, "\n`````\n") {
		t.Fatalf("dynamic origin-framed rendering:\n%s", rendered)
	}
}

func TestPathLabelRendersCanonicalReferenceDirections(t *testing.T) {
	path := []core.LineageLink{
		{SrcType: core.LineagePlanningSession, SrcID: "session-1", DstType: core.LineageReferenceDocumentVersion, DstID: "ref-1:v2", Kind: "consulted"},
		{SrcType: core.LineageRequirementVersion, SrcID: "req-1:v1", DstType: core.LineageReferenceDocumentVersion, DstID: "ref-1:v2", Kind: "derived_from"},
	}
	want := "planning_session:session-1 ->[consulted]-> reference_document_version:ref-1:v2 | requirement_version:req-1:v1 ->[derived_from]-> reference_document_version:ref-1:v2"
	if got := pathLabel(path); got != want {
		t.Fatalf("reference path label=%q, want %q", got, want)
	}
}

func TestBlueprintSectionUsesCanonicalDecompositionAndExactID(t *testing.T) {
	encoded, err := json.Marshal(pipeline.StructuredSpec{
		Markdown:   "## Intent\nRetain the useful parent intent.\n\n## Non-goals\nDo not select neighboring children.",
		Acceptance: []pipeline.AcceptanceCriterion{{ID: "AC-1", Criterion: "works", Verify: "test"}},
		Decomposition: []pipeline.DecompositionItem{
			{ID: "SUB-1", Repo: "conveyor", Summary: "Implement the exact child", DependsOn: []string{"SUB-10"}},
			{ID: "SUB-10", Repo: "conveyor", Summary: "Wrong neighboring child", DependsOn: []string{}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := pipeline.RenderStructuredSpec(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	got := blueprintSection(spec.Markdown, "SUB-1")
	for _, want := range []string{"## Intent", "Retain the useful parent intent.", "## Non-goals", "Implement the exact child", "Depends on: SUB-10"} {
		if !strings.Contains(got, want) {
			t.Fatalf("section missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Wrong neighboring child") {
		t.Fatalf("SUB-1 matched SUB-10:\n%s", got)
	}
}

func TestAssembleRetainsDirectReviewEvidenceUnderBytePressure(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	now := time.Now().UTC()
	dependency := core.Task{ID: "dependency", Workspace: "demo", Title: strings.Repeat("adjacent", 30), State: core.TaskMerged, CreatedAt: now}
	if err := st.CreateTask(ctx, dependency); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "review-task", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, Dependencies: []core.TaskRelation{{ID: dependency.ID, State: dependency.State}}, CreatedAt: now.Add(time.Second)}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	evidence, err := st.CreateArtifact(ctx, core.Artifact{Name: "proof.png", ContentType: "image/png", Role: core.ArtifactRoleVerificationEvidence, TaskID: task.ID}, []byte("png"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.CreateArtifact(ctx, core.Artifact{Name: strings.Repeat("local-context-", 30) + ".md", ContentType: "text/markdown", Role: core.ArtifactRoleTaskContext, TaskID: task.ID}, []byte("lower-priority local context")); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ExecutionSettings: &config.ContextualExecutionSettings{ControlPlane: config.ControlPlaneSettings{Planning: config.PlanningSettings{Context: config.LineageContextSettings{Depth: 3, Nodes: 32, RenderableBytes: 400, ArtifactRefs: 8}}}}}
	result, err := Assemble(ctx, st, cfg, []core.LineageNode{{Type: core.LineageTask, ID: task.ID}}, task.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].ID != evidence.ID {
		t.Fatalf("authorized artifacts=%+v, want direct evidence %s", result.Artifacts, evidence.ID)
	}
	if len(result.Items) != 1 || result.Items[0].SelectionReason != "direct_task_verification_evidence" || result.OmittedCount == 0 {
		t.Fatalf("items=%+v omitted=%d", result.Items, result.OmittedCount)
	}
}

func TestAssembleReportsRenderableByteExhaustion(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "task-budget", Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-task-budget", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateArtifact(ctx, core.Artifact{Name: "context.md", ContentType: "text/markdown", TaskID: task.ID}, []byte("bounded context")); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ExecutionSettings: &config.ContextualExecutionSettings{ControlPlane: config.ControlPlaneSettings{Planning: config.PlanningSettings{Context: config.LineageContextSettings{Depth: 3, Nodes: 32, RenderableBytes: 1}}}}}
	result, err := Assemble(ctx, st, cfg, []core.LineageNode{{Type: core.LineageTask, ID: task.ID}}, task.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 0 || result.OmittedCount != 1 {
		t.Fatalf("items=%d omitted=%d, want zero items and one omission", len(result.Items), result.OmittedCount)
	}
	if len(result.ExhaustionReasons) != 1 || result.ExhaustionReasons[0] != "renderable_bytes" {
		t.Fatalf("exhaustion reasons=%v, want renderable_bytes", result.ExhaustionReasons)
	}
}
