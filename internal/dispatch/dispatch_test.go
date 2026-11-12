package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

func composeReviewOutput(review pipeline.Review) string {
	data, _ := json.Marshal(review)
	return "```conveyor:review\n" + string(data) + "\n```"
}

type capturingInputAgent struct {
	input inprocess.Input
	model string
	calls int
}

func TestTransitionDoesNotDemoteTaskWithLiveClaim(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "live-claim-demotion", Workspace: "demo", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "live-review", ClientToken: "secret", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, &config.Config{Workspace: "demo"}, nil)
	dispatcher.DisableMemoryQueueForTest()
	if err := dispatcher.transition(ctx, task.ID, core.TaskStageBounce, core.StageImplement, ""); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskRunning || current.NextStage != core.StageReview {
		t.Fatalf("task after demotion attempt=%+v err=%v", current, err)
	}
}

func (agent *capturingInputAgent) Run(_ context.Context, model string, input inprocess.Input) (inprocess.Result, error) {
	agent.calls++
	agent.model = model
	agent.input = input
	return inprocess.Result{Output: "```conveyor:triage\n{\"class\":\"feature\",\"route\":\"proceed\",\"summary\":\"context received\"}\n```"}, nil
}

func TestInProcessDispatchUsesAndAttributesEnvironmentModelOverride(t *testing.T) {
	t.Setenv(config.ControlPlaneModelEnv, "general")
	t.Setenv(config.TriageModelEnv, "triage-override")
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "model-override", Workspace: "demo", Repo: "api", Title: "Resolve model", State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "stored", Timeout: time.Minute}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	if err = dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if agent.model != "triage-override" {
		t.Fatalf("agent model=%q", agent.model)
	}
	jobs, err := st.ListJobs(ctx, task.ID)
	if err != nil || len(jobs) != 1 || jobs[0].ModelTier != "triage-override" {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind != "pipeline.dispatched" {
			continue
		}
		var payload struct {
			Model string `json:"model"`
		}
		if err = json.Unmarshal(event.Payload, &payload); err != nil || payload.Model != "triage-override" {
			t.Fatalf("dispatch payload=%s err=%v", event.Payload, err)
		}
		return
	}
	t.Fatal("pipeline.dispatched event not found")
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
	ctx := store.WithActor(context.Background(), store.SystemActor())
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
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
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
	return st.raceRead(func() ([]core.WorkOrder, error) { return st.Store.ListTaskWorkOrders(ctx, taskID) })
}

// ListTaskWorkOrdersStored routes the dispatcher's stage-hold read through the
// same two-arrival barrier so both dispatches snapshot before either creates.
func (st *concurrentSpecDispatchStore) ListTaskWorkOrdersStored(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	return st.raceRead(func() ([]core.WorkOrder, error) { return st.Store.ListTaskWorkOrdersStored(ctx, taskID) })
}

func (st *concurrentSpecDispatchStore) raceRead(read func() ([]core.WorkOrder, error)) ([]core.WorkOrder, error) {
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

	orders, err := read()
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
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "mcp-spec", Workspace: "demo", Repo: "api", BaseBranch: "main", Branch: "conveyor/task-mcp-spec", State: core.TaskQueued, NextStage: core.StageSpec, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	// A stale legacy route may still say in_process. Spec dispatch ignores
	// that execution marker and creates an MCP work order (DEC-132(2)).
	cfg := &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Routing: config.Routing{Stages: map[string]config.StageRoute{"spec": {TimeoutText: "30m", Execution: config.ExecutionInProcess}}}}
	d := New(st, cfg, agent)
	if err := d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].Stage != core.StageSpec || orders[0].ExecutionTimeoutText != "30m" || orders[0].RequiredHarness != "" || orders[0].RequiredModel != "" || orders[0].RequiredHarnessConfig != nil {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	if agent.calls != 0 {
		t.Fatalf("in-process spec fallback ran %d time(s)", agent.calls)
	}
}

// A stale review route that still says in_process, or names no execution,
// opens an MCP review round and never calls a model: no Conveyor component
// invokes a model for an agent stage, and no seat model reaches a route
// (DEC-132(2), (4); component-work-orders).
func TestStaleInProcessReviewRouteOpensMCPReviewRound(t *testing.T) {
	for name, execution := range map[string]config.ExecutionMode{"in_process": config.ExecutionInProcess, "unset": ""} {
		t.Run(name, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := core.Task{ID: "stale-review-" + name, Workspace: "demo", Repo: "api", Title: "Review", PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageReview,
				Branch: "conveyor/task-stale-review", BaseBranch: "main", CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			agent := &capturingInputAgent{}
			cfg := &config.Config{Workspace: "demo", MaxBounces: 2, WorkOrderQueueTimeout: time.Hour, Repos: []config.Repo{{Name: "api", Base: "main"}},
				Routing: config.Routing{Triage: config.TriageRoute{Model: "deploy-triage", Timeout: time.Minute}, Stages: map[string]config.StageRoute{
					"review": {Execution: execution, TimeoutText: "45m", Timeout: 45 * time.Minute},
				}},
				Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}}}
			d := New(st, cfg, agent)
			d.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) {
				t.Fatal("review dispatch read a branch diff for a model prompt")
				return "", nil
			}
			if err := d.DispatchNow(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			if agent.calls != 0 {
				t.Fatalf("review stage called a model %d time(s)", agent.calls)
			}
			orders, err := st.ListTaskWorkOrders(ctx, task.ID)
			if err != nil || len(orders) != 1 || orders[0].Stage != core.StageReview || orders[0].ReviewRound != 1 || orders[0].ExecutionTimeoutText != "45m" ||
				orders[0].RequiredModel != "" || orders[0].RequiredHarness != "" || orders[0].RequiredEffort != "" {
				t.Fatalf("review orders=%+v err=%v", orders, err)
			}
			jobs, err := st.ListJobs(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, job := range jobs {
				if job.Runner == "in-process" {
					t.Fatalf("review dispatch started an in-process job: %+v", job)
				}
			}
		})
	}
}

// runTriage runs only the triage stage: handed any agent stage, it refuses
// before creating a job or calling a model (DEC-132(2), (3)).
func TestRunTriageRefusesAgentStages(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	for _, stage := range []core.Stage{core.StageSpec, core.StageImplement, core.StageReview, core.StageVerify} {
		st := store.NewMemory()
		task := core.Task{ID: "refuse-" + string(stage), Workspace: "demo", Repo: "api", State: core.TaskQueued, NextStage: stage, CreatedAt: time.Now()}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		agent := &capturingInputAgent{}
		cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "deploy-triage", Timeout: time.Minute}, Stages: map[string]config.StageRoute{
			string(stage): {Execution: config.ExecutionInProcess, Timeout: time.Minute},
		}}}
		d := New(st, cfg, agent)
		if err := d.runTriage(ctx, cfg, task); err == nil || !strings.Contains(err.Error(), "does not run in process") {
			t.Fatalf("%s: runTriage error = %v", stage, err)
		}
		jobs, err := st.ListJobs(ctx, task.ID)
		if err != nil || len(jobs) != 0 || agent.calls != 0 {
			t.Fatalf("%s: jobs=%+v calls=%d err=%v", stage, jobs, agent.calls, err)
		}
	}
}

func TestConcurrentSpecDispatchCreatesOneWorkOrder(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	memory := store.NewMemory()
	task := core.Task{ID: "concurrent-mcp-spec", Workspace: "demo", Repo: "api", BaseBranch: "main", State: core.TaskQueued, NextStage: core.StageSpec, CreatedAt: time.Now()}
	if err := memory.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	st := newConcurrentSpecDispatchStore(memory)
	cfg := &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Routing: config.Routing{Stages: map[string]config.StageRoute{"spec": {TimeoutText: "30m", Execution: config.ExecutionMCP}}}}
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
	createCalls, arrivals, raced := st.createCalls, st.arrivals, st.raceObserved
	st.mu.Unlock()
	if arrivals != 2 || !raced {
		t.Fatalf("dispatch order reads arrivals=%d raceObserved=%v, want both dispatches through the barrier", arrivals, raced)
	}
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
	listErr, readErr                       bool
	neighborhoodCalls, scopedArtifactCalls int
}

func (st *artifactContextFailureStore) ListLineageNeighborhood(ctx context.Context, roots []core.LineageNode, budget core.LineageTraversalBudget) ([]core.LineageLink, error) {
	st.neighborhoodCalls++
	return st.Store.ListLineageNeighborhood(ctx, roots, budget)
}

func (st *artifactContextFailureStore) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	if st.listErr {
		return nil, errors.New("artifact list unavailable")
	}
	return st.Store.ListArtifacts(ctx)
}

func (st *artifactContextFailureStore) ListArtifactsForLineage(ctx context.Context, nodes []core.LineageNode) ([]core.Artifact, error) {
	st.scopedArtifactCalls++
	if st.listErr {
		return nil, errors.New("artifact list unavailable")
	}
	return st.Store.ListArtifactsForLineage(ctx, nodes)
}

func (st *artifactContextFailureStore) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	if st.readErr {
		return core.Artifact{}, nil, errors.New("artifact read unavailable")
	}
	return st.Store.GetArtifact(ctx, id)
}

func TestPipelinePreparesTextImageDocumentAndAudioArtifactInputs(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "artifact-context", Workspace: "demo", Repo: "api", Title: "Use attachments", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	largeText := bytes.Repeat([]byte("a"), (32<<10)+17)
	for _, item := range []struct {
		name, contentType string
		content           []byte
	}{
		{name: "large.txt", contentType: "text/plain", content: largeText},
		{name: "design.png", contentType: "image/png", content: testimage.PNG("png")},
		{name: "requirements.pdf", contentType: "application/pdf", content: []byte("pdf")},
		{name: "interview.mp3", contentType: "audio/mpeg", content: []byte("mp3")},
	} {
		if _, err := st.CreateArtifact(ctx, core.Artifact{Name: item.name, ContentType: item.contentType, TaskID: task.ID}, item.content); err != nil {
			t.Fatal(err)
		}
	}
	// Negative context: a workspace-unattached upload (the shape a retired
	// feature attachment takes after migration 142/0018) and another task's
	// attachment never enter this task's model input (component-artifacts).
	unrelated := core.Task{ID: "unrelated-context", Workspace: "demo", Repo: "api", Title: "Other work", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, unrelated); err != nil {
		t.Fatal(err)
	}
	for _, negative := range []core.Artifact{
		{Name: "unattached.txt", ContentType: "text/plain"},
		{Name: "unrelated-task.txt", ContentType: "text/plain", TaskID: unrelated.ID},
	} {
		if _, err := st.CreateArtifact(ctx, negative, []byte("must not enter live model context: "+negative.Name)); err != nil {
			t.Fatal(err)
		}
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "gpt", Effort: "low", Timeout: time.Minute}}}
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
		if attachment.Name == "unattached.txt" || attachment.Name == "unrelated-task.txt" {
			t.Fatalf("%s entered this task's live model context", attachment.Name)
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
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := newHistoricalArtifactStore()
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
	sibling := core.Task{ID: "context-sibling", Workspace: "demo", Title: "Merged sibling", State: core.TaskMerged,
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
		{name: "oversized-ci.log", taskID: sibling.ID, content: strings.Repeat("x", maxModelAttachmentBytes+1)},
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
	dispatcher := New(st, &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "gpt", Timeout: time.Minute}}}, agent)
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
	for _, want := range []string{"untrusted historical context", "sibling_outcome", "Merged sibling [merged]", "```text", `body omitted by triage byte budget: "oversized-ci.log"`} {
		if !strings.Contains(agent.input.Prompt, want) {
			t.Fatalf("dispatch prompt omitted %q:\n%s", want, agent.input.Prompt)
		}
	}
}

func TestPipelineRetriesKeepGeneratedTranscriptsOutOfStageInput(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "artifact-retry", Workspace: "demo", Repo: "api", Title: "Retry safely", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateArtifact(ctx, core.Artifact{Name: "original.png", ContentType: "image/png", TaskID: task.ID}, testimage.PNG("original")); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &failingTranscriptAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "gpt-5.6-terra", Timeout: time.Minute}}}
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
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
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
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"spec": {Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	// The spec stage's input is built without a model call: agent stages
	// are MCP work orders (DEC-132(2); component-work-orders).
	input, err := dispatcher.buildStageInput(store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), cfg, core.StageSpec, task)
	if err != nil {
		t.Fatal(err)
	}
	if agent.calls != 0 {
		t.Fatalf("calls = %d, want 0", agent.calls)
	}
	if input.OutputSchema == nil || input.OutputSchema.Name != "conveyor_plan" || input.OutputSchema.Schema == nil {
		t.Fatalf("spec output schema = %+v", input.OutputSchema)
	}
	for _, expected := range []string{"# Prior specification revision v1", "Old intent.", "# Human gate feedback", "Target v1.28, not v1.27."} {
		if !strings.Contains(input.Prompt, expected) {
			t.Fatalf("spec prompt missing %q:\n%s", expected, input.Prompt)
		}
	}
}

func TestInProcessReviewEmbedsBranchDiff(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "review-diff", Workspace: "demo", Repo: "api", Title: "Review the change", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageReview, Branch: "conveyor/task-review-diff", BaseBranch: "main", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]string{"base_sha": "base-sha", "head_sha": "head-sha"})}); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	dispatcher.ReviewDiff = func(_ context.Context, _ *config.Config, got core.Task) (string, error) {
		if got.ID != task.ID {
			t.Fatalf("ReviewDiff received task %q, want %q", got.ID, task.ID)
		}
		return "diff --git a/app.txt b/app.txt\n-v1\n+v2\n", nil
	}
	input, err := dispatcher.buildStageInput(ctx, cfg, core.StageReview, task)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"# Authoritative review comparison", "github_base_head_compare", "Scope: full", "Baseline SHA: `base-sha`", "Reviewed head SHA: `head-sha`", "# Branch diff (base-sha...head-sha)", "supporting changed-path evidence", "````diff", "diff --git a/app.txt b/app.txt", "+v2"} {
		if !strings.Contains(input.Prompt, expected) {
			t.Fatalf("review prompt missing %q:\n%s", expected, input.Prompt)
		}
	}
}

