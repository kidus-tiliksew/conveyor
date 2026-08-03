package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"gopkg.in/yaml.v3"
)

type capturingInputAgent struct {
	input inprocess.Input
	calls int
}

func (agent *capturingInputAgent) Run(_ context.Context, _ string, input inprocess.Input) (inprocess.Result, error) {
	agent.calls++
	agent.input = input
	return inprocess.Result{Output: "```conveyor:triage\n{\"class\":\"feature\",\"route\":\"implement\",\"summary\":\"context received\"}\n```"}, nil
}

type failingTranscriptAgent struct {
	attachmentCounts []int
}

type concurrentSpecDispatchStore struct {
	store.Store

	mu             sync.Mutex
	arrivals       int
	firstTimedOut  bool
	raceObserved   bool
	arrivalsReady  chan struct{}
	snapshots      int
	snapshotsReady chan struct{}
	createCalls    int
}

type taskReadFailureStore struct{ store.Store }

func (taskReadFailureStore) GetTask(context.Context, string) (core.Task, error) {
	return core.Task{}, errors.New("task read unavailable")
}

func TestTransitionDoesNotPersistWithoutKnownDestination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "transition-read-failure", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{Store: taskReadFailureStore{Store: st}}
	if err := d.transition(ctx, task.ID, core.TaskStageAdvance, core.StageImplement, ""); err == nil {
		t.Fatal("transition succeeded despite task read failure")
	}
	persisted, err := st.GetTask(ctx, task.ID)
	if err != nil || persisted.State != core.TaskRunning {
		t.Fatalf("task=%+v err=%v", persisted, err)
	}
}

func TestBlueprintParentDoesNotCreateImplementOrder(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	parent := core.Task{
		ID: "blueprint-parent-no-order", Workspace: "demo", Repo: "conveyor",
		Branch: "conveyor/task-blueprint-parent-no-order",
		State:  core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(),
	}
	child := core.Task{
		ID: "blueprint-child-order-owner", Workspace: "demo", Repo: "conveyor",
		Branch: "conveyor/task-blueprint-child-order-owner",
		State:  core.TaskQueued, NextStage: core.StageImplement,
		ParentTaskID: parent.ID, OriginSpecVersion: 1, OriginSubID: "SUB-1",
		CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, nil, nil)
	if err := dispatcher.DispatchNow(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, parent.ID)
	if err != nil || len(orders) != 0 {
		t.Fatalf("parent orders=%+v err=%v", orders, err)
	}
}

func newConcurrentSpecDispatchStore(st store.Store) *concurrentSpecDispatchStore {
	return &concurrentSpecDispatchStore{
		Store:          st,
		arrivalsReady:  make(chan struct{}),
		snapshotsReady: make(chan struct{}),
	}
}

func (st *concurrentSpecDispatchStore) ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	st.mu.Lock()
	st.arrivals++
	arrival := st.arrivals
	if arrival == 2 && !st.firstTimedOut {
		st.raceObserved = true
		close(st.arrivalsReady)
	}
	st.mu.Unlock()

	if arrival == 1 {
		select {
		case <-st.arrivalsReady:
		case <-time.After(50 * time.Millisecond):
			st.mu.Lock()
			st.firstTimedOut = true
			st.mu.Unlock()
		}
	}

	orders, err := st.Store.ListTaskWorkOrders(ctx, taskID)
	st.mu.Lock()
	concurrent := st.raceObserved
	if concurrent {
		st.snapshots++
		if st.snapshots == 2 {
			close(st.snapshotsReady)
		}
	}
	st.mu.Unlock()
	if concurrent {
		<-st.snapshotsReady
	}
	return orders, err
}

func (st *concurrentSpecDispatchStore) CreateStageWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, job core.Job, order core.WorkOrder) (bool, error) {
	created, err := st.Store.CreateStageWorkOrderCommand(ctx, lease, job, order)
	if created {
		st.mu.Lock()
		st.createCalls++
		st.mu.Unlock()
	}
	return created, err
}

func TestSpecStageDispatchesMCPWorkOrderWithoutInProcessFallback(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "mcp-spec", Workspace: "demo", Repo: "api", BaseBranch: "main", Branch: "conveyor/task-mcp-spec", State: core.TaskQueued, NextStage: core.StageSpec, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	// A stale pre-§21.33 route may still say in_process. New spec dispatch must
	// ignore that execution marker and create an MCP work order; only an already
	// in-flight legacy call may finish through the old completion path.
	cfg := &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Harnesses: []config.Harness{{Name: "codex", Command: []string{"codex", "{prompt}"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"spec": {Model: "gpt-spec", ModelPolicy: config.ModelPolicyExplicit, Harness: "codex", TimeoutText: "30m", Execution: config.ExecutionInProcess}}}}
	d := New(st, cfg, agent)
	if err := d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].Stage != core.StageSpec || orders[0].RequiredHarness != "codex" || orders[0].RequiredHarnessConfig == nil {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	if agent.calls != 0 {
		t.Fatalf("in-process spec fallback ran %d time(s)", agent.calls)
	}
}

func TestConcurrentSpecDispatchCreatesOneWorkOrder(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	memory := store.NewMemory()
	task := core.Task{ID: "concurrent-mcp-spec", Workspace: "demo", Repo: "api", BaseBranch: "main", State: core.TaskQueued, NextStage: core.StageSpec, CreatedAt: time.Now()}
	if err := memory.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	st := newConcurrentSpecDispatchStore(memory)
	cfg := &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Harnesses: []config.Harness{{Name: "codex", Command: []string{"codex", "{prompt}"}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"spec": {Model: "gpt-spec", ModelPolicy: config.ModelPolicyExplicit, Harness: "codex", TimeoutText: "30m", Execution: config.ExecutionMCP}}}}
	d := New(st, cfg, &capturingInputAgent{})

	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- d.DispatchNow(ctx, task.ID)
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	orders, err := memory.ListTaskWorkOrders(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	createCalls := st.createCalls
	st.mu.Unlock()
	if createCalls != 1 || len(orders) != 1 || orders[0].Stage != core.StageSpec {
		t.Fatalf("create calls=%d orders=%+v, want one spec work order", createCalls, orders)
	}
}

func (agent *failingTranscriptAgent) Run(_ context.Context, model string, input inprocess.Input) (inprocess.Result, error) {
	agent.attachmentCounts = append(agent.attachmentCounts, len(input.Attachments))
	attempt := len(agent.attachmentCounts)
	return inprocess.Result{
		Transcript: []byte(fmt.Sprintf(`{"attempt":%d}`, attempt)),
		Diagnostic: &inprocess.Diagnostic{Phase: "retry_exhausted", Provider: "openai_responses", Model: model, AttachmentCount: len(input.Attachments), Attempts: 3, HTTPStatus: 500, Retryable: true},
	}, errors.New("provider retry exhausted")
}

type artifactContextFailureStore struct {
	store.Store
	listErr bool
	readErr bool
}

func (st *artifactContextFailureStore) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	if st.listErr {
		return nil, errors.New("artifact list unavailable")
	}
	return st.Store.ListArtifacts(ctx)
}

func (st *artifactContextFailureStore) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	if st.readErr {
		return core.Artifact{}, nil, errors.New("artifact read unavailable")
	}
	return st.Store.GetArtifact(ctx, id)
}

func TestPipelinePreparesTextImageDocumentAndAudioArtifactInputs(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	if err := st.CreateFeature(ctx, core.Feature{ID: "retired-feature", Workspace: "demo", Name: "Retired"}); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "artifact-context", Workspace: "demo", Repo: "api", Title: "Use attachments", FeatureID: "retired-feature", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	largeText := bytes.Repeat([]byte("a"), (1<<20)+17)
	for _, item := range []struct {
		name, contentType string
		content           []byte
	}{
		{name: "large.txt", contentType: "text/plain", content: largeText},
		{name: "design.png", contentType: "image/png", content: []byte("png")},
		{name: "requirements.pdf", contentType: "application/pdf", content: []byte("pdf")},
		{name: "interview.mp3", contentType: "audio/mpeg", content: []byte("mp3")},
	} {
		if _, err := st.CreateArtifact(ctx, core.Artifact{Name: item.name, ContentType: item.contentType, TaskID: task.ID}, item.content); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateArtifact(ctx, core.Artifact{
		Name: "retired-feature.txt", ContentType: "text/plain", FeatureID: "retired-feature",
	}, []byte("must not enter live model context")); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt", Effort: "low", Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || agent.input.Effort != "low" || len(agent.input.Attachments) != 4 {
		t.Fatalf("calls=%d attachments=%+v", agent.calls, agent.input.Attachments)
	}
	kinds := map[inprocess.AttachmentKind]int{}
	foundLargeText := false
	for _, attachment := range agent.input.Attachments {
		kinds[attachment.Kind]++
		if attachment.Name == "retired-feature.txt" {
			t.Fatal("retired feature-scoped artifact entered live model context")
		}
		if attachment.Name == "large.txt" {
			foundLargeText = len(attachment.Content) == len(largeText)
		}
	}
	if !foundLargeText || kinds[inprocess.AttachmentDocument] != 2 || kinds[inprocess.AttachmentImage] != 1 || kinds[inprocess.AttachmentAudio] != 1 {
		t.Fatalf("kinds=%+v foundLargeText=%t", kinds, foundLargeText)
	}
}

func TestPipelineIncludesLineageDerivedSiblingArtifact(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	now := time.Now().UTC()
	parent := core.Task{ID: "context-blueprint", Workspace: "demo", State: core.TaskAwaiting, CreatedAt: now}
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: parent.ID, Content: "shared context"})
	if err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "context-child", Workspace: "demo", Repo: "api", Title: "Use sibling context", State: core.TaskQueued, NextStage: core.StageTriage,
		ParentTaskID: parent.ID, OriginSpecVersion: spec.Version, CreatedAt: now}
	sibling := core.Task{ID: "context-sibling", Workspace: "demo", State: core.TaskRunning,
		ParentTaskID: parent.ID, OriginSpecVersion: spec.Version, CreatedAt: now}
	unrelated := core.Task{ID: "context-unrelated", Workspace: "demo", State: core.TaskRunning, CreatedAt: now}
	for _, item := range []core.Task{task, sibling, unrelated} {
		if err = st.CreateTask(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		name, taskID, content string
	}{
		{name: "direct.md", taskID: task.ID, content: "direct context"},
		{name: "sibling.md", taskID: sibling.ID, content: "sibling outcome"},
		{name: "unrelated.md", taskID: unrelated.ID, content: "must stay out"},
	} {
		if _, err = st.CreateArtifact(ctx, core.Artifact{Name: item.name, ContentType: "text/markdown", TaskID: item.taskID}, []byte(item.content)); err != nil {
			t.Fatal(err)
		}
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	dispatcher := New(st, &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage": {Model: "gpt", Timeout: time.Minute},
	}}}, agent)
	dispatcher.Pack = bundle
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, attachment := range agent.input.Attachments {
		names[attachment.Name] = true
	}
	if !names["direct.md"] || !names["sibling.md"] || names["unrelated.md"] || len(names) != 2 {
		t.Fatalf("lineage-derived attachment names=%v", names)
	}
}

