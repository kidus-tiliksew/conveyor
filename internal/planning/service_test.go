package planning

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
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

func TestExplorationLazilyPinsConfiguredReposAndKeepsImmutableRevision(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	primary := createPlanningRepo(t, filepath.Join(tmp, "primary"), "README.md", "primary\n")
	secondary := createPlanningRepo(t, filepath.Join(tmp, "secondary"), "internal/eligibility.go",
		"package internal\n\nfunc eligible() bool { return true }\n")
	cfg := &config.Config{
		Workspace: "demo", CacheDir: filepath.Join(tmp, "cache"),
		Repos: []config.Repo{
			{Name: "primary", URL: "file://" + primary, Base: "main"},
			{Name: "secondary", URL: "file://" + secondary, Base: "main"},
		},
		PlanningModels: []string{"planner", "planner-alt"},
		ExecutionSettings: &config.ContextualExecutionSettings{ControlPlane: config.ControlPlaneSettings{
			Planning: config.PlanningSettings{
				Model: "planner", Effort: "high", TimeoutText: "10m", ExplorationOutputTokens: 100,
			},
		}},
	}
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	service := &Service{
		Store: st, Git: gitx.NewManager(cfg.CacheDir, ""),
		ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
	}
	if _, err := service.CreateSession(ctx, "Rejected", "", "outside"); err == nil ||
		!strings.Contains(err.Error(), "configured models: planner, planner-alt") {
		t.Fatalf("off-allowlist model error=%v", err)
	}
	session, err := service.CreateSession(ctx, "Explore eligibility", "", "planner-alt")
	if err != nil {
		t.Fatal(err)
	}
	if session.Model != "planner-alt" || session.Effort != "high" ||
		session.PinnedRevisions["primary"] == "" || session.PinnedRevisions["secondary"] != "" {
		t.Fatalf("created session=%+v", session)
	}

	listed, err := service.explorationTool(ctx, session, toolCall{
		Name: "list_files", ArgumentsJSON: `{"repo":"secondary","path":"internal","glob":"*.go","depth":0}`,
	})
	if err != nil || !strings.Contains(listed.Output.(string), "internal/eligibility.go") {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	pinned, err := st.GetPlanningSession(ctx, session.ID)
	if err != nil || pinned.PinnedRevisions["secondary"] == "" {
		t.Fatalf("secondary was not pinned: %+v err=%v", pinned, err)
	}
	secondaryRevision := pinned.PinnedRevisions["secondary"]

	if err := os.WriteFile(filepath.Join(secondary, "internal", "eligibility.go"),
		[]byte("package internal\n\nfunc eligible() bool { return false }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runPlanningGit(t, secondary, "add", ".")
	runPlanningGit(t, secondary, "commit", "-m", "advance secondary")
	read, err := service.explorationTool(ctx, pinned, toolCall{
		Name: "read_file", ArgumentsJSON: `{"repo":"secondary","path":"internal/eligibility.go","offset":1,"limit":10}`,
	})
	if err != nil || !strings.Contains(read.Output.(string), "return true") ||
		strings.Contains(read.Output.(string), "return false") {
		t.Fatalf("read=%+v err=%v", read, err)
	}
	if !strings.Contains(read.Output.(string), "internal/eligibility.go (lines 1–3 of 3)") ||
		!strings.Contains(read.Output.(string), "     3\tfunc eligible") {
		t.Fatalf("read_file contract=%q", read.Output)
	}
	grepResult, err := service.explorationTool(ctx, pinned, toolCall{
		Name: "grep", ArgumentsJSON: `{"repo":"secondary","pattern":"eligible","context":0,"mode":"content"}`,
	})
	if err != nil || !strings.Contains(grepResult.Output.(string), "internal/eligibility.go:3:") ||
		strings.Contains(grepResult.Output.(string), secondaryRevision) {
		t.Fatalf("grep=%+v err=%v", grepResult, err)
	}
	historyResult, err := service.explorationTool(ctx, pinned, toolCall{
		Name: "history", ArgumentsJSON: `{"repo":"secondary","path":"internal/eligibility.go","n":20}`,
	})
	if err != nil || !strings.Contains(historyResult.Output.(string), "initial") ||
		!strings.Contains(historyResult.Output.(string), "Latest commit context") {
		t.Fatalf("history=%+v err=%v", historyResult, err)
	}
	after, _ := st.GetPlanningSession(ctx, session.ID)
	if after.PinnedRevisions["secondary"] != secondaryRevision {
		t.Fatalf("secondary revision changed: %s -> %s", secondaryRevision, after.PinnedRevisions["secondary"])
	}
	if _, err = service.explorationTool(ctx, after, toolCall{
		Name: "grep", ArgumentsJSON: `{"repo":"outside","pattern":"eligible","context":0,"mode":"content"}`,
	}); err == nil || !strings.Contains(err.Error(), "configured repositories: primary, secondary") {
		t.Fatalf("unknown repo error=%v", err)
	}

	if _, err = st.RecordPlanningExplorationTokens(ctx, session.ID, 1500); err != nil {
		t.Fatal(err)
	}
	low, err := service.explorationTool(ctx, after, toolCall{
		Name: "grep", ArgumentsJSON: `{"repo":"secondary","pattern":"eligible","context":0,"mode":"content"}`,
	})
	if err != nil || !strings.Contains(low.Output.(string), "session exploration budget low; prefer targeted reads") {
		t.Fatalf("low-budget output=%+v err=%v", low, err)
	}
}

func TestExplorationToolSchemasAreStrictAndExposeNoRevision(t *testing.T) {
	schemas := explorationToolSchemas()
	if len(schemas) != 4 {
		t.Fatalf("schema count=%d", len(schemas))
	}
	for _, schema := range schemas {
		parameters := schema["parameters"].(map[string]any)
		properties := parameters["properties"].(map[string]any)
		if parameters["additionalProperties"] != false {
			t.Fatalf("%s accepts additional properties", schema["name"])
		}
		if _, exists := properties["ref"]; exists {
			t.Fatalf("%s exposes a caller-controlled revision", schema["name"])
		}
		if _, exists := properties["repo"]; !exists {
			t.Fatalf("%s omits repo selection", schema["name"])
		}
	}
}

func createPlanningRepo(t *testing.T, directory, file, content string) string {
	t.Helper()
	runPlanningGit(t, "", "init", "-b", "main", directory)
	runPlanningGit(t, directory, "config", "user.email", "planning@example.com")
	runPlanningGit(t, directory, "config", "user.name", "Planning Test")
	target := filepath.Join(directory, file)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runPlanningGit(t, directory, "add", ".")
	runPlanningGit(t, directory, "commit", "-m", "initial")
	return directory
}

func runPlanningGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
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

func TestServiceAllocatesDeterministicRequirementSlugSuffixes(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	seed := func(id, slug, title string) {
		t.Helper()
		if _, _, err := st.CreateRequirement(ctx, core.Requirement{
			ID: id, Slug: slug, Title: title,
		}, core.RequirementVersion{
			Content:    "Seeded prose.",
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Seeded."}},
			Origin:     core.RequirementOriginFeatureMigration,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("req-auth", "auth", "Auth")
	seed("req-auth-2", "auth-2", "Auth 2")
	service := &Service{Store: st}
	version := core.RequirementVersion{
		Content:    "New prose.",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "New."}},
		Origin:     core.RequirementOriginFeatureMigration,
	}
	auth, _, err := service.createRequirementWithAvailableSlug(ctx, "req-auth-new", "Auth", version)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Slug != "auth-3" {
		t.Fatalf("same-title slug=%q want auth-3", auth.Slug)
	}
	authTwo, _, err := service.createRequirementWithAvailableSlug(ctx, "req-auth-two-new", "Auth 2", version)
	if err != nil {
		t.Fatal(err)
	}
	if authTwo.Slug != "auth-2-2" {
		t.Fatalf("independent-base collision slug=%q want auth-2-2", authTwo.Slug)
	}
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

func TestServiceAbandonmentWinsBeforeFinalizationWithoutVisibleOutput(t *testing.T) {
	tests := []struct {
		name          string
		sessionID     string
		call          toolCall
		finalizer     func(store.Store, *bool) BlueprintFinalizer
		requirementID string
		taskID        string
	}{
		{
			name: "requirement", sessionID: "session-260730-abandon-req",
			call: toolCall{
				ID: "call-final", Name: "finalize_requirement",
				ArgumentsJSON: jsonString(t, requirementArgs{
					Title: "Must not survive", Prose: "This output loses the abandonment race.",
					Statements: []core.RequirementStatement{{
						ID: "REQ-1", Statement: "No output remains after abandonment.",
					}},
				}),
			},
			requirementID: "req-260730-abandon-req",
		},
		{
			name: "blueprint", sessionID: "session-260730-abandon-task",
			call: toolCall{
				ID: "call-final", Name: "finalize_blueprint",
				ArgumentsJSON: jsonString(t, blueprintArgs{
					Title: "Must not survive", Repo: "conveyor",
					Markdown: "## Intent\n\nLose the abandonment race.\n\n## Non-goals\n\nNo durable output.",
					Acceptance: []pipeline.AcceptanceCriterion{{
						ID: "AC-1", Criterion: "No output remains after abandonment.", Verify: "test",
					}},
				}),
			},
			taskID: "260730-abandon-task",
			finalizer: func(st store.Store, called *bool) BlueprintFinalizer {
				return func(
					ctx context.Context,
					sessionID, taskID, title, repo string,
					_ pipeline.StructuredSpec,
					_ string,
				) (core.Task, core.SpecVersion, error) {
					*called = true
					task := core.Task{
						ID: taskID, Workspace: "demo", Source: "planning:" + sessionID,
						Title: title, Repo: repo, State: core.TaskAwaiting,
					}
					if err := st.CreateTask(ctx, task); err != nil {
						return core.Task{}, core.SpecVersion{}, err
					}
					return task, core.SpecVersion{TaskID: taskID, Version: 1}, nil
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, st, session := planningFixture(t, tt.sessionID)
			called := false
			service := &Service{
				Store: st, Agent: &scriptedAgent{outputs: []string{
					decisionJSON(t, "", []toolCall{tt.call}),
				}}, Model: "planner",
			}
			if tt.finalizer != nil {
				service.FinalizeBlueprint = tt.finalizer(st, &called)
			}

			// The model's final tool request is already durable and visible on
			// the stream when abandonment wins. The finalization region must
			// recheck active state before invoking any produced-write callback.
			err := service.Run(ctx, session.ID, UserMessage{Content: "Finalize it."}, func(part map[string]any) error {
				if part["type"] == "tool-input-available" {
					_, abandonErr := st.AbandonPlanningSession(ctx, session.ID)
					return abandonErr
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "abandoned") {
				t.Fatalf("Run error=%v, want terminal abandonment", err)
			}
			if called {
				t.Fatal("blueprint finalizer ran after abandonment won")
			}
			if tt.requirementID != "" {
				if _, getErr := st.GetRequirement(ctx, tt.requirementID); getErr == nil {
					t.Fatalf("requirement %s remained visible", tt.requirementID)
				}
			}
			if tt.taskID != "" {
				if _, getErr := st.GetTask(ctx, tt.taskID); getErr == nil {
					t.Fatalf("task %s remained visible", tt.taskID)
				}
			}
			artifacts, listErr := st.ListArtifacts(ctx)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(artifacts) != 0 {
				t.Fatalf("artifacts after abandonment=%+v, want none", artifacts)
			}
			abandoned, getErr := st.GetPlanningSession(ctx, session.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if abandoned.Status != core.PlanningSessionAbandoned ||
				abandoned.ProducedRequirementID != "" ||
				abandoned.ProducedTaskID != "" ||
				abandoned.TranscriptArtifactID != "" {
				t.Fatalf("abandoned session=%+v", abandoned)
			}
		})
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

type blockingArtifactStore struct {
	store.Store
	beforeArtifact   chan struct{}
	continueArtifact chan struct{}
}

func (s *blockingArtifactStore) CreateArtifact(
	ctx context.Context,
	artifact core.Artifact,
	content []byte,
) (core.Artifact, error) {
	close(s.beforeArtifact)
	select {
	case <-s.continueArtifact:
	case <-ctx.Done():
		return core.Artifact{}, ctx.Err()
	}
	return s.Store.CreateArtifact(ctx, artifact, content)
}

func TestServiceLateAbandonmentCannotSplitProducedWritesFromFinalization(t *testing.T) {
	ctx, underlying, session := planningFixture(t, "session-260730-finalize")
	blocked := &blockingArtifactStore{
		Store: underlying, beforeArtifact: make(chan struct{}), continueArtifact: make(chan struct{}),
	}
	args := requirementArgs{
		Title: "Atomic boundary", Prose: "Finalization is serialized.",
		Statements: []core.RequirementStatement{{
			ID: "REQ-1", Statement: "Abandonment cannot split produced lineage.",
		}},
	}
	service := &Service{
		Store: blocked,
		Agent: &scriptedAgent{outputs: []string{decisionJSON(t, "", []toolCall{{
			ID: "call-final", Name: "finalize_requirement", ArgumentsJSON: jsonString(t, args),
		}})}},
		Model: "planner",
	}
	runDone := make(chan error, 1)
	go func() {
		runDone <- service.Run(ctx, session.ID, UserMessage{Content: "Finalize atomically."}, func(map[string]any) error {
			return nil
		})
	}()
	select {
	case <-blocked.beforeArtifact:
	case <-time.After(2 * time.Second):
		t.Fatal("planning run did not produce the requirement before archival")
	}
	abandonDone := make(chan error, 1)
	go func() {
		_, err := underlying.AbandonPlanningSession(ctx, session.ID)
		abandonDone <- err
	}()
	select {
	case err := <-abandonDone:
		t.Fatalf("abandonment interleaved inside finalization: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(blocked.continueArtifact)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("planning run did not finalize")
	}
	select {
	case err := <-abandonDone:
		if err == nil || !strings.Contains(err.Error(), "already finalized") {
			t.Fatalf("late abandonment error=%v, want finalized rejection", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late abandonment remained blocked")
	}
	finalized, err := underlying.GetPlanningSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	requirement, requirementErr := underlying.GetRequirement(ctx, finalized.ProducedRequirementID)
	artifact, _, artifactErr := underlying.GetArtifact(ctx, finalized.TranscriptArtifactID)
	if requirementErr != nil || artifactErr != nil ||
		finalized.Status != core.PlanningSessionFinalized ||
		artifact.RequirementID != requirement.ID {
		t.Fatalf(
			"finalized=%+v requirement=%+v requirement_err=%v artifact=%+v artifact_err=%v",
			finalized, requirement, requirementErr, artifact, artifactErr,
		)
	}
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
