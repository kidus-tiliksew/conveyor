package storetest

// RequirementInsight conformance (feature-requirement-insights RI-2, RI-5;
// req-requirement-insights REQ-2, REQ-3, REQ-4; DEC-87 (2)). Every case runs
// unchanged on memory, PostgreSQL, and SingleStore.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunRequirementInsightConformance runs the RI-2 input read and RI-5
// headline commands against one fixture.
func RunRequirementInsightConformance(t *testing.T, x Fixture) {
	if x.SeedEvents == nil {
		t.Fatal("factory must provide SeedEvents for requirement insight conformance")
	}
	h := newInsightHarness(t, x)
	t.Run("InputsAddRemoveReadd", h.inputsAddRemoveReadd)
	t.Run("InputsRepeatedAddAndTimestampTie", h.inputsRepeatedAddAndTimestampTie)
	t.Run("InputsIgnoresActivationAndPendingProposal", h.inputsIgnoresActivationAndPendingProposal)
	t.Run("InputsEarliestTerminalTimes", h.inputsEarliestTerminalTimes)
	t.Run("InputsBodyDependenciesAndDesigns", h.inputsBodyDependenciesAndDesigns)
	t.Run("InputsCurrentConfirmedVersion", h.inputsCurrentConfirmedVersion)
	t.Run("InputsUnknownArchivedUnconfirmed", h.inputsUnknownArchivedUnconfirmed)
	t.Run("InputsEmptyAndWorkspaceIsolation", h.inputsEmptyAndWorkspaceIsolation)
	t.Run("HeadlineInsertAndReplace", h.headlineInsertAndReplace)
	t.Run("HeadlineEqualFingerprintNoWrite", h.headlineEqualFingerprintNoWrite)
	t.Run("HeadlineWorkspaceIsolation", h.headlineWorkspaceIsolation)
	t.Run("HeadlineConcurrentSameFingerprint", h.headlineConcurrentSameFingerprint)
	t.Run("HeadlineWritesNoEventsOrControlState", h.headlineWritesNoEventsOrControlState)
}

type insightHarness struct {
	x       Fixture
	st      store.Backend
	ctx     context.Context
	plane   *taskops.Plane
	created time.Time
	foreign context.Context
}

const insightBodySentinel = "INSIGHT-BODY"

func newInsightHarness(t *testing.T, x Fixture) *insightHarness {
	ctx := store.WithActor(x.Context, store.Actor{ID: requirementConformanceActor, Role: core.ActorHuman})
	foreignID := x.Workspace + "-insight-foreign"
	foreign := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: requirementConformanceActor, Role: core.ActorHuman}), foreignID)
	if _, err := x.Backend.BootstrapWorkspaceConfig(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), foreignID), &config.Config{Workspace: foreignID, Repos: x.Config.Repos}); err != nil {
		t.Fatal(err)
	}
	return &insightHarness{x: x, st: x.Backend, ctx: ctx, plane: taskops.New(x.Backend), created: time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second), foreign: foreign}
}

func (h *insightHarness) task(t *testing.T, ctx context.Context, title string, dependencies ...string) core.Task {
	t.Helper()
	workspace, _ := store.WorkspaceFromContext(ctx)
	id := core.NewTaskID()
	h.created = h.created.Add(time.Second)
	task := core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: title, Body: insightBodySentinel + " " + title, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: h.created}
	if err := h.st.CreateTaskWithDependencies(ctx, task, dependencies); err != nil {
		t.Fatal(err)
	}
	return task
}

func (h *insightHarness) requirement(t *testing.T, ctx context.Context, id string, confirm bool, statements ...core.RequirementStatement) string {
	t.Helper()
	if id == "" {
		id = "req-insight-" + strings.ToLower(core.NewTaskID())
	}
	if len(statements) == 0 {
		statements = []core.RequirementStatement{{ID: "REQ-1", Statement: "Insight " + id + ".", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "first"}}}}
	}
	_, version, err := h.st.CreateRequirement(ctx, core.Requirement{ID: id, Title: "Title " + id}, core.RequirementVersion{Content: "# " + id + " v1", Origin: core.RequirementOriginOperator, Statements: statements})
	if err != nil {
		t.Fatal(err)
	}
	if confirm {
		if _, _, err = h.st.ConfirmRequirementVersion(ctx, id, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func (h *insightHarness) attach(t *testing.T, ctx context.Context, taskID string, change store.TaskContextChange) {
	t.Helper()
	if _, err := h.st.UpdateTaskContext(ctx, taskID, change); err != nil {
		t.Fatalf("context change %s %+v: %v", taskID, change, err)
	}
}

func addRequirement(id string) store.TaskContextChange {
	return store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{id}}}
}