func TestPipelineRetriesKeepGeneratedTranscriptsOutOfStageInput(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "artifact-retry", Workspace: "demo", Repo: "api", Title: "Retry safely", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateArtifact(ctx, core.Artifact{Name: "original.png", ContentType: "image/png", TaskID: task.ID}, []byte("original-user-image")); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &failingTranscriptAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt-5.6-terra", Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	for attempt := 0; attempt < 2; attempt++ {
		if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			if _, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskInterventionRedirect, NextStage: core.StageTriage, ProjectStages: true}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !reflect.DeepEqual(agent.attachmentCounts, []int{1, 1}) {
		t.Fatalf("attachment counts grew across retry: %v", agent.attachmentCounts)
	}
	artifacts, err := st.ListArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[core.ArtifactRole]int{}
	for _, artifact := range artifacts {
		roles[artifact.Role]++
	}
	if roles[core.ArtifactRoleTaskContext] != 1 || roles[core.ArtifactRoleGeneratedAudit] != 2 {
		t.Fatalf("artifact roles = %+v artifacts=%+v", roles, artifacts)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		transcript, transcriptErr := st.GetTranscript(ctx, fmt.Sprintf("%s-triage-%d", task.ID, attempt))
		if transcriptErr != nil || !strings.HasPrefix(transcript.URI, "artifact://") {
			t.Fatalf("attempt %d transcript=%+v err=%v", attempt, transcript, transcriptErr)
		}
	}
}

func TestSpecStageInputThreadsPriorRevisionAndGateFeedback(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "spec-feedback", Workspace: "demo", Repo: "api", Title: "Revise the spec", Mode: core.TaskModeAuto, PolicyVersion: 1, SpecApproval: true, State: core.TaskQueued, NextStage: core.StageSpec, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "# Declined draft\n\nOld intent."}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateIntervention(ctx, core.Intervention{TaskID: task.ID, ActorID: "operator", ActorRole: core.ActorHuman, Action: core.InterventionRedirect, ReasonCode: "spec_stale", Comment: "Target v1.28, not v1.27."}); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"spec": {Model: "gpt", Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	// Simulate completion of a spec call that was already in flight when
	// §21.33 moved new spec dispatch to MCP work orders.
	_ = dispatcher.runInProcess(store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), cfg, task, cfg.Routing.Stages["spec"])
	if agent.calls != 1 {
		t.Fatalf("calls = %d, want 1", agent.calls)
	}
	if agent.input.OutputSchema == nil || agent.input.OutputSchema.Name != "conveyor_spec" || agent.input.OutputSchema.Schema == nil {
		t.Fatalf("spec output schema = %+v", agent.input.OutputSchema)
	}
	for _, expected := range []string{"# Prior specification revision v1", "Old intent.", "# Human gate feedback", "Target v1.28, not v1.27."} {
		if !strings.Contains(agent.input.Prompt, expected) {
			t.Fatalf("spec prompt missing %q:\n%s", expected, agent.input.Prompt)
		}
	}
}

func TestInProcessReviewEmbedsBranchDiff(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "review-diff", Workspace: "demo", Repo: "api", Title: "Review the change", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageReview, Branch: "conveyor/task-review-diff", BaseBranch: "main", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Model: "gpt", Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	dispatcher.ReviewDiff = func(_ context.Context, _ *config.Config, got core.Task) (string, error) {
		if got.ID != task.ID {
			t.Fatalf("ReviewDiff received task %q, want %q", got.ID, task.ID)
		}
		return "diff --git a/app.txt b/app.txt\n-v1\n+v2\n", nil
	}
	_ = dispatcher.DispatchNow(ctx, task.ID)
	if agent.calls != 1 {
		t.Fatalf("calls = %d, want 1", agent.calls)
	}
	for _, expected := range []string{"# Branch diff (conveyor/task-review-diff vs main)", "````diff", "diff --git a/app.txt b/app.txt", "+v2"} {
		if !strings.Contains(agent.input.Prompt, expected) {
			t.Fatalf("review prompt missing %q:\n%s", expected, agent.input.Prompt)
		}
	}
}

func TestInProcessReviewStatesWhenBranchHasNoChanges(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "review-empty", Workspace: "demo", Repo: "api", Title: "Review nothing", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageReview, Branch: "conveyor/task-review-empty", BaseBranch: "main", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Model: "gpt", Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	dispatcher.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) { return "\n", nil }
	_ = dispatcher.DispatchNow(ctx, task.ID)
	if agent.calls != 1 {
		t.Fatalf("calls = %d, want 1", agent.calls)
	}
	if !strings.Contains(agent.input.Prompt, "Branch conveyor/task-review-empty contains no changes against base main.") {
		t.Fatalf("review prompt missing empty-diff statement:\n%s", agent.input.Prompt)
	}
}

func TestInProcessReviewDiffFailuresStopBeforeModelExecution(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "review-diff-fail", Workspace: "demo", Repo: "api", Title: "Review the change", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageReview, Branch: "conveyor/task-review-diff-fail", BaseBranch: "main", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Model: "gpt", Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	dispatcher.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) {
		return "", errors.New("origin unreachable")
	}
	if err = dispatcher.DispatchNow(ctx, task.ID); err == nil || !strings.Contains(err.Error(), "origin unreachable") {
		t.Fatalf("DispatchNow error = %v, want branch diff failure", err)
	}
	if agent.calls != 0 {
		t.Fatalf("model executed despite diff failure: calls = %d", agent.calls)
	}

	oversized := strings.Repeat("a", maxModelDiffBytes+1)
	dispatcher.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) { return oversized, nil }
	if err = dispatcher.DispatchNow(ctx, task.ID); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("DispatchNow error = %v, want oversized diff failure", err)
	}
	if agent.calls != 0 {
		t.Fatalf("model executed despite oversized diff: calls = %d", agent.calls)
	}
}

func TestPipelineArtifactContextFailuresStopBeforeModelExecution(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		contentType string
		listErr     bool
		readErr     bool
		want        string
	}{
		{name: "unsupported", contentType: "application/zip", want: "unsupported context artifact"},
		{name: "list failure", contentType: "text/plain", listErr: true, want: "artifact list unavailable"},
		{name: "read failure", contentType: "text/plain", readErr: true, want: "artifact read unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := store.WithWorkspace(context.Background(), "demo")
			base := store.NewMemory()
			task := core.Task{ID: "failure-" + strings.ReplaceAll(test.name, " ", "-"), Workspace: "demo", Repo: "api", Title: "Context failure", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
			if err := base.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if _, err := base.CreateArtifact(ctx, core.Artifact{Name: "context.bin", ContentType: test.contentType, TaskID: task.ID}, []byte("content")); err != nil {
				t.Fatal(err)
			}
			wrapped := &artifactContextFailureStore{Store: base, listErr: test.listErr, readErr: test.readErr}
			bundle, err := pack.Load("../../pack")
			if err != nil {
				t.Fatal(err)
			}
			agent := &capturingInputAgent{}
			dispatcher := New(wrapped, &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt", Timeout: time.Minute}}}}, agent)
			dispatcher.Pack = bundle
			err = dispatcher.DispatchNow(ctx, task.ID)
			if err == nil || !strings.Contains(err.Error(), test.want) || agent.calls != 0 {
				t.Fatalf("error=%v calls=%d", err, agent.calls)
			}
			current, getErr := base.GetTask(ctx, task.ID)
			events, eventErr := base.ListEvents(ctx, task.ID)
			contextFailures := 0
			var diagnostic struct {
				Phase           string   `json:"phase"`
				Provider        string   `json:"provider"`
				Model           string   `json:"model"`
				AttachmentCount int      `json:"attachment_count"`
				AttachmentTypes []string `json:"attachment_types"`
			}
			for _, event := range events {
				if event.Kind == "artifact.context_failed" {
					contextFailures++
					if json.Unmarshal(event.Payload, &diagnostic) != nil {
						t.Fatalf("invalid diagnostic payload: %s", event.Payload)
					}
				}
			}
			if getErr != nil || eventErr != nil || current.State != core.TaskQueued || contextFailures != 1 {
				t.Fatalf("task=%+v events=%+v errors=%v/%v", current, events, getErr, eventErr)
			}
			if diagnostic.Phase != "attachment_preparation" || diagnostic.Provider != "openai_responses" || diagnostic.Model != "gpt" {
				t.Fatalf("diagnostic = %+v", diagnostic)
			}
			if !test.listErr && (diagnostic.AttachmentCount != 1 || len(diagnostic.AttachmentTypes) != 1 || diagnostic.AttachmentTypes[0] != test.contentType) {
				t.Fatalf("attachment summary = %+v", diagnostic)
			}
		})
	}
}

type reviewAcceptanceFlakyStore struct {
	store.Store
	failures int
}

func approvedMergeFixture(t *testing.T, githubRepo string) (context.Context, store.Store, core.Task, *Dispatcher) {
	return approvedMergeFixtureWithScope(t, githubRepo, config.RefreshReviewDelta)
}

func approvedMergeFixtureWithScope(t *testing.T, githubRepo, scope string) (context.Context, store.Store, core.Task, *Dispatcher) {
	return approvedMergeFixtureWithScopeAndGate(t, githubRepo, scope, false)
}

func approvedMergeFixtureWithScopeAndGate(t *testing.T, githubRepo, scope string, mergeApproval bool) (context.Context, store.Store, core.Task, *Dispatcher) {
	t.Helper()
	ctx := store.WithWorkspace(context.Background(), "test")
	st := store.NewMemory()
	task := core.Task{ID: "merge-task", Workspace: "test", Repo: "app", BaseBranch: "main", Branch: "conveyor/merge-task", State: core.TaskApproved, MergeApproval: mergeApproval, SetupContract: config.ExecutionSetup{RefreshReview: scope}, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: githubRepo}}}, nil)
	return ctx, st, task, d
}

