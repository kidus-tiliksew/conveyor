package storetest

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

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runFeedbackReportActs holds every backend to the reads and acts of
// reports: each act against each report and link state with its event,
// notice, Inbox flag, and stored actor and time; the refusals that record
// nothing; the acts against a report entry, the end of linking, and the same
// act at the same moment; and the list filters, order, paging, and crate
// counts (component-feedback-reports invariants 17 to 23;
// feature-production-feedback-listening FR-3; req-production-feedback REQ-7,
// AC-6.2, AC-7.1, AC-7.2, AC-7.4, AC-7.5, and AC-11.1). Each case starts
// from a fresh workspace.
func runFeedbackReportActs(t *testing.T, factory Factory) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, Fixture)
	}{
		{"LinkActsAgainstEveryLinkState", runFeedbackActLinkStates},
		{"LinkTargetsRefused", runFeedbackActLinkTargets},
		{"CrateLinks", runFeedbackActCrateLinks},
		{"RemoveLink", runFeedbackActRemoveLink},
		{"RemovalSurvivesLinking", runFeedbackActRemovalSurvivesLinking},
		{"CloseAndReopen", runFeedbackActCloseReopen},
		{"EveryActRaisesOneNotice", runFeedbackActNotices},
		{"ClosedRefusesEveryActButReopen", runFeedbackActClosedRefuses},
		{"CloseReasonRedactedAndBounded", runFeedbackActCloseReason},
		{"EntryAfterAUserClose", runFeedbackActEntryAfterClose},
		{"FixTask", runFeedbackActFixTask},
		{"FixTaskRefusalsRecordNothing", runFeedbackActFixTaskRefusals},
		{"UnknownAndForeignReport", runFeedbackActUnknownReport},
		{"ActsObserveOnly", runFeedbackActObservesOnly},
		{"SameActTwiceAtOnce", runFeedbackActConcurrentSame},
		{"RemovalAgainstTheEndOfLinking", runFeedbackActConcurrentLinking},
		{"ActsAgainstAReportEntry", runFeedbackActConcurrentEntry},
		{"TaskFilteredPageAgainstRemovals", runFeedbackReadTaskFilterConcurrent},
		{"FixTaskAndCausesReadTogether", runFeedbackReadFixTaskConcurrent},
		{"CratePageAgainstMembershipChanges", runFeedbackReadCrateConcurrent},
		{"PageAndTotalAgainstCloses", runFeedbackReadTotalConcurrent},
		{"ListFiltersOrderAndPaging", runFeedbackReportListFilters},
		{"CrateCountsAgreeWithTheList", runFeedbackReportCrateCounts},
		{"Details", runFeedbackReportDetails},
		{"ReadWorkBoundedByThePage", runFeedbackReportReadWork},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := factory.fresh(t, nil)
			if x.SeedFeedbackReportLink == nil || x.WorkspaceEvents == nil {
				t.Fatal("conformance fixture must provide SeedFeedbackReportLink and WorkspaceEvents for the report act cases")
			}
			c.run(t, x)
		})
	}
}

// actUser is the user who records the acts.
var actUser = store.Actor{ID: "user:usr-operator", Role: core.ActorUser}

func actContext(x Fixture) context.Context { return store.WithActor(x.Context, actUser) }

// actFixture is one stored report with a user context and the helpers the
// act cases share.
type actFixture struct {
	t      *testing.T
	x      Fixture
	env    feedbackEnvironment
	report store.FeedbackReport
	ctx    context.Context
}

func newActFixture(t *testing.T, x Fixture, key string) *actFixture {
	t.Helper()
	env := newFeedbackEnvironment(t, x, "Production "+key, "watcher")
	env.sendReports(t, 0, reportEntry(key))
	return &actFixture{t: t, x: x, env: env, report: env.reportByKey(t, key), ctx: actContext(x)}
}

func (f *actFixture) act(act store.FeedbackReportAct) (store.FeedbackReportActResult, error) {
	f.t.Helper()
	if act.ReportID == "" {
		act.ReportID = f.report.ID
	}
	if act.At.IsZero() {
		act.At = feedbackTime(time.Hour)
	}
	return f.x.Backend.RecordFeedbackReportAct(f.ctx, act)
}

func (f *actFixture) changed(act store.FeedbackReportAct) {
	f.t.Helper()
	result, err := f.act(act)
	requireOK(f.t, err)
	if !result.Changed {
		f.t.Fatalf("act %+v changed nothing", act)
	}
}

func (f *actFixture) unchanged(act store.FeedbackReportAct) {
	f.t.Helper()
	before := f.state()
	result, err := f.act(act)
	requireOK(f.t, err)
	if result.Changed {
		f.t.Fatalf("act %+v changed something", act)
	}
	if after := f.state(); after != before {
		f.t.Fatalf("an act that changed nothing recorded something:\nbefore %s\nafter  %s", before, after)
	}
}

func (f *actFixture) refused(act store.FeedbackReportAct, check func(error) bool) {
	f.t.Helper()
	before := f.state()
	_, err := f.act(act)
	if err == nil || !check(err) {
		f.t.Fatalf("act %+v err=%v", act, err)
	}
	if after := f.state(); after != before {
		f.t.Fatalf("a refused act recorded something:\nbefore %s\nafter  %s", before, after)
	}
}

// state renders everything an act can change: the report, its links, and
// the report events.
func (f *actFixture) state() string {
	f.t.Helper()
	report, err := f.x.Backend.GetFeedbackReport(f.x.Context, f.report.ID)
	requireOK(f.t, err)
	links, err := f.x.Backend.ListFeedbackReportLinks(f.x.Context, f.report.ID)
	requireOK(f.t, err)
	raw, err := json.Marshal([]any{report, links, reportEvents(f.t, f.x)})
	requireOK(f.t, err)
	return string(raw)
}

func (f *actFixture) stored() store.FeedbackReport {
	f.t.Helper()
	report, err := f.x.Backend.GetFeedbackReport(f.x.Context, f.report.ID)
	requireOK(f.t, err)
	return report
}

func (f *actFixture) links() map[string]store.FeedbackReportLink {
	f.t.Helper()
	links, err := f.x.Backend.ListFeedbackReportLinks(f.x.Context, f.report.ID)
	requireOK(f.t, err)
	out := map[string]store.FeedbackReportLink{}
	for _, link := range links {
		key := link.TaskID
		if key == "" {
			key = "crate:" + link.CrateID
		}
		out[key] = link
	}
	return out
}

// seed stores one link of the report to a task.
func (f *actFixture) seed(n int, taskID, state string, evidence ...store.FeedbackReportEvidence) store.FeedbackReportLink {
	f.t.Helper()
	at := feedbackTime(time.Minute)
	link := store.FeedbackReportLink{ID: store.FeedbackReportLinkID(f.report.ID, n), ReportID: f.report.ID, TaskID: taskID, State: state,
		Evidence: evidence, CreatedAt: at, CreatedBy: store.SystemActor().ID}
	if link.Evidence == nil {
		link.Evidence = []store.FeedbackReportEvidence{}
	}
	switch state {
	case store.FeedbackReportLinkConfirmed:
		link.ConfirmedBy, link.ConfirmedAt = "user:usr-earlier", &at
	case store.FeedbackReportLinkRemoved:
		link.RemovedBy, link.RemovedAt = "user:usr-earlier", &at
	}
	f.x.SeedFeedbackReportLink(f.t, f.x.Context, link)
	return link
}

// requireActEvent checks the newest report event: its kind, the user actor,
// no task, and a payload of IDs only.
func requireActEvent(t *testing.T, x Fixture, kind string, want map[string]any) {
	t.Helper()
	events := reportEvents(t, x)
	if len(events) == 0 {
		t.Fatalf("no report event, want %s", kind)
	}
	event := events[len(events)-1]
	if event.Kind != kind || event.TaskID != "" || event.ActorID != actUser.ID || event.ActorRole != core.ActorUser {
		t.Fatalf("event=%+v, want a task-less %s by %s", event, kind, actUser.ID)
	}
	var payload map[string]any
	requireOK(t, json.Unmarshal(event.Payload, &payload))
	encodedWant, _ := json.Marshal(want)
	var normalized map[string]any
	requireOK(t, json.Unmarshal(encodedWant, &normalized))
	if !reflect.DeepEqual(payload, normalized) {
		t.Fatalf("%s payload=%v, want exactly %v", kind, payload, normalized)
	}
}

