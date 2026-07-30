package planning

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type scriptedAgent struct {
	outputs []string
	inputs  []inprocess.Input
	models  []string
}

func (a *scriptedAgent) Run(_ context.Context, model string, input inprocess.Input) (inprocess.Result, error) {
	a.inputs = append(a.inputs, input)
	a.models = append(a.models, model)
	if len(a.outputs) == 0 {
		return inprocess.Result{}, fmt.Errorf("script exhausted")
	}
	output := a.outputs[0]
	a.outputs = a.outputs[1:]
	return inprocess.Result{Output: output, Model: model}, nil
}

func TestServiceStreamsAndPersistsTextParts(t *testing.T) {
	ctx, st, session := planningFixture(t, "session-text")
	agent := &scriptedAgent{outputs: []string{decisionJSON(t, "Which repository should this target?", nil)}}
	service := &Service{Store: st, Agent: agent, Model: "planner"}
	var chunks []map[string]any
	if err := service.Run(ctx, session.ID, UserMessage{Content: "Plan a retry policy."}, func(part map[string]any) error {
		chunks = append(chunks, part)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if agent.models[0] != "planner" || agent.inputs[0].OutputSchema == nil ||
		agent.inputs[0].OutputSchema.Name != "planning_step" {
		t.Fatalf("model inputs = %+v / %+v", agent.models, agent.inputs[0])
	}
	assertChunkTypes(t, chunks, "start", "start-step", "text-start", "text-delta", "text-end", "finish-step", "finish")
	messages, err := st.ListPlanningMessages(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != core.PlanningMessageUser ||
		messages[1].Role != core.PlanningMessageAssistant ||
		messages[1].Content != "Which repository should this target?" ||
		!strings.Contains(string(messages[1].Parts), `"text-delta"`) {
		t.Fatalf("messages = %+v", messages)
	}
}

func TestServiceFinalizesUnconfirmedRequirementAndArchivesTranscript(t *testing.T) {
	ctx, st, session := planningFixture(t, "session-260730-a1b2c3")
	args := requirementArgs{
		Title: "Retry policy", Prose: "Retries must remain bounded and explainable.",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Retry attempts stop at the configured bound."}},
	}
	agent := &scriptedAgent{outputs: []string{decisionJSON(t, "", []toolCall{{
		ID: "call-final", Name: "finalize_requirement", ArgumentsJSON: jsonString(t, args),
	}})}}
	service := &Service{Store: st, Agent: agent, Model: "planner"}
	var chunks []map[string]any
	if err := service.Run(ctx, session.ID, UserMessage{Content: "Capture this requirement."}, func(part map[string]any) error {
		chunks = append(chunks, part)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requirement, err := st.GetRequirement(ctx, "req-260730-a1b2c3")
	if err != nil {
		t.Fatal(err)
	}
	version, err := st.GetRequirementVersion(ctx, requirement.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if requirement.CurrentVersion != 0 || version.Confirmed ||
		version.Origin != core.RequirementOriginChat || version.OriginSessionID != session.ID {
		t.Fatalf("requirement/version = %+v / %+v", requirement, version)
	}
	finalized, err := st.GetPlanningSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Status != core.PlanningSessionFinalized ||
		finalized.ProducedRequirementID != requirement.ID ||
		finalized.ProducedTaskID != "" || finalized.TranscriptArtifactID == "" {
		t.Fatalf("finalized session = %+v", finalized)
	}
	artifact, content, err := st.GetArtifact(ctx, finalized.TranscriptArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Role != core.ArtifactRoleGeneratedAudit || artifact.RequirementID != requirement.ID ||
		!strings.Contains(string(content), `"tool-output-available"`) {
		t.Fatalf("artifact = %+v content=%s", artifact, content)
	}
	assertChunkTypes(t, chunks, "start", "start-step", "tool-input-available", "tool-output-available", "finish-step", "finish")
}

func TestServiceFinalizesBlueprintAtExistingGateContract(t *testing.T) {
	ctx, st, session := planningFixture(t, "session-260730-b4c5d6")
	args := blueprintArgs{
		Title: "Bound retries", Repo: "conveyor",
		Markdown: "## Intent\n\nBound retries.\n\n## Non-goals\n\nNo queue rewrite.",
		Acceptance: []pipeline.AcceptanceCriterion{{
			ID: "AC-1", Criterion: "Retries stop at the configured bound.", Verify: "test",
		}},
	}
	agent := &scriptedAgent{outputs: []string{decisionJSON(t, "", []toolCall{{
		ID: "call-blueprint", Name: "finalize_blueprint", ArgumentsJSON: jsonString(t, args),
	}})}}
	var gotModel string
	service := &Service{
		Store: st, Agent: agent, Model: "planner",
		FinalizeBlueprint: func(
			ctx context.Context,
			sessionID, taskID, title, repo string,
			spec pipeline.StructuredSpec,
			model string,
		) (core.Task, core.SpecVersion, error) {
			gotModel = model
			task := core.Task{
				ID: taskID, Workspace: "demo", Source: "planning:" + sessionID,
				Title: title, Repo: repo, State: core.TaskAwaiting, NextStage: core.StageImplement,
			}
			if err := st.CreateTask(ctx, task); err != nil {
				return core.Task{}, core.SpecVersion{}, err
			}
			return task, core.SpecVersion{TaskID: taskID, Version: 1, Content: spec.Markdown}, nil
		},
	}
	if err := service.Run(ctx, session.ID, UserMessage{Content: "Finalize the blueprint."}, func(map[string]any) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	finalized, err := st.GetPlanningSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotModel != "planner" || finalized.Status != core.PlanningSessionFinalized ||
		finalized.ProducedTaskID != "260730-b4c5d6" || finalized.ProducedRequirementID != "" {
		t.Fatalf("model=%q finalized=%+v", gotModel, finalized)
	}
	artifact, _, err := st.GetArtifact(ctx, finalized.TranscriptArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.TaskID != finalized.ProducedTaskID || artifact.Role != core.ArtifactRoleGeneratedAudit {
		t.Fatalf("transcript artifact = %+v", artifact)
	}
}

func TestServiceStopsAtBoundedToolLoop(t *testing.T) {
	ctx, st, session := planningFixture(t, "session-bounded")
	call := []toolCall{{ID: "call-read", Name: "list_requirements", ArgumentsJSON: `{}`}}
	agent := &scriptedAgent{outputs: []string{
		decisionJSON(t, "", call),
		decisionJSON(t, "", []toolCall{{ID: "call-read-again", Name: "list_requirements", ArgumentsJSON: `{}`}}),
	}}
	service := &Service{Store: st, Agent: agent, Model: "planner", MaxSteps: 2}
	err := service.Run(ctx, session.ID, UserMessage{Content: "Keep reading forever."}, func(map[string]any) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "bounded 2-step limit") {
		t.Fatalf("error = %v", err)
	}
	restored, getErr := st.GetPlanningSession(ctx, session.ID)
	if getErr != nil || restored.Status != core.PlanningSessionActive {
		t.Fatalf("session=%+v err=%v", restored, getErr)
	}
}

func TestServiceEmitsStepBoundariesAroundToolLoop(t *testing.T) {
	ctx, st, session := planningFixture(t, "session-step-boundaries")
	agent := &scriptedAgent{outputs: []string{
		decisionJSON(t, "", []toolCall{{
			ID: "call-read", Name: "list_requirements", ArgumentsJSON: `{}`,
		}}),
		decisionJSON(t, "I found no existing requirement; what should the first statement guarantee?", nil),
	}}
	service := &Service{Store: st, Agent: agent, Model: "planner"}
	var chunks []map[string]any
	if err := service.Run(ctx, session.ID, UserMessage{Content: "Check the corpus first."}, func(part map[string]any) error {
		chunks = append(chunks, part)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertChunkTypes(t, chunks,
		"start", "start-step",
		"tool-input-available", "tool-output-available", "finish-step",
		"start-step", "text-start", "text-delta", "text-end", "finish-step", "finish",
	)
}

func planningFixture(t *testing.T, id string) (context.Context, store.Store, core.PlanningSession) {
	t.Helper()
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: id, Title: "Planning"})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, st, session
}

func decisionJSON(t *testing.T, text string, calls []toolCall) string {
	t.Helper()
	if calls == nil {
		calls = []toolCall{}
	}
	return jsonString(t, decision{ResponseText: text, ToolCalls: calls})
}

func jsonString(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertChunkTypes(t *testing.T, chunks []map[string]any, want ...string) {
	t.Helper()
	got := make([]string, len(chunks))
	for index, chunk := range chunks {
		got[index], _ = chunk["type"].(string)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("chunk types=%v, want %v", got, want)
	}
}