func removeRequirement(id string) store.TaskContextChange {
	return store.TaskContextChange{Remove: store.TaskContextInput{RequirementIDs: []string{id}}}
}

func (h *insightHarness) read(t *testing.T, ctx context.Context, requirementID string) store.RequirementInsightInputs {
	t.Helper()
	inputs, err := h.st.ReadRequirementInsightInputs(ctx, requirementID)
	if err != nil {
		t.Fatalf("read inputs %s: %v", requirementID, err)
	}
	return inputs
}

func servingIDs(inputs store.RequirementInsightInputs) []string {
	ids := []string{}
	for _, task := range inputs.Tasks {
		ids = append(ids, task.Task.ID)
	}
	return ids
}

func servingTask(t *testing.T, inputs store.RequirementInsightInputs, id string) store.RequirementInsightTask {
	t.Helper()
	for _, task := range inputs.Tasks {
		if task.Task.ID == id {
			return task
		}
	}
	t.Fatalf("task %s is not serving: %v", id, servingIDs(inputs))
	return store.RequirementInsightTask{}
}

// latestRequirementAdd returns the time of a task's latest requirement add
// event for requirementID, read back through the public history.
func (h *insightHarness) latestRequirementAdd(t *testing.T, ctx context.Context, taskID, requirementID string) time.Time {
	t.Helper()
	events, err := h.st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var latest time.Time
	for _, event := range events {
		var payload struct {
			ID string `json:"id"`
		}
		if event.Kind == store.TaskContextRequirementAdded && json.Unmarshal(event.Payload, &payload) == nil && payload.ID == requirementID {
			latest = event.At
		}
	}
	if latest.IsZero() {
		t.Fatalf("task %s has no add event for %s", taskID, requirementID)
	}
	return latest
}

func sortedStrings(values ...string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}

func (h *insightHarness) inputsAddRemoveReadd(t *testing.T) {
	requirement := h.requirement(t, h.ctx, "", true)
	removed := h.task(t, h.ctx, "removed")
	readded := h.task(t, h.ctx, "readded")
	h.attach(t, h.ctx, removed.ID, addRequirement(requirement))
	h.attach(t, h.ctx, removed.ID, removeRequirement(requirement))
	h.attach(t, h.ctx, readded.ID, addRequirement(requirement))
	h.attach(t, h.ctx, readded.ID, removeRequirement(requirement))
	h.attach(t, h.ctx, readded.ID, addRequirement(requirement))
	inputs := h.read(t, h.ctx, requirement)
	if got := servingIDs(inputs); !slices.Equal(got, []string{readded.ID}) {
		t.Fatalf("serving %v, want only the re-added task %s", got, readded.ID)
	}
	if got, want := servingTask(t, inputs, readded.ID).ServingSince, h.latestRequirementAdd(t, h.ctx, readded.ID, requirement); !got.Equal(want) {
		t.Fatalf("serving since %s, want the re-add at %s", got, want)
	}
}