func TestMergeApprovedTaskMergesOnlyAfterAuthoritativeConfirmation(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	views, merges := 0, 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		views++
		if views == 1 {
			return githubtrigger.PullRequest{Number: 12, URL: "https://github.com/acme/app/pull/12", State: "open", Mergeable: "MERGEABLE", BaseSHA: "base-sha", HeadSHA: "head-sha"}, nil
		}
		return githubtrigger.PullRequest{Number: 12, URL: "https://github.com/acme/app/pull/12", State: "closed", Merged: true, BaseSHA: "base-sha", HeadSHA: "head-sha"}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error { merges++; return nil }

	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskMerged || merges != 1 || views != 2 {
		t.Fatalf("task=%+v merges=%d views=%d err=%v", current, merges, views, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var requested, confirmed int
	for _, event := range events {
		requested += boolInt(event.Kind == "merge.requested")
		confirmed += boolInt(event.Kind == "merge.confirmed")
	}
	if requested != 1 || confirmed != 1 {
		t.Fatalf("events=%+v", events)
	}
	links, err := st.ListLineageLinks(ctx)
	if err != nil || len(links) != 1 || links[0].Kind != "merged_range" || links[0].DstID != core.CommitRangeLineageID("acme/app", "base-sha", "head-sha") {
		t.Fatalf("merge lineage=%+v err=%v", links, err)
	}
	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if merges != 1 || views != 2 {
		t.Fatalf("idempotent retry merges=%d views=%d", merges, views)
	}
}

func TestMergeApprovedTaskReconcilesAlreadyMergedPR(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "closed", Merged: true}, nil
	}
	mergeCalled := false
	d.RequestMerge = func(context.Context, string, int) error { mergeCalled = true; return nil }
	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.State != core.TaskMerged || mergeCalled {
		t.Fatalf("task=%+v mergeCalled=%t", current, mergeCalled)
	}
	events, _ := st.ListEvents(ctx, task.ID)
	found := false
	for _, event := range events {
		found = found || event.Kind == "merge.reconciled"
	}
	if !found {
		t.Fatalf("events=%+v", events)
	}
}

func TestMergeApprovedTaskFailuresStayApprovedAndAreAudited(t *testing.T) {
	for _, test := range []struct {
		name       string
		githubRepo string
		view       func(context.Context, string, string) (githubtrigger.PullRequest, error)
		merge      func(context.Context, string, int) error
		reason     string
		category   githubtrigger.ForgeErrorCategory
	}{
		{name: "non GitHub repository", reason: "unsupported_repository"},
		{name: "missing pull request", githubRepo: "acme/app", reason: "missing_pull_request", category: githubtrigger.ForgeStatus, view: func(context.Context, string, string) (githubtrigger.PullRequest, error) {
			return githubtrigger.PullRequest{}, &githubtrigger.Error{Category: githubtrigger.ForgeStatus, Err: githubtrigger.ErrPullRequestNotFound}
		}},
		{name: "forge merge failure", githubRepo: "acme/app", reason: "forge_merge_failed", category: githubtrigger.ForgePermission, view: func(context.Context, string, string) (githubtrigger.PullRequest, error) {
			return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE"}, nil
		}, merge: func(context.Context, string, int) error {
			return &githubtrigger.Error{Category: githubtrigger.ForgePermission, Err: errors.New("branch protection")}
		}},
		{name: "unconfirmed result", githubRepo: "acme/app", reason: "merge_unconfirmed", view: func() func(context.Context, string, string) (githubtrigger.PullRequest, error) {
			calls := 0
			return func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				calls++
				return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", Merged: false}, nil
			}
		}(), merge: func(context.Context, string, int) error { return nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, st, task, d := approvedMergeFixture(t, test.githubRepo)
			if test.view != nil {
				d.ViewPullRequest = test.view
			}
			if test.merge != nil {
				d.RequestMerge = test.merge
			}
			if err := d.MergeApprovedTask(ctx, task); err == nil {
				t.Fatal("expected merge error")
			}
			current, _ := st.GetTask(ctx, task.ID)
			if current.State != core.TaskApproved {
				t.Fatalf("state=%s", current.State)
			}
			events, _ := st.ListEvents(ctx, task.ID)
			last := events[len(events)-1]
			if last.Kind != "merge.failed" || !strings.Contains(string(last.Payload), test.reason) {
				t.Fatalf("last event=%+v", last)
			}
			var payload struct {
				ForgeErrorCategory string `json:"forge_error_category"`
			}
			if err := json.Unmarshal(last.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.ForgeErrorCategory != string(test.category) {
				t.Fatalf("category=%q want=%q payload=%s", payload.ForgeErrorCategory, test.category, last.Payload)
			}
		})
	}
}

func TestMergeApprovedTaskSerializesConcurrentRequests(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	merged, mergeCalls := false, 0
	var forgeMu sync.Mutex
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		forgeMu.Lock()
		defer forgeMu.Unlock()
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", Merged: merged}, nil
	}
	d.RequestMerge = func(context.Context, string, int) error {
		forgeMu.Lock()
		defer forgeMu.Unlock()
		mergeCalls++
		merged = true
		return nil
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- d.MergeApprovedTask(ctx, task)
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.State != core.TaskMerged || mergeCalls != 1 {
		t.Fatalf("state=%s mergeCalls=%d", current.State, mergeCalls)
	}
}

func TestMergeReadinessUnknownIsPendingWithoutMergeFailure(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	calls := 0
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		calls++
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "UNKNOWN", HeadSHA: "head-1"}, nil
	}
	readiness, err := d.ReadMergeReadiness(ctx, task)
	if err != nil || readiness.State != "UNKNOWN" || calls != 3 {
		t.Fatalf("readiness=%+v calls=%d err=%v", readiness, calls, err)
	}
	events, _ := st.ListEvents(ctx, task.ID)
	for _, event := range events {
		if event.Kind == "merge.failed" {
			t.Fatalf("UNKNOWN recorded merge failure: %+v", event)
		}
	}
}

func TestMergeReadinessSurfacesForgeCategoryWithoutRecordingGetNoise(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{}, &githubtrigger.Error{Category: githubtrigger.ForgeRequest, Err: errors.New("request timed out")}
	}
	_, err := d.ReadMergeReadiness(ctx, task)
	if err == nil || githubtrigger.ErrorCategory(err) != githubtrigger.ForgeRequest || !strings.Contains(err.Error(), "forge_request: request timed out") {
		t.Fatalf("readiness error=%v category=%q", err, githubtrigger.ErrorCategory(err))
	}
	events, listErr := st.ListEvents(ctx, task.ID)
	if listErr != nil {
		t.Fatal(listErr)
	}
	for _, event := range events {
		if event.Kind == "merge.failed" {
			t.Fatalf("GET-driven readiness recorded merge noise: %+v", event)
		}
	}
}

func TestConflictFixDispatchIsIdempotentAndCarriesFrozenContract(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "approved-head"}, nil
	}
	first, err := d.DispatchConflictFix(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.DispatchConflictFix(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || second.ID != first.ID || first.ReasonCode != "merge-conflict" || first.BaselineSHA != "approved-head" {
		t.Fatalf("orders first=%+v second=%+v", first, second)
	}
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	interventions, _ := st.ListInterventions(ctx, task.ID)
	if len(orders) != 1 || len(interventions) != 1 || interventions[0].ActorRole != core.ActorSystem || interventions[0].ReasonCode != "merge-conflict" {
		t.Fatalf("orders=%+v interventions=%+v", orders, interventions)
	}
}

func TestStaleApprovalDispatchesDeltaRefreshRound(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.Cfg.Routing = config.Routing{Stages: map[string]config.StageRoute{"review": {Model: "reviewer", Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}}}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", HeadSHA: "new-head"}, nil
	}
	if err := d.MergeApprovedTask(ctx, task); err == nil || !strings.Contains(err.Error(), "refresh review dispatched") {
		t.Fatalf("err=%v", err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if !current.ApprovalStale || current.RefreshBaselineSHA != "approved-head" || current.RefreshHeadSHA != "new-head" || len(orders) != 1 || orders[0].ReviewKind != "refresh" || orders[0].ReviewScope != "delta" || orders[0].BaselineSHA != "approved-head" || orders[0].HeadSHA != "new-head" {
		t.Fatalf("task=%+v orders=%+v", current, orders)
	}
}

func TestCleanStaleApprovalCanSkipRefreshUnderFrozenNone(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "MERGEABLE", HeadSHA: "clean-head"}, nil
	}
	if err := d.MergeApprovedTask(ctx, task); err == nil {
		t.Fatal("expected fail-closed stale response")
	}
	current, _ := st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if current.ApprovalStale || current.ApprovedHeadSHA != "clean-head" || current.State != core.TaskApproved || len(orders) != 0 {
		t.Fatalf("task=%+v orders=%+v", current, orders)
	}
}

func TestConflictResolutionForcesDeltaWhenFrozenPolicyIsNone(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.Cfg.Routing = config.Routing{Stages: map[string]config.StageRoute{"review": {Model: "reviewer", Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}}}
	current, _ := st.GetTask(ctx, task.ID)
	if err := d.beginRefreshLocked(ctx, current, "resolved-head", "merge-conflict", true); err != nil {
		t.Fatal(err)
	}
	current, _ = st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if !current.ApprovalStale || current.RefreshReviewScope != config.RefreshReviewDelta || len(orders) != 1 || orders[0].ReviewScope != config.RefreshReviewDelta {
		t.Fatalf("task=%+v orders=%+v", current, orders)
	}
}

func TestGateOffConflictingMergeAutomaticallyDispatchesFix(t *testing.T) {
	ctx, st, task, d := approvedMergeFixture(t, "acme/app")
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "approved-head"}, nil
	}
	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if len(orders) != 1 || orders[0].ReasonCode != "merge-conflict" {
		t.Fatalf("orders=%+v", orders)
	}
}

func TestConflictingMergeGateRetriesRecordOneBlockedEvent(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewNone, true)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "conflicting-head"}, nil
	}
	if readiness, err := d.ReadMergeReadiness(ctx, task); err != nil || readiness.State != "CONFLICTING" {
		t.Fatalf("readiness=%+v err=%v", readiness, err)
	}
	for range 2 {
		if err := d.MergeApprovedTask(ctx, task); err == nil || !strings.Contains(err.Error(), "merge conflicts") {
			t.Fatalf("err=%v", err)
		}
	}
	if blocked, _ := st.CountEvents(ctx, task.ID, "merge.blocked"); blocked != 1 {
		t.Fatalf("merge.blocked events=%d, want 1", blocked)
	}
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if len(orders) != 0 {
		t.Fatalf("human-gated merge retries created orders=%+v", orders)
	}
}