func countEvents(t *testing.T, x Fixture, kind string) int {
	t.Helper()
	count := 0
	for _, event := range reportEvents(t, x) {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

func mergedActTask(t *testing.T, x Fixture, crateID string) core.Task {
	t.Helper()
	return feedbackMergedTask(t, x, "app", crateID)
}

func openActTask(t *testing.T, x Fixture) core.Task {
	t.Helper()
	id := fixtureTaskID()
	task := core.Task{ID: id, Workspace: x.Workspace, Repo: "app", Title: "Open " + id, Body: "Open.", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(), Branch: "conveyor/task-" + id}
	requireOK(t, x.Backend.CreateTask(x.Context, task))
	return task
}

// foreignContext bootstraps a second workspace and returns its context.
func foreignContext(t *testing.T, x Fixture) context.Context {
	t.Helper()
	foreign := x.Workspace + "-other"
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), foreign)
	cfg := &config.Config{Workspace: foreign}
	if x.Config != nil {
		cfg.Repos = x.Config.Repos
	}
	if _, err := x.Backend.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func actTargetError(code string) func(error) bool {
	return func(err error) bool {
		var target *store.FeedbackReportActError
		return errors.As(err, &target) && target.Code == code
	}
}

func fieldError(field string) func(error) bool {
	return func(err error) bool {
		var refusal *store.FeedbackFieldError
		return errors.As(err, &refusal) && refusal.Field == field
	}
}

func runFeedbackActLinkStates(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	none, possible, confirmed, removed := mergedActTask(t, x, ""), mergedActTask(t, x, ""), mergedActTask(t, x, ""), mergedActTask(t, x, "")
	evidence := lineEvidence(feedbackCommit("ab"))
	f.seed(1, possible.ID, store.FeedbackReportLinkPossible, evidence)
	held := f.seed(2, confirmed.ID, store.FeedbackReportLinkConfirmed)
	f.seed(3, removed.ID, store.FeedbackReportLinkRemoved, evidence)
	notices := x.Backend.Changes().Subscribe(x.Workspace, changes.Workspace)
	defer notices.Close()
	settle(notices)

	// A confirmed link is left as it is, and the report stays in the Inbox.
	f.unchanged(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, TaskID: confirmed.ID})
	if !f.stored().InInbox {
		t.Fatal("an act that changed nothing took the report out of the Inbox")
	}
	select {
	case n := <-notices.C():
		t.Fatalf("an act that changed nothing raised %+v", n)
	case <-time.After(200 * time.Millisecond):
	}
	if got := f.links()[confirmed.ID]; !reflect.DeepEqual(got, held) {
		t.Fatalf("confirmed link=%+v, want %+v", got, held)
	}

	// Confirming a possible link keeps its evidence and stores the actor and
	// time, cut to the stored precision.
	at := feedbackTime(time.Hour).Add(789 * time.Nanosecond)
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, TaskID: possible.ID, At: at})
	awaitNotice(t, notices, "workspace notice after a confirmation")
	link := f.links()[possible.ID]
	if link.State != store.FeedbackReportLinkConfirmed || link.ConfirmedBy != actUser.ID || link.ConfirmedAt == nil || !link.ConfirmedAt.Equal(store.EnvironmentTime(at)) ||
		!reflect.DeepEqual(link.Evidence, []store.FeedbackReportEvidence{evidence}) || link.ID != store.FeedbackReportLinkID(f.report.ID, 1) {
		t.Fatalf("confirmed possible link=%+v", link)
	}
	if f.stored().InInbox {
		t.Fatal("a confirmation left the report in the Inbox")
	}
	requireActEvent(t, x, store.FeedbackReportLinkConfirmedEvent, map[string]any{"report_id": f.report.ID, "environment_id": f.report.EnvironmentID, "task_id": possible.ID})

	// Adding a link to a task with no link stores a new confirmed link with
	// an ID no stored link holds.
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, TaskID: none.ID})
	added := f.links()[none.ID]
	if added.State != store.FeedbackReportLinkConfirmed || added.CreatedBy != actUser.ID || added.ConfirmedBy != actUser.ID || len(added.Evidence) != 0 ||
		added.ID != store.FeedbackReportLinkID(f.report.ID, 4) {
		t.Fatalf("added link=%+v", added)
	}

	// A later manual link to a task whose link a user removed is accepted
	// and confirmed, and the removal is cleared.
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, TaskID: removed.ID})
	again := f.links()[removed.ID]
	if again.State != store.FeedbackReportLinkConfirmed || again.RemovedAt != nil || again.RemovedBy != "" || len(again.Evidence) != 1 {
		t.Fatalf("relinked removed link=%+v", again)
	}
	if n := countEvents(t, x, store.FeedbackReportLinkConfirmedEvent); n != 3 {
		t.Fatalf("link_confirmed events=%d, want 3", n)
	}
	if f.stored().State != store.FeedbackReportOpen {
		t.Fatal("a link act changed the report's state")
	}
}

func runFeedbackActLinkTargets(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	open := openActTask(t, x)
	foreign := foreignContext(t, x)
	foreignTask := feedbackMergedTask(t, Fixture{Backend: x.Backend, Context: foreign, Workspace: x.Workspace + "-other"}, "app", "")
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, TaskID: open.ID}, actTargetError(store.FeedbackActTaskNotMerged))
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, TaskID: "task-missing"}, actTargetError(store.FeedbackActTaskNotFound))
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, TaskID: foreignTask.ID}, actTargetError(store.FeedbackActTaskNotFound))
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActLink}, fieldError("body"))
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, TaskID: open.ID, CrateID: "crate-x"}, fieldError("body"))
	f.refused(store.FeedbackReportAct{Kind: "merge", TaskID: open.ID}, fieldError("kind"))
	if !f.stored().InInbox {
		t.Fatal("a refused act took the report out of the Inbox")
	}
}

func runFeedbackActCrateLinks(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Spring"})
	requireOK(t, err)
	foreign := foreignContext(t, x)
	foreignCrate, err := x.Backend.CreateCrate(foreign, store.CrateInput{Name: "Elsewhere"})
	requireOK(t, err)
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, CrateID: crate.ID})
	link := f.links()["crate:"+crate.ID]
	if link.State != store.FeedbackReportLinkConfirmed || link.TaskID != "" || link.ConfirmedBy != actUser.ID {
		t.Fatalf("crate link=%+v", link)
	}
	requireActEvent(t, x, store.FeedbackReportLinkConfirmedEvent, map[string]any{"report_id": f.report.ID, "environment_id": f.report.EnvironmentID, "crate_id": crate.ID})
	f.unchanged(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, CrateID: crate.ID})
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, CrateID: "crate-missing"}, actTargetError(store.FeedbackActCrateNotFound))
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, CrateID: foreignCrate.ID}, actTargetError(store.FeedbackActCrateNotFound))
	// A removed crate link is confirmed again by a later link.
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActRemoveLink, LinkID: link.ID})
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, CrateID: crate.ID})
	if again := f.links()["crate:"+crate.ID]; again.ID != link.ID || again.State != store.FeedbackReportLinkConfirmed {
		t.Fatalf("relinked crate link=%+v", again)
	}
}

func runFeedbackActRemoveLink(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	task := mergedActTask(t, x, "")
	link := f.seed(1, task.ID, store.FeedbackReportLinkPossible, lineEvidence(feedbackCommit("ab")))
	other := newActFixture(t, x, "other")
	otherLink := other.seed(1, task.ID, store.FeedbackReportLinkPossible)
	at := feedbackTime(2 * time.Hour).Add(5 * time.Nanosecond)
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActRemoveLink, LinkID: link.ID, At: at})
	removed := f.links()[task.ID]
	if removed.State != store.FeedbackReportLinkRemoved || removed.RemovedBy != actUser.ID || removed.RemovedAt == nil || !removed.RemovedAt.Equal(store.EnvironmentTime(at)) || len(removed.Evidence) != 1 {
		t.Fatalf("removed link=%+v", removed)
	}
	report := f.stored()
	if report.State != store.FeedbackReportOpen || report.InInbox {
		t.Fatalf("report after a removal=%+v, want open and out of the Inbox", report)
	}
	requireActEvent(t, x, store.FeedbackReportLinkRemovedEvent, map[string]any{"report_id": f.report.ID, "environment_id": f.report.EnvironmentID, "link_id": link.ID, "task_id": task.ID})
	// Removing a removed link changes nothing, and a link of another report
	// or no link at all is not found.
	f.unchanged(store.FeedbackReportAct{Kind: store.FeedbackReportActRemoveLink, LinkID: link.ID})
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActRemoveLink, LinkID: otherLink.ID}, actTargetError(store.FeedbackActLinkNotFound))
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActRemoveLink, LinkID: "lnk:missing"}, actTargetError(store.FeedbackActLinkNotFound))
	if got := other.links()[task.ID]; got.State != store.FeedbackReportLinkPossible {
		t.Fatalf("another report's link=%+v", got)
	}
}

