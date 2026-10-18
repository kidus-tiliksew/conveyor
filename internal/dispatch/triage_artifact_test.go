package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// artifactReadCountingStore records body reads so a test can prove that an
// omitted artifact was never fetched.
type artifactReadCountingStore struct {
	store.Store
	mu    sync.Mutex
	reads map[string]int
}

func (s *artifactReadCountingStore) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	s.mu.Lock()
	s.reads[id]++
	s.mu.Unlock()
	return s.Store.GetArtifact(ctx, id)
}

func (s *artifactReadCountingStore) readCount(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads[id]
}

type unsupportedArtifactFixture struct {
	d       *Dispatcher
	st      *artifactReadCountingStore
	ctx     context.Context
	agent   *capturingInputAgent
	sibling core.Task
}

func newUnsupportedArtifactFixture(t *testing.T) *unsupportedArtifactFixture {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := &artifactReadCountingStore{Store: store.NewMemory(), reads: map[string]int{}}
	sibling := core.Task{ID: "adjacent-sibling", Workspace: "demo", Repo: "api", State: core.TaskQueued, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, sibling); err != nil {
		t.Fatal(err)
	}
	agent := &capturingInputAgent{}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt", Timeout: time.Minute}}}}
	d := New(st, cfg, agent)
	var err error
	d.Pack, err = pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	return &unsupportedArtifactFixture{d: d, st: st, ctx: ctx, agent: agent, sibling: sibling}
}