func (h *insightHarness) inputsRepeatedAddAndTimestampTie(t *testing.T) {
	requirement := h.requirement(t, h.ctx, "", true)
	repeated := h.task(t, h.ctx, "repeated")
	h.attach(t, h.ctx, repeated.ID, addRequirement(requirement))
	h.attach(t, h.ctx, repeated.ID, addRequirement(requirement))
	tieAdd := h.task(t, h.ctx, "tie ends with add")
	tieRemove := h.task(t, h.ctx, "tie ends with remove")
	tieAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	payload := core.JSONPayload(map[string]any{"id": requirement})
	// Equal times order by ID: the higher-ranked event is the latest.
	h.x.SeedEvents(t, h.ctx, 0, []core.Event{
		{ID: 1, TaskID: tieAdd.ID, Kind: store.TaskContextRequirementRemoved, Payload: payload, At: tieAt},
		{ID: 2, TaskID: tieAdd.ID, Kind: store.TaskContextRequirementAdded, Payload: payload, At: tieAt},
		{ID: 3, TaskID: tieRemove.ID, Kind: store.TaskContextRequirementAdded, Payload: payload, At: tieAt},
		{ID: 4, TaskID: tieRemove.ID, Kind: store.TaskContextRequirementRemoved, Payload: payload, At: tieAt},
	})
	// A later-recorded add with an earlier time is not the latest event.
	late := h.task(t, h.ctx, "backdated add after remove")
	h.attach(t, h.ctx, late.ID, addRequirement(requirement))
	h.attach(t, h.ctx, late.ID, removeRequirement(requirement))
	h.x.SeedEvents(t, h.ctx, 0, []core.Event{{ID: 1, TaskID: late.ID, Kind: store.TaskContextRequirementAdded, Payload: payload, At: tieAt.Add(-time.Hour)}})

	inputs := h.read(t, h.ctx, requirement)
	if got, want := servingIDs(inputs), sortedStrings(repeated.ID, tieAdd.ID); !slices.Equal(got, want) {
		t.Fatalf("serving %v, want %v", got, want)
	}
	if got, want := servingTask(t, inputs, repeated.ID).ServingSince, h.latestRequirementAdd(t, h.ctx, repeated.ID, requirement); !got.Equal(want) {
		t.Fatalf("repeated add serving since %s, want the latest add %s", got, want)
	}
	if got := servingTask(t, inputs, tieAdd.ID).ServingSince; !got.Equal(tieAt) {
		t.Fatalf("tied add serving since %s, want %s", got, tieAt)
	}
}

func (h *insightHarness) inputsIgnoresActivationAndPendingProposal(t *testing.T) {
	requirement := h.requirement(t, h.ctx, "", true)
	proposed := h.task(t, h.ctx, "pending proposal only")
	if _, _, err := h.st.ProposeTaskContext(h.ctx, core.TaskContextProposalInput{TaskID: proposed.ID, TargetKind: core.TaskContextProposalRequirement, TargetID: requirement, Source: core.TaskContextProposalPlanning, Justification: "Pending only."}); err != nil {
		t.Fatal(err)
	}
	activatedAfterRemove := h.task(t, h.ctx, "activated after remove")
	h.attach(t, h.ctx, activatedAfterRemove.ID, addRequirement(requirement))
	h.attach(t, h.ctx, activatedAfterRemove.ID, removeRequirement(requirement))
	activatedServing := h.task(t, h.ctx, "activated while attached")
	h.attach(t, h.ctx, activatedServing.ID, addRequirement(requirement))
	addAt := h.latestRequirementAdd(t, h.ctx, activatedServing.ID, requirement)
	payload := core.JSONPayload(map[string]any{"id": requirement})
	later := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	h.x.SeedEvents(t, h.ctx, 0, []core.Event{
		{ID: 1, TaskID: activatedAfterRemove.ID, Kind: store.TaskContextRequirementActive, Payload: payload, At: later},
		{ID: 2, TaskID: activatedServing.ID, Kind: store.TaskContextRequirementActive, Payload: payload, At: later},
	})
	inputs := h.read(t, h.ctx, requirement)
	if got := servingIDs(inputs); !slices.Equal(got, []string{activatedServing.ID}) {
		t.Fatalf("serving %v, want only %s", got, activatedServing.ID)
	}
	if got := servingTask(t, inputs, activatedServing.ID).ServingSince; !got.Equal(addAt) {
		t.Fatalf("serving since %s, want the add at %s, not the activation", got, addAt)
	}
}

func (h *insightHarness) stateChangedAt(t *testing.T, ctx context.Context, taskID string, to core.TaskState) time.Time {
	t.Helper()
	events, err := h.st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		var payload struct {
			To core.TaskState `json:"to"`
		}
		if event.Kind == "task.state_changed" && json.Unmarshal(event.Payload, &payload) == nil && payload.To == to {
			return event.At
		}
	}
	t.Fatalf("task %s has no transition to %s", taskID, to)
	return time.Time{}
}