func runFeedbackActRemovalSurvivesLinking(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	task := mergedActTask(t, x, "")
	link := f.seed(1, task.ID, store.FeedbackReportLinkPossible)
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActRemoveLink, LinkID: link.ID})
	attemptAt := claimedAt(t, x, f.report.ID, feedbackTime(time.Minute))
	finished, err := x.Backend.FinishFeedbackReportLinking(x.Context, store.FeedbackReportLinking{ReportID: f.report.ID, EndedAt: feedbackTime(2 * time.Minute), AttemptAt: attemptAt,
		Links: []store.FeedbackReportLinkCandidate{{TaskID: task.ID, Evidence: []store.FeedbackReportEvidence{lineEvidence(feedbackCommit("cd"))}}}})
	requireOK(t, err)
	if !finished {
		t.Fatal("the end of linking wrote nothing")
	}
	if got := f.links()[task.ID]; got.State != store.FeedbackReportLinkRemoved || len(got.Evidence) != 0 {
		t.Fatalf("link after linking=%+v, want the removal kept and no evidence added", got)
	}
}

func runFeedbackActCloseReopen(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	task := mergedActTask(t, x, "")
	f.seed(1, task.ID, store.FeedbackReportLinkConfirmed)
	before := f.links()
	closedAt := feedbackTime(time.Hour).Add(321 * time.Nanosecond)
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, Reason: "Fixed in the next release", At: closedAt})
	report := f.stored()
	if report.State != store.FeedbackReportClosed || report.InInbox || report.ClosedAt == nil || !report.ClosedAt.Equal(store.EnvironmentTime(closedAt)) ||
		report.ClosedBy != actUser.ID || report.CloseReason != "Fixed in the next release" {
		t.Fatalf("closed report=%+v", report)
	}
	requireActEvent(t, x, store.FeedbackReportClosedEvent, map[string]any{"report_id": f.report.ID, "environment_id": f.report.EnvironmentID})
	if !reflect.DeepEqual(f.links(), before) {
		t.Fatal("closing changed the links")
	}
	reopenedAt := feedbackTime(2 * time.Hour)
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActReopen, At: reopenedAt})
	report = f.stored()
	if report.State != store.FeedbackReportOpen || report.InInbox || report.ClosedAt != nil || report.ClosedBy != "" || report.CloseReason != "" ||
		report.ReopenedAt == nil || !report.ReopenedAt.Equal(reopenedAt) || report.ReopenOrigin == nil ||
		*report.ReopenOrigin != (store.FeedbackReportOrigin{Source: store.FeedbackSourceUser, Label: "", Actor: actUser.ID}) {
		t.Fatalf("reopened report=%+v", report)
	}
	requireActEvent(t, x, store.FeedbackReportReopenedEvent, map[string]any{"report_id": f.report.ID, "environment_id": f.report.EnvironmentID})
	if !reflect.DeepEqual(f.links(), before) {
		t.Fatal("reopening changed the links")
	}
	// Reopening an open report changes nothing.
	f.unchanged(store.FeedbackReportAct{Kind: store.FeedbackReportActReopen})
}

func runFeedbackActNotices(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	task, fix := mergedActTask(t, x, ""), openActTask(t, x)
	notices := x.Backend.Changes().Subscribe(x.Workspace, changes.Workspace)
	defer notices.Close()
	settle(notices)
	for _, act := range []store.FeedbackReportAct{
		{Kind: store.FeedbackReportActLink, TaskID: task.ID},
		{Kind: store.FeedbackReportActRemoveLink, LinkID: store.FeedbackReportLinkID(f.report.ID, 1)},
		{Kind: store.FeedbackReportActFixTask, TaskID: fix.ID, CauseTaskIDs: []string{task.ID}},
		{Kind: store.FeedbackReportActClose, Reason: "done"},
		{Kind: store.FeedbackReportActReopen},
	} {
		f.changed(act)
		if n := awaitNotice(t, notices, "workspace notice after "+act.Kind); n.Kind != changes.Workspace || n.Workspace != x.Workspace {
			t.Fatalf("notice after %s=%+v", act.Kind, n)
		}
		settle(notices)
	}
	// An act that changes nothing raises no notice.
	f.unchanged(store.FeedbackReportAct{Kind: store.FeedbackReportActReopen})
	select {
	case n := <-notices.C():
		t.Fatalf("an act that changed nothing raised %+v", n)
	case <-time.After(300 * time.Millisecond):
	}
}

func runFeedbackActClosedRefuses(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	task := mergedActTask(t, x, "")
	link := f.seed(1, task.ID, store.FeedbackReportLinkPossible)
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, Reason: "done"})
	closed := func(err error) bool { return errors.Is(err, store.ErrFeedbackReportClosed) }
	for _, act := range []store.FeedbackReportAct{
		{Kind: store.FeedbackReportActLink, TaskID: task.ID},
		{Kind: store.FeedbackReportActLink, TaskID: "task-missing"},
		{Kind: store.FeedbackReportActRemoveLink, LinkID: link.ID},
		{Kind: store.FeedbackReportActRemoveLink, LinkID: "lnk:missing"},
		{Kind: store.FeedbackReportActClose, Reason: "again"},
		{Kind: store.FeedbackReportActFixTask, TaskID: task.ID, CauseTaskIDs: []string{task.ID}},
	} {
		f.refused(act, closed)
	}
}

func runFeedbackActCloseReason(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	tooLong := strings.Repeat("é", store.FeedbackReportCloseReasonMax+1)
	for _, reason := range []string{"", tooLong, "bad\x00reason", "bad \xff"} {
		f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, Reason: reason}, fieldError("reason"))
	}
	// 500 characters of two bytes each are accepted: the bound counts
	// characters, not bytes.
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, Reason: strings.Repeat("é", store.FeedbackReportCloseReasonMax)})
	other := newActFixture(t, x, "secret")
	secret := "ghp_" + strings.Repeat("a", 36)
	other.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, Reason: "Token " + secret + " leaked"})
	if reason := other.stored().CloseReason; strings.Contains(reason, secret) || !strings.HasPrefix(reason, "Token ") {
		t.Fatalf("stored reason=%q, want the secret redacted", reason)
	}
	for _, event := range reportEvents(t, x) {
		if strings.Contains(string(event.Payload), "Token") {
			t.Fatalf("event %+v holds the reason", event)
		}
	}
}

func runFeedbackActEntryAfterClose(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	closedAt := feedbackTime(time.Hour).Add(400 * time.Nanosecond)
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, Reason: "done", At: closedAt})
	// An entry whose last_seen is the stored closing time is not later, and
	// leaves the report closed.
	entry := reportEntry("crash")
	entry.FirstSeen = feedbackReportTimePtr(0)
	entry.LastSeen = store.StatedFeedbackReportTime(store.EnvironmentTime(closedAt))
	f.env.sendReports(t, 2*time.Hour, entry)
	if report := f.stored(); report.State != store.FeedbackReportClosed || report.Count != 2 {
		t.Fatalf("report after an entry at the closing time=%+v", report)
	}
	// One nanosecond after the stored closing time is later and reopens it.
	entry.LastSeen = store.StatedFeedbackReportTime(store.EnvironmentTime(closedAt).Add(time.Nanosecond))
	f.env.sendReports(t, 3*time.Hour, entry)
	report := f.stored()
	if report.State != store.FeedbackReportOpen || !report.InInbox || report.ReopenOrigin == nil || report.ReopenOrigin.Source != store.FeedbackSourceReport {
		t.Fatalf("report after a later entry=%+v", report)
	}
}