func TestChangedHeadConflictingReadinessKeepsGateBlockedWithoutRefresh(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewNone, true)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "conflicting-head"}, nil
	}
	readiness, err := d.ReadMergeReadiness(ctx, task)
	if err != nil || readiness.State != "CONFLICTING" {
		t.Fatalf("readiness=%+v err=%v", readiness, err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if current.ApprovalStale || current.State != core.TaskApproved || len(orders) != 0 {
		t.Fatalf("task=%+v orders=%+v", current, orders)
	}
	if _, err := d.ReadMergeReadiness(ctx, task); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := st.CountEvents(ctx, task.ID, "merge.blocked"); blocked != 1 {
		t.Fatalf("merge.blocked events=%d, want 1", blocked)
	}
	orders, _ = st.ListTaskWorkOrders(ctx, task.ID)
	if len(orders) != 0 {
		t.Fatalf("repeated human-gated readiness created orders=%+v", orders)
	}
}

func TestChangedHeadConflictingAutoMergeDispatchesFixBeforeRefresh(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "conflicting-head"}, nil
	}
	if err := d.MergeApprovedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if current.ApprovalStale || current.State != core.TaskQueued || current.NextStage != core.StageImplement || len(orders) != 1 || orders[0].ReasonCode != "merge-conflict" || orders[0].BaselineSHA != "approved-head" {
		t.Fatalf("task=%+v orders=%+v", current, orders)
	}
}

func TestChangedHeadConflictingReadinessAutoDispatchesFixBeforeRefresh(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScope(t, "acme/app", config.RefreshReviewNone)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: "CONFLICTING", HeadSHA: "conflicting-head"}, nil
	}
	readiness, err := d.ReadMergeReadiness(ctx, task)
	if err != nil || readiness.State != "CONFLICTING" {
		t.Fatalf("readiness=%+v err=%v", readiness, err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if current.ApprovalStale || current.State != core.TaskQueued || current.NextStage != core.StageImplement || len(orders) != 1 || orders[0].ReasonCode != "merge-conflict" || orders[0].BaselineSHA != "approved-head" {
		t.Fatalf("task=%+v orders=%+v", current, orders)
	}
	if _, err := d.ReadMergeReadiness(ctx, task); err != nil {
		t.Fatal(err)
	}
	orders, _ = st.ListTaskWorkOrders(ctx, task.ID)
	if blocked, _ := st.CountEvents(ctx, task.ID, "merge.blocked"); blocked != 1 || len(orders) != 1 {
		t.Fatalf("merge.blocked events=%d orders=%+v", blocked, orders)
	}
	if dispatched, _ := st.CountEvents(ctx, task.ID, "merge.conflict_fix_dispatched"); dispatched != 1 {
		t.Fatalf("merge.conflict_fix_dispatched events=%d, want 1", dispatched)
	}
}

func TestResolvedReadinessAllowsNewConflictEpisode(t *testing.T) {
	ctx, st, task, d := approvedMergeFixtureWithScopeAndGate(t, "acme/app", config.RefreshReviewNone, true)
	if err := st.BindTaskApproval(ctx, task.ID, "approved-head"); err != nil {
		t.Fatal(err)
	}
	mergeable := "CONFLICTING"
	d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
		return githubtrigger.PullRequest{Number: 12, State: "open", Mergeable: mergeable, HeadSHA: "approved-head"}, nil
	}
	if _, err := d.ReadMergeReadiness(ctx, task); err != nil {
		t.Fatal(err)
	}
	mergeable = "MERGEABLE"
	if readiness, err := d.ReadMergeReadiness(ctx, task); err != nil || readiness.State != "MERGEABLE" {
		t.Fatalf("resolved readiness=%+v err=%v", readiness, err)
	}
	mergeable = "CONFLICTING"
	if _, err := d.ReadMergeReadiness(ctx, task); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := st.CountEvents(ctx, task.ID, "merge.blocked"); blocked != 2 {
		t.Fatalf("merge.blocked events=%d, want 2", blocked)
	}
	if cleared, _ := st.CountEvents(ctx, task.ID, "merge.conflict_cleared"); cleared != 1 {
		t.Fatalf("merge.conflict_cleared events=%d, want 1", cleared)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestSourceIssueNumberEnforcesConfiguredRepository(t *testing.T) {
	for _, source := range []string{"github:acme/api#42", "https://github.com/acme/api/issues/42"} {
		number, err := sourceIssueNumber("acme/api", source)
		if err != nil || number != 42 {
			t.Fatalf("source=%q number=%d err=%v", source, number, err)
		}
	}
	if _, err := sourceIssueNumber("acme/api", "github:other/api#42"); err == nil {
		t.Fatal("cross-repository source issue was accepted")
	}
}

func TestFrozenSetupSourcesImplementationAndReviewDispatch(t *testing.T) {
	settings := func(harness, model string) config.ContextualExecutionSettings {
		return config.ContextualExecutionSettings{
			ControlPlane:   config.ControlPlaneSettings{Triage: config.ModelTimeoutSettings{Model: "control", TimeoutText: "20m"}, Spec: config.ModelTimeoutSettings{Model: "control", TimeoutText: "30m"}},
			Implementation: config.ImplementationSettings{Harness: harness, Model: model, ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "2h"},
			Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "45m"},
		}
	}
	harness := func(name string) config.Harness {
		return config.Harness{Name: name, Command: []string{name, "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--effort", "high"}}, ProbeCommand: []string{name, "--version"}, ProbeTimeoutText: "5s"}
	}
	backend := config.ExecutionSetup{Name: "backend", ExecutionSettings: settings("codex", "gpt-backend"), Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-review", Harness: "codex"}}}}
	frontend := config.ExecutionSetup{Name: "frontend", ExecutionSettings: settings("claude", "claude-ui"), Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "claude-review", Harness: "claude", Effort: "high"}}}}
	cfg := (&config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Harnesses: []config.Harness{harness("codex"), harness("claude")}, Setups: []config.ExecutionSetup{backend, frontend}, DefaultSetup: "backend"}).WithSetup(backend)
	cfg.Setups, cfg.DefaultSetup = []config.ExecutionSetup{backend, frontend}, "backend"

	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "frozen-frontend", Workspace: "demo", Title: "Frontend", SetupName: frontend.Name, SetupContract: frontend, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, cfg, nil)
	if err := dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].RequiredHarness != "claude" || orders[0].RequiredModel != "claude-ui" || orders[0].RequiredEffort != "high" || orders[0].ExecutionTimeoutText != "2h" {
		t.Fatalf("implementation orders=%+v err=%v", orders, err)
	}
	jobs, reviewOrders, err := BuildReviewRound(cfg, task, cfg.Routing.Stages["review"], 1)
	if err != nil || len(jobs) != 1 || len(reviewOrders) != 1 || reviewOrders[0].RequiredHarness != "claude" || reviewOrders[0].RequiredModel != "claude-review" || reviewOrders[0].ExecutionTimeoutText != "45m" {
		t.Fatalf("review jobs=%+v orders=%+v err=%v", jobs, reviewOrders, err)
	}
}

func TestPRBodyClosesDurablyAssociatedIssue(t *testing.T) {
	task := core.Task{ID: "task-1", Source: "cli", GitHub: &core.GitHubLifecycle{IssueNumber: 42}}
	body := PRBody(task, core.Artifact{
		ID: "abc123", Name: "proof `shot`.png", ContentType: "image/png", SizeBytes: 42,
		Role: core.ArtifactRoleVerificationEvidence, TaskID: task.ID,
		DownloadURL: "https://private.invalid/artifact?token=secret",
	})
	if !strings.Contains(body, "Closes #42") {
		t.Fatalf("body=%q", body)
	}
	if strings.Count(body, "<!-- conveyor:verification-evidence -->") != 1 ||
		!strings.Contains(body, "abc123") || !strings.Contains(body, "proof 'shot'.png") ||
		strings.Contains(body, "private.invalid") || strings.Contains(body, "token=secret") {
		t.Fatalf("unsafe or incomplete evidence body=%q", body)
	}
}

func TestSpecApprovalQueuesSourceIssueLifecycle(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "spec-task", Workspace: "test", Repo: "app", Source: "github:acme/app#19", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "## Intent\nApproved"})
	if err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "spec-job", TaskID: task.ID, Stage: core.StageSpec, State: core.JobDone}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	gate, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true})
	if err != nil {
		t.Fatal(err)
	}
	task = gate.Task
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	if err = d.HandleIntervention(ctx, task, job, core.Intervention{Action: core.InterventionApprove}); err != nil {
		t.Fatal(err)
	}
	lifecycle, ok, err := st.GetGitHubLifecycle(ctx, task.ID)
	if err != nil || !ok || lifecycle.SpecVersion != spec.Version || lifecycle.SourceIssueNumber != 19 {
		t.Fatalf("lifecycle=%+v ok=%t err=%v", lifecycle, ok, err)
	}
}

func TestMergeAndRecoveryInterventionsDoNotRequireSpec(t *testing.T) {
	for _, command := range []core.TaskCommand{core.TaskGateMerge, core.TaskJobFail, core.TaskStageBounceLimit} {
		for _, action := range []core.InterventionAction{core.InterventionApprove, core.InterventionRedirect} {
			t.Run(string(command)+"/"+string(action), func(t *testing.T) {
				ctx := store.WithWorkspace(t.Context(), "demo")
				st := store.NewMemory()
				task := core.Task{
					ID: "no-spec-" + string(command) + "-" + string(action), Workspace: "demo", Repo: "api",
					PolicyVersion: 1, SpecApproval: false, MergeApproval: true,
					State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: "reviewed-head", CreatedAt: time.Now(),
				}
				if err := st.CreateTask(ctx, task); err != nil {
					t.Fatal(err)
				}
				job := core.Job{ID: task.ID + "-job", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}
				if err := st.CreateJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				gate, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{
					Kind: command, RecoveryStage: core.StageImplement, ProjectStages: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				task = gate.Task
				intervention := core.Intervention{TaskID: task.ID, JobID: job.ID, Action: action, ReasonCode: "operator-action"}
				if err = st.CreateIntervention(ctx, intervention); err != nil {
					t.Fatal(err)
				}
				d := New(st, &config.Config{Workspace: "demo"}, nil)
				if err = d.HandleIntervention(ctx, task, job, intervention); err != nil {
					t.Fatal(err)
				}
				current, err := st.GetTask(ctx, task.ID)
				if err != nil {
					t.Fatal(err)
				}
				if action == core.InterventionApprove {
					if current.State != core.TaskApproved || current.ApprovedHeadSHA != "reviewed-head" {
						t.Fatalf("approved task=%+v", current)
					}
				} else if current.State != core.TaskQueued || current.NextStage != core.StageImplement {
					t.Fatalf("redirected task=%+v", current)
				}
				if _, exists, specErr := st.GetLatestSpecVersion(ctx, task.ID); specErr != nil || exists {
					t.Fatalf("spec exists=%t err=%v", exists, specErr)
				}
				if count, countErr := st.CountEvents(ctx, task.ID, "intervention."+string(action)); countErr != nil || count != 1 {
					t.Fatalf("intervention events=%d err=%v", count, countErr)
				}
				if count, countErr := st.CountEvents(ctx, task.ID, "blueprint.materialized"); countErr != nil || count != 0 {
					t.Fatalf("materialization events=%d err=%v", count, countErr)
				}
			})
		}
	}
}

func TestMergeGateIgnoresUnapprovedNewerSpec(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{
		ID: "merge-with-newer-spec", Workspace: "demo", Repo: "api", PolicyVersion: 1,
		SpecApproval: true, MergeApproval: true, State: core.TaskRunning, NextStage: core.StageReview,
		ReviewedHeadSHA: "reviewed-head", CreatedAt: time.Now(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	gate, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true})
	if err != nil {
		t.Fatal(err)
	}
	task = gate.Task
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "new unapproved revision"})
	if err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "demo"}, nil)
	if err = d.HandleIntervention(ctx, task, job, core.Intervention{Action: core.InterventionApprove, ReasonCode: "approved"}); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskApproved || current.ApprovedHeadSHA != "reviewed-head" {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	latest, exists, err := st.GetLatestSpecVersion(ctx, task.ID)
	if err != nil || !exists || latest.Version != spec.Version || latest.Approved {
		t.Fatalf("latest spec=%+v exists=%t err=%v", latest, exists, err)
	}
	if count, countErr := st.CountEvents(ctx, task.ID, "blueprint.materialized"); countErr != nil || count != 0 {
		t.Fatalf("materialization events=%d err=%v", count, countErr)
	}
}