// task creates a triage task whose lineage reaches the sibling's artifacts
// through a dependency edge.
func (f *unsupportedArtifactFixture) task(t *testing.T, id, body string) core.Task {
	t.Helper()
	task := core.Task{ID: id, Workspace: "demo", Repo: "api", Title: "Adjacent artifact", Body: body, PolicyVersion: 1, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := f.st.CreateTask(f.ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AddTaskDependency(f.ctx, store.DependencyAdditionRequest{TaskID: task.ID, DependsOnTaskID: f.sibling.ID, Reason: "context", RequestID: id + "-dependency"}); err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *unsupportedArtifactFixture) artifact(t *testing.T, owner, name, contentType string, content []byte) core.Artifact {
	t.Helper()
	artifact, err := f.st.CreateArtifact(f.ctx, core.Artifact{Name: name, ContentType: contentType, TaskID: owner, Role: core.ArtifactRoleTaskContext}, content)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func unsupportedOmissionNote(artifact core.Artifact) string {
	return fmt.Sprintf("Context artifact body omitted as unsupported model input: %s (%s, %d bytes, id %s) from task %s; this attachment kind is not supported model input; the body is unread.", artifact.Name, artifact.ContentType, artifact.SizeBytes, artifact.ID, artifact.TaskID)
}

func attachmentByID(attachments []inprocess.Attachment, id string) (inprocess.Attachment, bool) {
	for _, attachment := range attachments {
		if attachment.ID == id {
			return attachment, true
		}
	}
	return inprocess.Attachment{}, false
}

func deliveryByID(freshness core.ContextFreshness, id string) (core.ContextDelivery, bool) {
	for _, delivery := range freshness.Deliveries {
		if delivery.ArtifactID == id {
			return delivery, true
		}
	}
	return core.ContextDelivery{}, false
}

func TestTriageOmitsUnsupportedAdjacentArtifact(t *testing.T) {
	f := newUnsupportedArtifactFixture(t)
	task := f.task(t, "adjacent-omit", "Exact task intent.")
	gzip := f.artifact(t, f.sibling.ID, "attempt.manifest.json.gz", "application/gzip", []byte("\x1f\x8b\x08\x00compressed manifest"))
	notes := []byte("Supported adjacent evidence.")
	text := f.artifact(t, f.sibling.ID, "notes.txt", "text/plain", notes)

	input, err := f.d.buildStageInput(f.ctx, f.d.Cfg, core.StageTriage, task)
	if err != nil {
		t.Fatalf("buildStageInput error = %v, want adjacent unsupported artifact omitted", err)
	}
	if _, ok := attachmentByID(input.Attachments, gzip.ID); ok {
		t.Fatal("unsupported adjacent artifact was attached")
	}
	if got := f.st.readCount(gzip.ID); got != 0 {
		t.Fatalf("unsupported adjacent artifact body reads = %d, want 0", got)
	}
	supported, ok := attachmentByID(input.Attachments, text.ID)
	if !ok || string(supported.Content) != string(notes) || supported.Kind != inprocess.AttachmentDocument {
		t.Fatalf("supported adjacent attachment = %+v present=%t", supported, ok)
	}
	note := unsupportedOmissionNote(gzip)
	if strings.Count(input.Prompt, note) != 1 {
		t.Fatalf("prompt carries %d copies of omission note %q", strings.Count(input.Prompt, note), note)
	}
	if !strings.Contains(input.Prompt, "Exact task intent.") {
		t.Fatal("task intent missing from prompt")
	}
	omitted, ok := deliveryByID(input.ContextFreshness, gzip.ID)
	if !ok || !omitted.Omitted || omitted.Fetched || omitted.State == "fetched" {
		t.Fatalf("unsupported artifact delivery = %+v present=%t, want omitted and not fetched", omitted, ok)
	}
	if delivered, ok := deliveryByID(input.ContextFreshness, text.ID); !ok || delivered.Omitted {
		t.Fatalf("supported artifact delivery = %+v present=%t, want not omitted", delivered, ok)
	}
	if input.ContextFreshness.ObservationRevision != core.ContextDigest(input.ContextFreshness.Deliveries) {
		t.Fatal("observation revision does not match the prepared deliveries")
	}

	// The provider-input observation reads the prepared input through the
	// memory store's in-process observation boundary.
	observeCtx := store.WithActor(store.WithWorkspace(t.Context(), "demo"), store.Actor{ID: "dispatcher", Role: core.ActorSystem})
	job := core.Job{ID: task.ID + "-triage-observe", TaskID: task.ID, State: core.JobRunning, Stage: core.StageTriage, Runner: "in-process", StartedAt: time.Now()}
	if err = f.st.Store.CreateJob(observeCtx, job); err != nil {
		t.Fatal(err)
	}
	observed := (&Dispatcher{Store: f.st.Store}).observeInProcessInput(observeCtx, task, job, input)
	if !observed.ObservationRecorded {
		t.Fatalf("provider-input observation not recorded: %+v", observed)
	}
	if delivery, ok := deliveryByID(observed, gzip.ID); !ok || !delivery.Omitted || delivery.Fetched {
		t.Fatalf("observed unsupported delivery = %+v present=%t, want omitted", delivery, ok)
	}
	if delivery, ok := deliveryByID(observed, text.ID); !ok || !delivery.Fetched {
		t.Fatalf("observed supported delivery = %+v present=%t, want fetched", delivery, ok)
	}
	events, err := f.st.ListEvents(f.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]string{}
	for _, event := range events {
		if event.Kind != "context.artifact_fetch_observed" {
			continue
		}
		var observation core.ContextObservation
		if json.Unmarshal(event.Payload, &observation) != nil {
			t.Fatalf("invalid observation payload: %s", event.Payload)
		}
		outcomes[observation.ArtifactID] = observation.Outcome
	}
	if outcomes[gzip.ID] != "omitted" || outcomes[text.ID] != "fetched" {
		t.Fatalf("provider-input observations = %v, want gzip omitted and text fetched", outcomes)
	}

	if err = f.d.DispatchNow(f.ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if f.agent.calls != 1 {
		t.Fatalf("agent calls = %d, want 1", f.agent.calls)
	}
	if _, ok := attachmentByID(f.agent.input.Attachments, gzip.ID); ok {
		t.Fatal("provider input carries the unsupported adjacent artifact")
	}
	if _, ok := attachmentByID(f.agent.input.Attachments, text.ID); !ok {
		t.Fatal("provider input lost the supported adjacent artifact")
	}
	if !strings.Contains(f.agent.input.Prompt, note) {
		t.Fatal("provider prompt lost the omission note")
	}
	if got := f.st.readCount(gzip.ID); got != 0 {
		t.Fatalf("unsupported adjacent artifact body reads after dispatch = %d, want 0", got)
	}
	if events, err = f.st.ListEvents(f.ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "artifact.context_failed" || event.Kind == "dispatch.failed" {
			t.Fatalf("unexpected %s event: %s", event.Kind, event.Payload)
		}
	}
}

func TestTriageTaskLocalUnsupportedArtifactStillFails(t *testing.T) {
	f := newUnsupportedArtifactFixture(t)
	task := f.task(t, "local-unsupported", "Exact task intent.")
	f.artifact(t, task.ID, "attempt.manifest.json.gz", "application/gzip", []byte("\x1f\x8b\x08\x00compressed manifest"))

	if _, err := f.d.buildStageInput(f.ctx, f.d.Cfg, core.StageTriage, task); err == nil || !strings.Contains(err.Error(), "unsupported context artifact") {
		t.Fatalf("buildStageInput error = %v, want unsupported context artifact", err)
	}
	if err := f.d.DispatchNow(f.ctx, task.ID); err == nil || !strings.Contains(err.Error(), "unsupported context artifact") || f.agent.calls != 0 {
		t.Fatalf("DispatchNow error = %v calls = %d, want failure before model execution", err, f.agent.calls)
	}
}

func TestNonTriageStageAdjacentUnsupportedArtifactStillFails(t *testing.T) {
	f := newUnsupportedArtifactFixture(t)
	task := f.task(t, "spec-unsupported", "Exact task intent.")
	f.artifact(t, f.sibling.ID, "attempt.manifest.json.gz", "application/gzip", []byte("\x1f\x8b\x08\x00compressed manifest"))

	if _, err := f.d.buildStageInput(f.ctx, f.d.Cfg, core.StageSpec, task); err == nil || !strings.Contains(err.Error(), "unsupported context artifact") {
		t.Fatalf("spec buildStageInput error = %v, want unsupported context artifact", err)
	}
}

// The unsupported-artifact note is prompt text, so it is charged to the
// text/history allowance and can evict an admitted optional body; the byte
// limits themselves stay unchanged (component-triage).
func TestTriageUnsupportedOmissionNoteCountsTowardTextBudget(t *testing.T) {
	evidence := []byte(strings.Repeat("adjacent evidence ", 4<<10))
	// Each probe uses a fresh store, so inputs differ only by the task body.
	build := func(body string) (inprocess.Input, core.Artifact, core.Artifact) {
		t.Helper()
		f := newUnsupportedArtifactFixture(t)
		gzip := f.artifact(t, f.sibling.ID, "attempt.manifest.json.gz", "application/gzip", []byte("\x1f\x8b\x08\x00compressed manifest"))
		text := f.artifact(t, f.sibling.ID, "evidence.txt", "text/plain", evidence)
		input, err := f.d.buildStageInput(f.ctx, f.d.Cfg, core.StageTriage, f.task(t, "budget-probe", body))
		if err != nil {
			t.Fatalf("buildStageInput error = %v", err)
		}
		return input, gzip, text
	}
	short, gzip, text := build("x")
	long, _, _ := build("x" + strings.Repeat("y", 1000))
	note := unsupportedOmissionNote(gzip)
	for _, probe := range []inprocess.Input{short, long} {
		if _, ok := attachmentByID(probe.Attachments, text.ID); !ok || !strings.Contains(probe.Prompt, note) {
			t.Fatal("probe did not admit the supported body beside the note")
		}
	}
	base := measureTriageInput(short).TextBytes
	perByte := (measureTriageInput(long).TextBytes - base) / 1000
	if perByte < 1 {
		t.Fatalf("text bytes per body byte = %d", perByte)
	}
	// Overshoot by half the note: the padded input fits without the note and
	// exceeds the allowance with it.
	pad := (maxTriageInitialBytes - base + len(note)/2 + perByte - 1) / perByte
	unbounded := base + pad*perByte
	if unbounded <= maxTriageInitialBytes || unbounded-len(note) > maxTriageInitialBytes {
		t.Fatalf("fixture text bytes = %d with a %d-byte note do not straddle the %d-byte limit", unbounded, len(note), maxTriageInitialBytes)
	}
	got, _, _ := build("x" + strings.Repeat("z", pad))
	if _, ok := attachmentByID(got.Attachments, text.ID); ok {
		t.Fatal("supported body still attached although the omission note exhausted the text allowance")
	}
	if !strings.Contains(got.Prompt, note) {
		t.Fatal("unsupported omission note missing after eviction")
	}
	if !strings.Contains(got.Prompt, fmt.Sprintf("Context artifact body omitted by triage byte budget: %s (%s, %d bytes, id %s)", text.Name, text.ContentType, len(evidence), text.ID)) {
		t.Fatal("evicted supported body has no budget omission note")
	}
	if measured := measureTriageInput(got).TextBytes; measured > maxTriageInitialBytes {
		t.Fatalf("bounded input text bytes = %d exceed %d", measured, maxTriageInitialBytes)
	}
}