func runFeedbackActFixTask(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	fix, later := openActTask(t, x), openActTask(t, x)
	causeA, causeB := mergedActTask(t, x, ""), mergedActTask(t, x, "")
	f.seed(1, causeA.ID, store.FeedbackReportLinkPossible, lineEvidence(feedbackCommit("ab")))
	at := feedbackTime(time.Hour)
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, TaskID: fix.ID, CauseTaskIDs: []string{causeA.ID, causeB.ID}, At: at})
	report := f.stored()
	if report.FixTaskID != fix.ID || report.FixTaskSetBy != actUser.ID || report.FixTaskSetAt == nil || !report.FixTaskSetAt.Equal(at) || report.InInbox {
		t.Fatalf("report after the fix task=%+v", report)
	}
	links := f.links()
	if links[causeA.ID].State != store.FeedbackReportLinkConfirmed || len(links[causeA.ID].Evidence) != 1 || links[causeB.ID].State != store.FeedbackReportLinkConfirmed {
		t.Fatalf("cause links=%+v", links)
	}
	requireActEvent(t, x, store.FeedbackReportFixTaskSetEvent, map[string]any{"report_id": f.report.ID, "environment_id": f.report.EnvironmentID, "task_id": fix.ID, "cause_task_ids": []string{causeA.ID, causeB.ID}})
	// The same fix task with confirmed causes changes nothing.
	f.unchanged(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, TaskID: fix.ID, CauseTaskIDs: []string{causeB.ID}})
	// A later fix task replaces the earlier one.
	laterAt := feedbackTime(2 * time.Hour)
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, TaskID: later.ID, At: laterAt})
	report = f.stored()
	if report.FixTaskID != later.ID || !report.FixTaskSetAt.Equal(laterAt) {
		t.Fatalf("report after a later fix task=%+v", report)
	}
	requireActEvent(t, x, store.FeedbackReportFixTaskSetEvent, map[string]any{"report_id": f.report.ID, "environment_id": f.report.EnvironmentID, "task_id": later.ID, "cause_task_ids": []string{}})
	// The fix task writes nothing on the task.
	stored, err := x.Backend.GetTask(x.Context, later.ID)
	requireOK(t, err)
	if stored.State != later.State || stored.TaskCrateID() != "" {
		t.Fatalf("fix task after the act=%+v", stored)
	}
}

func runFeedbackActFixTaskRefusals(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	fix, open := openActTask(t, x), openActTask(t, x)
	cause := mergedActTask(t, x, "")
	tooMany := make([]string, store.MaxFeedbackReportCauseTasks+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("task-%d", i)
	}
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, TaskID: "task-missing"}, actTargetError(store.FeedbackActTaskNotFound))
	// A cause that is not merged refuses the whole act: the fix task is not
	// recorded and the merged cause is not confirmed.
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, TaskID: fix.ID, CauseTaskIDs: []string{cause.ID, open.ID}}, actTargetError(store.FeedbackActTaskNotMerged))
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, TaskID: fix.ID, CauseTaskIDs: []string{cause.ID, "task-missing"}}, actTargetError(store.FeedbackActTaskNotFound))
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, TaskID: fix.ID, CauseTaskIDs: tooMany}, fieldError("cause_task_ids"))
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, TaskID: fix.ID, CauseTaskIDs: []string{cause.ID, cause.ID}}, fieldError("cause_task_ids"))
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, TaskID: fix.ID, CauseTaskIDs: []string{""}}, fieldError("cause_task_ids"))
	f.refused(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask}, fieldError("task_id"))
	// Exactly 50 distinct merged causes are accepted.
	causes := []string{}
	for range store.MaxFeedbackReportCauseTasks {
		causes = append(causes, mergedActTask(t, x, "").ID)
	}
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, TaskID: fix.ID, CauseTaskIDs: causes})
	if links := f.links(); len(links) != store.MaxFeedbackReportCauseTasks {
		t.Fatalf("links after 50 causes=%d", len(links))
	}
}

func runFeedbackActUnknownReport(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	foreign := foreignContext(t, x)
	foreignActs := store.WithActor(foreign, actUser)
	for _, ctx := range []context.Context{f.ctx, foreignActs} {
		reportID := "rpt-261125-000000"
		if ctx == foreignActs {
			reportID = f.report.ID
		}
		_, err := x.Backend.RecordFeedbackReportAct(ctx, store.FeedbackReportAct{Kind: store.FeedbackReportActReopen, ReportID: reportID, At: feedbackTime(time.Hour)})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("act on %s err=%v, want not found", reportID, err)
		}
		if _, err := x.Backend.GetFeedbackReport(ctx, reportID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("read of %s err=%v, want not found", reportID, err)
		}
	}
	// An act needs a typed actor.
	if _, err := x.Backend.RecordFeedbackReportAct(store.WithActor(x.Context, store.Actor{}), store.FeedbackReportAct{Kind: store.FeedbackReportActReopen, ReportID: f.report.ID, At: feedbackTime(time.Hour)}); err == nil {
		t.Fatal("an act without an actor was accepted")
	}
}

func runFeedbackActObservesOnly(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	cause := mergedActTask(t, x, "")
	fix := openActTask(t, x)
	snapshot := func() string {
		t.Helper()
		tasks, err := x.Backend.ListTasks(x.Context)
		requireOK(t, err)
		events := []core.Event{}
		for _, task := range tasks {
			taskEvents, err := x.Backend.ListEvents(x.Context, task.ID)
			requireOK(t, err)
			events = append(events, taskEvents...)
		}
		raw, err := json.Marshal([]any{tasks, events})
		requireOK(t, err)
		return string(raw)
	}
	before := snapshot()
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, TaskID: cause.ID})
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, TaskID: fix.ID, CauseTaskIDs: []string{cause.ID}})
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, Reason: "done"})
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActReopen})
	if after := snapshot(); after != before {
		t.Fatalf("an act changed a task or a task event:\nbefore %s\nafter  %s", before, after)
	}
}

// runConcurrently runs each function at once and returns their errors.
func runConcurrently(fns ...func() error) []error {
	errs := make([]error, len(fns))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

func runFeedbackActConcurrentSame(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	task := mergedActTask(t, x, "")
	confirm := func() error {
		_, err := x.Backend.RecordFeedbackReportAct(f.ctx, store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: f.report.ID, TaskID: task.ID, At: feedbackTime(time.Hour)})
		return err
	}
	for _, err := range runConcurrently(confirm, confirm) {
		requireOK(t, err)
	}
	if n := countEvents(t, x, store.FeedbackReportLinkConfirmedEvent); n != 1 {
		t.Fatalf("link_confirmed events after two confirmations at once=%d, want 1", n)
	}
	// The stored rows are counted before they are grouped by task, so two
	// rows for one task fail too.
	rows, err := x.Backend.ListFeedbackReportLinks(x.Context, f.report.ID)
	requireOK(t, err)
	if len(rows) != 1 || rows[0].TaskID != task.ID || rows[0].State != store.FeedbackReportLinkConfirmed {
		t.Fatalf("link rows after two confirmations at once=%+v", rows)
	}
	closeReport := func() error {
		_, err := x.Backend.RecordFeedbackReportAct(f.ctx, store.FeedbackReportAct{Kind: store.FeedbackReportActClose, ReportID: f.report.ID, Reason: "done", At: feedbackTime(2 * time.Hour)})
		return err
	}
	errs := runConcurrently(closeReport, closeReport)
	succeeded, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrFeedbackReportClosed):
			refused++
		default:
			t.Fatalf("close at once err=%v", err)
		}
	}
	if succeeded != 1 || refused != 1 || countEvents(t, x, store.FeedbackReportClosedEvent) != 1 {
		t.Fatalf("two closes at once: %d succeeded, %d refused, %d events", succeeded, refused, countEvents(t, x, store.FeedbackReportClosedEvent))
	}
}