func TestRecordedSpecGateWithoutSpecFallsThrough(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{
		ID: "spec-gate-without-spec", Workspace: "demo", Repo: "api",
		State: core.TaskRunning, ReviewedHeadSHA: "reviewed-head", CreatedAt: time.Now(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	gate, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true})
	if err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "demo"}, nil)
	if err = d.HandleIntervention(ctx, gate.Task, core.Job{Stage: core.StageSpec}, core.Intervention{Action: core.InterventionApprove}); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskApproved || current.ApprovedHeadSHA != "reviewed-head" {
		t.Fatalf("task=%+v err=%v", current, err)
	}
}

func TestIssuePublisherRejectsLifecycleFromDifferentConfiguredRepository(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "repo-boundary", Workspace: "test", Repo: "app", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, task.ID, spec.Version); err != nil {
		t.Fatal(err)
	}
	if err = st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: task.ID, Repository: "other/app", SpecVersion: spec.Version}); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	worker := &githubIssuePublicationWorker{dispatcher: d}
	err = worker.Work(ctx, &river.Job[queueargs.GitHubIssuePublicationArgs]{JobRow: &rivertype.JobRow{ID: 1, Attempt: 1, MaxAttempts: 5}, Args: queueargs.GitHubIssuePublicationArgs{WorkspaceID: "test", TaskID: task.ID}})
	if err == nil || !strings.Contains(err.Error(), "does not match configured") {
		t.Fatalf("error=%v", err)
	}
	lifecycle, _, _ := st.GetGitHubLifecycle(ctx, task.ID)
	if lifecycle.LastError == "" || lifecycle.State != core.GitHubPublicationRetrying {
		t.Fatalf("lifecycle=%+v", lifecycle)
	}
}

func TestIssuePublisherBoundsAmbiguousRecoveryBeforeOneCreateReauthorization(t *testing.T) {
	ctx := store.WithWorkspace(context.Background(), "test")
	st := store.NewMemory()
	task := core.Task{ID: "lost-ack", Workspace: "test", Repo: "app", Title: "Lost ack", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, task.ID, spec.Version); err != nil {
		t.Fatal(err)
	}
	if err = st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: task.ID, Repository: "acme/app", SpecVersion: spec.Version}); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	publishCalls := 0
	authorizedCreates := 0
	d.PublishIssue = func(publishCtx context.Context, publication githubtrigger.IssuePublication) (githubtrigger.IssuePublicationResult, error) {
		publishCalls++
		switch publishCalls {
		case 1:
			if !publication.AllowCreate {
				t.Fatal("first publication did not permit create")
			}
			if err := publication.BeforeCreate(publishCtx); err != nil {
				t.Fatal(err)
			}
			authorizedCreates++
			return githubtrigger.IssuePublicationResult{}, errors.New("GitHub rejected create before accepting it")
		case 2, 3:
			if publication.AllowCreate {
				t.Fatalf("recovery miss %d permitted an early create", publishCalls-1)
			}
			return githubtrigger.IssuePublicationResult{}, githubtrigger.ErrIssueReconciliationPending
		case 4:
			if !publication.AllowCreate {
				t.Fatal("bounded reconciliation did not reauthorize create")
			}
			if err := publication.BeforeCreate(publishCtx); err != nil {
				t.Fatal(err)
			}
			authorizedCreates++
			return githubtrigger.IssuePublicationResult{Number: 42, URL: "https://github.com/acme/app/issues/42"}, nil
		default:
			t.Fatalf("unexpected publication call %d", publishCalls)
			return githubtrigger.IssuePublicationResult{}, nil
		}
	}
	worker := &githubIssuePublicationWorker{dispatcher: d}
	job := func(attempt int) *river.Job[queueargs.GitHubIssuePublicationArgs] {
		return &river.Job[queueargs.GitHubIssuePublicationArgs]{
			JobRow: &rivertype.JobRow{ID: int64(attempt), Attempt: attempt, MaxAttempts: 5},
			Args:   queueargs.GitHubIssuePublicationArgs{WorkspaceID: "test", TaskID: task.ID},
		}
	}
	if err = worker.Work(ctx, job(1)); err == nil {
		t.Fatal("first ambiguous publication succeeded")
	}
	lifecycle, _, _ := st.GetGitHubLifecycle(ctx, task.ID)
	if lifecycle.CreateState != core.GitHubCreateReconciling || lifecycle.CreateAttempts != 1 || lifecycle.ReconcileMisses != 0 || lifecycle.Attempts != 1 {
		t.Fatalf("lifecycle after failed create=%+v", lifecycle)
	}
	if err = worker.Work(ctx, job(2)); !errors.Is(err, githubtrigger.ErrIssueReconciliationPending) {
		t.Fatalf("first reconciliation error=%v", err)
	}
	lifecycle, _, _ = st.GetGitHubLifecycle(ctx, task.ID)
	if lifecycle.CreateAttempts != 1 || lifecycle.ReconcileMisses != 1 || authorizedCreates != 1 {
		t.Fatalf("lifecycle after first reconciliation miss=%+v authorizedCreates=%d", lifecycle, authorizedCreates)
	}
	if err = worker.Work(ctx, job(3)); !errors.Is(err, githubtrigger.ErrIssueReconciliationPending) {
		t.Fatalf("second reconciliation error=%v", err)
	}
	lifecycle, _, _ = st.GetGitHubLifecycle(ctx, task.ID)
	if lifecycle.CreateAttempts != 1 || lifecycle.ReconcileMisses != githubIssueReconciliationMissesBeforeCreate || authorizedCreates != 1 {
		t.Fatalf("lifecycle after bounded reconciliation=%+v authorizedCreates=%d", lifecycle, authorizedCreates)
	}
	if err = worker.Work(ctx, job(4)); err != nil {
		t.Fatal(err)
	}
	lifecycle, _, _ = st.GetGitHubLifecycle(ctx, task.ID)
	if lifecycle.State != core.GitHubPublicationPublished || lifecycle.CreateState != core.GitHubCreateConfirmed || lifecycle.CreateAttempts != 2 || lifecycle.ReconcileMisses != 0 || lifecycle.IssueNumber != 42 || authorizedCreates != 2 {
		t.Fatalf("lifecycle after reauthorized create=%+v authorizedCreates=%d", lifecycle, authorizedCreates)
	}
}

func TestReconcileGitHubLifecyclesRepairsApprovalOutboxGapOnce(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "reconcile-issue", Workspace: "test", Repo: "app", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, task.ID, spec.Version); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	if repaired, reconcileErr := d.ReconcileGitHubLifecycles(ctx); reconcileErr != nil || repaired != 1 {
		t.Fatalf("first reconcile repaired=%d err=%v", repaired, reconcileErr)
	}
	if repaired, reconcileErr := d.ReconcileGitHubLifecycles(ctx); reconcileErr != nil || repaired != 0 {
		t.Fatalf("second reconcile repaired=%d err=%v", repaired, reconcileErr)
	}
}

func TestReviewHistoryKeepsRequestedChangesAndResolution(t *testing.T) {
	events := []core.Event{
		{Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_work_order_id": "review-1", "review_round": 1, "review_seat": 1, "verdict": "changes_requested", "reason_code": "tests", "feedback": "Add coverage."})},
		{Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_work_order_id": "review-2", "review_round": 2, "review_seat": 1, "verdict": "approve", "reason_code": "approved"})},
		{Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"review_round": 2, "verdict": "approve"})},
	}
	history := reviewHistory(events)
	if len(history) != 2 || history[0].ResolutionState != "resolved" || history[1].ResolutionState != "accepted" {
		t.Fatalf("history=%+v", history)
	}
}

func TestReviewRoundStatusUsesAggregateVerdict(t *testing.T) {
	pending := []core.Event{{Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_round": 2, "review_seat": 1, "verdict": "approve"})}}
	if got := reviewRoundStatus(pending, 2, "approve"); got != "pending" {
		t.Fatalf("pending panel status=%q", got)
	}
	completed := append(pending, core.Event{Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"review_round": 2, "verdict": "changes_requested"})})
	if got := reviewRoundStatus(completed, 2, "approve"); got != "failure" {
		t.Fatalf("aggregate panel status=%q", got)
	}
	if got := reviewRoundStatus(completed, 1, "approve"); got != "pending" {
		t.Fatalf("other round leaked into status=%q", got)
	}
}

func (st *reviewAcceptanceFlakyStore) AcceptReviewDecisionCommand(ctx context.Context, lease taskops.TaskLease, decision core.ReviewDecision) error {
	if st.failures > 0 {
		st.failures--
		return errors.New("review acceptance unavailable")
	}
	return st.Store.AcceptReviewDecisionCommand(ctx, lease, decision)
}

type sequenceAgent struct {
	outputs []string
	inputs  []inprocess.Input
	next    int
}