func (h *insightHarness) inputsEarliestTerminalTimes(t *testing.T) {
	requirement := h.requirement(t, h.ctx, "", true)
	merged := h.task(t, h.ctx, "merged")
	closed := h.task(t, h.ctx, "closed")
	open := h.task(t, h.ctx, "open")
	seeded := h.task(t, h.ctx, "seeded transitions")
	for _, task := range []core.Task{merged, closed, open, seeded} {
		h.attach(t, h.ctx, task.ID, addRequirement(requirement))
	}
	if _, err := h.plane.Perform(h.ctx, merged.ID, taskops.Command{Kind: core.TaskMergeRecover}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.plane.Perform(h.ctx, closed.ID, taskops.Command{Kind: core.TaskCancel}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Microsecond)
	transition := func(to core.TaskState) []byte {
		return core.JSONPayload(map[string]any{"from": core.TaskQueued, "to": to, "command": "fixture"})
	}
	// The earliest transition wins even when it was recorded later with a
	// higher ID; transitions to other states are ignored.
	h.x.SeedEvents(t, h.ctx, 0, []core.Event{
		{ID: 1, TaskID: seeded.ID, Kind: "task.state_changed", Payload: transition(core.TaskMerged), At: base.Add(2 * time.Hour)},
		{ID: 2, TaskID: seeded.ID, Kind: "task.state_changed", Payload: transition(core.TaskMerged), At: base.Add(time.Hour)},
		{ID: 3, TaskID: seeded.ID, Kind: "task.state_changed", Payload: transition(core.TaskClosed), At: base.Add(3 * time.Hour)},
		{ID: 4, TaskID: seeded.ID, Kind: "task.state_changed", Payload: transition(core.TaskRunning), At: base},
	})
	inputs := h.read(t, h.ctx, requirement)
	if got := servingTask(t, inputs, merged.ID); !got.MergedAt.Equal(h.stateChangedAt(t, h.ctx, merged.ID, core.TaskMerged)) || !got.ClosedAt.IsZero() || got.Task.State != core.TaskMerged {
		t.Fatalf("merged task %+v", got)
	}
	if got := servingTask(t, inputs, closed.ID); !got.ClosedAt.Equal(h.stateChangedAt(t, h.ctx, closed.ID, core.TaskClosed)) || !got.MergedAt.IsZero() || got.Task.State != core.TaskClosed {
		t.Fatalf("closed task %+v", got)
	}
	if got := servingTask(t, inputs, open.ID); !got.MergedAt.IsZero() || !got.ClosedAt.IsZero() {
		t.Fatalf("open task terminal times %s %s", got.MergedAt, got.ClosedAt)
	}
	got := servingTask(t, inputs, seeded.ID)
	if !got.MergedAt.Equal(base.Add(time.Hour)) || !got.ClosedAt.Equal(base.Add(3*time.Hour)) {
		t.Fatalf("seeded merged=%s closed=%s, want %s and %s", got.MergedAt, got.ClosedAt, base.Add(time.Hour), base.Add(3*time.Hour))
	}
	// Event times are reported as recorded; the current state stays the
	// row's, so the two are independent inputs.
	if got.Task.State != core.TaskQueued {
		t.Fatalf("seeded task state %s, want the stored queued row", got.Task.State)
	}
}

func (h *insightHarness) design(t *testing.T, ctx context.Context) string {
	t.Helper()
	id := "design-insight-" + strings.ToLower(core.NewTaskID())
	_, version, err := h.st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: "Design " + id, Category: "Component design"}, core.SystemDesignVersion{Content: designContent(id), Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = h.st.ConfirmSystemDesignVersion(ctx, id, version.Version); err != nil {
		t.Fatal(err)
	}
	return id
}

func (h *insightHarness) inputsBodyDependenciesAndDesigns(t *testing.T) {
	requirement := h.requirement(t, h.ctx, "", true)
	first := h.task(t, h.ctx, "first dependency")
	second := h.task(t, h.ctx, "second dependency")
	dependent := h.task(t, h.ctx, "dependent", second.ID, first.ID)
	kept, dropped, other := h.design(t, h.ctx), h.design(t, h.ctx), h.design(t, h.ctx)
	h.attach(t, h.ctx, dependent.ID, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{requirement}, DesignIDs: []string{kept, dropped, other}}})
	h.attach(t, h.ctx, dependent.ID, store.TaskContextChange{Remove: store.TaskContextInput{DesignIDs: []string{dropped}}})
	h.attach(t, h.ctx, first.ID, addRequirement(requirement))

	inputs := h.read(t, h.ctx, requirement)
	if got, want := servingIDs(inputs), sortedStrings(first.ID, dependent.ID); !slices.Equal(got, want) {
		t.Fatalf("serving %v, want %v", got, want)
	}
	got := servingTask(t, inputs, dependent.ID)
	if got.Task.Body != dependent.Body || got.Task.Title != dependent.Title || got.Task.Workspace != h.x.Workspace || got.Task.State != core.TaskQueued || got.Task.Repo != "conveyor" || !got.Task.CreatedAt.Equal(dependent.CreatedAt) {
		t.Fatalf("serving task row %+v, want body, title, state, repository, and creation time of %+v", got.Task, dependent)
	}
	if want := sortedStrings(first.ID, second.ID); !slices.Equal(got.DependsOn, want) {
		t.Fatalf("depends_on %v, want %v", got.DependsOn, want)
	}
	if want := sortedStrings(kept, other); !slices.Equal(got.Designs, want) {
		t.Fatalf("designs %v, want %v", got.Designs, want)
	}
	plain := servingTask(t, inputs, first.ID)
	if plain.DependsOn == nil || len(plain.DependsOn) != 0 || plain.Designs == nil || len(plain.Designs) != 0 {
		t.Fatalf("task without relations %+v", plain)
	}
}