func runFeedbackActConcurrentLinking(t *testing.T, x Fixture) {
	for round := range 3 {
		f := newActFixture(t, x, fmt.Sprintf("crash-%d", round))
		task := mergedActTask(t, x, "")
		link := f.seed(1, task.ID, store.FeedbackReportLinkPossible)
		attemptAt := claimedAt(t, x, f.report.ID, feedbackTime(time.Minute))
		errs := runConcurrently(
			func() error {
				_, err := f.act(store.FeedbackReportAct{Kind: store.FeedbackReportActRemoveLink, LinkID: link.ID})
				return err
			},
			func() error {
				_, err := x.Backend.FinishFeedbackReportLinking(x.Context, store.FeedbackReportLinking{ReportID: f.report.ID, EndedAt: feedbackTime(2 * time.Minute), AttemptAt: attemptAt,
					Links: []store.FeedbackReportLinkCandidate{{TaskID: task.ID, Evidence: []store.FeedbackReportEvidence{lineEvidence(feedbackCommit("cd"))}}}})
				return err
			})
		for _, err := range errs {
			requireOK(t, err)
		}
		got := f.links()[task.ID]
		if got.State != store.FeedbackReportLinkRemoved || got.RemovedBy != actUser.ID {
			t.Fatalf("link after a removal and the end of linking at once=%+v, want the removal kept", got)
		}
		if report := f.stored(); report.LinkingEndedAt == nil {
			t.Fatal("linking did not end")
		}
	}
	if countEvents(t, x, store.FeedbackReportLinkRemovedEvent) != 3 || countEvents(t, x, store.FeedbackReportLinkedEvent) != 3 {
		t.Fatalf("events=%+v", reportEvents(t, x))
	}
}

func runFeedbackActConcurrentEntry(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	entry := reportEntry("crash")
	entry.Count = 5
	errs := runConcurrently(
		func() error {
			_, err := f.act(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, Reason: "done", At: feedbackTime(time.Hour)})
			return err
		},
		func() error {
			// The entry's last_seen is before the closing time, so whichever
			// order the two run in, the report ends closed.
			e := entry
			e.FirstSeen, e.LastSeen = feedbackReportTimePtr(0), feedbackReportTimePtr(30*time.Minute)
			return x.Backend.RecordFeedbackReports(x.Context, f.env.reportBatch(2*time.Hour, e))
		})
	for _, err := range errs {
		requireOK(t, err)
	}
	report := f.stored()
	if report.State != store.FeedbackReportClosed || report.Count != 6 || report.CloseReason != "done" {
		t.Fatalf("report after a close and an entry at once=%+v", report)
	}
	if countEvents(t, x, store.FeedbackReportClosedEvent) != 1 || countEvents(t, x, store.FeedbackReportReopenedEvent) != 0 {
		t.Fatalf("events=%+v", reportEvents(t, x))
	}
}

// listIDs reads one page of a list and returns its report IDs and total.
func listIDs(t *testing.T, x Fixture, query store.FeedbackReportQuery) ([]string, int) {
	t.Helper()
	if query.Limit == 0 {
		query.Limit = store.MaxFeedbackReportQueryLimit
	}
	if query.State == "" {
		query.State = store.FeedbackReportOpen
	}
	page, err := x.Backend.ListFeedbackReports(x.Context, query)
	requireOK(t, err)
	ids := make([]string, len(page.Reports))
	for i, report := range page.Reports {
		ids[i] = report.ID
	}
	return ids, page.Total
}

func runFeedbackReportListFilters(t *testing.T, x Fixture) {
	production := newFeedbackEnvironment(t, x, "Production", "watcher")
	staging := newFeedbackEnvironment(t, x, "Staging", "watcher")
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Spring"})
	requireOK(t, err)
	other, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Summer"})
	requireOK(t, err)
	member, outsider := mergedActTask(t, x, crate.ID), mergedActTask(t, x, "")
	// Three reports share one last_seen, so the ID breaks the tie.
	for i, key := range []string{"a", "b", "c"} {
		entry := reportEntry(key)
		entry.FirstSeen, entry.LastSeen = feedbackReportTimePtr(0), feedbackReportTimePtr(10*time.Minute)
		production.sendReports(t, time.Duration(i+1)*time.Hour, entry)
	}
	newest := reportEntry("newest")
	newest.FirstSeen, newest.LastSeen = feedbackReportTimePtr(0), feedbackReportTimePtr(20*time.Minute)
	staging.sendReports(t, time.Hour, newest)
	closed := reportEntry("closed")
	production.sendReports(t, time.Hour, closed)
	memberOnly := reportEntry("member-only")
	memberOnly.FirstSeen, memberOnly.LastSeen = feedbackReportTimePtr(0), feedbackReportTimePtr(time.Minute)
	staging.sendReports(t, 2*time.Hour, memberOnly)
	a, b, c := production.reportByKey(t, "a"), production.reportByKey(t, "b"), production.reportByKey(t, "c")
	n, cl, mo := staging.reportByKey(t, "newest"), production.reportByKey(t, "closed"), staging.reportByKey(t, "member-only")
	ctx := actContext(x)
	act := func(act store.FeedbackReportAct) {
		t.Helper()
		act.At = feedbackTime(5 * time.Hour)
		_, err := x.Backend.RecordFeedbackReportAct(ctx, act)
		requireOK(t, err)
	}
	act(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, ReportID: cl.ID, Reason: "done"})
	// a links the member task and the crate: it belongs to the crate twice
	// and is counted once. b links the outsider, and its member link is
	// removed. c has no link.
	act(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: a.ID, TaskID: member.ID})
	act(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: a.ID, CrateID: crate.ID})
	act(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: b.ID, TaskID: outsider.ID})
	act(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: b.ID, TaskID: member.ID})
	links, err := x.Backend.ListFeedbackReportLinks(x.Context, b.ID)
	requireOK(t, err)
	for _, link := range links {
		if link.TaskID == member.ID {
			act(store.FeedbackReportAct{Kind: store.FeedbackReportActRemoveLink, ReportID: b.ID, LinkID: link.ID})
		}
	}
	act(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: n.ID, CrateID: other.ID})
	act(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: mo.ID, TaskID: member.ID})

	for _, c2 := range []struct {
		name  string
		query store.FeedbackReportQuery
		want  []string
	}{
		{"open, newest last_seen first, ID breaks the tie", store.FeedbackReportQuery{}, append(sortedTie(n.ID, a.ID, b.ID, c.ID), mo.ID)},
		{"closed", store.FeedbackReportQuery{State: store.FeedbackReportClosed}, []string{cl.ID}},
		{"environment", store.FeedbackReportQuery{EnvironmentID: staging.id}, []string{n.ID, mo.ID}},
		{"task: served links only", store.FeedbackReportQuery{TaskID: member.ID}, []string{a.ID, mo.ID}},
		{"task: the outsider", store.FeedbackReportQuery{TaskID: outsider.ID}, []string{b.ID}},
		{"crate: direct and member links, counted once", store.FeedbackReportQuery{CrateID: crate.ID}, []string{a.ID, mo.ID}},
		{"crate: the other crate", store.FeedbackReportQuery{CrateID: other.ID}, []string{n.ID}},
		{"no crate", store.FeedbackReportQuery{NoCrate: true}, sortedTie("", b.ID, c.ID)[1:]},
		{"no crate and a crate select nothing", store.FeedbackReportQuery{NoCrate: true, CrateID: crate.ID}, []string{}},
		{"unknown environment", store.FeedbackReportQuery{EnvironmentID: "env-missing"}, []string{}},
		{"closed and a crate", store.FeedbackReportQuery{State: store.FeedbackReportClosed, CrateID: crate.ID}, []string{}},
	} {
		ids, total := listIDs(t, x, c2.query)
		if !slices.Equal(ids, c2.want) || total != len(c2.want) {
			t.Fatalf("%s: ids=%v total=%d, want %v", c2.name, ids, total, c2.want)
		}
	}
	// Paging: the filters apply before the page and the count, and a page
	// past the end still counts.
	all, _ := listIDs(t, x, store.FeedbackReportQuery{})
	for offset := range len(all) + 2 {
		ids, total := listIDs(t, x, store.FeedbackReportQuery{Limit: 2, Offset: offset})
		want := all[min(offset, len(all)):min(offset+2, len(all))]
		if !slices.Equal(ids, want) || total != len(all) {
			t.Fatalf("page at %d: ids=%v total=%d, want %v of %d", offset, ids, total, want, len(all))
		}
	}
	// The page carries the details of its reports, read with it.
	page, err := x.Backend.ListFeedbackReports(x.Context, store.FeedbackReportQuery{State: store.FeedbackReportOpen, CrateID: crate.ID, Limit: 10})
	requireOK(t, err)
	if len(page.Details.Links[a.ID]) != 2 || len(page.Details.Links[mo.ID]) != 1 || page.Details.Tasks[member.ID].CrateID != crate.ID {
		t.Fatalf("page details=%+v", page.Details)
	}
	// Membership is read at the read: when the member leaves the crate, a
	// still belongs through its direct crate link, and the report linked
	// only to the member no longer belongs.
	_, err = x.Backend.ClearTaskCrate(x.Context, member.ID)
	requireOK(t, err)
	if ids, _ := listIDs(t, x, store.FeedbackReportQuery{CrateID: crate.ID}); !slices.Equal(ids, []string{a.ID}) {
		t.Fatalf("crate list after the member left=%v, want only the report with a direct crate link", ids)
	}
	if ids, _ := listIDs(t, x, store.FeedbackReportQuery{NoCrate: true}); !slices.Contains(ids, mo.ID) {
		t.Fatalf("no_crate list after the member left=%v, want the report linked only to the member", ids)
	}
	if _, err := x.Backend.ListFeedbackReports(x.Context, store.FeedbackReportQuery{State: "all", Limit: 1}); err == nil {
		t.Fatal("a query with an unknown state was accepted")
	}
	if _, err := x.Backend.ListFeedbackReports(x.Context, store.FeedbackReportQuery{State: store.FeedbackReportOpen, Limit: store.MaxFeedbackReportQueryLimit + 1}); err == nil {
		t.Fatal("a query over the page bound was accepted")
	}
}