func (agent *sequenceAgent) Run(_ context.Context, _ string, input inprocess.Input) (inprocess.Result, error) {
	agent.inputs = append(agent.inputs, input)
	output := agent.outputs[agent.next]
	agent.next++
	return inprocess.Result{Output: output, TokensIn: 20, TokensOut: 10}, nil
}

func TestSpecStructuredValidationFeedsPreciseErrorIntoRetry(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "structured-spec-retry", Workspace: "demo", Repo: "api", Title: "Retry semantics", PolicyVersion: 1, SpecApproval: true, State: core.TaskQueued, NextStage: core.StageSpec, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	invalid := structuredSpecOutput("# Retry\n\n## Intent\nShip it.\n\n## Non-goals\nNone.", "Ship it", "")
	invalid = strings.Replace(invalid, `"verify":"test"`, `"verify":"tests"`, 1)
	agent := &sequenceAgent{outputs: []string{invalid, structuredSpecOutput("# Retry\n\n## Intent\nShip it.\n\n## Non-goals\nNone.", "Ship it", "")}}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 3, Routing: config.Routing{Stages: map[string]config.StageRoute{"spec": {Model: "gpt", Execution: config.ExecutionInProcess, Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	if err = dispatcher.runInProcess(store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), cfg, task, cfg.Routing.Stages["spec"]); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskQueued || current.NextStage != core.StageSpec {
		t.Fatalf("after invalid output task=%+v err=%v", current, err)
	}
	if err = dispatcher.runInProcess(store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), cfg, current, cfg.Routing.Stages["spec"]); err != nil {
		t.Fatal(err)
	}
	if len(agent.inputs) != 2 || !strings.Contains(agent.inputs[1].Prompt, "# Previous output rejected") || !strings.Contains(agent.inputs[1].Prompt, `invalid verify value "tests"`) {
		t.Fatalf("retry prompt did not preserve precise validation error:\n%s", agent.inputs[1].Prompt)
	}
}

func structuredSpecOutput(markdown, criterion, ref string) string {
	value := map[string]any{
		"markdown":      markdown,
		"acceptance":    []map[string]any{{"id": "AC-1", "criterion": criterion, "verify": "test", "ref": ref}},
		"decomposition": []any{},
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestInProcessUsageRecordsTokensWithoutCost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "high-usage", Workspace: "demo", Repo: "api", Title: "Small fix", Level: core.L0, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &sequenceAgent{outputs: []string{"```conveyor:triage\n{\"class\":\"chore\",\"route\":\"implement\",\"summary\":\"Ready.\"}\n```"}}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage": {Model: "gpt-newly-configured", Execution: config.ExecutionInProcess, Timeout: time.Minute},
	}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskQueued || current.NextStage != core.StageImplement {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	job, ok, err := st.GetLatestJob(ctx, task.ID)
	if err != nil || !ok || job.State != core.JobDone || job.CostUSD != nil || job.TokensIn != 20 || job.TokensOut != 10 {
		t.Fatalf("job=%+v ok=%t err=%v", job, ok, err)
	}
}

func TestInProcessTriageAndSpecAdvanceToImplementWorkOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "task", Workspace: "demo", Repo: "api", Title: "Add audit export", Body: "Specify and implement it", BaseBranch: "main", Branch: "conveyor/task-task", Level: core.L2, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &sequenceAgent{outputs: []string{
		"```conveyor:triage\n{\"class\":\"feature\",\"route\":\"spec\",\"summary\":\"Needs an accepted contract.\"}\n```",
		structuredSpecOutput("# Audit export\n\n## Intent\nAdd the export.\n\n## Non-goals\nNo unrelated formats.", "Export tests pass", "./..."),
	}}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"triage":    {Model: "gpt-5.4", Execution: config.ExecutionInProcess, Timeout: time.Minute},
		"spec":      {Model: "gpt-5.4", Execution: config.ExecutionInProcess, Timeout: time.Minute},
		"implement": {Model: "operator-owned", Execution: config.ExecutionMCP, Timeout: time.Hour},
		"review":    {Model: "operator-owned", Execution: config.ExecutionMCP, Timeout: time.Hour},
	}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle

	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	inFlightSpec, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = dispatcher.runInProcess(store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), cfg, inFlightSpec, cfg.Routing.Stages["spec"]); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskAwaiting || current.RecoveryStage != core.StageImplement {
		t.Fatalf("after spec task=%+v err=%v", current, err)
	}
	spec, ok, err := st.GetLatestSpecVersion(ctx, task.ID)
	if err != nil || !ok || spec.AcceptanceCount != 1 || spec.Approved {
		t.Fatalf("spec=%+v ok=%v err=%v", spec, ok, err)
	}
	latest, ok, err := st.GetLatestJob(ctx, task.ID)
	if err != nil || !ok {
		t.Fatalf("latest job ok=%v err=%v", ok, err)
	}
	if err = dispatcher.HandleIntervention(ctx, current, latest, core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionApprove, ReasonCode: "approved"}); err != nil {
		t.Fatal(err)
	}
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].Stage != core.StageImplement || orders[0].State != core.WorkOrderQueued {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
}

func TestImplementationDispatchSnapshotsNormalizedHarnessAndModel(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "snapshot-implement", Workspace: "demo", Repo: "api", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	harness := config.Harness{Name: "codex", MCPTransport: config.MCPTransportTOMLOverride, Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: map[string][]string{"high": {"--config", `model_reasoning_effort="high"`}}, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s", StallTimeoutText: "45s"}
	cfg := &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Harnesses: []config.Harness{harness}, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"implement": {Model: "gpt-5", ModelPolicy: config.ModelPolicyExplicit, EffectiveModel: "gpt-5", Harness: "codex", Effort: "high", Timeout: time.Hour, TimeoutText: "1h", Execution: config.ExecutionMCP},
	}}}
	dispatcher := New(st, cfg, nil)
	if err := dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	order := orders[0]
	if order.RequiredModel != "gpt-5" || order.RequiredHarness != "codex" || order.RequiredEffort != "high" || order.RequiredHarnessConfig == nil || order.RequiredHarnessConfig.Name != "codex" || order.RequiredHarnessConfig.MCPTransport != config.MCPTransportTOMLOverride || order.RequiredHarnessConfig.StallTimeoutText != "45s" || !reflect.DeepEqual(order.RequiredHarnessConfig.EffortArgv, []string{"--config", `model_reasoning_effort="high"`}) {
		t.Fatalf("snapshotted order=%+v", order)
	}
	cfg.Harnesses[0].EffortArgs["high"] = []string{"--config", `model_reasoning_effort="low"`}
	cfg.Harnesses[0].StallTimeoutText = "2s"
	if !reflect.DeepEqual(order.RequiredHarnessConfig.EffortArgv, []string{"--config", `model_reasoning_effort="high"`}) || order.RequiredHarnessConfig.StallTimeoutText != "45s" {
		t.Fatalf("hot reload mutated in-flight effort argv: %+v", order.RequiredHarnessConfig)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil || len(events) == 0 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	var audit map[string]any
	if err = json.Unmarshal(events[len(events)-1].Payload, &audit); err != nil || audit["required_effort"] != "high" {
		t.Fatalf("implementation effort audit=%+v err=%v", audit, err)
	}
	if argv, ok := audit["effort_argv"].([]any); !ok || len(argv) != 2 || argv[1] != `model_reasoning_effort="high"` {
		t.Fatalf("implementation effort argv audit=%+v", audit)
	}
}

func TestHarnessSnapshotPreservesEnvironmentAttachmentIdentity(t *testing.T) {
	cfg := &config.Config{Harnesses: []config.Harness{{
		Name: "grok", MCPTransport: config.MCPTransportEnvironment, MCPAttachment: "conveyor",
		Command: []string{"grok", "--single", "{prompt}"}, ProbeCommand: []string{"grok", "--version"}, ProbeTimeoutText: "30s",
	}}}
	snapshot, ok := reviewHarnessSnapshot(cfg, "grok")
	if !ok || snapshot.MCPTransport != config.MCPTransportEnvironment || snapshot.MCPAttachment != "conveyor" {
		t.Fatalf("environment snapshot=%+v ok=%v", snapshot, ok)
	}
	cfg.Harnesses[0].MCPAttachment = "replacement"
	if snapshot.MCPAttachment != "conveyor" {
		t.Fatalf("hot reload mutated attachment identity: %+v", snapshot)
	}
}

func TestImplementationDispatchNeverSnapshotsUndeclaredExplicitSymbol(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "reject-symbolic-implement", Workspace: "demo", Repo: "api", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	document := implementationModelDocument(config.ModelPolicyExplicit, "subscription", nil)
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = config.ParseWorkspaceDocument(raw, &config.Config{Workspace: "demo", PackDir: "."}, "symbolic dispatch test"); err == nil || !strings.Contains(err.Error(), `symbolic model "subscription" requires harness_default model policy`) {
		t.Fatalf("explicit symbolic model error=%v", err)
	}
	orders, listErr := st.ListTaskWorkOrders(ctx, task.ID)
	if listErr != nil || len(orders) != 0 {
		t.Fatalf("invalid symbolic model created work orders=%+v err=%v", orders, listErr)
	}
}

func TestImplementationDispatchSnapshotsDeclaredDefaultSentinel(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "declared-symbolic-implement", Workspace: "demo", Repo: "api", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	document := implementationModelDocument(config.ModelPolicyHarnessDefault, "subscription", []string{"subscription"})
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseWorkspaceDocument(raw, &config.Config{Workspace: "demo", PackDir: "."}, "declared sentinel dispatch test")
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, cfg, nil)
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].RequiredModel != "subscription" {
		t.Fatalf("declared sentinel orders=%+v err=%v", orders, err)
	}
}

func implementationModelDocument(policy, model string, sentinels []string) config.WorkspaceDocument {
	return config.WorkspaceDocument{
		Workspace: "demo", MaxBounces: 2,
		ExecutionSettings: &config.ContextualExecutionSettings{
			ControlPlane: config.ControlPlaneSettings{
				Triage: config.ModelTimeoutSettings{Model: "gpt", TimeoutText: "20m"},
				Spec:   config.ModelTimeoutSettings{Model: "gpt", TimeoutText: "30m"},
			},
			Implementation: config.ImplementationSettings{Harness: "codex", Model: model, ModelPolicy: policy, TimeoutText: "1h"},
			Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "1h", FallbackModel: "gpt-review", FallbackHarness: "codex"},
		},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"triage":    {Model: "gpt", TimeoutText: "20m", Execution: config.ExecutionInProcess},
			"spec":      {Model: "gpt", TimeoutText: "30m", Execution: config.ExecutionInProcess},
			"implement": {Model: model, ModelPolicy: policy, Harness: "codex", TimeoutText: "1h", Execution: config.ExecutionMCP},
			"review":    {Model: "gpt-review", Harness: "codex", TimeoutText: "1h", Execution: config.ExecutionMCP},
		}},
		Harnesses: []config.Harness{{
			Name: "codex", Command: []string{"codex", "{prompt}", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"},
			DefaultModelSentinels: sentinels, ProbeCommand: []string{"codex", "--version"}, ProbeTimeoutText: "5s",
		}},
		Repos: []config.Repo{{Name: "api", URL: "https://example.test/api", Base: "main"}},
	}
}