func (h *insightHarness) inputsCurrentConfirmedVersion(t *testing.T) {
	statementsV1 := []core.RequirementStatement{{ID: "REQ-1", Statement: "Version one.", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "one"}}}}
	requirement := h.requirement(t, h.ctx, "", true, statementsV1...)
	statementsV2 := []core.RequirementStatement{
		{ID: "REQ-1", Statement: "Version two.", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "one"}, {ID: "AC-1.2", Statement: "two"}}},
		{ID: "REQ-2", Statement: "Added.", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-2.1", Statement: "added"}}},
	}
	proposal, err := h.st.ProposeRequirementVersion(h.ctx, core.RequirementVersion{RequirementID: requirement, Content: "# " + requirement + " v2", Origin: core.RequirementOriginOperator, Statements: statementsV2})
	if err != nil {
		t.Fatal(err)
	}
	// The read carries the stored title and the confirmed version's stored
	// content and statements, as the public document reads return them.
	expect := func(label string, version int, statements []core.RequirementStatement) {
		t.Helper()
		document, err := h.st.GetRequirement(h.ctx, requirement)
		requireOK(t, err)
		stored, err := h.st.GetRequirementVersion(h.ctx, requirement, version)
		requireOK(t, err)
		got := h.read(t, h.ctx, requirement).Requirement
		if got.ID != requirement || got.Title != document.Title || got.CurrentVersion != version || document.CurrentVersion != version || got.Content != stored.Content || !strings.Contains(got.Content, "v"+fmt.Sprint(version)) || !reflect.DeepEqual(got.Statements, statements) {
			t.Fatalf("%s read %+v, want version %d of %+v", label, got, version, stored)
		}
	}
	expect("pending proposal", 1, statementsV1)
	if _, _, err = h.st.ConfirmRequirementVersion(h.ctx, requirement, proposal.Version); err != nil {
		t.Fatal(err)
	}
	expect("confirmed revision", proposal.Version, statementsV2)
}