func TestInProcessReviewStatesWhenBranchHasNoChanges(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "review-empty", Workspace: "demo", Repo: "api", Title: "Review nothing", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageReview, Branch: "conveyor/task-review-empty", BaseBranch: "main", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]string{"base_sha": "empty-base", "head_sha": "empty-head"})}); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	dispatcher.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) { return "\n", nil }
	input, err := dispatcher.buildStageInput(ctx, cfg, core.StageReview, task)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Baseline SHA: `empty-base`", "Reviewed head SHA: `empty-head`", "completed successfully and contains no changes"} {
		if !strings.Contains(input.Prompt, expected) {
			t.Fatalf("review prompt missing %q:\n%s", expected, input.Prompt)
		}
	}
}

func TestInProcessReviewRendersFrozenRefreshDeltaComparison(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{
		ID: "review-refresh-delta", Workspace: "demo", Repo: "api", Title: "Review the delta", Mode: core.TaskModeAuto,
		PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageReview, Branch: "conveyor/task-review-refresh-delta", BaseBranch: "main",
		ApprovalStale: true, RefreshBaselineSHA: "approved-head", RefreshHeadSHA: "replacement-head", RefreshReviewScope: config.RefreshReviewDelta,
		CreatedAt: time.Now(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	dispatcher.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) { return "delta", nil }
	input, err := dispatcher.buildStageInput(ctx, cfg, core.StageReview, task)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Scope: delta", "Baseline SHA: `approved-head`", "Reviewed head SHA: `replacement-head`", "# Branch diff (approved-head...replacement-head)"} {
		if !strings.Contains(input.Prompt, expected) {
			t.Fatalf("refresh prompt missing %q:\n%s", expected, input.Prompt)
		}
	}
}

func TestInProcessReviewDiffFailuresStopBeforeModelExecution(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "review-diff-fail", Workspace: "demo", Repo: "api", Title: "Review the change", Mode: core.TaskModeAuto, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageReview, Branch: "conveyor/task-review-diff-fail", BaseBranch: "main", CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]string{"base_sha": "failure-base", "head_sha": "failure-head"})}); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	dispatcher.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) {
		return "", errors.New("origin unreachable")
	}
	if _, err = dispatcher.buildStageInput(ctx, cfg, core.StageReview, task); err == nil || !strings.Contains(err.Error(), "origin unreachable") {
		t.Fatalf("review input error = %v, want branch diff failure", err)
	}
	if agent.calls != 0 {
		t.Fatalf("model executed despite diff failure: calls = %d", agent.calls)
	}

	oversized := strings.Repeat("a", maxModelDiffBytes+1)
	dispatcher.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) { return oversized, nil }
	if _, err = dispatcher.buildStageInput(ctx, cfg, core.StageReview, task); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("review input error = %v, want oversized diff failure", err)
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
			ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
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
			dispatcher := New(wrapped, &config.Config{Workspace: "demo", Routing: config.Routing{Triage: config.TriageRoute{Model: "gpt", Timeout: time.Minute}}}, agent)
			dispatcher.Pack = bundle
			err = dispatcher.DispatchNow(ctx, task.ID)
			if err == nil || !strings.Contains(err.Error(), test.want) || agent.calls != 0 {
				t.Fatalf("error=%v calls=%d", err, agent.calls)
			}
			if wrapped.neighborhoodCalls != 2 || wrapped.scopedArtifactCalls != 1 {
				t.Fatalf("context queries neighborhood=%d artifacts=%d, want one citation query plus one shared artifact traversal", wrapped.neighborhoodCalls, wrapped.scopedArtifactCalls)
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
			if diagnostic.Phase != "attachment_preparation" || diagnostic.Provider != "openai_responses" || diagnostic.Model != config.ResolveControlPlaneModel("triage", "gpt") {
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

type mergeIdentityStore struct{ store.Store }

func (s mergeIdentityStore) GetCallerIdentity(context.Context, string, string) (core.CallerIdentity, error) {
	return core.CallerIdentity{ID: "usr-approver", DisplayName: "Approving Operator", Email: "approver@example.com"}, nil
}

// AuthorizeWorkspace grants operate_gates to every person whose user ID does
// not start with "no-gates-", so approval tests can name a person without
// the capability (req-risk-based-review AC-8.3).
func (s mergeIdentityStore) AuthorizeWorkspace(_ context.Context, userID, _ string, capability core.Capability) (bool, error) {
	return userID != "" && !strings.HasPrefix(userID, "no-gates-") && capability == core.CapabilityOperateGates, nil
}

func TestOrdinaryReviewWithoutOrderHeadFallsBackToPullRequestOpenedHead(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	st := store.NewMemory()
	task := core.Task{ID: "ordinary-review-head", Workspace: "test", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, MergeApproval: true, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "reviewer"}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"head_sha": "ordinary-head"})}); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test"}, nil)
	if err := d.ApplyExternalReviewPinned(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "ordinary review"}, order.ID, "ordinary-session", "reviewer", []core.ServedRequirementContext{}, emptyGovernanceAuthority()); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.ReviewedHeadSHA != "ordinary-head" {
		t.Fatalf("ordinary reviewed head=%q err=%v", current.ReviewedHeadSHA, err)
	}
}

func boolRef(value bool) *bool { return &value }

func emptyGovernanceAuthority() *core.GovernanceSnapshot {
	return &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{}, Decisions: []core.Decision{}}
}

func TestValidateGovernanceAssessmentUsesPinnedSplitAuthority(t *testing.T) {
	snapshot := core.GovernanceSnapshot{
		Designs: []core.GovernanceDesignContext{{ID: "DESIGN-runtime", Version: 2}},
		Decisions: []core.Decision{
			{ID: "DEC-1", Status: core.DecisionConfirmed},
			{ID: "DEC-2", Status: core.DecisionSuperseded},
		},
		PendingDesignProposals: []core.PendingSystemDesignProposal{{DocumentID: "DESIGN-pending", Version: 3, ProposalEventID: 42, OriginTaskID: "task-a"}},
	}
	tests := []struct {
		name       string
		assessment core.GovernanceAssessment
		wantError  string
	}{
		{name: "valid", assessment: core.GovernanceAssessment{DesignApplicable: boolRef(true), DecisionCitable: boolRef(true), CitedIDs: []string{"DESIGN-runtime", "DEC-1"}, SupersededIDs: []string{"DEC-2"}}},
		{name: "known design unknown", assessment: core.GovernanceAssessment{DesignApplicable: boolRef(true), DecisionCitable: boolRef(true), UnknownIDs: []string{"DESIGN-runtime"}}, wantError: `unknown_ids entry "DESIGN-runtime" is present in the pinned governing authority and belongs in cited_ids`},
		{name: "known decision ungoverned", assessment: core.GovernanceAssessment{DesignApplicable: boolRef(true), DecisionCitable: boolRef(true), UngovernedIDs: []string{"DEC-1"}}, wantError: `ungoverned_ids entry "DEC-1" is present in the pinned governing authority and belongs in cited_ids`},
		{name: "superseded accepted only as finding", assessment: core.GovernanceAssessment{DesignApplicable: boolRef(true), DecisionCitable: boolRef(true), CitedIDs: []string{"DEC-2"}}, wantError: `cited id "DEC-2" is not confirmed governing authority in the pinned snapshot`},
		{name: "pending proposal is not citable authority", assessment: core.GovernanceAssessment{DesignApplicable: boolRef(true), DecisionCitable: boolRef(true), CitedIDs: []string{"DESIGN-pending"}}, wantError: `cited id "DESIGN-pending" is not confirmed governing authority in the pinned snapshot`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			review := pipeline.Review{GovernanceAssessment: &test.assessment}
			err := validateGovernanceAssessment(snapshot, &review)
			if test.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("error=%v want %q", err, test.wantError)
			}
		})
	}
}

func TestValidateGovernanceAssessmentRejectsPinnedContractMismatches(t *testing.T) {
	pinned := core.GovernanceSnapshot{
		Designs:   []core.GovernanceDesignContext{{ID: "DESIGN-runtime", Version: 2}},
		Decisions: []core.Decision{{ID: "DEC-1", Status: core.DecisionConfirmed}, {ID: "DEC-2", Status: core.DecisionSuperseded}},
	}
	empty := core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{}, Decisions: []core.Decision{}}
	tests := []struct {
		name       string
		snapshot   core.GovernanceSnapshot
		assessment *core.GovernanceAssessment
		want       string
	}{
		{name: "design applicability mismatch", snapshot: pinned, assessment: &core.GovernanceAssessment{DesignApplicable: boolRef(false), DecisionCitable: boolRef(true)}, want: "design_applicable=false"},
		{name: "decision citability mismatch", snapshot: pinned, assessment: &core.GovernanceAssessment{DesignApplicable: boolRef(true), DecisionCitable: boolRef(false)}, want: "decision_citable=false"},
		{name: "assessment absent with authority", snapshot: pinned, assessment: nil, want: "governance_assessment is required"},
		{name: "finding against empty pin", snapshot: empty, assessment: &core.GovernanceAssessment{DesignApplicable: boolRef(false), DecisionCitable: boolRef(false), UnknownIDs: []string{"DESIGN-missing"}}, want: "findings must be empty"},
		{name: "confirmed id listed as superseded", snapshot: pinned, assessment: &core.GovernanceAssessment{DesignApplicable: boolRef(true), DecisionCitable: boolRef(true), SupersededIDs: []string{"DEC-1"}}, want: `superseded id "DEC-1" is not a superseded decision`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			review := pipeline.Review{GovernanceAssessment: test.assessment}
			err := validateGovernanceAssessment(test.snapshot, &review)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want %q", err, test.want)
			}
		})
	}
}

func TestValidateGovernanceAssessmentAllowsDecisionWithoutDesignAndIgnoresLiveRace(t *testing.T) {
	decisionOnly := core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{}, Decisions: []core.Decision{{ID: "DEC-1", Status: core.DecisionConfirmed}}}
	review := pipeline.Review{GovernanceAssessment: &core.GovernanceAssessment{DesignApplicable: boolRef(false), DecisionCitable: boolRef(true), CitedIDs: []string{"DEC-1"}}}
	if err := validateGovernanceAssessment(decisionOnly, &review); err != nil {
		t.Fatalf("decision-only assessment rejected: %v", err)
	}
	legacyFalse := false
	legacy := pipeline.Review{GovernanceAssessment: &core.GovernanceAssessment{Applicable: &legacyFalse, CitedIDs: []string{"DEC-1"}}}
	if err := validateGovernanceAssessment(decisionOnly, &legacy); err != nil || legacy.GovernanceAssessment.DecisionCitable == nil || !*legacy.GovernanceAssessment.DecisionCitable {
		t.Fatalf("legacy decision-only mapping=%+v err=%v", legacy.GovernanceAssessment, err)
	}
	// A design confirmed after claim is intentionally absent from this pin.
	emptyPin := core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{}, Decisions: []core.Decision{}}
	contractFaithful := pipeline.Review{GovernanceAssessment: &core.GovernanceAssessment{DesignApplicable: boolRef(false), DecisionCitable: boolRef(false)}}
	if err := validateGovernanceAssessment(emptyPin, &contractFaithful); err != nil {
		t.Fatalf("empty pinned authority rejected after hypothetical live change: %v", err)
	}
}

// A task's frozen policy sources stage timeouts for dispatch; review seat
// contents and workspace routes never become executor requirements
// (DEC-104(2)).
func TestFrozenPolicySourcesImplementationAndReviewDispatch(t *testing.T) {
	policy := func(seat config.ReviewSeat) config.PolicyContract {
		return config.PolicyContract{StageTimeouts: config.PolicyStageTimeouts{Spec: "30m", Implement: "2h", Review: "45m"}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{seat}}}
	}
	backend := policy(config.ReviewSeat{})
	frontend := policy(config.ReviewSeat{})
	routes := map[string]config.StageRoute{
		"implement": {Execution: config.ExecutionMCP},
		"review":    {Execution: config.ExecutionMCP},
	}
	cfg := (&config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Routing: config.Routing{Triage: config.TriageRoute{Model: "control", TimeoutText: "20m"}, Stages: routes}, Review: backend.Review}).WithPolicy(backend)

	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "frozen-frontend", Workspace: "demo", Title: "Frontend", SetupName: "frontend", SetupContract: frontend, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, cfg, nil)
	if err := dispatcher.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(orders) != 1 || orders[0].RequiredHarness != "" || orders[0].RequiredModel != "" || orders[0].RequiredEffort != "" || orders[0].ExecutionTimeoutText != "2h" {
		t.Fatalf("implementation orders=%+v err=%v", orders, err)
	}
	minimum := store.ReviewSeatMinimum(task.SetupContract, len(cfg.Review.Seats))
	jobs, reviewOrders, err := BuildReviewRound(cfg, task, cfg.Routing.Stages["review"], 1, minimum, minimum)
	if err != nil || len(jobs) != 1 || len(reviewOrders) != 1 || reviewOrders[0].RequiredHarness != "" || reviewOrders[0].RequiredModel != "" || reviewOrders[0].RequiredEffort != "" || reviewOrders[0].ExecutionTimeoutText != "45m" {
		t.Fatalf("review jobs=%+v orders=%+v err=%v", jobs, reviewOrders, err)
	}
}