func TestHumanTriageRouteAlwaysAwaitsInput(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "policy-spec", Workspace: "demo", Repo: "api", Title: "Policy", Hold: true, PolicyVersion: 1, SpecApproval: true, MergeApproval: false, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &sequenceAgent{outputs: []string{"```conveyor:triage\n{\"class\":\"feature\",\"route\":\"human\",\"summary\":\"Needs review.\"}\n```", structuredSpecOutput("# Policy spec\n\n## Intent\nDefine it.\n\n## Non-goals\nNone.", "Works", "./...")}}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt", Execution: config.ExecutionInProcess, Timeout: time.Minute}, "spec": {Model: "gpt", Execution: config.ExecutionInProcess, Timeout: time.Minute}, "implement": {Model: "operator", Execution: config.ExecutionMCP, Timeout: time.Hour}, "review": {Model: "operator", Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
	d := New(st, cfg, agent)
	d.Pack = bundle
	if err = d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.State != core.TaskAwaiting || current.RecoveryStage != core.StageTriage {
		t.Fatalf("after triage=%+v", current)
	}
}

func TestRequestedSpecChangesRequireRevisedApprovalBeforeImplementation(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "spec-revision", Workspace: "demo", Repo: "api", Title: "Revise policy", Mode: core.TaskModeManual, PolicyVersion: 1, SpecApproval: true, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	first, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "# First spec"})
	if err != nil {
		t.Fatal(err)
	}
	firstJob := core.Job{ID: "spec-revision-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobDone}
	if err = st.CreateJob(ctx, firstJob); err != nil {
		t.Fatal(err)
	}
	gate, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true})
	if err != nil {
		t.Fatal(err)
	}
	task = gate.Task

	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &sequenceAgent{outputs: []string{structuredSpecOutput("# Revised spec\n\n## Intent\nCorrect the workflow.\n\n## Non-goals\nNone.", "Requested changes require another approval.", "internal/dispatch/dispatch_test.go")}}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{
		"spec":      {Model: "gpt", Execution: config.ExecutionInProcess, Timeout: time.Minute},
		"implement": {Model: "operator", Execution: config.ExecutionMCP, Timeout: time.Hour},
	}}}
	d := New(st, cfg, agent)
	d.Pack = bundle

	intervention := core.Intervention{TaskID: task.ID, JobID: firstJob.ID, Action: core.InterventionRedirect, ReasonCode: "spec-changes", Comment: "Revise the contract."}
	if err = st.CreateIntervention(ctx, intervention); err != nil {
		t.Fatal(err)
	}
	if err = d.HandleIntervention(ctx, task, firstJob, intervention); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != core.TaskQueued || current.NextStage != core.StageSpec || current.RecoveryStage != "" {
		t.Fatalf("after requested changes=%+v", current)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 0 {
		t.Fatalf("requested changes created implementation orders=%+v err=%v", orders, err)
	}

	// Complete the legacy call that was already in flight before the redirect;
	// any new delivery of this queued stage would create an MCP work order.
	if err = d.runInProcess(store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), cfg, current, cfg.Routing.Stages["spec"]); err != nil {
		t.Fatal(err)
	}
	current, err = st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	revised, ok, err := st.GetLatestSpecVersion(ctx, task.ID)
	if err != nil || !ok {
		t.Fatalf("revised spec ok=%t err=%v", ok, err)
	}
	if revised.Version != first.Version+1 || revised.Approved || current.State != core.TaskAwaiting || current.RecoveryStage != core.StageImplement {
		t.Fatalf("revised spec=%+v task=%+v", revised, current)
	}
	orders, err = st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 0 {
		t.Fatalf("unapproved revision created implementation orders=%+v err=%v", orders, err)
	}

	latestJob, ok, err := st.GetLatestJob(ctx, task.ID)
	if err != nil || !ok || latestJob.Stage != core.StageSpec {
		t.Fatalf("latest spec job=%+v ok=%t err=%v", latestJob, ok, err)
	}
	if err = d.HandleIntervention(ctx, current, latestJob, core.Intervention{Action: core.InterventionApprove, ReasonCode: "approved"}); err != nil {
		t.Fatal(err)
	}
	current, err = st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != core.TaskQueued || current.NextStage != core.StageImplement {
		t.Fatalf("after revised approval=%+v", current)
	}
	if err = d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err = st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].Stage != core.StageImplement {
		t.Fatalf("approved revision implementation orders=%+v err=%v", orders, err)
	}

	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	redirectAt, revisionAt, implementationAt := -1, -1, -1
	for i, event := range events {
		switch event.Kind {
		case "intervention.redirect":
			redirectAt = i
		case "spec.version_created":
			if redirectAt >= 0 && i > redirectAt {
				revisionAt = i
			}
		case "work_order.created":
			if revisionAt >= 0 && i > revisionAt {
				implementationAt = i
			}
		}
	}
	if redirectAt < 0 || revisionAt <= redirectAt || implementationAt <= revisionAt {
		t.Fatalf("workflow history redirect=%d revision=%d implementation=%d events=%+v", redirectAt, revisionAt, implementationAt, events)
	}
}

func TestExternalReviewBounceCreatesNextImplementOrderWithFeedback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "bounce-task", Workspace: "test", Repo: "app", Level: core.L2, State: core.TaskRunning, Branch: "conveyor/bounce", BaseBranch: "main", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, job := range []core.Job{
		{ID: "implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobDone, ModelTier: "impl", StartedAt: time.Now()},
		{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()},
	} {
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Execution: config.ExecutionMCP}}}}
	d := New(st, cfg, nil)
	d.DisableMemoryQueueForTest()
	if err := d.ApplyExternalReview(ctx, task, core.Job{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, ModelTier: "review"}, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "missing coverage", Feedback: "add the test"}, "review-1", "review-session", "review"); err != nil {
		t.Fatal(err)
	}
	if err := d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].Stage != core.StageImplement {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, orders[0].ID, core.WorkOrderClaim{SessionID: "warm-implement-session", ClientToken: "warm-token", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	interventions, err := st.ListInterventions(ctx, task.ID)
	if err != nil || len(interventions) != 1 || interventions[0].Comment != "add the test" {
		t.Fatalf("interventions=%+v err=%v", interventions, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	bounces := 0
	for _, event := range events {
		if event.Kind == "pipeline.bounced" {
			bounces++
		}
	}
	if err != nil || bounces != 1 {
		t.Fatalf("bounces=%d err=%v", bounces, err)
	}
}

func TestBounceWindowResetsAfterHumanIntervention(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "test")
	st := store.NewMemory()
	task := core.Task{ID: "window-task", Workspace: "test", Repo: "app", Level: core.L2, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2}
	d := New(st, cfg, nil)
	d.DisableMemoryQueueForTest()

	// Two agent bounces exhaust the unsupervised window and park the task.
	if err := d.bounce(ctx, cfg, task.ID, job.ID, "tests", "round one"); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
		t.Fatal(err)
	}
	if err := d.bounce(ctx, cfg, task.ID, job.ID, "tests", "round two"); err != nil {
		t.Fatal(err)
	}
	parked, err := st.GetTask(ctx, task.ID)
	if err != nil || parked.State != core.TaskAwaiting {
		t.Fatalf("parked task=%+v err=%v", parked, err)
	}
	if limits, _ := st.CountEvents(ctx, task.ID, "pipeline.bounce_limit"); limits != 1 {
		t.Fatalf("bounce_limit events=%d", limits)
	}

	// A human redirect grants a fresh window (spec §21.17): the next bounce
	// re-queues instead of re-parking.
	if err := st.CreateIntervention(ctx, core.Intervention{TaskID: task.ID, JobID: job.ID, ActorID: "operator", ActorRole: core.ActorHuman, Action: core.InterventionRedirect, ReasonCode: "changes-requested", Comment: "keep going"}); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskInterventionRedirect, NextStage: core.StageImplement, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
		t.Fatal(err)
	}
	if err := d.bounce(ctx, cfg, task.ID, job.ID, "tests", "round three"); err != nil {
		t.Fatal(err)
	}
	resumed, err := st.GetTask(ctx, task.ID)
	if err != nil || resumed.State != core.TaskQueued || resumed.NextStage != core.StageImplement {
		t.Fatalf("resumed task=%+v err=%v", resumed, err)
	}
	if limits, _ := st.CountEvents(ctx, task.ID, "pipeline.bounce_limit"); limits != 1 {
		t.Fatalf("bounce_limit events after reset=%d", limits)
	}
}

func TestExternalReviewAtBounceCapStopsAtHumanGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "cap-task", Workspace: "test", Repo: "app", Level: core.L2, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", MaxBounces: 1}, nil)
	d.DisableMemoryQueueForTest()
	if err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "stop", Feedback: "human help"}, job.ID, "review-session", "review"); err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetTask(ctx, task.ID)
	if err != nil || updated.State != core.TaskAwaiting || updated.RecoveryStage != core.StageImplement || updated.NextStage != "" {
		t.Fatalf("task=%+v err=%v", updated, err)
	}
	orders, _ := st.ListTaskWorkOrders(ctx, task.ID)
	if len(orders) != 0 {
		t.Fatalf("unexpected follow-up orders: %+v", orders)
	}
}