func (h *insightHarness) inputsUnknownArchivedUnconfirmed(t *testing.T) {
	archived := h.requirement(t, h.ctx, "", true)
	task := h.task(t, h.ctx, "serves the archived requirement")
	h.attach(t, h.ctx, task.ID, addRequirement(archived))
	if err := h.st.ArchiveRequirement(h.ctx, archived, requirementConformanceActor, nil); err != nil {
		t.Fatal(err)
	}
	unconfirmed := h.requirement(t, h.ctx, "", false)
	for name, id := range map[string]string{"unknown": "req-insight-unknown-" + strings.ToLower(core.NewTaskID()), "archived": archived, "unconfirmed": unconfirmed} {
		if _, err := h.st.ReadRequirementInsightInputs(h.ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s requirement err=%v, want ErrNotFound", name, err)
		}
	}
}

func (h *insightHarness) inputsEmptyAndWorkspaceIsolation(t *testing.T) {
	statements := []core.RequirementStatement{{ID: "REQ-1", Statement: "Shared."}}
	shared := "req-insight-shared-" + strings.ToLower(core.NewTaskID())
	h.requirement(t, h.ctx, shared, true, statements...)
	empty := h.read(t, h.ctx, shared)
	if empty.Tasks == nil || len(empty.Tasks) != 0 || empty.Requirement.ID != shared || empty.Requirement.CurrentVersion != 1 || empty.Requirement.Statements == nil {
		t.Fatalf("empty read %+v", empty)
	}
	if _, err := h.st.ReadRequirementInsightInputs(h.foreign, shared); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign read of a home requirement err=%v, want ErrNotFound", err)
	}
	// The same requirement ID in another workspace serves only its own tasks.
	h.requirement(t, h.foreign, shared, true, statements...)
	foreignTask := h.task(t, h.foreign, "foreign server")
	h.attach(t, h.foreign, foreignTask.ID, addRequirement(shared))
	homeTask := h.task(t, h.ctx, "home server")
	h.attach(t, h.ctx, homeTask.ID, addRequirement(shared))
	if got := servingIDs(h.read(t, h.ctx, shared)); !slices.Equal(got, []string{homeTask.ID}) {
		t.Fatalf("home serving %v, want %s", got, homeTask.ID)
	}
	if got := servingIDs(h.read(t, h.foreign, shared)); !slices.Equal(got, []string{foreignTask.ID}) {
		t.Fatalf("foreign serving %v, want %s", got, foreignTask.ID)
	}
}

func insightHeadline(requirementID, fingerprint, text string) store.RequirementInsightHeadline {
	return store.RequirementInsightHeadline{RequirementID: requirementID, Fingerprint: fingerprint, Headline: text, Model: "model-" + fingerprint, TokensIn: int64(len(text)), TokensOut: int64(len(fingerprint)), GeneratedBy: "user:insight-" + fingerprint}
}

func sameHeadline(a, b store.RequirementInsightHeadline) bool {
	return a.WorkspaceID == b.WorkspaceID && a.RequirementID == b.RequirementID && a.Fingerprint == b.Fingerprint && a.Headline == b.Headline &&
		a.Model == b.Model && a.TokensIn == b.TokensIn && a.TokensOut == b.TokensOut && a.GeneratedBy == b.GeneratedBy && a.GeneratedAt.Equal(b.GeneratedAt)
}

func (h *insightHarness) getHeadline(t *testing.T, ctx context.Context, requirementID string) store.RequirementInsightHeadline {
	t.Helper()
	headline, err := h.st.GetRequirementInsightHeadline(ctx, requirementID)
	if err != nil {
		t.Fatalf("get headline %s: %v", requirementID, err)
	}
	return headline
}