// sortedTie orders the first ID, which has the newest last_seen, first and
// the rest, which share one last_seen, by ID.
func sortedTie(first string, rest ...string) []string {
	rest = slices.Clone(rest)
	slices.Sort(rest)
	return append([]string{first}, rest...)
}

func runFeedbackReportCrateCounts(t *testing.T, x Fixture) {
	production := newFeedbackEnvironment(t, x, "Production", "watcher")
	staging := newFeedbackEnvironment(t, x, "Staging", "watcher")
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Spring"})
	requireOK(t, err)
	member := mergedActTask(t, x, crate.ID)
	production.sendReports(t, time.Hour, reportEntry("direct"), reportEntry("member"), reportEntry("both"), reportEntry("closed"), reportEntry("removed"), reportEntry("none"))
	staging.sendReports(t, time.Hour, reportEntry("member"))
	ctx := actContext(x)
	act := func(environment feedbackEnvironment, key string, act store.FeedbackReportAct) {
		t.Helper()
		act.ReportID, act.At = environment.reportByKey(t, key).ID, feedbackTime(5*time.Hour)
		_, err := x.Backend.RecordFeedbackReportAct(ctx, act)
		requireOK(t, err)
	}
	act(production, "direct", store.FeedbackReportAct{Kind: store.FeedbackReportActLink, CrateID: crate.ID})
	act(production, "member", store.FeedbackReportAct{Kind: store.FeedbackReportActLink, TaskID: member.ID})
	act(production, "both", store.FeedbackReportAct{Kind: store.FeedbackReportActLink, TaskID: member.ID})
	act(production, "both", store.FeedbackReportAct{Kind: store.FeedbackReportActLink, CrateID: crate.ID})
	act(production, "closed", store.FeedbackReportAct{Kind: store.FeedbackReportActLink, CrateID: crate.ID})
	act(production, "closed", store.FeedbackReportAct{Kind: store.FeedbackReportActClose, Reason: "done"})
	act(production, "removed", store.FeedbackReportAct{Kind: store.FeedbackReportActLink, CrateID: crate.ID})
	removed := production.reportByKey(t, "removed")
	links, err := x.Backend.ListFeedbackReportLinks(x.Context, removed.ID)
	requireOK(t, err)
	act(production, "removed", store.FeedbackReportAct{Kind: store.FeedbackReportActRemoveLink, LinkID: links[0].ID})
	act(staging, "member", store.FeedbackReportAct{Kind: store.FeedbackReportActLink, TaskID: member.ID})
	counts, err := x.Backend.CountCrateOpenFeedbackReports(x.Context, crate.ID)
	requireOK(t, err)
	want := map[string]int{production.id: 3, staging.id: 1}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts=%v, want %v", counts, want)
	}
	// The counts and the crate list agree on what belongs to the crate.
	for environment, count := range want {
		if _, total := listIDs(t, x, store.FeedbackReportQuery{CrateID: crate.ID, EnvironmentID: environment}); total != count {
			t.Fatalf("list of %s=%d, count=%d", environment, total, count)
		}
	}
	empty, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Empty"})
	requireOK(t, err)
	if counts, err := x.Backend.CountCrateOpenFeedbackReports(x.Context, empty.ID); err != nil || len(counts) != 0 {
		t.Fatalf("counts of an empty crate=%v err=%v", counts, err)
	}
	_, err = x.Backend.ClearTaskCrate(x.Context, member.ID)
	requireOK(t, err)
	counts, err = x.Backend.CountCrateOpenFeedbackReports(x.Context, crate.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(counts, map[string]int{production.id: 2}) {
		t.Fatalf("counts after the member left=%v", counts)
	}
}

func runFeedbackReportDetails(t *testing.T, x Fixture) {
	f := newActFixture(t, x, "crash")
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Spring"})
	requireOK(t, err)
	member, fix := mergedActTask(t, x, crate.ID), openActTask(t, x)
	f.seed(1, member.ID, store.FeedbackReportLinkPossible)
	f.seed(2, "task-missing", store.FeedbackReportLinkPossible)
	removedTask := mergedActTask(t, x, "")
	f.seed(3, removedTask.ID, store.FeedbackReportLinkRemoved)
	f.changed(store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, TaskID: fix.ID})
	report := f.stored()
	reports, details, err := x.Backend.ReadFeedbackReports(x.Context, []string{"rpt-261125-000000", report.ID, report.ID})
	requireOK(t, err)
	if len(reports) != 1 || !reflect.DeepEqual(reports[0], report) {
		t.Fatalf("ReadFeedbackReports with a missing and a repeated ID=%+v, want only %+v once", reports, report)
	}
	// One report for each distinct ID, in the order of first occurrence.
	other := newActFixture(t, x, "other")
	ordered, _, err := x.Backend.ReadFeedbackReports(x.Context, []string{other.report.ID, report.ID, other.report.ID})
	requireOK(t, err)
	if len(ordered) != 2 || ordered[0].ID != other.report.ID || ordered[1].ID != report.ID {
		t.Fatalf("ReadFeedbackReports order=%+v", ordered)
	}
	// The links read with the record decode exactly as the link rows do.
	rows, err := x.Backend.ListFeedbackReportLinks(x.Context, report.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(details.Links[report.ID], rows) {
		t.Fatalf("view links=%+v\nlink rows=%+v", details.Links[report.ID], rows)
	}
	if len(details.Links[report.ID]) != 3 || details.Notes[report.ID] == nil || len(details.Notes[report.ID]) != 0 {
		t.Fatalf("details=%+v", details)
	}
	if ref := details.Tasks[member.ID]; ref.Title != member.Title || ref.CrateID != crate.ID || ref.CrateName != "Spring" {
		t.Fatalf("member ref=%+v", ref)
	}
	if ref := details.Tasks[fix.ID]; ref.Title != fix.Title || ref.CrateID != "" {
		t.Fatalf("fix ref=%+v", ref)
	}
	if _, found := details.Tasks["task-missing"]; found {
		t.Fatal("a task the workspace does not hold has a ref")
	}
	served := 0
	for _, link := range details.Links[report.ID] {
		if store.FeedbackReportServed(link, details.Tasks, details.Crates) {
			served++
		}
	}
	if served != 1 {
		t.Fatalf("served links=%d, want only the member link", served)
	}
	read, readDetails, err := x.Backend.ReadFeedbackReport(x.Context, report.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(read, report) || !reflect.DeepEqual(readDetails, details) {
		t.Fatalf("ReadFeedbackReport=%+v %+v, want %+v %+v", read, readDetails, report, details)
	}
	if _, _, err := x.Backend.ReadFeedbackReport(x.Context, "rpt-261125-000000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ReadFeedbackReport of a missing report err=%v", err)
	}
	if none, empty, err := x.Backend.ReadFeedbackReports(x.Context, nil); err != nil || len(none) != 0 || len(empty.Links) != 0 {
		t.Fatalf("read of no report=%+v %+v err=%v", none, empty, err)
	}
	// A list page carries the same record and details as the read of one
	// report.
	page, err := x.Backend.ListFeedbackReports(x.Context, store.FeedbackReportQuery{State: store.FeedbackReportOpen, TaskID: member.ID, Limit: 5})
	requireOK(t, err)
	if len(page.Reports) != 1 || !reflect.DeepEqual(page.Reports[0], report) || !reflect.DeepEqual(page.Details.Links[report.ID], rows) ||
		!reflect.DeepEqual(page.Details.Tasks[fix.ID], details.Tasks[fix.ID]) || !reflect.DeepEqual(page.Details.Tasks[member.ID], details.Tasks[member.ID]) {
		t.Fatalf("page=%+v, want the report and its details", page)
	}
}