func TestReviewCitationValidationUsesInProcessBounceAndExternalRetry(t *testing.T) {
	ctx := store.WithWorkspace(context.Background(), "test")
	st := store.NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "citation-bounce", Workspace: "test", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-citation", Title: "Citation contract"}, core.RequirementVersion{
		Content: "Review cites confirmed intent.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Review cites confirmed intent.\n```",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Review cites confirmed intent."}},
		Origin: core.RequirementOriginChat, OriginSessionID: "planning-citation",
	})
	if err != nil {
		t.Fatal(err)
	}
	human := store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	if _, _, err = st.ConfirmRequirementVersion(human, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ProposeRequirementServes(ctx, task.ID, requirement.ID, core.RequirementServesPlanning, false); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConfirmRequirementServes(human, task.ID, requirement.ID); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "citation-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: now}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2}
	d := New(st, cfg, nil)
	d.DisableMemoryQueueForTest()
	result := pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "looks good"}
	if err = d.ApplyExternalReview(ctx, task, job, result, job.ID, "external-session", "review"); err == nil || !strings.Contains(err.Error(), "assessment is required") {
		t.Fatalf("external review error=%v, want retryable validation error", err)
	}
	if count, _ := st.CountEvents(ctx, task.ID, "review.output_invalid"); count != 0 {
		t.Fatalf("external validation created %d in-process bounce events", count)
	}
	output := "```conveyor:review\n{\"verdict\":\"approve\",\"reason_code\":\"approved\",\"summary\":\"looks good\",\"feedback\":\"\"}\n```"
	if err = d.completeOutput(ctx, cfg, task, job, output, "in-process"); err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetTask(ctx, task.ID)
	if err != nil || updated.State != core.TaskQueued || updated.NextStage != core.StageReview {
		t.Fatalf("citation bounce task=%+v err=%v", updated, err)
	}
	if count, _ := st.CountEvents(ctx, task.ID, "review.output_invalid"); count != 1 {
		t.Fatalf("in-process citation validation bounce events=%d, want 1", count)
	}
}

func TestValidateReviewCitationsCoversEveryAssessmentBranch(t *testing.T) {
	served := []core.ServedRequirementContext{{ID: "req-runtime"}}
	tests := []struct {
		name   string
		served []core.ServedRequirementContext
		value  *core.RequirementCitationAssessment
		want   string
	}{
		{name: "linked missing", served: served, want: "assessment is required"},
		{name: "linked applicability mismatch", served: served, value: &core.RequirementCitationAssessment{}, want: "does not match"},
		{name: "unlinked applicability mismatch", value: &core.RequirementCitationAssessment{Applicable: true}, want: "does not match"},
		{name: "unlinked cited finding", value: &core.RequirementCitationAssessment{CitedIDs: []string{"REQ-1"}}, want: "findings must be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := pipeline.Review{RequirementCitations: tt.value}
			err := validateReviewCitations(&result, tt.served)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validation error=%v, want %q", err, tt.want)
			}
		})
	}
	t.Run("unlinked omission auto fills", func(t *testing.T) {
		result := pipeline.Review{}
		if err := validateReviewCitations(&result, nil); err != nil || result.RequirementCitations == nil || result.RequirementCitations.Applicable {
			t.Fatalf("auto-fill result=%+v err=%v", result.RequirementCitations, err)
		}
	})
}

func TestReviewAcceptanceFailureRollsBackAndRetryCommitsOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := store.NewMemory()
	task := core.Task{ID: "publication-failure", Workspace: "test", Repo: "app", Level: core.L0, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := base.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()}
	if err := base.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	flaky := &reviewAcceptanceFlakyStore{Store: base, failures: 1}
	d := New(flaky, &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	d.DisableMemoryQueueForTest()
	err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review")
	if err == nil || !strings.Contains(err.Error(), "review acceptance unavailable") {
		t.Fatalf("error = %v", err)
	}
	updated, getErr := base.GetTask(ctx, task.ID)
	if getErr != nil || updated.State != core.TaskRunning {
		t.Fatalf("task=%+v err=%v", updated, getErr)
	}
	events, _ := base.ListEvents(ctx, task.ID)
	for _, event := range events {
		if event.Kind == "review.completed" || event.Kind == "review.publication_queued" {
			t.Fatalf("partial review acceptance event persisted: %s", event.Kind)
		}
	}
	if err = d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review"); err != nil {
		t.Fatalf("recovery retry failed: %v", err)
	}
	publication, err := base.GetReviewPublication(ctx, job.ID)
	if err != nil || publication.State != core.ReviewPublicationQueued {
		t.Fatalf("publication=%+v err=%v", publication, err)
	}
	events, _ = base.ListEvents(ctx, task.ID)
	completed := 0
	for _, event := range events {
		if event.Kind == "review.completed" {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("review.completed events = %d, want 1", completed)
	}
}

func TestReviewForRepoWithoutGitHubDoesNotCreateOrReconcilePublication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "non-github-review", Workspace: "test", Repo: "local", Level: core.L0, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "non-github-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "local", URL: "file:///tmp/local"}}}, nil)
	d.DisableMemoryQueueForTest()
	if err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetReviewPublication(ctx, job.ID); err == nil {
		t.Fatal("non-GitHub repository created review publication")
	}
	if repaired, err := st.ReconcileReviewPublications(ctx); err != nil || repaired != 0 {
		t.Fatalf("non-GitHub reconciliation=%d err=%v", repaired, err)
	}
	if _, err := st.GetReviewPublication(ctx, job.ID); err == nil {
		t.Fatal("non-GitHub review was reconciled into publication")
	}
}

func TestExistingUnacceptedReviewEventRepairsRouting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	task := core.Task{ID: "partial-review", Workspace: "test", Repo: "app", Level: core.L0, State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "partial-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, StartedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{
		"review_work_order_id": job.ID, "verdict": "changes_requested", "publication_eligible": true,
	})}); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	d.DisableMemoryQueueForTest()
	if err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "retry", Feedback: "fix it"}, job.ID, "review-session", "review"); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskQueued || current.NextStage != core.StageImplement {
		t.Fatalf("repaired task=%+v err=%v", current, err)
	}
	events, _ := st.ListEvents(ctx, task.ID)
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Kind]++
	}
	if counts["review.completed"] != 1 || counts["pipeline.bounced"] != 1 || counts["review.accepted"] != 1 {
		t.Fatalf("recovery event counts=%v", counts)
	}
	if publication, getErr := st.GetReviewPublication(ctx, job.ID); getErr != nil || publication.State != core.ReviewPublicationQueued {
		t.Fatalf("repaired publication=%+v err=%v", publication, getErr)
	}
}

func TestExternalReviewApprovePreservesLevelRouting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		level    core.EscalationLevel
		state    core.TaskState
		recovery core.Stage
	}{
		{name: "L0 direct approval", level: core.L0, state: core.TaskApproved},
		{name: "L2 final human gate", level: core.L2, state: core.TaskAwaiting, recovery: core.StageImplement},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMemory()
			task := core.Task{ID: "approve-" + string(test.level), Workspace: "test", Repo: "app", Level: test.level, State: core.TaskRunning, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review", StartedAt: time.Now()}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			d := New(st, &config.Config{Workspace: "test", MaxBounces: 2}, nil)
			d.DisableMemoryQueueForTest()
			if err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review"); err != nil {
				t.Fatal(err)
			}
			updated, err := st.GetTask(ctx, task.ID)
			if err != nil || updated.State != test.state || updated.RecoveryStage != test.recovery {
				t.Fatalf("task=%+v err=%v", updated, err)
			}
		})
	}
}

func TestResolvedMergeGateControlsHumanWaitOrAutomaticMerge(t *testing.T) {
	for _, test := range []struct {
		name          string
		mergeApproval bool
		want          core.TaskState
	}{{"human merge gate", true, core.TaskAwaiting}, {"automatic merge", false, core.TaskMerged}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := store.WithWorkspace(t.Context(), "test")
			st := store.NewMemory()
			task := core.Task{ID: "policy-" + test.name, Workspace: "test", Repo: "app", Branch: "conveyor/policy", PolicyVersion: 1, SpecApproval: false, MergeApproval: test.mergeApproval, State: core.TaskRunning, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning, StartedAt: time.Now()}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			d := New(st, &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
			d.DisableMemoryQueueForTest()
			merged := false
			d.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				return githubtrigger.PullRequest{Number: 7, State: map[bool]string{true: "closed", false: "open"}[merged], Merged: merged, Mergeable: "MERGEABLE"}, nil
			}
			d.RequestMerge = func(context.Context, string, int) error { merged = true; return nil }
			if err := d.ApplyExternalReview(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review-model"); err != nil {
				t.Fatal(err)
			}
			updated, _ := st.GetTask(ctx, task.ID)
			if updated.State != test.want {
				t.Fatalf("state=%s want=%s", updated.State, test.want)
			}
		})
	}
}

func TestUnanimousReviewPanelSurvivesRestartAndUsesResolvedMergeGate(t *testing.T) {
	for _, test := range []struct {
		name          string
		mergeApproval bool
		want          core.TaskState
	}{{"human merge gate", true, core.TaskAwaiting}, {"automatic merge", false, core.TaskMerged}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := store.WithWorkspace(t.Context(), "test")
			st := store.NewMemory()
			now := time.Now().UTC()
			task := core.Task{ID: "panel-policy-" + test.name, Workspace: "test", Repo: "app", Branch: "conveyor/panel-policy", Mode: core.TaskModeAuto, PolicyVersion: 1, MergeApproval: test.mergeApproval, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			jobs := []core.Job{
				{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending},
				{ID: task.ID + "-review-1-seat-2", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending},
			}
			orders := []core.WorkOrder{
				{ID: jobs[0].ID, TaskID: task.ID, JobID: jobs[0].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, RequiredModel: "gpt-review", RequiredHarness: "codex", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now},
				{ID: jobs[1].ID, TaskID: task.ID, JobID: jobs[1].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 2, RequiredModel: "claude-review", RequiredHarness: "claude", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now},
			}
			if err := storetest.For(st).CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}
			firstDispatcher := New(st, cfg, nil)
			firstDispatcher.DisableMemoryQueueForTest()
			if err := firstDispatcher.ApplyExternalReview(ctx, task, jobs[0], pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "seat one passes", Feedback: "seat one evidence"}, orders[0].ID, "review-session-1", "gpt-review"); err != nil {
				t.Fatal(err)
			}
			if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskRunning {
				t.Fatalf("panel advanced before unanimous verdict: %+v", current)
			}

			// A new dispatcher instance represents restart recovery before the
			// second durable verdict arrives.
			restarted := New(st, cfg, nil)
			restarted.DisableMemoryQueueForTest()
			merged := false
			restarted.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
				return githubtrigger.PullRequest{Number: 7, State: map[bool]string{true: "closed", false: "open"}[merged], Merged: merged, Mergeable: "MERGEABLE"}, nil
			}
			restarted.RequestMerge = func(context.Context, string, int) error { merged = true; return nil }
			if err := restarted.ApplyExternalReview(ctx, task, jobs[1], pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "seat two passes", Feedback: "seat two evidence"}, orders[1].ID, "review-session-2", "claude-review"); err != nil {
				t.Fatal(err)
			}
			updated, _ := st.GetTask(ctx, task.ID)
			if updated.State != test.want {
				t.Fatalf("state=%s want=%s", updated.State, test.want)
			}
			if count, err := st.CountEvents(ctx, task.ID, "review.round_completed"); err != nil || count != 1 {
				t.Fatalf("completed rounds=%d err=%v", count, err)
			}
		})
	}
}