func (h *insightHarness) headlineInsertAndReplace(t *testing.T) {
	requirement := h.requirement(t, h.ctx, "", true)
	if _, err := h.st.GetRequirementInsightHeadline(h.ctx, requirement); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent headline err=%v, want ErrNotFound", err)
	}
	before := time.Now().UTC().Add(-time.Second)
	first := insightHeadline(requirement, "fp-one", "First headline.")
	first.WorkspaceID = "caller-supplied-workspace"
	first.GeneratedAt = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	stored, err := h.st.RecordRequirementInsightHeadline(h.ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	want := first
	want.WorkspaceID, want.GeneratedAt = h.x.Workspace, stored.GeneratedAt
	if !sameHeadline(stored, want) || stored.GeneratedAt.Before(before) || stored.GeneratedAt.Location() != time.UTC {
		t.Fatalf("stored %+v, want %+v with a fresh UTC storage time", stored, want)
	}
	if got := h.getHeadline(t, h.ctx, requirement); !sameHeadline(got, stored) {
		t.Fatalf("read back %+v, want %+v", got, stored)
	}
	second := insightHeadline(requirement, "fp-two", "Second headline.")
	replaced, err := h.st.RecordRequirementInsightHeadline(h.ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	want = second
	want.WorkspaceID, want.GeneratedAt = h.x.Workspace, replaced.GeneratedAt
	if !sameHeadline(replaced, want) || replaced.GeneratedAt.Before(stored.GeneratedAt) {
		t.Fatalf("replaced %+v, want every field of %+v", replaced, want)
	}
	if got := h.getHeadline(t, h.ctx, requirement); !sameHeadline(got, replaced) {
		t.Fatalf("read back %+v, want %+v", got, replaced)
	}
	for _, invalid := range []store.RequirementInsightHeadline{insightHeadline("", "fp", "x"), insightHeadline(requirement, "", "x")} {
		if _, err := h.st.RecordRequirementInsightHeadline(h.ctx, invalid); err == nil {
			t.Fatalf("invalid headline %+v was accepted", invalid)
		}
	}
	if got := h.getHeadline(t, h.ctx, requirement); !sameHeadline(got, replaced) {
		t.Fatalf("refused writes changed the row to %+v", got)
	}
}

func (h *insightHarness) headlineEqualFingerprintNoWrite(t *testing.T) {
	requirement := h.requirement(t, h.ctx, "", true)
	stored, err := h.st.RecordRequirementInsightHeadline(h.ctx, insightHeadline(requirement, "fp-same", "Original."))
	if err != nil {
		t.Fatal(err)
	}
	refused := insightHeadline(requirement, "fp-same", "A different paragraph.")
	refused.Model, refused.TokensIn, refused.TokensOut, refused.GeneratedBy = "other-model", 999, 999, "user:someone-else"
	if _, err := h.st.RecordRequirementInsightHeadline(h.ctx, refused); !errors.Is(err, store.ErrInsightHeadlineCurrent) {
		t.Fatalf("equal fingerprint err=%v, want ErrInsightHeadlineCurrent", err)
	}
	if store.ErrInsightHeadlineCurrent.Error() != "insight_headline_current" {
		t.Fatalf("sentinel text %q", store.ErrInsightHeadlineCurrent)
	}
	if got := h.getHeadline(t, h.ctx, requirement); !sameHeadline(got, stored) {
		t.Fatalf("refusal changed the row: %+v, want %+v", got, stored)
	}
}

func (h *insightHarness) headlineWorkspaceIsolation(t *testing.T) {
	requirement := h.requirement(t, h.ctx, "", true)
	home, err := h.st.RecordRequirementInsightHeadline(h.ctx, insightHeadline(requirement, "fp-home", "Home."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.GetRequirementInsightHeadline(h.foreign, requirement); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign read err=%v, want ErrNotFound", err)
	}
	// The same key and fingerprint in another workspace is a separate row.
	foreign, err := h.st.RecordRequirementInsightHeadline(h.foreign, insightHeadline(requirement, "fp-home", "Foreign."))
	if err != nil {
		t.Fatalf("foreign record: %v", err)
	}
	foreignWorkspace, _ := store.WorkspaceFromContext(h.foreign)
	if foreign.WorkspaceID != foreignWorkspace || foreign.Headline != "Foreign." {
		t.Fatalf("foreign row %+v", foreign)
	}
	if got := h.getHeadline(t, h.ctx, requirement); !sameHeadline(got, home) {
		t.Fatalf("home row changed to %+v", got)
	}
	if got := h.getHeadline(t, h.foreign, requirement); !sameHeadline(got, foreign) {
		t.Fatalf("foreign row %+v, want %+v", got, foreign)
	}
}

// concurrentHeadlines starts every record at once behind a closed channel
// and joins them all; no case sleeps.
func (h *insightHarness) concurrentHeadlines(headlines []store.RequirementInsightHeadline) ([]store.RequirementInsightHeadline, []error) {
	start := make(chan struct{})
	results := make([]store.RequirementInsightHeadline, len(headlines))
	errs := make([]error, len(headlines))
	var ready, done sync.WaitGroup
	for i := range headlines {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			results[i], errs[i] = h.st.RecordRequirementInsightHeadline(h.ctx, headlines[i])
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	return results, errs
}

func (h *insightHarness) headlineConcurrentSameFingerprint(t *testing.T) {
	requirement := h.requirement(t, h.ctx, "", true)
	race := func(label, fingerprint string) store.RequirementInsightHeadline {
		t.Helper()
		var headlines []store.RequirementInsightHeadline
		for i := 0; i < 6; i++ {
			headline := insightHeadline(requirement, fingerprint, fmt.Sprintf("%s caller %d.", label, i))
			headline.GeneratedBy = fmt.Sprintf("user:%s-%d", label, i)
			headlines = append(headlines, headline)
		}
		results, errs := h.concurrentHeadlines(headlines)
		var winner store.RequirementInsightHeadline
		winners := 0
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
				winner = results[i]
			case errors.Is(err, store.ErrInsightHeadlineCurrent):
			default:
				t.Fatalf("%s caller %d: %v", label, i, err)
			}
		}
		if winners != 1 {
			t.Fatalf("%s: %d winners, want exactly one", label, winners)
		}
		if got := h.getHeadline(t, h.ctx, requirement); !sameHeadline(got, winner) {
			t.Fatalf("%s: stored %+v, want the winner %+v unchanged", label, got, winner)
		}
		return winner
	}
	// Callers race an absent row, then a stored row with a new fingerprint.
	first := race("absent", "fp-race-one")
	second := race("replace", "fp-race-two")
	if second.Fingerprint == first.Fingerprint || second.GeneratedAt.Before(first.GeneratedAt) {
		t.Fatalf("replacement %+v after %+v", second, first)
	}
}

func (h *insightHarness) headlineWritesNoEventsOrControlState(t *testing.T) {
	requirement := h.requirement(t, h.ctx, "", true)
	task := h.task(t, h.ctx, "control state")
	h.attach(t, h.ctx, task.ID, addRequirement(requirement))
	snapshot := func() []any {
		t.Helper()
		events, err := h.st.ListEvents(h.ctx, task.ID)
		requireOK(t, err)
		stored, err := h.st.GetTask(h.ctx, task.ID)
		requireOK(t, err)
		tasks, err := h.st.ListTasks(h.ctx)
		requireOK(t, err)
		orders, err := h.st.ListWorkOrders(h.ctx)
		requireOK(t, err)
		document, err := h.st.GetRequirement(h.ctx, requirement)
		requireOK(t, err)
		versions, err := h.st.ListRequirementVersions(h.ctx, requirement)
		requireOK(t, err)
		documentEvents, err := h.st.ListRequirementEvents(h.ctx, requirement)
		requireOK(t, err)
		links, err := h.st.ListLineageLinks(h.ctx)
		requireOK(t, err)
		inputs := h.read(t, h.ctx, requirement)
		out := []any{len(events), events[len(events)-1].ID, stored, len(tasks), len(orders), document.CurrentVersion, document.UpdatedAt.UTC(), len(versions), len(documentEvents), len(links), inputs}
		if h.x.WorkspaceEvents != nil {
			workspaceEvents, err := h.x.WorkspaceEvents(h.ctx, "")
			requireOK(t, err)
			out = append(out, len(workspaceEvents))
		}
		return out
	}
	before := snapshot()
	if _, err := h.st.RecordRequirementInsightHeadline(h.ctx, insightHeadline(requirement, "fp-control-one", "One.")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.RecordRequirementInsightHeadline(h.ctx, insightHeadline(requirement, "fp-control-two", "Two.")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.RecordRequirementInsightHeadline(h.ctx, insightHeadline(requirement, "fp-control-two", "Refused.")); !errors.Is(err, store.ErrInsightHeadlineCurrent) {
		t.Fatalf("equal fingerprint err=%v", err)
	}
	if after := snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("headline writes changed control state:\nbefore %+v\nafter  %+v", before, after)
	}
}