func TestCapturedLegacySpecGateCanCompleteMaterialization(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemoryWithConfig(&config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "api", Base: "main"}}})
	task := core.Task{ID: "legacy-gate", Workspace: "demo", Repo: "api", State: core.TaskRunning, SpecApproval: true, PolicyVersion: 1, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: task.ID, Content: "## Intent\n\nLegacy.\n\n## Non-goals\n\nNone.", LegacyGate: true,
		AcceptanceCount: 1, Acceptance: core.JSONPayload([]pipeline.AcceptanceCriterion{{ID: "AC-1", Criterion: "Legacy promise completes", Verify: "test"}}),
		Decomposition: core.JSONPayload([]core.BlueprintDecompositionItem{{ID: "SUB-1", Repo: "api", Summary: "Finish the promised child"}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobDone}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	gate, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true})
	if err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "api", Base: "main"}}}, nil)
	if err = d.HandleIntervention(ctx, gate.Task, job, core.Intervention{Action: core.InterventionApprove, ReasonCode: "approved"}); err != nil {
		t.Fatal(err)
	}
	approved, ok, err := st.GetLatestSpecVersion(ctx, task.ID)
	current, taskErr := st.GetTask(ctx, task.ID)
	if err != nil || taskErr != nil || !ok || !approved.Approved || approved.Version != spec.Version || len(current.Children) != 1 {
		t.Fatalf("approved=%+v ok=%t task=%+v err=%v taskErr=%v", approved, ok, current, err, taskErr)
	}
}

func TestPlanSubmissionPreservesSpecGateLifecycleSequences(t *testing.T) {
	t.Parallel()
	type eventProjection struct {
		Kind    string
		Payload string
	}
	for _, tc := range []struct {
		name   string
		gate   bool
		action core.InterventionAction
	}{
		{name: "gate on", gate: true},
		{name: "gate approval", gate: true, action: core.InterventionApprove},
		{name: "gate redirect", gate: true, action: core.InterventionRedirect},
		{name: "gate off direct to implement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(plan bool) []eventProjection {
				ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
				st := store.NewMemory()
				task := core.Task{ID: "lifecycle-equivalence", Workspace: "demo", Repo: "api", PolicyVersion: 1, SpecApproval: tc.gate, State: core.TaskRunning, NextStage: core.StageSpec, CreatedAt: time.Unix(1, 0).UTC()}
				if err := st.CreateTask(ctx, task); err != nil {
					t.Fatal(err)
				}
				job := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
				if err := st.CreateJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				d := New(st, &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "api"}}}, nil)
				var err error
				if plan {
					_, err = d.ApplyExternalPlan(ctx, task, job, pipeline.StructuredPlan{Markdown: "## Approach\nReuse lifecycle.\n\n## Files touched\n- internal/dispatch/dispatch.go\n\n## Ordering\n1. Submit.\n\n## Risks\n- Drift.\n\n## Done criteria\n- Events match.", Decomposition: []pipeline.DecompositionItem{}}, "codex", "gpt")
				} else {
					legacy, parseErr := pipeline.RenderStructuredSpec(`{"markdown":"## Intent\nReuse lifecycle.\n\n## Non-goals\nNone.","acceptance":[{"id":"AC-1","criterion":"Events match","verify":"test"}],"decomposition":[]}`)
					if parseErr != nil {
						t.Fatal(parseErr)
					}
					_, err = d.completeSpecVersion(ctx, task, legacy, "codex", "gpt")
				}
				if err != nil {
					t.Fatal(err)
				}
				if tc.action != "" {
					current, getErr := st.GetTask(ctx, task.ID)
					if getErr != nil {
						t.Fatal(getErr)
					}
					if err = d.HandleIntervention(ctx, current, job, core.Intervention{TaskID: task.ID, JobID: job.ID, Action: tc.action, ReasonCode: "test", Comment: "test"}); err != nil {
						t.Fatal(err)
					}
				}
				events, listErr := st.ListEvents(ctx, task.ID)
				if listErr != nil {
					t.Fatal(listErr)
				}
				result := make([]eventProjection, 0, len(events))
				for _, event := range events {
					payload := string(event.Payload)
					if event.Kind == "spec.version_created" {
						payload = `{"version":1,"acceptance_count":"document-specific"}`
					}
					result = append(result, eventProjection{Kind: event.Kind, Payload: payload})
				}
				return result
			}
			specEvents, planEvents := run(false), run(true)
			if !reflect.DeepEqual(specEvents, planEvents) {
				t.Fatalf("spec events=%v plan events=%v", specEvents, planEvents)
			}
		})
	}
}

const planReviewSeatsMarkdown = "## Approach\nPlan the panel.\n\n## Files touched\n- internal/dispatch/dispatch.go\n\n## Ordering\n1. Submit.\n\n## Risks\n- Loss.\n\n## Done criteria\n- The planned count reaches review."

// newPlanReviewSeatsTask creates a plan-stage task with a frozen panel of
// minimum seats and its spec job.
func newPlanReviewSeatsTask(t *testing.T, ctx context.Context, st store.Store, id string, gate bool, minimum int) (core.Task, core.Job) {
	t.Helper()
	task := core.Task{ID: id, Workspace: "demo", Repo: "api", Title: "Planned seats", PolicyVersion: 1, SpecApproval: gate, State: core.TaskRunning, NextStage: core.StageSpec,
		Branch: "conveyor/task-" + id, BaseBranch: "main", CreatedAt: time.Now(),
		SetupContract: config.PolicyContract{Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, minimum)}, StageTimeouts: config.PolicyStageTimeouts{Review: "45m"}}}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	return task, job
}

func requirePlanVersionSeats(t *testing.T, ctx context.Context, st store.Store, taskID string, version int, want *int, approved bool) {
	t.Helper()
	stored, ok, err := st.GetSpecVersion(ctx, taskID, version)
	if err != nil || !ok || stored.Approved != approved || (stored.ReviewSeats == nil) != (want == nil) || (want != nil && *stored.ReviewSeats != *want) {
		t.Fatalf("plan version %d=%+v ok=%t err=%v, want review_seats %v approved %t", version, stored, ok, err, want, approved)
	}
}

// TestPlanReviewSeatsPersistenceAndApproval proves the planned review seat
// count reaches the plan version through ApplyExternalPlan and in-process plan
// output, that manual and automatic approval leave it unchanged, that
// omission and legacy spec rendering store none, and that in-process output
// below the minimum takes the existing invalid-output path without a version
// (req-review-gates-evidence AC-2.8; feature-review-seat-minimum RS-3).
func TestPlanReviewSeatsPersistenceAndApproval(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Workspace: "demo", MaxBounces: 3, Repos: []config.Repo{{Name: "api"}}}
	for _, tc := range []struct {
		name    string
		gate    bool
		seats   *int
		approve bool
	}{
		{name: "gate on manual approval", gate: true, seats: seats(3), approve: true},
		{name: "gate on pending", gate: true, seats: seats(2)},
		// No plan is approved without a person's decision, whatever the
		// task's legacy policy (component-work-orders invariant 56).
		{name: "gate off still waits for approval", seats: seats(3)},
		{name: "omitted", gate: true, approve: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task, job := newPlanReviewSeatsTask(t, ctx, st, "plan-seats-persist", tc.gate, 2)
			d := New(st, cfg, nil)
			version, err := d.ApplyExternalPlan(ctx, task, job, pipeline.StructuredPlan{Markdown: planReviewSeatsMarkdown, Decomposition: []pipeline.DecompositionItem{}, ReviewSeats: tc.seats}, "codex", "gpt")
			if err != nil {
				t.Fatal(err)
			}
			requirePlanVersionSeats(t, ctx, st, task.ID, version.Version, tc.seats, false)
			if tc.approve {
				current, _ := st.GetTask(ctx, task.ID)
				if err = d.HandleIntervention(ctx, current, job, core.Intervention{TaskID: task.ID, JobID: job.ID, Action: core.InterventionApprove, ReasonCode: "plan-approved"}); err != nil {
					t.Fatal(err)
				}
				requirePlanVersionSeats(t, ctx, st, task.ID, version.Version, tc.seats, true)
			}
		})
	}
	t.Run("legacy spec stores none", func(t *testing.T) {
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
		st := store.NewMemory()
		task, _ := newPlanReviewSeatsTask(t, ctx, st, "plan-seats-legacy", false, 2)
		legacy, err := pipeline.RenderStructuredSpec(`{"markdown":"## Intent\nKeep legacy.\n\n## Non-goals\nNone.","acceptance":[{"id":"AC-1","criterion":"Legacy","verify":"test"}],"decomposition":[]}`)
		if err != nil || legacy.ReviewSeats != nil {
			t.Fatalf("legacy spec=%+v err=%v", legacy, err)
		}
		version, err := New(st, cfg, nil).completeSpecVersion(ctx, task, legacy, "codex", "gpt")
		if err != nil {
			t.Fatal(err)
		}
		requirePlanVersionSeats(t, ctx, st, task.ID, version.Version, nil, false)
	})
	t.Run("in-process output", func(t *testing.T) {
		encoded, err := json.Marshal(planReviewSeatsMarkdown)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name  string
			seats string
			want  *int
		}{{name: "count", seats: "4", want: seats(4)}, {name: "null", seats: "null"}} {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task, job := newPlanReviewSeatsTask(t, ctx, st, "plan-seats-inprocess-"+tc.name, true, 2)
			output := `{"markdown":` + string(encoded) + `,"decomposition":[],"review_seats":` + tc.seats + `}`
			if err = New(st, cfg, nil).completeOutput(ctx, cfg, task, job, output, "planner", 0); err != nil {
				t.Fatal(err)
			}
			requirePlanVersionSeats(t, ctx, st, task.ID, 1, tc.want, false)
		}
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
		st := store.NewMemory()
		task, job := newPlanReviewSeatsTask(t, ctx, st, "plan-seats-inprocess-below", true, 2)
		output := `{"markdown":` + string(encoded) + `,"decomposition":[],"review_seats":1}`
		if err = New(st, cfg, nil).completeOutput(ctx, cfg, task, job, output, "planner", 0); err != nil {
			t.Fatal(err)
		}
		if _, ok, getErr := st.GetLatestSpecVersion(ctx, task.ID); getErr != nil || ok {
			t.Fatalf("below-minimum in-process plan wrote a version ok=%t err=%v", ok, getErr)
		}
		events, err := st.ListEvents(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		refused := false
		for _, event := range events {
			refused = refused || (event.Kind == "spec.output_invalid" && strings.Contains(string(event.Payload), "review_seats_below_minimum") && strings.Contains(string(event.Payload), "minimum_seat_count=2"))
		}
		if !refused {
			t.Fatalf("below-minimum in-process plan events=%+v", events)
		}
	})
}

// TestPlanReviewSeatsFirstRoundPlanned proves an approved plan's count above
// the minimum opens the first real review round at that count with source
// planned in the record and review.round_created, and that an approved count
// equal to the minimum opens the minimum. The resolver's floor is the
// submission-floor proof while RS-4's admission is absent from this base
// (req-review-gates-evidence AC-2.3, AC-2.4, AC-2.7, AC-2.8;
// feature-review-seat-minimum RS-3).
func TestPlanReviewSeatsFirstRoundPlanned(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		planned *int
		count   int
		source  string
	}{
		{name: "planned above minimum", planned: seats(4), count: 4, source: store.ReviewSeatSourcePlanned},
		{name: "planned equal to minimum", planned: seats(2), count: 2, source: store.ReviewSeatSourceMinimum},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task, job := newPlanReviewSeatsTask(t, ctx, st, "plan-seats-round", true, 2)
			cfg := &config.Config{Workspace: "demo", MaxBounces: 2, WorkOrderQueueTimeout: time.Hour, Repos: []config.Repo{{Name: "api", Base: "main"}},
				Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP, TimeoutText: "45m"}}}}
			d := New(st, cfg, nil)
			version, err := d.ApplyExternalPlan(ctx, task, job, pipeline.StructuredPlan{Markdown: planReviewSeatsMarkdown, Decomposition: []pipeline.DecompositionItem{}, ReviewSeats: tc.planned}, "codex", "gpt")
			if err != nil {
				t.Fatal(err)
			}
			current, _ := st.GetTask(ctx, task.ID)
			if err = d.HandleIntervention(ctx, current, job, core.Intervention{TaskID: task.ID, JobID: job.ID, Action: core.InterventionApprove, ReasonCode: "plan-approved"}); err != nil {
				t.Fatal(err)
			}
			requirePlanVersionSeats(t, ctx, st, task.ID, version.Version, tc.planned, true)
			// Implementation and its submission are outside RS-3; the task
			// starts implementation and advances to review as a submission would.
			if err = d.transition(ctx, task.ID, core.TaskDispatchStart, "", ""); err != nil {
				t.Fatal(err)
			}
			if err = d.transition(ctx, task.ID, core.TaskStageAdvance, core.StageReview, ""); err != nil {
				t.Fatal(err)
			}
			current, _ = st.GetTask(ctx, task.ID)
			floor, err := ResolveTaskReviewRoundSeats(ctx, st, cfg, current, 1)
			if err != nil || floor.FloorSeatCount != *tc.planned || floor.MinimumSeatCount != 2 || floor.SeatCount != tc.count || floor.Source != tc.source {
				t.Fatalf("resolution=%+v err=%v", floor, err)
			}
			if err = d.DispatchNow(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			orders, err := st.ListTaskWorkOrders(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			seat := 0
			for _, order := range orders {
				if order.Stage != core.StageReview {
					continue
				}
				seat++
				if order.ReviewRound != 1 || order.ReviewSeat != seat || order.ID != fmt.Sprintf("%s-review-1-seat-%d", task.ID, seat) {
					t.Fatalf("review seat %d=%+v", seat, order)
				}
			}
			records, err := st.ListReviewRoundSeats(ctx, task.ID)
			if err != nil || seat != tc.count || len(records) != 1 || records[0].ReviewRound != 1 || records[0].SeatCount != tc.count || records[0].SeatCountSource != tc.source || records[0].MinimumSeatCount != 2 {
				t.Fatalf("review seats=%d records=%+v err=%v", seat, records, err)
			}
			events, err := st.ListEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			created := 0
			for _, event := range events {
				if event.Kind != "review.round_created" {
					continue
				}
				created++
				var payload struct {
					ReviewRound      int    `json:"review_round"`
					SeatCount        int    `json:"seat_count"`
					SeatCountSource  string `json:"seat_count_source"`
					MinimumSeatCount int    `json:"minimum_seat_count"`
				}
				if err = json.Unmarshal(event.Payload, &payload); err != nil || payload.ReviewRound != 1 || payload.SeatCount != tc.count || payload.SeatCountSource != tc.source || payload.MinimumSeatCount != 2 {
					t.Fatalf("review.round_created=%s err=%v", event.Payload, err)
				}
			}
			if created != 1 {
				t.Fatalf("review.round_created events=%d", created)
			}
		})
	}
}