// readUntil runs read repeatedly until write returns, and once more after,
// and fails on the first error either returns.
func readUntil(t *testing.T, write func() error, read func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- write() }()
	for {
		select {
		case err := <-done:
			requireOK(t, err)
			requireOK(t, read())
			return
		default:
			if err := read(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// runFeedbackReadTaskFilterConcurrent removes the links of many reports to
// one task while lists filtered by that task run. Every report a page serves
// holds, in the details read with it, a served link to the task: the filter
// and the projected links come from one view (component-feedback-reports
// invariant 21).
func runFeedbackReadTaskFilterConcurrent(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	task := mergedActTask(t, x, "")
	ctx := actContext(x)
	entries := []store.FeedbackReportEntry{}
	for i := range 30 {
		entries = append(entries, reportEntry(fmt.Sprintf("crash-%02d", i)))
	}
	e.sendReports(t, 0, entries...)
	links := []store.FeedbackReportAct{}
	for _, report := range e.reports(t) {
		_, err := x.Backend.RecordFeedbackReportAct(ctx, store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: report.ID, TaskID: task.ID, At: feedbackTime(time.Hour)})
		requireOK(t, err)
		links = append(links, store.FeedbackReportAct{Kind: store.FeedbackReportActRemoveLink, ReportID: report.ID, LinkID: store.FeedbackReportLinkID(report.ID, 1), At: feedbackTime(2 * time.Hour)})
	}
	readUntil(t, func() error {
		for _, act := range links {
			if _, err := x.Backend.RecordFeedbackReportAct(ctx, act); err != nil {
				return err
			}
		}
		return nil
	}, func() error {
		page, err := x.Backend.ListFeedbackReports(x.Context, store.FeedbackReportQuery{State: store.FeedbackReportOpen, TaskID: task.ID, Limit: 100})
		if err != nil {
			return err
		}
		if len(page.Reports) != page.Total {
			return fmt.Errorf("page of %d reports with total %d", len(page.Reports), page.Total)
		}
		for _, report := range page.Reports {
			if !slices.ContainsFunc(page.Details.Links[report.ID], func(link store.FeedbackReportLink) bool {
				return link.TaskID == task.ID && store.FeedbackReportServed(link, page.Details.Tasks, page.Details.Crates)
			}) {
				return fmt.Errorf("report %s is on a page filtered by task %s without a served link to it: %+v", report.ID, task.ID, page.Details.Links[report.ID])
			}
		}
		return nil
	})
	if ids, total := listIDs(t, x, store.FeedbackReportQuery{TaskID: task.ID}); len(ids) != 0 || total != 0 {
		t.Fatalf("task list after every removal=%v total=%d", ids, total)
	}
}

// runFeedbackReadFixTaskConcurrent records a fix task with one cause on many
// reports while the reports are read. A read that shows the cause link
// confirmed shows the fix task, and the reverse: the fix task and the cause
// link of one act are read together (component-feedback-reports invariant
// 18).
func runFeedbackReadFixTaskConcurrent(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	fix, cause := openActTask(t, x), mergedActTask(t, x, "")
	ctx := actContext(x)
	entries := []store.FeedbackReportEntry{}
	for i := range 30 {
		entries = append(entries, reportEntry(fmt.Sprintf("crash-%02d", i)))
	}
	e.sendReports(t, 0, entries...)
	reports := e.reports(t)
	ids := make([]string, len(reports))
	for i, report := range reports {
		ids[i] = report.ID
	}
	check := func(report store.FeedbackReport, details store.FeedbackReportDetails) error {
		confirmed := slices.ContainsFunc(details.Links[report.ID], func(link store.FeedbackReportLink) bool {
			return link.TaskID == cause.ID && link.State == store.FeedbackReportLinkConfirmed
		})
		if confirmed != (report.FixTaskID == fix.ID) {
			return fmt.Errorf("report %s read with fix task %q and cause link confirmed=%t", report.ID, report.FixTaskID, confirmed)
		}
		if report.FixTaskID == fix.ID {
			if _, ok := details.Tasks[fix.ID]; !ok {
				return fmt.Errorf("report %s read with fix task %s and no task record", report.ID, fix.ID)
			}
		}
		return nil
	}
	readUntil(t, func() error {
		for _, id := range ids {
			if _, err := x.Backend.RecordFeedbackReportAct(ctx, store.FeedbackReportAct{Kind: store.FeedbackReportActFixTask, ReportID: id, TaskID: fix.ID, CauseTaskIDs: []string{cause.ID}, At: feedbackTime(time.Hour)}); err != nil {
				return err
			}
		}
		return nil
	}, func() error {
		read, details, err := x.Backend.ReadFeedbackReports(x.Context, ids)
		if err != nil {
			return err
		}
		for _, report := range read {
			if err := check(report, details); err != nil {
				return err
			}
		}
		page, err := x.Backend.ListFeedbackReports(x.Context, store.FeedbackReportQuery{State: store.FeedbackReportOpen, Limit: 100})
		if err != nil {
			return err
		}
		for _, report := range page.Reports {
			if err := check(report, page.Details); err != nil {
				return err
			}
		}
		one, oneDetails, err := x.Backend.ReadFeedbackReport(x.Context, ids[len(ids)-1])
		if err != nil {
			return err
		}
		return check(one, oneDetails)
	})
}

// runFeedbackReadCrateConcurrent moves the member task of a crate in and
// out of the crate while lists filtered by the crate run. Every report a
// page serves belongs to the crate by the links and the membership read
// with it (component-feedback-reports invariant 21).
func runFeedbackReadCrateConcurrent(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Spring"})
	requireOK(t, err)
	member := mergedActTask(t, x, crate.ID)
	ctx := actContext(x)
	entries := []store.FeedbackReportEntry{}
	for i := range 10 {
		entries = append(entries, reportEntry(fmt.Sprintf("crash-%02d", i)))
	}
	e.sendReports(t, 0, entries...)
	for _, report := range e.reports(t) {
		_, err := x.Backend.RecordFeedbackReportAct(ctx, store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: report.ID, TaskID: member.ID, At: feedbackTime(time.Hour)})
		requireOK(t, err)
	}
	readUntil(t, func() error {
		for range 10 {
			if _, err := x.Backend.ClearTaskCrate(x.Context, member.ID); err != nil {
				return err
			}
			if _, err := x.Backend.SetTaskCrate(x.Context, member.ID, crate.ID); err != nil {
				return err
			}
		}
		return nil
	}, func() error {
		page, err := x.Backend.ListFeedbackReports(x.Context, store.FeedbackReportQuery{State: store.FeedbackReportOpen, CrateID: crate.ID, Limit: 100})
		if err != nil {
			return err
		}
		if len(page.Reports) != page.Total {
			return fmt.Errorf("page of %d reports with total %d", len(page.Reports), page.Total)
		}
		for _, report := range page.Reports {
			if !slices.ContainsFunc(page.Details.Links[report.ID], func(link store.FeedbackReportLink) bool {
				return store.FeedbackReportInCrate(link, page.Details.Tasks, page.Details.Crates, crate.ID)
			}) {
				return fmt.Errorf("report %s is on the crate page without belonging to the crate by its details", report.ID)
			}
		}
		return nil
	})
}

// runFeedbackReadTotalConcurrent closes reports while open lists run. A page
// that starts at the first report and holds every selected report serves
// as many reports as its total.
func runFeedbackReadTotalConcurrent(t *testing.T, x Fixture) {
	e := newFeedbackEnvironment(t, x, "Production", "watcher")
	ctx := actContext(x)
	entries := []store.FeedbackReportEntry{}
	for i := range 30 {
		entries = append(entries, reportEntry(fmt.Sprintf("crash-%02d", i)))
	}
	e.sendReports(t, 0, entries...)
	reports := e.reports(t)
	readUntil(t, func() error {
		for _, report := range reports {
			if _, err := x.Backend.RecordFeedbackReportAct(ctx, store.FeedbackReportAct{Kind: store.FeedbackReportActClose, ReportID: report.ID, Reason: "done", At: feedbackTime(time.Hour)}); err != nil {
				return err
			}
		}
		return nil
	}, func() error {
		page, err := x.Backend.ListFeedbackReports(x.Context, store.FeedbackReportQuery{State: store.FeedbackReportOpen, Limit: 100})
		if err != nil {
			return err
		}
		if len(page.Reports) != page.Total {
			return fmt.Errorf("page of %d reports with total %d", len(page.Reports), page.Total)
		}
		for _, report := range page.Reports {
			if report.State != store.FeedbackReportOpen {
				return fmt.Errorf("closed report %s on an open page", report.ID)
			}
		}
		// A page past the end still counts in the same view.
		past, err := x.Backend.ListFeedbackReports(x.Context, store.FeedbackReportQuery{State: store.FeedbackReportClosed, Limit: 1, Offset: 1000})
		if err != nil {
			return err
		}
		if len(past.Reports) != 0 || past.Total > len(reports) {
			return fmt.Errorf("page past the end=%d reports, total %d", len(past.Reports), past.Total)
		}
		return nil
	})
}

// feedbackReadWorkRows bounds the rows one read or act of this case may
// read: on a database every row of every statement, the claim and the lock
// included, and in memory every record, link, and note copied out. The
// case's history holds more than eight times as many rows.
const feedbackReadWorkRows = 30

// runFeedbackReportReadWork holds every read and act of reports to work the
// page bounds, not the workspace's history (component-feedback-reports
// invariants 21, 23, and 24; req-production-feedback AC-10.2): in a
// workspace of 120 reports, 120 links, 30 tasks, and 21 crates, a page of
// two under each filter, the read of one report, a crate's counts, and an
// act each read at most feedbackReadWorkRows rows, and a list over its
// RefuseAbove reads its count and no report. A database backend counts with
// its read ledger, and the volatile backend counts the records, links, and
// notes it copies out.
func runFeedbackReportReadWork(t *testing.T, x Fixture) {
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: "Spring"})
	requireOK(t, err)
	for i := range 20 {
		_, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: fmt.Sprintf("Other %02d", i)})
		requireOK(t, err)
	}
	tasks := []core.Task{}
	for i := range 30 {
		crateID := ""
		if i%2 == 0 {
			crateID = crate.ID
		}
		tasks = append(tasks, mergedActTask(t, x, crateID))
	}
	environments := []feedbackEnvironment{}
	reports := []store.FeedbackReport{}
	for i := range 3 {
		e := newFeedbackEnvironment(t, x, fmt.Sprintf("Production %d", i), "watcher")
		e.fillReports(t, time.Hour, "crash", 40)
		environments = append(environments, e)
		reports = append(reports, e.reports(t)...)
	}
	at := feedbackTime(2 * time.Hour)
	for i, report := range reports {
		x.SeedFeedbackReportLink(t, x.Context, store.FeedbackReportLink{ID: store.FeedbackReportLinkID(report.ID, 1), ReportID: report.ID,
			TaskID: tasks[i%len(tasks)].ID, State: store.FeedbackReportLinkPossible, Evidence: []store.FeedbackReportEvidence{}, CreatedAt: at, CreatedBy: store.SystemActor().ID})
	}
	drain := func() (uint64, bool) {
		switch {
		case x.DrainReadRows != nil:
			var rows uint64
			for _, count := range x.DrainReadRows() {
				rows += count
			}
			return rows, true
		case x.DrainFeedbackReportReadCopies != nil:
			return x.DrainFeedbackReportReadCopies(), true
		}
		return 0, false
	}
	measureWithin := func(name string, bound uint64, work func()) {
		t.Helper()
		drain()
		work()
		if rows, counted := drain(); counted && rows > bound {
			t.Fatalf("%s read %d rows, want at most %d whatever the history", name, rows, bound)
		}
	}
	measure := func(name string, work func()) {
		t.Helper()
		measureWithin(name, feedbackReadWorkRows, work)
	}
	page := func(name string, query store.FeedbackReportQuery, total int) {
		t.Helper()
		measure(name, func() {
			query.State, query.Limit = store.FeedbackReportOpen, 2
			listed, err := x.Backend.ListFeedbackReports(x.Context, query)
			requireOK(t, err)
			if listed.Total != total || len(listed.Reports) != 2 || len(listed.Details.Links) != 2 {
				t.Fatalf("%s page=%d reports, %d with links, total %d, want 2 of %d", name, len(listed.Reports), len(listed.Details.Links), listed.Total, total)
			}
			for _, report := range listed.Reports {
				if !store.FeedbackReportMatches(query, listed.Details.Links[report.ID], listed.Details.Tasks, listed.Details.Crates) {
					t.Fatalf("%s page holds report %s outside its filters", name, report.ID)
				}
			}
		})
	}
	page("the open list", store.FeedbackReportQuery{}, 120)
	page("an environment's list", store.FeedbackReportQuery{EnvironmentID: environments[1].id}, 40)
	page("a task's list", store.FeedbackReportQuery{TaskID: tasks[0].ID}, 4)
	page("a crate's list", store.FeedbackReportQuery{CrateID: crate.ID}, 60)
	page("the list of no crate", store.FeedbackReportQuery{NoCrate: true}, 60)
	// A refused list reads its count and the claim: a few rows on a
	// database, and no record, link, or note copied in memory.
	refusedRows := uint64(8)
	if x.DrainReadRows == nil {
		refusedRows = 0
	}
	measureWithin("a refused list", refusedRows, func() {
		query := store.FeedbackReportQuery{State: store.FeedbackReportOpen, Limit: store.MaxFeedbackReportQueryLimit, RefuseAbove: 10}
		listed, err := x.Backend.ListFeedbackReports(x.Context, query)
		requireOK(t, err)
		if !listed.Refused(query) || listed.Total != 120 || len(listed.Reports) != 0 || len(listed.Details.Links) != 0 {
			t.Fatalf("refused list=%d reports, %d with links, total %d", len(listed.Reports), len(listed.Details.Links), listed.Total)
		}
	})
	measure("a list under its RefuseAbove", func() {
		query := store.FeedbackReportQuery{State: store.FeedbackReportOpen, TaskID: tasks[1].ID, Limit: 2, RefuseAbove: 10}
		listed, err := x.Backend.ListFeedbackReports(x.Context, query)
		requireOK(t, err)
		if listed.Refused(query) || listed.Total != 4 || len(listed.Reports) != 2 {
			t.Fatalf("list under its RefuseAbove=%d reports, total %d", len(listed.Reports), listed.Total)
		}
	})
	measure("the read of one report", func() {
		read, details, err := x.Backend.ReadFeedbackReport(x.Context, reports[7].ID)
		requireOK(t, err)
		if read.ID != reports[7].ID || len(details.Links) != 1 || len(details.Links[read.ID]) != 1 {
			t.Fatalf("read=%s with links %+v", read.ID, details.Links)
		}
	})
	measure("a crate's counts", func() {
		counts, err := x.Backend.CountCrateOpenFeedbackReports(x.Context, crate.ID)
		requireOK(t, err)
		want := map[string]int{}
		for _, e := range environments {
			want[e.id] = 20
		}
		if !reflect.DeepEqual(counts, want) {
			t.Fatalf("counts=%v, want %v", counts, want)
		}
	})
	measure("an act", func() {
		result, err := x.Backend.RecordFeedbackReportAct(actContext(x), store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: reports[3].ID, TaskID: tasks[5].ID, At: feedbackTime(3 * time.Hour)})
		requireOK(t, err)
		if !result.Changed {
			t.Fatal("the act changed nothing")
		}
	})
}