func TestMergeAndRecoveryInterventionsDoNotRequireSpec(t *testing.T) {
	for _, command := range []core.TaskCommand{core.TaskGateMerge, core.TaskJobFail, core.TaskStageBounceLimit} {
		for _, action := range []core.InterventionAction{core.InterventionApprove, core.InterventionRedirect} {
			t.Run(string(command)+"/"+string(action), func(t *testing.T) {
				ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
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
				err = d.HandleIntervention(ctx, task, job, intervention)
				if command == core.TaskJobFail && action == core.InterventionApprove {
					// No review approved the state a job failure holds, so
					// its approve is refused and the task stays at the gate
					// (component-next-action invariant 7).
					current, getErr := st.GetTask(ctx, task.ID)
					if !errors.Is(err, store.ErrApproveWithoutReview) || getErr != nil || current.State != core.TaskAwaiting || current.ApprovedHeadSHA != "" {
						t.Fatalf("job-failure approve err=%v task=%+v", err, current)
					}
					return
				}
				if err != nil {
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
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
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
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
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
	results []inprocess.Result
	inputs  []inprocess.Input
	next    int
}

func (agent *sequenceAgent) Run(_ context.Context, _ string, input inprocess.Input) (inprocess.Result, error) {
	agent.inputs = append(agent.inputs, input)
	if len(agent.results) > 0 {
		result := agent.results[agent.next]
		agent.next++
		return result, nil
	}
	output := agent.outputs[agent.next]
	agent.next++
	return inprocess.Result{Output: output, TokensIn: 20, TokensOut: 10}, nil
}

func TestSpecStructuredValidationFeedsPreciseErrorIntoRetry(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "structured-spec-retry", Workspace: "demo", Repo: "api", Title: "Retry semantics", PolicyVersion: 1, SpecApproval: true, State: core.TaskQueued, NextStage: core.StageSpec, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	invalid := structuredSpecOutput("```conveyor:acceptance\n- id: AC-1\n```", "", "")
	agent := &sequenceAgent{outputs: []string{invalid, structuredSpecOutput("# Retry\n\n## Intent\nShip it.\n\n## Non-goals\nNone.", "Ship it", "")}}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 3, Routing: config.Routing{Stages: map[string]config.StageRoute{"spec": {Execution: config.ExecutionInProcess, Timeout: time.Minute}}}}
	dispatcher := New(st, cfg, agent)
	dispatcher.Pack = bundle
	completeStageOutputForTest(t, store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), dispatcher, cfg, task, invalid)
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskQueued || current.NextStage != core.StageSpec {
		t.Fatalf("after invalid output task=%+v err=%v", current, err)
	}
	retry, err := dispatcher.buildStageInput(store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), cfg, core.StageSpec, current)
	if err != nil {
		t.Fatal(err)
	}
	if len(agent.inputs) != 0 || !strings.Contains(retry.Prompt, "# Previous output rejected") || !strings.Contains(retry.Prompt, `plans cannot contain conveyor: machine fences`) {
		t.Fatalf("retry prompt did not preserve precise validation error (model calls %d):\n%s", len(agent.inputs), retry.Prompt)
	}
}

func structuredSpecOutput(markdown, criterion, ref string) string {
	value := map[string]any{
		"markdown":      "## Approach\n" + markdown + "\n\n## Files touched\n- internal/example.go\n\n## Ordering\n1. Implement safely.\n\n## Risks\n- Preserve lifecycle semantics.\n\n## Done criteria\n- Repository checks pass.",
		"decomposition": []any{},
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestInProcessUsageRecordsTokensWithoutCost(t *testing.T) {
	t.Parallel()
	ctx := store.WithActor(context.Background(), store.SystemActor())
	st := store.NewMemory()
	task := core.Task{ID: "high-usage", Workspace: "demo", Repo: "api", Title: "Small fix", Level: core.L0, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &sequenceAgent{outputs: []string{"```conveyor:triage\n{\"class\":\"chore\",\"route\":\"proceed\",\"summary\":\"Ready.\"}\n```"}}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "gpt-newly-configured", Timeout: time.Minute}}}
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
	ctx := store.WithActor(context.Background(), store.SystemActor())
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
		"```conveyor:triage\n{\"class\":\"feature\",\"route\":\"proceed\",\"summary\":\"Needs an accepted contract.\"}\n```",
		structuredSpecOutput("# Audit export\n\n## Intent\nAdd the export.\n\n## Non-goals\nNo unrelated formats.", "Export tests pass", "./..."),
	}}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "gpt-5.4", Timeout: time.Minute}, Stages: map[string]config.StageRoute{
		"spec":      {Execution: config.ExecutionInProcess, Timeout: time.Minute},
		"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour},
		"review":    {Execution: config.ExecutionMCP, Timeout: time.Hour},
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
	if len(agent.inputs) != 1 {
		t.Fatalf("model calls = %d, want triage only", len(agent.inputs))
	}
	completeStageOutputForTest(t, store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), dispatcher, cfg, inFlightSpec, agent.outputs[1])
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskAwaiting || current.RecoveryStage != core.StageImplement {
		t.Fatalf("after spec task=%+v err=%v", current, err)
	}
	spec, ok, err := st.GetLatestSpecVersion(ctx, task.ID)
	if err != nil || !ok || spec.AcceptanceCount != 0 || spec.Approved {
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

func TestHumanTriageRouteEntersInvalidOutputBounce(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "policy-spec", Workspace: "demo", Repo: "api", Title: "Policy", Hold: true, PolicyVersion: 1, SpecApproval: true, MergeApproval: false, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &sequenceAgent{outputs: []string{"```conveyor:triage\n{\"class\":\"feature\",\"route\":\"human\",\"summary\":\"Needs review.\"}\n```"}}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "gpt", Timeout: time.Minute}, Stages: map[string]config.StageRoute{"spec": {Execution: config.ExecutionInProcess, Timeout: time.Minute}, "implement": {Execution: config.ExecutionMCP, Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
	d := New(st, cfg, agent)
	d.Pack = bundle
	if err = d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.State != core.TaskQueued || current.NextStage != core.StageTriage {
		t.Fatalf("after triage=%+v", current)
	}
	if count, err := st.CountEvents(ctx, task.ID, "triage.output_invalid"); err != nil || count != 1 {
		t.Fatalf("invalid-output events=%d err=%v", count, err)
	}
}

// TestTriageInvalidOutputAtBounceLimitFailsJob proves that triage output that
// stays invalid through the bounce limit appends pipeline.bounce_limit, fails
// the job with an error naming the invalid output and its last validation
// error, and waits at the job-failure gate, never a review gate. The Retry
// redirect, recorded as a human intervention as POST /v1/tasks/{id}/review
// records it, gives triage a fresh bounce window; a redirect without that
// record would fail again on the next invalid output (component-triage
// invariant 26; DEC-110(2); req-operator-next-action AC-1.2, AC-1.8).
func TestTriageInvalidOutputAtBounceLimitFailsJob(t *testing.T) {
	for _, tc := range []struct {
		name             string
		recordRetry      bool
		wantAfterRetry   core.TaskState
		wantInvalidCount int
	}{
		{name: "retry recorded as a human intervention", recordRetry: true, wantAfterRetry: core.TaskQueued, wantInvalidCount: 3},
		{name: "redirect without an intervention record", recordRetry: false, wantAfterRetry: core.TaskAwaiting, wantInvalidCount: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := core.Task{ID: "triage-invalid", Workspace: "demo", Repo: "api", Title: "Triage invalid", Hold: true, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			bundle, err := pack.Load("../../pack")
			if err != nil {
				t.Fatal(err)
			}
			invalid := "```conveyor:triage\n{\"class\":\"feature\",\"route\":\"proceed\",\"summary\":\"\"}\n```"
			agent := &sequenceAgent{outputs: []string{invalid, invalid, invalid}}
			cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "gpt", Timeout: time.Minute}, Stages: map[string]config.StageRoute{"spec": {Execution: config.ExecutionInProcess, Timeout: time.Minute}, "implement": {Execution: config.ExecutionMCP, Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
			d := New(st, cfg, agent)
			d.Pack = bundle
			if err = d.DispatchNow(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskQueued || current.NextStage != core.StageTriage {
				t.Fatalf("after the first invalid output task=%+v", current)
			}
			if err = d.DispatchNow(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			failed, err := st.GetTask(ctx, task.ID)
			if err != nil || failed.State != core.TaskAwaiting || failed.GateKind != core.GateKindJobFailure || failed.RecoveryStage != core.StageTriage || failed.NextStage != "" {
				t.Fatalf("at the bounce limit task=%+v err=%v, want a triage job failure", failed, err)
			}
			events, err := st.ListEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			var limit, failure map[string]any
			for _, event := range events {
				switch event.Kind {
				case "pipeline.bounce_limit":
					_ = json.Unmarshal(event.Payload, &limit)
				case "job.failed":
					_ = json.Unmarshal(event.Payload, &failure)
				}
			}
			if limit["source"] != "triage.output_invalid" || limit["window"] != float64(2) {
				t.Fatalf("bounce limit=%v", limit)
			}
			message, _ := failure["error"].(string)
			diagnostic, _ := failure["diagnostic"].(map[string]any)
			if !strings.HasPrefix(message, "Triage output stayed invalid after 2 attempts, so triage has no outcome. Last validation error: ") ||
				!strings.HasSuffix(message, "triage summary is required") || failure["source"] != "triage.output_invalid" || diagnostic["phase"] != TriageOutputValidationPhase {
				t.Fatalf("job failure=%v", failure)
			}
			latest, ok, err := st.GetLatestJob(ctx, task.ID)
			if err != nil || !ok || latest.Stage != core.StageTriage || latest.State != core.JobDone {
				t.Fatalf("latest job=%+v ok=%t err=%v, want the completed triage job whose output stayed invalid", latest, ok, err)
			}
			action, err := store.ReadNextAction(ctx, st, task.ID, store.NextActionOptions{})
			if err != nil || action.Kind != nextaction.KindJobFailure || action.Label != "Retry triage" || action.Summary != "Triage returned invalid output until the bounce limit, so it has no outcome." {
				t.Fatalf("next action=%+v err=%v", action, err)
			}
			for _, text := range []string{action.Label, action.Summary, action.Question, action.IfIgnored} {
				if strings.Contains(strings.ToLower(text), "review") {
					t.Fatalf("triage job failure names review: %q", text)
				}
			}
			// The Retry the dashboard's triage recovery card posts: a redirect
			// that returns to the recorded recovery stage.
			retry := core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionRedirect, ReasonCode: "changes-requested", Comment: "Retry triage."}
			if err = d.HandleIntervention(ctx, failed, latest, retry); err != nil {
				t.Fatal(err)
			}
			if tc.recordRetry {
				operator := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "operator", Role: core.ActorHuman}), "demo")
				if err = st.CreateIntervention(operator, retry); err != nil {
					t.Fatal(err)
				}
			}
			if current, _ := st.GetTask(ctx, task.ID); current.State != core.TaskQueued || current.NextStage != core.StageTriage {
				t.Fatalf("after retry task=%+v", current)
			}
			if err = d.DispatchNow(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			after, err := st.GetTask(ctx, task.ID)
			if err != nil || after.State != tc.wantAfterRetry {
				t.Fatalf("after one more invalid output task=%+v err=%v, want %s", after, err, tc.wantAfterRetry)
			}
			if count, _ := st.CountEvents(ctx, task.ID, "triage.output_invalid"); count != tc.wantInvalidCount {
				t.Fatalf("invalid-output events=%d want %d", count, tc.wantInvalidCount)
			}
			if after.State == core.TaskQueued && after.NextStage != core.StageTriage {
				t.Fatalf("a fresh window bounces back to triage: %+v", after)
			}
		})
	}
}

func TestTriageProceedSelectsNextStageFromPolicyAlone(t *testing.T) {
	for _, tc := range []struct {
		name          string
		policyVersion int
		specApproval  bool
		level         core.EscalationLevel
		want          core.Stage
	}{
		{name: "versioned spec on", policyVersion: 1, specApproval: true, want: core.StageSpec},
		{name: "versioned spec off", policyVersion: 1, specApproval: false, want: core.StageImplement},
		{name: "legacy L2", level: core.L2, want: core.StageSpec},
		{name: "legacy L3", level: core.L3, want: core.StageSpec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := core.Task{ID: "triage-policy", Workspace: "demo", Repo: "api", PolicyVersion: tc.policyVersion, SpecApproval: tc.specApproval, Level: tc.level, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			bundle, err := pack.Load("../../pack")
			if err != nil {
				t.Fatal(err)
			}
			agent := &sequenceAgent{outputs: []string{"```conveyor:triage\n{\"class\":\"feature\",\"route\":\"proceed\",\"summary\":\"Ready.\"}\n```"}}
			d := New(st, &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "gpt", Timeout: time.Minute}}}, agent)
			d.Pack = bundle
			if err = d.DispatchNow(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			current, err := st.GetTask(ctx, task.ID)
			if err != nil || current.State != core.TaskQueued || current.NextStage != tc.want {
				t.Fatalf("task=%+v err=%v; want queued at %s", current, err, tc.want)
			}
		})
	}
}

func TestTriageParkRecordsReasonAndRemainsRecoverable(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "triage-park", Workspace: "demo", Repo: "api", PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	agent := &sequenceAgent{outputs: []string{"```conveyor:triage\n{\"class\":\"chore\",\"route\":\"parked\",\"summary\":\"The task targets a different repository.\"}\n```"}}
	d := New(st, &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Triage: config.TriageRoute{Model: "gpt", Timeout: time.Minute}}}, agent)
	d.Pack = bundle
	if err = d.DispatchNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskParked {
		t.Fatalf("parked task=%+v err=%v", current, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundReason := false
	foundToolCount := false
	for _, event := range events {
		if event.Kind == "triage.completed" && strings.Contains(string(event.Payload), "different repository") {
			foundReason = true
			var payload map[string]any
			if json.Unmarshal(event.Payload, &payload) == nil && payload["tool_calls_executed"] == float64(0) {
				foundToolCount = true
			}
		}
	}
	if !foundReason || !foundToolCount {
		t.Fatalf("triage completion did not retain parking reason: %+v", events)
	}
	outcome, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskRecover, NextStage: core.StageTriage, ProjectStages: true})
	if err != nil || outcome.Task.State != core.TaskQueued || outcome.Task.NextStage != core.StageTriage {
		t.Fatalf("recovery outcome=%+v err=%v", outcome, err)
	}
}

func TestHistoricalRouteHumanAwaitingTaskCanRejectOrCancelButNotApprove(t *testing.T) {
	for _, action := range []core.InterventionAction{core.InterventionReject, core.InterventionCancel, core.InterventionApprove} {
		t.Run(string(action), func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := core.Task{ID: "legacy-route-human-" + string(action), Workspace: "demo", Repo: "api", State: core.TaskAwaiting, RecoveryStage: core.StageTriage, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-triage-1", TaskID: task.ID, Stage: core.StageTriage, State: core.JobDone}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: job.ID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{
				"from": core.TaskRunning, "to": core.TaskAwaiting, "command": "triage.route_human",
			})}); err != nil {
				t.Fatal(err)
			}
			d := New(st, &config.Config{Workspace: "demo"}, nil)
			intervention := core.Intervention{TaskID: task.ID, JobID: job.ID, Action: action, ReasonCode: "operator-action"}
			err := d.HandleIntervention(ctx, task, job, intervention)
			current, getErr := st.GetTask(ctx, task.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			switch action {
			case core.InterventionReject, core.InterventionCancel:
				if err != nil || current.State != core.TaskClosed {
					t.Fatalf("action=%s task=%+v err=%v", action, current, err)
				}
			case core.InterventionApprove:
				// An awaiting task without a recorded gate kind states
				// job_failure, so its approve is refused before any head is
				// read (component-next-action invariant 7).
				if !errors.Is(err, store.ErrApproveWithoutReview) || current.State != core.TaskAwaiting {
					t.Fatalf("approve task=%+v err=%v", current, err)
				}
			}
		})
	}
}

func TestRequestedSpecChangesRequireRevisedApprovalBeforeImplementation(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
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
		"spec":      {Execution: config.ExecutionInProcess, Timeout: time.Minute},
		"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour},
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
	completeStageOutputForTest(t, store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem}), d, cfg, current, agent.outputs[0])
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
	ctx := store.WithActor(context.Background(), store.SystemActor())
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
	if err := d.ApplyExternalReviewPinned(ctx, task, core.Job{ID: "review-1", TaskID: task.ID, Stage: core.StageReview, ModelTier: "review"}, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "missing coverage", Feedback: "add the test"}, "review-1", "review-session", "review", []core.ServedRequirementContext{}, emptyGovernanceAuthority()); err != nil {
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

func TestReviewPathsProjectOnlyEligibleEvidenceSupport(t *testing.T) {
	for _, reviewPath := range []string{"in-process", "external-mcp"} {
		t.Run(reviewPath, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
			st := store.NewMemory()
			task := core.Task{ID: "evidence-" + reviewPath, Workspace: "test", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "review"}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			eligible, err := st.CreateArtifact(ctx, core.Artifact{Name: "evidence.png", ContentType: "image/png", Role: core.ArtifactRoleVerificationEvidence, TaskID: task.ID}, testimage.PNG("png"))
			if err != nil {
				t.Fatal(err)
			}
			ineligible, err := st.CreateArtifact(ctx, core.Artifact{Name: "context.txt", ContentType: "text/plain", Role: core.ArtifactRoleTaskContext, TaskID: task.ID}, []byte("context"))
			if err != nil {
				t.Fatal(err)
			}
			d := New(st, &config.Config{Workspace: "test", MaxBounces: 2}, nil)
			d.DisableMemoryQueueForTest()
			review := pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: reviewPath, Feedback: "revise"}
			if reviewPath == "external-mcp" {
				err = d.ApplyExternalReviewPinned(ctx, task, job, review, job.ID, "review-session", "review", []core.ServedRequirementContext{}, emptyGovernanceAuthority())
			} else {
				err = d.applyReview(ctx, &config.Config{Workspace: "test", MaxBounces: 2}, task, job, review, "codex", job.ID, "review-session", "review", nil, nil, false, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			links, err := st.ListLineageLinks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			foundEligible := false
			for _, link := range links {
				if link.Kind != "supports" {
					continue
				}
				if link.SrcID == ineligible.ID {
					t.Fatalf("ineligible artifact gained supports edge: %+v", link)
				}
				if link.SrcID == eligible.ID {
					foundEligible = true
				}
			}
			if !foundEligible {
				t.Fatalf("eligible evidence missing supports edge: %+v", links)
			}
		})
	}
}

func TestBounceWindowResetsAfterHumanIntervention(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
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

	// A human redirect grants a fresh window: the next bounce
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
	ctx := store.WithActor(context.Background(), store.SystemActor())
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
	if err := d.ApplyExternalReviewPinned(ctx, task, job, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "stop", Feedback: "human help"}, job.ID, "review-session", "review", []core.ServedRequirementContext{}, emptyGovernanceAuthority()); err != nil {
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
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
	st := store.NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "citation-bounce", Workspace: "test", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]string{"base_sha": "citation-base", "head_sha": "citation-head"})}); err != nil {
		t.Fatal(err)
	}
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-citation", Title: "Citation contract"}, core.RequirementVersion{
		Content:    "# Review cites confirmed intent.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Review cites confirmed intent.\n```",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Review cites confirmed intent."}},
		Origin:     core.RequirementOriginChat, OriginSessionID: "planning-citation",
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
	served, err := store.ServedRequirementsForTask(ctx, st, task.ID, 256)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.ApplyExternalReviewPinned(ctx, task, job, result, job.ID, "external-session", "review", served.Requirements, emptyGovernanceAuthority()); err == nil || !strings.Contains(err.Error(), "assessment is required") {
		t.Fatalf("external review error=%v, want retryable validation error", err)
	}
	if count, _ := st.CountEvents(ctx, task.ID, "review.output_invalid"); count != 0 {
		t.Fatalf("external validation created %d in-process bounce events", count)
	}
	output := "```conveyor:review\n{\"verdict\":\"approve\",\"reason_code\":\"approved\",\"summary\":\"looks good\",\"feedback\":\"\"}\n```"
	if err = d.completeOutput(ctx, cfg, task, job, output, "in-process", 0); err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetTask(ctx, task.ID)
	if err != nil || updated.State != core.TaskQueued || updated.NextStage != core.StageReview {
		t.Fatalf("citation bounce task=%+v err=%v", updated, err)
	}
	if count, _ := st.CountEvents(ctx, task.ID, "review.output_invalid"); count != 1 {
		t.Fatalf("in-process citation validation bounce events=%d, want 1", count)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	d.Pack = bundle
	d.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) { return "", nil }
	input, err := d.buildStageInput(ctx, cfg, core.StageReview, updated)
	if err != nil || !strings.Contains(input.Prompt, "requirement_citations assessment is required") {
		t.Fatalf("citation retry prompt error=%v prompt=%s", err, input.Prompt)
	}
	if _, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
		t.Fatal(err)
	}
	updated, err = st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.completeOutput(ctx, cfg, updated, job, output, "in-process", 0); err != nil {
		t.Fatal(err)
	}
	updated, err = st.GetTask(ctx, task.ID)
	if err != nil || updated.State != core.TaskAwaiting || updated.RecoveryStage != core.StageReview {
		t.Fatalf("citation bounce-limit task=%+v err=%v", updated, err)
	}
}

func TestValidateReviewCitationsCoversEveryAssessmentBranch(t *testing.T) {
	served := []core.ServedRequirementContext{{ID: "req-runtime", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "Pinned criterion"}}}}}}
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
		{name: "unlinked unknown finding", value: &core.RequirementCitationAssessment{UnknownIDs: []string{"REQ-X"}}, want: "findings must be empty"},
		{name: "unlinked unserved finding", value: &core.RequirementCitationAssessment{UnservedIDs: []string{"REQ-2"}}, want: "findings must be empty"},
		{name: "unlinked conflict finding", value: &core.RequirementCitationAssessment{Conflicts: []string{"REQ-3 changed"}}, want: "findings must be empty"},
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
	t.Run("pinned requirement and acceptance criterion are accepted", func(t *testing.T) {
		result := pipeline.Review{RequirementCitations: &core.RequirementCitationAssessment{Applicable: true, CitedIDs: []string{"REQ-1", "AC-1.1"}}}
		if err := validateReviewCitations(&result, served); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("id absent from pinned version is rejected", func(t *testing.T) {
		result := pipeline.Review{RequirementCitations: &core.RequirementCitationAssessment{Applicable: true, CitedIDs: []string{"AC-1.2"}}}
		if err := validateReviewCitations(&result, served); err == nil || !strings.Contains(err.Error(), `cited id "AC-1.2" is not present in the confirmed served requirement version`) {
			t.Fatalf("validation error=%v", err)
		}
	})
	// Regression for the first live citation-validated review (task
	// 260805-98aa4c): the reviewer filed pinned-served ACs under unserved_ids,
	// repurposing the field as "served but not exercised by this diff".
	t.Run("served id filed under unserved is rejected", func(t *testing.T) {
		result := pipeline.Review{RequirementCitations: &core.RequirementCitationAssessment{Applicable: true, CitedIDs: []string{"REQ-1"}, UnservedIDs: []string{"AC-1.1"}}}
		if err := validateReviewCitations(&result, served); err == nil || !strings.Contains(err.Error(), `unserved_ids entry "AC-1.1" is present in the pinned served requirement version`) {
			t.Fatalf("validation error=%v", err)
		}
	})
	t.Run("served id filed under unknown is rejected", func(t *testing.T) {
		result := pipeline.Review{RequirementCitations: &core.RequirementCitationAssessment{Applicable: true, UnknownIDs: []string{"AC-1.1"}}}
		if err := validateReviewCitations(&result, served); err == nil || !strings.Contains(err.Error(), `unknown_ids entry "AC-1.1" is present in the pinned served requirement version`) {
			t.Fatalf("validation error=%v", err)
		}
	})
	t.Run("finding lists must be disjoint", func(t *testing.T) {
		result := pipeline.Review{RequirementCitations: &core.RequirementCitationAssessment{Applicable: true, UnknownIDs: []string{"REQ-9"}, UnservedIDs: []string{"REQ-9"}}}
		if err := validateReviewCitations(&result, served); err == nil || !strings.Contains(err.Error(), `appears in both unknown_ids and unserved_ids`) {
			t.Fatalf("validation error=%v", err)
		}
	})
	t.Run("genuinely unserved and unknown ids are accepted", func(t *testing.T) {
		result := pipeline.Review{RequirementCitations: &core.RequirementCitationAssessment{Applicable: true, CitedIDs: []string{"AC-1.1"}, UnknownIDs: []string{"REQ-404"}, UnservedIDs: []string{"REQ-9"}}}
		if err := validateReviewCitations(&result, served); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLegacyDoneHeadingPromptAndValidatorAgreeNoExecutionPlan(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	st := store.NewMemory()
	task := core.Task{ID: "legacy-done-heading", Workspace: "test", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]string{"base_sha": "legacy-base", "head_sha": "legacy-head"})}); err != nil {
		t.Fatal(err)
	}
	legacy := "## Definition of done\n\n- Legacy checks pass.\n\n```conveyor:spec\n{\"acceptance\":[]}\n```"
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: legacy})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, task.ID, spec.Version); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending, ModelTier: "reviewer"}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2}
	d := New(st, cfg, nil)
	d.Pack = bundle
	d.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) { return "", nil }
	d.DisableMemoryQueueForTest()
	input, err := d.buildStageInput(ctx, cfg, core.StageReview, task)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(input.Prompt, "No execution plan is available. The task description is the statement of done:") ||
		!strings.Contains(input.Prompt, "Record done_criteria_coverage with applicable=false") ||
		strings.Contains(input.Prompt, "Each list entry must be the verbatim-trimmed text of one criterion") {
		t.Fatalf("legacy prompt applicability diverged: %s", input.Prompt)
	}
	designApplicable, decisionCitable := false, false
	review := pipeline.Review{
		Verdict: "approve", ReasonCode: "approved", Summary: "legacy contract accepted",
		RequirementCitations: &core.RequirementCitationAssessment{CitedIDs: []string{}, UnknownIDs: []string{}, UnservedIDs: []string{}, Conflicts: []string{}},
		DoneCriteriaCoverage: &core.DoneCriteriaAssessment{Summary: "No execution plan is available", Satisfied: []string{}, Unsatisfied: []string{}, Unverified: []string{}, Conflicts: []string{}},
		GovernanceAssessment: &core.GovernanceAssessment{DesignApplicable: &designApplicable, DecisionCitable: &decisionCitable, CitedIDs: []string{}, UnknownIDs: []string{}, UngovernedIDs: []string{}, SupersededIDs: []string{}, Conflicts: []string{}},
	}
	if err = d.ApplyExternalReviewPinned(ctx, task, job, review, job.ID, "review-session", "reviewer", []core.ServedRequirementContext{}, emptyGovernanceAuthority()); err != nil {
		t.Fatal(err)
	}
}

func TestInProcessReviewUsesTaskScopedGovernanceForPromptAndValidation(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	st := store.NewMemory()
	design, attached, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "DESIGN-attached-inprocess", Title: "Attached authority", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Attached v1\n\n```conveyor:governs\n- repo: app\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, attached.Version); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "inprocess-task-governance", Workspace: "test", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err = st.CreateTaskWithDependenciesAndContext(ctx, task, nil, store.TaskContextInput{DesignIDs: []string{design.ID}}); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]string{"base_sha": "governance-base", "head_sha": "governance-head"})}); err != nil {
		t.Fatal(err)
	}
	newer, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: "# Newer v2\n\n```conveyor:governs\n- repo: app\n  paths:\n    - internal/v2/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, newer.Version, attached.Version); err != nil {
		t.Fatal(err)
	}
	pending, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "DESIGN-pending-inprocess", Title: "Pending authority", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Pending\n\n```conveyor:governs\n- repo: app\n  paths:\n    - internal/dispatch/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: task.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "reviewer", StartedAt: time.Now().UTC()}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2}
	d := New(st, cfg, nil)
	d.Pack = bundle
	d.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) { return "", nil }
	d.DisableMemoryQueueForTest()
	input, err := d.buildStageInput(ctx, cfg, core.StageReview, task)
	if err != nil {
		t.Fatal(err)
	}
	if input.GovernanceSnapshot == nil || len(input.GovernanceSnapshot.Designs) != 1 || input.GovernanceSnapshot.Designs[0].Version != attached.Version || !input.GovernanceSnapshot.Designs[0].PinnedAtAttachment || len(input.GovernanceSnapshot.PendingDesignProposals) != 0 {
		t.Fatalf("task-scoped in-process governance=%+v", input.GovernanceSnapshot)
	}
	for _, required := range []string{"older version pinned by the task attachment", "it stays binding", "no document read tools"} {
		if !strings.Contains(input.Prompt, required) {
			t.Fatalf("in-process prompt missing %q: %s", required, input.Prompt)
		}
	}
	if strings.Contains(input.Prompt, pending.ID) {
		t.Fatalf("in-process prompt carries the pending proposal %s: %s", pending.ID, input.Prompt)
	}
	// The prompt names documents; the pinned input keeps the bodies for validation.
	for _, designInput := range input.GovernanceSnapshot.Designs {
		if designInput.Content == "" || strings.Contains(input.Prompt, designInput.Content) {
			t.Fatalf("in-process prompt and snapshot disagree on the body of %s", designInput.ID)
		}
	}
	for _, forbidden := range []string{"search_documents", "get_document", "get_decision"} {
		if strings.Contains(input.Prompt, forbidden) {
			t.Fatalf("in-process prompt names read tool %q it does not have", forbidden)
		}
	}
	designApplicable, decisionCitable := true, false
	review := pipeline.Review{
		Verdict: "changes_requested", ReasonCode: "other", Summary: "exercise live task authority", Feedback: "retry",
		RequirementCitations: &core.RequirementCitationAssessment{CitedIDs: []string{}, UnknownIDs: []string{}, UnservedIDs: []string{}, Conflicts: []string{}},
		DoneCriteriaCoverage: &core.DoneCriteriaAssessment{Summary: "task fallback", Satisfied: []string{}, Unsatisfied: []string{}, Unverified: []string{}, Conflicts: []string{}},
		GovernanceAssessment: &core.GovernanceAssessment{DesignApplicable: &designApplicable, DecisionCitable: &decisionCitable, CitedIDs: []string{design.ID}, UnknownIDs: []string{}, UngovernedIDs: []string{}, SupersededIDs: []string{}, Conflicts: []string{}},
	}
	if err = d.applyReview(ctx, cfg, task, job, review, "in-process", job.ID, "", job.ModelTier, nil, nil, false, nil); err != nil {
		t.Fatalf("task-scoped live governance validation failed: %v", err)
	}
}

func TestExternalReviewUsesPinnedRequirementVersionAfterConfirmationMoves(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	st := store.NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "pinned-review-race", Workspace: "test", Repo: "app", Level: core.L0, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	requirement, first, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-pinned", Title: "Pinned authority"}, core.RequirementVersion{
		Content: "# Pinned authority", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Stable statement", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "Retired later"}}}},
		Origin: core.RequirementOriginChat, OriginSessionID: "session-first",
	})
	if err != nil {
		t.Fatal(err)
	}
	human := store.WithActor(ctx, store.Actor{ID: "operator", Role: core.ActorHuman})
	if _, _, err = st.ConfirmRequirementVersion(human, requirement.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ProposeRequirementServes(ctx, task.ID, requirement.ID, core.RequirementServesPlanning, false); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConfirmRequirementServes(human, task.ID, requirement.ID); err != nil {
		t.Fatal(err)
	}
	pinned, err := store.ServedRequirementsForTask(ctx, st, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning, ModelTier: "reviewer", StartedAt: now}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, ServedRequirementSnapshot: append([]core.ServedRequirementContext{}, pinned.Requirements...)}
	if err = storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	second, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
		RequirementID: requirement.ID, Content: "# Revised authority", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Stable statement"}},
		Origin: core.RequirementOriginChat, OriginSessionID: "session-second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(human, requirement.ID, second.Version, first.Version); err != nil {
		t.Fatal(err)
	}
	d := New(st, &config.Config{Workspace: "test", MaxBounces: 2}, nil)
	d.DisableMemoryQueueForTest()
	review := pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "contract-faithful", RequirementCitations: &core.RequirementCitationAssessment{Applicable: true, CitedIDs: []string{"REQ-1", "AC-1.1"}}}
	if err = d.ApplyExternalReviewPinned(ctx, task, job, review, order.ID, "review-session", "reviewer", order.ServedRequirementSnapshot, emptyGovernanceAuthority()); err != nil {
		t.Fatalf("pinned verdict rejected after confirmation moved: %v", err)
	}
}

func TestReviewAcceptanceFailureRollsBackAndRetryCommitsOnce(t *testing.T) {
	t.Parallel()
	ctx := store.WithActor(context.Background(), store.SystemActor())
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
	err := d.ApplyExternalReviewPinned(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review", []core.ServedRequirementContext{}, emptyGovernanceAuthority())
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
	if err = d.ApplyExternalReviewPinned(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review", []core.ServedRequirementContext{}, emptyGovernanceAuthority()); err != nil {
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
	ctx := store.WithActor(context.Background(), store.SystemActor())
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
	if err := d.ApplyExternalReviewPinned(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review", []core.ServedRequirementContext{}, emptyGovernanceAuthority()); err != nil {
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
	ctx := store.WithActor(context.Background(), store.SystemActor())
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
	if err := d.ApplyExternalReviewPinned(ctx, task, job, pipeline.Review{Verdict: "changes_requested", ReasonCode: "tests", Summary: "retry", Feedback: "fix it"}, job.ID, "review-session", "review", []core.ServedRequirementContext{}, emptyGovernanceAuthority()); err != nil {
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
			ctx := store.WithActor(context.Background(), store.SystemActor())
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
			if err := d.ApplyExternalReviewPinned(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review", []core.ServedRequirementContext{}, emptyGovernanceAuthority()); err != nil {
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
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
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
			if err := d.ApplyExternalReviewPinned(ctx, task, job, pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "passes"}, job.ID, "review-session", "review-model", []core.ServedRequirementContext{}, emptyGovernanceAuthority()); err != nil {
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
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
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
				{ID: jobs[0].ID, TaskID: task.ID, JobID: jobs[0].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, RequiredModel: "gpt-review", RequiredHarness: "codex", ServedRequirementSnapshot: []core.ServedRequirementContext{}, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now},
				{ID: jobs[1].ID, TaskID: task.ID, JobID: jobs[1].ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 2, RequiredModel: "claude-review", RequiredHarness: "claude", ServedRequirementSnapshot: []core.ServedRequirementContext{}, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now},
			}
			if err := storetest.For(st).CreateReviewRound(ctx, task.ID, jobs, orders); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Workspace: "test", MaxBounces: 2, Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}
			firstDispatcher := New(st, cfg, nil)
			firstDispatcher.DisableMemoryQueueForTest()
			if err := firstDispatcher.ApplyExternalReviewPinned(ctx, task, jobs[0], pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "seat one passes", Feedback: "seat one evidence"}, orders[0].ID, "review-session-1", "gpt-review", orders[0].ServedRequirementSnapshot, emptyGovernanceAuthority()); err != nil {
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
			if err := restarted.ApplyExternalReviewPinned(ctx, task, jobs[1], pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "seat two passes", Feedback: "seat two evidence"}, orders[1].ID, "review-session-2", "claude-review", orders[1].ServedRequirementSnapshot, emptyGovernanceAuthority()); err != nil {
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

func TestInProcessUnresolvedApprovalUsesInvalidVerdictRepair(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "test")
	st := store.NewMemory()
	task := core.Task{ID: "done-criteria-repair", Workspace: "test", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: storetest.ReviewDoneCriteriaPlan})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveSpecVersion(ctx, task.ID, spec.Version); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}
	if err = st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", MaxBounces: 2}
	d := New(st, cfg, nil)
	d.DisableMemoryQueueForTest()
	review := pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "focused checks pass", DoneCriteriaCoverage: &core.DoneCriteriaAssessment{Applicable: true, Summary: "required aggregate unverified", Unverified: []string{storetest.PR907MandatoryValidation}}}
	if err = d.completeOutput(ctx, cfg, task, job, composeReviewOutput(review), "in-process", 0); err != nil {
		t.Fatal(err)
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.State != core.TaskQueued || current.NextStage != core.StageReview || current.ApprovedHeadSHA != "" {
		t.Fatalf("repair task=%+v", current)
	}
	if count, _ := st.CountEvents(ctx, task.ID, "review.output_invalid"); count != 1 {
		t.Fatalf("repair events=%d", count)
	}
	for _, kind := range []string{"review.completed", "review.accepted", "review.publication_queued", "review.round_completed"} {
		if count, _ := st.CountEvents(ctx, task.ID, kind); count != 0 {
			t.Fatalf("invalid approval produced %s", kind)
		}
	}
}

// REQ-9/AC-9.5: exact-head evidence controls verdict admission, through invalid.
func TestDocumentationReviewAdmission(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	head := strings.Repeat("a", 40)
	task := core.Task{ID: "docs-review", Workspace: "demo", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: head, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	evidence := core.DocumentationGateEvidence{TaskID: task.ID, HeadSHA: head, BaseSHA: strings.Repeat("b", 40), PolicyJSON: json.RawMessage(`{"schema_version":1,"docs":[{"path":"docs/api.md","governs":["internal/**"]}],"rule":{"none_statement":"docs: none"}}`), ChangedPaths: []string{"internal/api.go", "docs/api.md"}}
	if err := st.RecordDocumentationGateEvidence(ctx, evidence); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "demo"}
	d := New(st, cfg, nil)
	valid := func() *core.DocumentationAssessment {
		return &core.DocumentationAssessment{Applicable: true, Summary: "docs cover API", UpdatedPaths: []string{"docs/api.md"}}
	}
	for _, tc := range []struct {
		name       string
		assessment *core.DocumentationAssessment
		head       string
	}{
		{"missing", nil, head},
		{"unresolved", &core.DocumentationAssessment{Applicable: true, Summary: "missing behavior", UpdatedPaths: []string{"docs/api.md"}, Unresolved: []string{"new error undocumented"}}, head},
		{"conflict", &core.DocumentationAssessment{Applicable: true, Summary: "conflict", UpdatedPaths: []string{"docs/api.md"}, Conflicts: []string{"docs contradict API"}}, head},
		{"wrong-path", &core.DocumentationAssessment{Applicable: true, Summary: "wrong path", UpdatedPaths: []string{"docs/wrong.md"}}, head},
		{"stale", valid(), strings.Repeat("c", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := st.ListEvents(ctx, task.ID)
			reviewTask := task
			reviewTask.ReviewedHeadSHA = tc.head
			result := pipeline.Review{Verdict: "approve", RequirementCitations: &core.RequirementCitationAssessment{Applicable: false}, DoneCriteriaCoverage: &core.DoneCriteriaAssessment{Applicable: false, Summary: "no approved plan"}, GovernanceAssessment: &core.GovernanceAssessment{DesignApplicable: boolRef(false), DecisionCitable: boolRef(false)}, DocumentationAssessment: tc.assessment}
			var refused error
			err := d.applyReview(ctx, cfg, reviewTask, core.Job{}, result, "reviewer", "", "", "", []core.ServedRequirementContext{}, &core.GovernanceSnapshot{}, false, func(err error) error { refused = err; return err })
			if err == nil || refused == nil || !strings.Contains(err.Error(), "documentation") {
				t.Fatalf("review admission err=%v callback=%v", err, refused)
			}
			after, _ := st.ListEvents(ctx, task.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("refused review wrote events")
			}
		})
	}
}

// REQ-9/AC-9.5–9.6: a tool-less reviewer can judge the exact-head admission facts.
func TestDocumentationInProcessContext(t *testing.T) {
	for _, mode := range []string{"current", "other-head", "absent"} {
		t.Run(mode, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
			task := core.Task{ID: "docs-input-" + mode, Workspace: "demo", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: head, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]string{"base_sha": base, "head_sha": head})}); err != nil {
				t.Fatal(err)
			}
			if mode != "absent" {
				evidenceHead := head
				if mode == "other-head" {
					evidenceHead = strings.Repeat("c", 40)
				}
				e := core.DocumentationGateEvidence{TaskID: task.ID, HeadSHA: evidenceHead, BaseSHA: base, PolicyJSON: json.RawMessage(`{"schema_version":1,"docs":[{"path":"docs/api.md","governs":["internal/**"]}],"rule":{"none_statement":"docs: none","reason_required":true}}`), ChangedPaths: []string{"internal/server.go"}, AgentAuthoredBody: "docs: none DOCUMENTATION-REASON"}
				if err := st.RecordDocumentationGateEvidence(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			bundle, err := pack.Load("../../pack")
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Workspace: "demo"}
			d := New(st, cfg, nil)
			d.Pack = bundle
			d.DisableMemoryQueueForTest()
			d.ReviewDiff = func(context.Context, *config.Config, core.Task) (string, error) {
				return "diff --git a/internal/server.go b/internal/server.go", nil
			}
			input, err := d.buildStageInput(ctx, cfg, core.StageReview, task)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "current" {
				if strings.Contains(input.Prompt, "# Exact-head documentation admission evidence") || strings.Contains(input.Prompt, "DOCUMENTATION-REASON") {
					t.Fatalf("%s leaked documentation evidence", mode)
				}
				return
			}
			for _, expected := range []string{"# Exact-head documentation admission evidence", "DOCUMENTATION-REASON", `"governs":["internal/**"]`, `"affected_documents":["docs/api.md"]`, "Documentation response contract", `"documentation_assessment":`, head, base, "untrusted observational data"} {
				if !strings.Contains(input.Prompt, expected) {
					t.Fatalf("in-process reviewer missing %q", expected)
				}
			}
		})
	}
}

// GitHub intake's per-issue lookup reads only tasks whose identifying text
// contains the issue source, never the workspace's full task list, and still
// matches the source exactly (component-runtime, GitHub intake).
func TestSourceTaskExistsReadsOnlyMatchingTasks(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "test")
	st := store.NewMemory()
	terminal := storetest.SeedTerminalHistory(t, st, ctx, "test", "app", 40, 5)
	taken := core.Task{ID: "taken-issue", Workspace: "test", Source: "github:acme/app#12", Repo: "app", BaseBranch: "main", Branch: "conveyor/taken-issue", State: core.TaskMerged, CreatedAt: time.Now()}
	open := core.Task{ID: "open-issue", Workspace: "test", Source: "github:acme/app#123", Repo: "app", BaseBranch: "main", Branch: "conveyor/open-issue", State: core.TaskQueued, CreatedAt: time.Now()}
	for _, task := range []core.Task{taken, open} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	recorder := storetest.NewReadRecorder(st)
	d := New(recorder, &config.Config{Workspace: "test", Repos: []config.Repo{{Name: "app", GitHub: "acme/app"}}}, nil)
	for source, want := range map[string]bool{"github:acme/app#12": true, "github:acme/app#123": true, "github:acme/app#1": false, "github:acme/app#9": false} {
		if got := d.sourceTaskExists(ctx, source); got != want {
			t.Fatalf("sourceTaskExists(%s)=%t, want %t", source, got, want)
		}
	}
	recorder.RequireNoTerminalReads(t, terminal, taken.ID)
}

// fullLedgerStore serves the complete ledger through ListCompactEvents: the
// read every switched dispatch fold made before the compact read existed
// (component-persistence, Compact task event reads).
type fullLedgerStore struct{ store.Store }

func (s fullLedgerStore) ListCompactEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	return s.Store.ListEvents(ctx, taskID)
}

// planRevisionGateEventCounter counts the event reads the gate issues.
type planRevisionGateEventCounter struct {
	store.Store
	reads int
}

func (s *planRevisionGateEventCounter) ListCompactEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	s.reads++
	return s.Store.ListCompactEvents(ctx, taskID)
}

// TestPendingPlanRevisionGateForTaskMatchesTheIDForm holds the loaded-task form
// to the ID form over a task carrying only ID and State, and reads events only
// for an awaiting task.
func TestPendingPlanRevisionGateForTaskMatchesTheIDForm(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := &planRevisionGateEventCounter{Store: store.NewMemory()}
	now := time.Now().UTC()
	gate := core.Event{Kind: "task.state_changed", At: now.Add(time.Second), Payload: core.JSONPayload(map[string]any{"command": core.TaskGatePlanRevision})}
	request := core.Event{Kind: "work_order.plan_revision_requested", At: now, Payload: core.JSONPayload(map[string]any{"work_order_id": "order", "attempt_id": "attempt", "rationale": "the API moved", "plan_version": 2})}
	for _, tc := range []struct {
		name        string
		state       core.TaskState
		events      []core.Event
		wantPending bool
		wantErr     error
		wantReads   int
	}{
		{name: "running", state: core.TaskRunning, events: []core.Event{request, gate}},
		{name: "awaiting another gate", state: core.TaskAwaiting, events: []core.Event{{Kind: "task.state_changed", At: now, Payload: core.JSONPayload(map[string]any{"command": core.TaskGateSpec})}}, wantReads: 1},
		{name: "pending", state: core.TaskAwaiting, events: []core.Event{request, gate}, wantPending: true, wantReads: 1},
		{name: "unreadable", state: core.TaskAwaiting, events: []core.Event{{Kind: "task.state_changed", At: now, Payload: json.RawMessage(`{"command":7}`)}}, wantPending: true, wantErr: store.ErrPlanRevisionGateUnreadable, wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "gate-" + strings.ReplaceAll(tc.name, " ", "-")
			if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: "demo", Repo: "conveyor", State: tc.state, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			for _, event := range tc.events {
				event.TaskID = id
				if err := st.AppendEvent(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			st.reads = 0
			got, pending, err := PendingPlanRevisionGateForTask(ctx, st, core.Task{ID: id, State: tc.state})
			if st.reads != tc.wantReads {
				t.Fatalf("event reads=%d want %d", st.reads, tc.wantReads)
			}
			want, wantPending, wantErr := PendingPlanRevisionGate(ctx, st, id)
			if got != want || pending != wantPending || fmt.Sprint(err) != fmt.Sprint(wantErr) {
				t.Fatalf("loaded form=%+v/%t/%v, ID form=%+v/%t/%v", got, pending, err, want, wantPending, wantErr)
			}
			if pending != tc.wantPending || !errors.Is(err, tc.wantErr) {
				t.Fatalf("pending=%t err=%v, want %t/%v", pending, err, tc.wantPending, tc.wantErr)
			}
			full, fullPending, fullErr := PendingPlanRevisionGateForTask(ctx, fullLedgerStore{st}, core.Task{ID: id, State: tc.state})
			if got != full || pending != fullPending || fmt.Sprint(err) != fmt.Sprint(fullErr) {
				t.Fatalf("compact events=%+v/%t/%v, full events=%+v/%t/%v", got, pending, err, full, fullPending, fullErr)
			}
			if tc.name == "pending" && got != (PlanRevisionGateContext{WorkOrderID: "order", AttemptID: "attempt", Rationale: "the API moved", PlanVersion: 2}) {
				t.Fatalf("gate context=%+v", got)
			}
		})
	}
}

// TestDispatchFoldsMatchOverCompactEvents runs the dispatcher's own event
// folds over a ledger holding historical work-order events with embedded
// snapshot bodies and compares each result between the complete and the
// compact read. The store conformance case proves the compact read on every
// backend; this test pins the folds outside the store package.
func TestDispatchFoldsMatchOverCompactEvents(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: "demo", Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageImplement, ApprovedHeadSHA: "approved-head", ReviewedHeadSHA: "reviewed-head", CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: id + "-implement-1", TaskID: id, Stage: core.StageImplement, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: job.Stage, State: core.WorkOrderQueued, QueueEnteredAt: task.CreatedAt, QueueDeadline: task.CreatedAt.Add(time.Hour), CreatedAt: task.CreatedAt}
	if _, err := storetest.CreateStageWorkOrder(ctx, st, job, order); err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("fold-worker"), SessionID: "fold-session", ClientToken: "fold-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	const marker = "dispatch-fold-snapshot-marker:"
	historical := claimed
	historical.GovernanceSnapshot = &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{{ID: "design-fold", Version: 1, Content: marker + strings.Repeat("d", 256<<10)}}}
	historical.ServedRequirementSnapshot = []core.ServedRequirementContext{{ID: "req-fold", Version: 1}}
	revision := map[string]any{"work_order_id": order.ID, "attempt_id": claimed.AttemptID, "rationale": "historical plan revision", "plan_version": 1, "governance_snapshot": historical.GovernanceSnapshot, "served_requirement_snapshot": historical.ServedRequirementSnapshot}
	for _, event := range []core.Event{
		{TaskID: id, JobID: job.ID, Kind: "work_order.claimed", Payload: core.JSONPayload(historical)},
		{TaskID: id, JobID: job.ID, Kind: "work_order.updated", Payload: core.JSONPayload(historical)},
		{TaskID: id, JobID: job.ID, Kind: "work_order.plan_revision_requested", Payload: core.JSONPayload(revision)},
		{TaskID: id, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 7, "url": "https://github.com/acme/conveyor/pull/7", "head_sha": "reviewed-head", "base_sha": "base-sha"})},
		{TaskID: id, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"review_round": 1, "verdict": "approve", "approved_head_sha": "approved-head", "reviews": []map[string]any{{"verdict": "approve", "reviewed_commit_sha": "reviewed-head", "review_seat": 1}}})},
		{TaskID: id, Kind: "merge.blocked", Payload: core.JSONPayload(map[string]any{"approved_head": "approved-head", "new_head": "new-head"})},
		{TaskID: id, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"reason_code": "merged_head_mismatch", "pull_request": 7, "merged_head_sha": "other-head", "merge_commit_sha": "merge-sha"})},
		{TaskID: id, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"command": core.TaskGateSpec})},
	} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	fullEvents, err := st.ListEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	compactEvents, err := st.ListCompactEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	embedded := 0
	for i := range fullEvents {
		if bytes.Contains(fullEvents[i].Payload, []byte(marker)) {
			embedded++
		}
		if bytes.Contains(compactEvents[i].Payload, []byte(marker)) {
			t.Fatalf("compact %s event carries the snapshot body", compactEvents[i].Kind)
		}
	}
	if embedded != 3 {
		t.Fatalf("full read holds %d embedded bodies, want the three historical work_order.* events", embedded)
	}
	same := func(name string, full, compact any) {
		t.Helper()
		if !reflect.DeepEqual(full, compact) {
			t.Fatalf("%s differs between the reads:\nfull:    %#v\ncompact: %#v", name, full, compact)
		}
	}
	results := func(values ...any) []any { return values }
	full, compact := &Dispatcher{Store: fullLedgerStore{st}}, &Dispatcher{Store: st}
	same("reviewedHeadFromEvents", full.reviewedHeadFromEvents(ctx, id), compact.reviewedHeadFromEvents(ctx, id))
	fullEpisode, fullOK, fullErr := full.currentMergeConflictEpisode(ctx, id)
	compactEpisode, compactOK, compactErr := compact.currentMergeConflictEpisode(ctx, id)
	same("currentMergeConflictEpisode", results(fullEpisode, fullOK, fullErr), results(compactEpisode, compactOK, compactErr))
	if !fullOK {
		t.Fatal("fixture opens no merge-conflict episode")
	}
	orders, err := st.ListTaskWorkOrders(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	same("conflictDispatchState", results(full.conflictDispatchState(ctx, id, fullEpisode, orders)), results(compact.conflictDispatchState(ctx, id, compactEpisode, orders)))
	same("hasRecoverableApprovedReview", results(full.hasRecoverableApprovedReview(ctx, task)), results(compact.hasRecoverableApprovedReview(ctx, task)))
	same("pendingSpecGate", results(full.pendingSpecGate(ctx, id)), results(compact.pendingSpecGate(ctx, id)))
	same("PendingPlanRevisionGate", results(PendingPlanRevisionGate(ctx, fullLedgerStore{st}, id)), results(PendingPlanRevisionGate(ctx, st, id)))
	same("PendingPlanRevisionGateForTask", results(PendingPlanRevisionGateForTask(ctx, fullLedgerStore{st}, task)), results(PendingPlanRevisionGate(ctx, st, id)))
	same("RecordedReviewComparison", results(RecordedReviewComparison(task, fullEvents)), results(RecordedReviewComparison(task, compactEvents)))
	same("RecordedReviewComparisonContext", results(RecordedReviewComparisonContext(task, fullEvents)), results(RecordedReviewComparisonContext(task, compactEvents)))
	same("reviewHistory", reviewHistory(fullEvents), reviewHistory(compactEvents))
	same("reviewRoundStatus", reviewRoundStatus(fullEvents, 1, "approve"), reviewRoundStatus(compactEvents, 1, "approve"))
	same("pendingGateMergeRequest", results(pendingGateMergeRequest(fullEvents)), results(pendingGateMergeRequest(compactEvents)))
	pr := githubtrigger.PullRequest{Number: 7, Merged: true, HeadSHA: "reviewed-head", MergeCommitSHA: "merge-sha"}
	same("forgeMergeReconciled", forgeMergeReconciled(fullEvents, "conveyor", pr), forgeMergeReconciled(compactEvents, "conveyor", pr))
	same("mergedHeadMismatchRecorded", mergedHeadMismatchRecorded(fullEvents, pr), mergedHeadMismatchRecorded(compactEvents, pr))
}

// snapshotFixtureBytes is the design body each seeded snapshot pins. A read
// that decodes even one snapshot reports at least this many bytes.
const snapshotFixtureBytes = 64 << 10

// snapshotFixtureOrders is the number of snapshot-bearing orders of other
// tasks in the review-dispatch fixture: 200 orders of 64 KiB, about 13 MB that
// a workspace-wide or snapshot-bearing read would decode.
const snapshotFixtureOrders = 200

func snapshotFixtureGovernance() core.GovernanceSnapshot {
	return core.GovernanceSnapshot{
		Designs:                []core.GovernanceDesignContext{{ID: "component-fixture", Title: "Fixture", Category: "component", Version: 1, Content: strings.Repeat("g", snapshotFixtureBytes), Governs: []core.GovernedScope{}}},
		Decisions:              []core.Decision{},
		PendingDesignProposals: []core.PendingSystemDesignProposal{},
	}
}

func snapshotFixtureRequirements() []core.ServedRequirementContext {
	return []core.ServedRequirementContext{{ID: "req-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Observe the dispatch"}}}}
}

// workOrderReadRecorder counts what every work-order read returned to the
// dispatcher: rows, rows of other tasks, and encoded snapshot bytes. A durable
// backend fills a snapshot field only by decoding its selected column, so zero
// snapshot bytes means no read selected a snapshot body.
type workOrderReadRecorder struct {
	store.Store
	taskID string

	mu            sync.Mutex
	calls         map[string]int
	rows          int
	foreignRows   int
	snapshotBytes int
}

func newWorkOrderReadRecorder(st store.Store, taskID string) *workOrderReadRecorder {
	return &workOrderReadRecorder{Store: st, taskID: taskID, calls: map[string]int{}}
}

func (r *workOrderReadRecorder) record(method string, orders []core.WorkOrder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[method]++
	for _, order := range orders {
		r.rows++
		if order.TaskID != r.taskID {
			r.foreignRows++
		}
		if order.ServedRequirementSnapshot != nil {
			encoded, _ := json.Marshal(order.ServedRequirementSnapshot)
			r.snapshotBytes += len(encoded)
		}
		if order.GovernanceSnapshot != nil {
			encoded, _ := json.Marshal(order.GovernanceSnapshot)
			r.snapshotBytes += len(encoded)
		}
	}
}

func (r *workOrderReadRecorder) ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	orders, err := r.Store.ListWorkOrders(ctx)
	r.record("ListWorkOrders", orders)
	return orders, err
}

func (r *workOrderReadRecorder) ListLiveWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	orders, err := r.Store.ListLiveWorkOrders(ctx)
	r.record("ListLiveWorkOrders", orders)
	return orders, err
}

func (r *workOrderReadRecorder) ListWorkOrdersForTasks(ctx context.Context, taskIDs []string) ([]core.WorkOrder, error) {
	orders, err := r.Store.ListWorkOrdersForTasks(ctx, taskIDs)
	r.record("ListWorkOrdersForTasks", orders)
	return orders, err
}

func (r *workOrderReadRecorder) ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	orders, err := r.Store.ListTaskWorkOrders(ctx, taskID)
	r.record("ListTaskWorkOrders", orders)
	return orders, err
}

func (r *workOrderReadRecorder) ListTaskWorkOrdersStored(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	orders, err := r.Store.ListTaskWorkOrdersStored(ctx, taskID)
	r.record("ListTaskWorkOrdersStored", orders)
	return orders, err
}

func (r *workOrderReadRecorder) ListTaskWorkOrdersSnapshot(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	orders, err := r.Store.ListTaskWorkOrdersSnapshot(ctx, taskID)
	r.record("ListTaskWorkOrdersSnapshot", orders)
	return orders, err
}

func (r *workOrderReadRecorder) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	order, err := r.Store.GetWorkOrder(ctx, id)
	if err == nil {
		r.record("GetWorkOrder", []core.WorkOrder{order})
	}
	return order, err
}

// requireReviewDispatchReadsOnlyTaskOrders dispatches the task's review stage
// through a recorder and requires that its work-order reads returned only the
// task's own rows, no snapshot byte, and no workspace-wide read. wantRefusal
// selects the readiness answer: a refused dispatch must fail with the
// readiness error, an admitted one must create the review round
// (component-work-orders, Snapshot-free fold reads).
func requireReviewDispatchReadsOnlyTaskOrders(t *testing.T, ctx context.Context, st store.Store, cfg *config.Config, taskID string, wantRefusal bool) {
	t.Helper()
	recorder := newWorkOrderReadRecorder(st, taskID)
	d := New(recorder, cfg, nil)
	var err error
	if wantRefusal {
		// The command plane routes a task without a sealed verify order back
		// to verify, so only a stale queue snapshot of the task reaches the
		// dispatcher's readiness refusal. Dispatch that snapshot.
		var task core.Task
		if task, err = st.GetTask(ctx, taskID); err != nil {
			t.Fatal(err)
		}
		task.NextStage = core.StageReview
		err = d.runTaskForSnapshot(store.WithActor(ctx, store.SystemActor("dispatcher")), task)
	} else {
		err = d.DispatchNow(ctx, taskID)
	}
	if wantRefusal {
		if err == nil || !strings.Contains(err.Error(), "review requires completed verification") {
			t.Fatalf("review dispatch without a sealed verify order: err=%v, want the readiness refusal", err)
		}
	} else if err != nil {
		t.Fatalf("review dispatch with a sealed verify order: %v", err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.calls["ListWorkOrders"] != 0 || recorder.calls["ListLiveWorkOrders"] != 0 {
		t.Fatalf("review dispatch read workspace-wide orders: %v", recorder.calls)
	}
	if recorder.rows == 0 || recorder.foreignRows != 0 || recorder.snapshotBytes != 0 {
		t.Fatalf("review dispatch work-order reads returned %d rows, %d of other tasks, and %d snapshot bytes; want only the task's rows and 0 snapshot bytes (calls %v)",
			recorder.rows, recorder.foreignRows, recorder.snapshotBytes, recorder.calls)
	}
	if wantRefusal {
		return
	}
	orders, err := st.ListTaskWorkOrders(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range orders {
		if order.Stage == core.StageReview {
			return
		}
	}
	t.Fatalf("admitted review dispatch created no review order: %+v", orders)
}

// TestReviewDispatchReadsOnlyTaskOrdersWithoutSnapshots dispatches review for
// a verify-stage task in a volatile workspace where 200 other tasks' orders
// and the task's own verify order each pin a 64 KiB governance snapshot. The
// readiness read used to return every one of them (about 13 MB); the
// dispatch must read only the task's orders and decode no snapshot byte, for
// the refused and the admitted readiness answer (component-work-orders,
// Snapshot-free fold reads).
func TestReviewDispatchReadsOnlyTaskOrdersWithoutSnapshots(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewVolatileBackend()
	defer st.Close()
	cfg := &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{
		"review": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
	}}}
	governance := snapshotFixtureGovernance()
	pinnedVerifyOrder := func(t *testing.T, task core.Task) core.WorkOrder {
		t.Helper()
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: task.ID + "-verify-1", TaskID: task.ID, Stage: core.StageVerify, State: core.JobPending}
		order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: "head", QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour), CreatedAt: time.Now().UTC()}
		if _, err := storetest.CreateStageWorkOrder(ctx, st, job, order); err != nil {
			t.Fatal(err)
		}
		claimed, err := storetest.ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("verifier"), SessionID: "verify-" + task.ID, ClientToken: "verify-" + task.ID, Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: snapshotFixtureRequirements(), Governance: &governance})
		if err != nil {
			t.Fatal(err)
		}
		return claimed
	}
	newTask := func(id string, stage core.Stage) core.Task {
		task := core.Task{ID: id, Workspace: "demo", Repo: "repo", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: stage, ReviewedHeadSHA: "head", CreatedAt: time.Now().UTC()}
		task.SetupContract.VerifyStage = true
		return task
	}
	for i := range snapshotFixtureOrders {
		pinnedVerifyOrder(t, newTask(fmt.Sprintf("foreign-%03d-%s", i, core.NewTaskID()), core.StageVerify))
	}
	task := newTask("review-"+core.NewTaskID(), core.StageVerify)
	verify := pinnedVerifyOrder(t, task)
	full, err := st.ListWorkOrders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seeded := newWorkOrderReadRecorder(st, task.ID)
	seeded.record("seed", full)
	if seeded.foreignRows != snapshotFixtureOrders || seeded.snapshotBytes < (snapshotFixtureOrders+1)*snapshotFixtureBytes {
		t.Fatalf("fixture holds %d foreign rows and %d snapshot bytes, want %d rows and at least %d bytes", seeded.foreignRows, seeded.snapshotBytes, snapshotFixtureOrders, (snapshotFixtureOrders+1)*snapshotFixtureBytes)
	}

	t.Run("Refused", func(t *testing.T) {
		requireReviewDispatchReadsOnlyTaskOrders(t, ctx, st, cfg, task.ID, true)
	})
	t.Run("Admitted", func(t *testing.T) {
		stored, err := st.GetWorkOrder(ctx, verify.ID)
		if err != nil {
			t.Fatal(err)
		}
		stored.State = core.WorkOrderCompleted
		stored.VerificationContextID = "context-fixture"
		if err = storetest.UpdateWorkOrder(ctx, st, stored, core.WorkOrderCmdSubmitVerification); err != nil {
			t.Fatal(err)
		}
		if after, err := st.GetWorkOrder(ctx, verify.ID); err != nil || after.GovernanceSnapshot == nil || after.VerificationContextID != "context-fixture" {
			t.Fatalf("sealed fixture order lost its snapshot or context: %+v %v", after, err)
		}
		if current, err := st.GetTask(ctx, task.ID); err == nil && current.State == core.TaskQueued {
			if _, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true}); err != nil {
			t.Fatal(err)
		}
		if current, err := st.GetTask(ctx, task.ID); err != nil || current.NextStage != core.StageReview {
			t.Fatalf("sealed fixture task did not reach review: next=%s err=%v", current.NextStage, err)
		}
		requireReviewDispatchReadsOnlyTaskOrders(t, ctx, st, cfg, task.ID, false)
	})
}

// TestHandleInterventionRefusesApproveWithoutReview proves the dispatcher
// refuses, under the task's lock and without the route's pre-check, an approve
// at a bounce limit that planning reached after an earlier review round
// recorded head A and the implementer pushed head B. The approve would
// otherwise bind A, a head review rejected. The task stays at its gate with no
// approved head and no event, and a review-sourced limit still approves
// (component-next-action invariant 7; req-operator-next-action AC-1.4).
func TestHandleInterventionRefusesApproveWithoutReview(t *testing.T) {
	for _, tc := range []struct {
		name    string
		source  any
		refused bool
	}{
		{name: "planning-sourced limit", source: "spec.output_invalid", refused: true},
		{name: "review-sourced limit", source: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := core.Task{ID: "earlier-head", Workspace: "demo", Repo: "conveyor", PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageSpec, ReviewedHeadSHA: "head-b", CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-spec-2", TaskID: task.ID, Stage: core.StageSpec, State: core.JobDone}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			limit := map[string]any{"window": 1, "max_bounces": 1}
			if tc.source != nil {
				limit["source"] = tc.source
			}
			for _, event := range []core.Event{
				{TaskID: task.ID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"reviewed_commit_sha": "head-a", "verdict": "changes_requested"})},
				{TaskID: task.ID, JobID: job.ID, Kind: "spec.output_invalid", Payload: core.JSONPayload(map[string]string{"error": "plan markdown is required"})},
				{TaskID: task.ID, JobID: job.ID, Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(limit)},
			} {
				if err := st.AppendEvent(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			gated, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskStageBounceLimit, RecoveryStage: core.StageSpec, ProjectStages: true})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := st.ListEvents(ctx, task.ID)
			d := New(st, &config.Config{Workspace: "demo"}, nil)
			err = d.HandleIntervention(ctx, gated.Task, job, core.Intervention{TaskID: task.ID, JobID: job.ID, Action: core.InterventionApprove, ReasonCode: "approved"})
			current, getErr := st.GetTask(ctx, task.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if !tc.refused {
				if err != nil || current.State != core.TaskApproved || current.ApprovedHeadSHA != "head-b" {
					t.Fatalf("err=%v task=%+v, want today's approve", err, current)
				}
				return
			}
			if !errors.Is(err, store.ErrApproveWithoutReview) {
				t.Fatalf("err=%v, want ErrApproveWithoutReview", err)
			}
			if current.State != core.TaskAwaiting || current.GateKind != core.GateKindReviewBounceLimit || current.ApprovedHeadSHA != "" {
				t.Fatalf("after a refused approve task=%+v, want it at its gate", current)
			}
			if after, _ := st.ListEvents(ctx, task.ID); len(after) != len(before) {
				t.Fatalf("events grew from %d to %d on a refused approve", len(before), len(after))
			}
		})
	}
}

// completeStageOutputForTest records one agent-stage output through the
// shared output handling, as an in-process call completed before agent stages
// became MCP work orders, without any model call (DEC-132(2)).
func completeStageOutputForTest(t *testing.T, ctx context.Context, d *Dispatcher, cfg *config.Config, task core.Task, output string) {
	t.Helper()
	prior, err := d.Store.ListJobs(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt := 1
	for _, job := range prior {
		if job.Stage == task.NextStage {
			attempt++
		}
	}
	job := core.Job{ID: fmt.Sprintf("%s-%s-%d", task.ID, task.NextStage, attempt), TaskID: task.ID, Stage: task.NextStage, Harness: "fixture", ModelTier: "fixture-model", Runner: "in-process", State: core.JobRunning, StartedAt: time.Now().UTC()}
	if _, err = taskops.New(d.Store).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskDispatchStart}); err != nil {
		t.Fatal(err)
	}
	if err = d.Store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	job.State, job.EndedAt = core.JobDone, time.Now().UTC()
	if err = d.Store.UpdateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err = d.completeOutput(ctx, cfg, task, job, output, "in-process", 0); err != nil {
		t.Fatal(err)
	}
}
