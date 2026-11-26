package storetest

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runFeedbackStatus holds every backend to the storage rules of
// component-production-feedback invariants 7, 8, and 14 and the merged-task
// read of feature-production-feedback-live FB-3 (req-production-feedback
// AC-1.4 and REQ-3). Each case starts from a fresh workspace.
func runFeedbackStatus(t *testing.T, factory Factory) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, Fixture)
	}{
		{"ResolutionClaims", runFeedbackResolutionClaims},
		{"ResolvedAnswerIsKept", runFeedbackResolvedKept},
		{"Inputs", runFeedbackInputs},
		{"Comparisons", runFeedbackComparisons},
		{"Admission", runFeedbackAdmission},
		{"AdmissionPutsTriedPairsLast", runFeedbackAdmissionFairness},
		{"BranchHeads", runFeedbackBranchHeads},
		{"MergedTasks", runFeedbackMergedTasks},
		{"Isolation", runFeedbackStatusIsolation},
		{"ObservesOnly", runFeedbackStatusObservesOnly},
		{"ConcurrentClaimsAndAdmissions", runFeedbackStatusConcurrent},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, factory.fresh(t, nil)) })
	}
}

// feedbackCommit is a full commit ID built from a short seed.
func feedbackCommit(seed string) string {
	return (seed + strings.Repeat("0", 40))[:40]
}

func feedbackRef(environmentID, service, commit string) store.FeedbackVersionRef {
	return store.FeedbackVersionRef{EnvironmentID: environmentID, Service: service, FeedbackVersion: store.FeedbackVersion{Commit: commit}}
}

// runFeedbackResolutionClaims proves the 10-minute rule: a claim needs a
// later signal at least 10 minutes after the prior attempt, or after a later
// time a forge set, and a resolved version is never claimed again
// (invariant 8; req-production-feedback AC-1.4).
func runFeedbackResolutionClaims(t *testing.T, x Fixture) {
	environment := newFeedbackEnvironment(t, x, "Production", "watcher")
	ref := feedbackRef(environment.id, "api", "abc1234")
	claim := func(signal, now time.Duration) bool {
		t.Helper()
		claimed, err := x.Backend.ClaimFeedbackResolution(x.Context, ref, feedbackTime(signal), feedbackTime(now))
		requireOK(t, err)
		return claimed
	}
	if !claim(0, time.Second) {
		t.Fatal("first claim refused")
	}
	if claim(time.Second, 2*time.Second) {
		t.Fatal("a second claim inside the attempt won")
	}
	requireOK(t, x.Backend.RecordFeedbackResolution(x.Context, store.FeedbackResolution{FeedbackVersionRef: ref, Reason: "forge_request", AttemptedAt: feedbackTime(time.Second)}))
	if claim(10*time.Minute, 10*time.Minute) {
		t.Fatal("a signal before 10 minutes after the attempt claimed it")
	}
	if !claim(10*time.Minute+time.Second, 10*time.Minute+2*time.Second) {
		t.Fatal("a signal 10 minutes after the attempt was refused")
	}
	// A forge that names a later time holds the version until then.
	requireOK(t, x.Backend.RecordFeedbackResolution(x.Context, store.FeedbackResolution{FeedbackVersionRef: ref, Reason: "forge_rate_limited", AttemptedAt: feedbackTime(11 * time.Minute), NextAttemptAt: feedbackTime(time.Hour)}))
	if claim(30*time.Minute, 30*time.Minute) {
		t.Fatal("a signal before the forge's retry time claimed it")
	}
	if !claim(time.Hour, time.Hour) {
		t.Fatal("a signal at the forge's retry time was refused")
	}
	commit := feedbackCommit("abc1234")
	requireOK(t, x.Backend.RecordFeedbackResolution(x.Context, store.FeedbackResolution{FeedbackVersionRef: ref, ResolvedRepository: "conveyor", ResolvedCommit: commit, AttemptedAt: feedbackTime(time.Hour)}))
	if claim(48*time.Hour, 48*time.Hour) {
		t.Fatal("a resolved version was claimed again")
	}
	inputs, err := x.Backend.ReadFeedbackInputs(x.Context, []string{environment.id})
	requireOK(t, err)
	if len(inputs.Resolutions) != 1 || !inputs.Resolutions[0].Resolved() || inputs.Resolutions[0].ResolvedCommit != commit || inputs.Resolutions[0].ResolvedRepository != "conveyor" || inputs.Resolutions[0].Reason != "" {
		t.Fatalf("resolutions=%+v", inputs.Resolutions)
	}
	// Refused outcomes record nothing.
	for name, bad := range map[string]store.FeedbackResolution{
		"short commit":  {FeedbackVersionRef: feedbackRef(environment.id, "web", "abc1234"), ResolvedRepository: "conveyor", ResolvedCommit: "abc1234", AttemptedAt: feedbackTime(0)},
		"no reason":     {FeedbackVersionRef: feedbackRef(environment.id, "web", "abc1234"), AttemptedAt: feedbackTime(0)},
		"no service":    {FeedbackVersionRef: feedbackRef(environment.id, "", "abc1234"), Reason: "no_match", AttemptedAt: feedbackTime(0)},
		"no repository": {FeedbackVersionRef: feedbackRef(environment.id, "web", "abc1234"), ResolvedCommit: commit, AttemptedAt: feedbackTime(0)},
	} {
		if err := x.Backend.RecordFeedbackResolution(x.Context, bad); err == nil {
			t.Fatalf("%s: recorded", name)
		}
	}
}

// runFeedbackResolvedKept proves that a resolved answer never changes and
// marks the version's running and deploy rows resolved.
func runFeedbackResolvedKept(t *testing.T, x Fixture) {
	environment := newFeedbackEnvironment(t, x, "Production", "watcher")
	environment.send(t, 0, feedbackService("api", "abc1234", "@v1"))
	ref := feedbackRef(environment.id, "api", "abc1234")
	commit := feedbackCommit("abc1234")
	requireOK(t, x.Backend.RecordFeedbackResolution(x.Context, store.FeedbackResolution{FeedbackVersionRef: ref, ResolvedRepository: "conveyor", ResolvedCommit: commit, AttemptedAt: feedbackTime(time.Second)}))
	requireOK(t, x.Backend.RecordFeedbackResolution(x.Context, store.FeedbackResolution{FeedbackVersionRef: ref, Reason: "no_match", AttemptedAt: feedbackTime(time.Hour)}))
	requireOK(t, x.Backend.RecordFeedbackResolution(x.Context, store.FeedbackResolution{FeedbackVersionRef: ref, ResolvedRepository: "app", ResolvedCommit: feedbackCommit("def"), AttemptedAt: feedbackTime(time.Hour)}))
	inputs, err := x.Backend.ReadFeedbackInputs(x.Context, []string{environment.id})
	requireOK(t, err)
	if len(inputs.Resolutions) != 1 || inputs.Resolutions[0].ResolvedCommit != commit || inputs.Resolutions[0].ResolvedRepository != "conveyor" || !inputs.Resolutions[0].AttemptedAt.Equal(feedbackTime(time.Second)) {
		t.Fatalf("resolutions=%+v", inputs.Resolutions)
	}
	if len(inputs.Running[environment.id]) != 2 || len(inputs.Deploys[environment.id]) != 2 {
		t.Fatalf("running=%+v deploys=%+v, want both versions in each", inputs.Running[environment.id], inputs.Deploys[environment.id])
	}
	for _, running := range inputs.Running[environment.id] {
		if running.Resolved != (running.Commit == "abc1234") {
			t.Fatalf("running=%+v", running)
		}
	}
	for _, deploy := range inputs.Deploys[environment.id] {
		if deploy.Resolved != (deploy.Commit == "abc1234") {
			t.Fatalf("deploy=%+v", deploy)
		}
	}
}

// runFeedbackInputs proves the batched read returns the newest signal,
// running versions, deploys, and resolutions of exactly the named
// environments.
func runFeedbackInputs(t *testing.T, x Fixture) {
	production := newFeedbackEnvironment(t, x, "Production", "watcher")
	staging := newFeedbackEnvironment(t, x, "Staging", "watcher")
	other := newFeedbackEnvironment(t, x, "Other", "watcher")
	production.send(t, 0, feedbackService("api", "abc1234"), feedbackService("web", "@v1"))
	production.send(t, time.Minute, feedbackService("api", "def5678"))
	staging.send(t, 2*time.Minute, feedbackService("api", "abc1234"))
	other.send(t, 3*time.Minute, feedbackService("api", "abc1234"))
	requireOK(t, x.Backend.RecordFeedbackResolution(x.Context, store.FeedbackResolution{FeedbackVersionRef: feedbackRef(other.id, "api", "abc1234"), Reason: "no_match", AttemptedAt: feedbackTime(3 * time.Minute)}))
	requireOK(t, x.Backend.RecordFeedbackResolution(x.Context, store.FeedbackResolution{FeedbackVersionRef: feedbackRef(staging.id, "api", "abc1234"), Reason: "no_match", AttemptedAt: feedbackTime(2 * time.Minute)}))
	inputs, err := x.Backend.ReadFeedbackInputs(x.Context, []string{production.id, staging.id, "env-missing"})
	requireOK(t, err)
	if len(inputs.Signals) != 2 || !inputs.Signals[production.id].LastSignalAt.Equal(feedbackTime(time.Minute)) || !inputs.Signals[staging.id].LastSignalAt.Equal(feedbackTime(2*time.Minute)) {
		t.Fatalf("signals=%+v", inputs.Signals)
	}
	if !slices.Equal(feedbackRendered(inputs.Running[production.id]), []string{"api=def5678", "web=@v1"}) || !slices.Equal(feedbackRendered(inputs.Running[staging.id]), []string{"api=abc1234"}) {
		t.Fatalf("running=%+v", inputs.Running)
	}
	if len(inputs.Deploys[production.id]) != 3 || len(inputs.Deploys[staging.id]) != 1 || len(inputs.Deploys[other.id]) != 0 || len(inputs.Running[other.id]) != 0 {
		t.Fatalf("deploys=%+v", inputs.Deploys)
	}
	if len(inputs.Resolutions) != 1 || inputs.Resolutions[0].EnvironmentID != staging.id || inputs.Resolutions[0].Reason != "no_match" || !inputs.Resolutions[0].NextAttemptAt.Equal(feedbackTime(12*time.Minute)) {
		t.Fatalf("resolutions=%+v", inputs.Resolutions)
	}
	empty, err := x.Backend.ReadFeedbackInputs(x.Context, nil)
	requireOK(t, err)
	if len(empty.Signals) != 0 || len(empty.Running) != 0 || len(empty.Deploys) != 0 || len(empty.Resolutions) != 0 {
		t.Fatalf("inputs of no environment=%+v", empty)
	}
}

func feedbackRendered(running []store.FeedbackRunningVersion) []string {
	out := []string{}
	for _, version := range running {
		out = append(out, version.Service+"="+renderFeedbackVersion(version.FeedbackVersion))
	}
	return out
}

// runFeedbackComparisons proves a stored answer is kept and never replaced,
// keyed by canonical repository, base, and head (invariant 14).
func runFeedbackComparisons(t *testing.T, x Fixture) {
	base, head, other := feedbackCommit("a1"), feedbackCommit("b2"), feedbackCommit("c3")
	key := store.FeedbackComparisonKey{Repository: "Acme/App", Base: base, Head: head}
	requireOK(t, x.Backend.RecordFeedbackComparisons(x.Context, []store.FeedbackComparison{{FeedbackComparisonKey: key, Status: store.FeedbackCompareAhead, ComparedAt: feedbackTime(0)}}))
	requireOK(t, x.Backend.RecordFeedbackComparisons(x.Context, []store.FeedbackComparison{
		{FeedbackComparisonKey: key, Status: store.FeedbackCompareDiverged, ComparedAt: feedbackTime(time.Hour)},
		{FeedbackComparisonKey: store.FeedbackComparisonKey{Repository: "acme/app", Base: other, Head: head}, Status: store.FeedbackCompareMissing, ComparedAt: feedbackTime(time.Hour)},
	}))
	got, err := x.Backend.ListFeedbackComparisons(x.Context, []store.FeedbackComparisonKey{
		{Repository: "ACME/app", Base: base, Head: head}, {Repository: "acme/app", Base: other, Head: head}, {Repository: "acme/app", Base: head, Head: base}, {Repository: "acme/other", Base: base, Head: head},
	})
	requireOK(t, err)
	if len(got) != 2 || got[0].Repository != "acme/app" || got[0].Base != base || got[0].Status != store.FeedbackCompareAhead || !got[0].ComparedAt.Equal(feedbackTime(0)) || got[1].Base != other || got[1].Status != store.FeedbackCompareMissing {
		t.Fatalf("comparisons=%+v", got)
	}
	if !got[0].Contains() || got[1].Contains() {
		t.Fatalf("contains=%v %v", got[0].Contains(), got[1].Contains())
	}
	for name, bad := range map[string]store.FeedbackComparison{
		"short base":     {FeedbackComparisonKey: store.FeedbackComparisonKey{Repository: "acme/app", Base: "abc1234", Head: head}, Status: store.FeedbackCompareAhead},
		"unknown status": {FeedbackComparisonKey: store.FeedbackComparisonKey{Repository: "acme/app", Base: base, Head: other}, Status: "maybe"},
		"no repository":  {FeedbackComparisonKey: store.FeedbackComparisonKey{Base: base, Head: other}, Status: store.FeedbackCompareAhead},
	} {
		if err := x.Backend.RecordFeedbackComparisons(x.Context, []store.FeedbackComparison{bad}); err == nil {
			t.Fatalf("%s: recorded", name)
		}
	}
	if none, err := x.Backend.ListFeedbackComparisons(x.Context, nil); err != nil || len(none) != 0 {
		t.Fatalf("comparisons of no key=%+v err=%v", none, err)
	}
}

// runFeedbackAdmission proves the workspace allowance of 60 comparisons in
// any minute, the in-flight rule, and that a stored answer spends nothing.
func runFeedbackAdmission(t *testing.T, x Fixture) {
	head := feedbackCommit("ff")
	keys := make([]store.FeedbackComparisonKey, 70)
	for i := range keys {
		keys[i] = store.FeedbackComparisonKey{Repository: "acme/app", Base: feedbackCommit(fmt.Sprintf("%02x", i)), Head: head}
	}
	requireOK(t, x.Backend.RecordFeedbackComparisons(x.Context, []store.FeedbackComparison{{FeedbackComparisonKey: keys[0], Status: store.FeedbackCompareAhead, ComparedAt: feedbackTime(0)}}))
	admitted, err := x.Backend.AdmitFeedbackComparisons(x.Context, keys[:30], feedbackTime(0), 0)
	requireOK(t, err)
	if len(admitted) != 29 || admitted[0] != keys[1] || admitted[28] != keys[29] {
		t.Fatalf("first admission=%d %v", len(admitted), admitted)
	}
	// In flight: the same pairs are not admitted again, and the allowance
	// left is 31.
	admitted, err = x.Backend.AdmitFeedbackComparisons(x.Context, keys, feedbackTime(10*time.Second), 0)
	requireOK(t, err)
	if len(admitted) != 31 || admitted[0] != keys[30] || admitted[30] != keys[60] {
		t.Fatalf("second admission=%d %v", len(admitted), admitted)
	}
	if admitted, err = x.Backend.AdmitFeedbackComparisons(x.Context, keys[61:], feedbackTime(50*time.Second), 0); err != nil || len(admitted) != 0 {
		t.Fatalf("admission past the allowance=%v err=%v", admitted, err)
	}
	// A minute after the first 29, they no longer count.
	if admitted, err = x.Backend.AdmitFeedbackComparisons(x.Context, keys[61:], feedbackTime(61*time.Second), 0); err != nil || len(admitted) != 9 {
		t.Fatalf("admission after the window=%v err=%v", admitted, err)
	}
	// After the lease an unanswered pair can be admitted again; an answered
	// one never is.
	requireOK(t, x.Backend.RecordFeedbackComparisons(x.Context, []store.FeedbackComparison{{FeedbackComparisonKey: keys[2], Status: store.FeedbackCompareBehind, ComparedAt: feedbackTime(0)}}))
	admitted, err = x.Backend.AdmitFeedbackComparisons(x.Context, keys[:4], feedbackTime(5*time.Minute), 0)
	requireOK(t, err)
	if !slices.Equal(admitted, []store.FeedbackComparisonKey{keys[1], keys[3]}) {
		t.Fatalf("admission after the lease=%v", admitted)
	}
	// most caps one admission, so a caller can charge each request when it
	// starts.
	if admitted, err = x.Backend.AdmitFeedbackComparisons(x.Context, keys[4:8], feedbackTime(5*time.Minute+time.Second), 1); err != nil || len(admitted) != 1 {
		t.Fatalf("admission of one=%v err=%v", admitted, err)
	}
	// A branch read is admitted by its branch name.
	branch := store.FeedbackComparisonKey{Repository: "acme/app", Head: "main"}
	if admitted, err = x.Backend.AdmitFeedbackComparisons(x.Context, []store.FeedbackComparisonKey{branch, branch}, feedbackTime(6*time.Minute), 0); err != nil || len(admitted) != 1 {
		t.Fatalf("branch admission=%v err=%v", admitted, err)
	}
}

// runFeedbackBranchHeads proves that only a newer observation replaces the
// stored one.
func runFeedbackBranchHeads(t *testing.T, x Fixture) {
	first, second := feedbackCommit("aa"), feedbackCommit("bb")
	requireOK(t, x.Backend.RecordFeedbackBranchHead(x.Context, store.FeedbackBranchHead{Repository: "Acme/App", Branch: "main", Commit: first, ObservedAt: feedbackTime(time.Minute)}))
	requireOK(t, x.Backend.RecordFeedbackBranchHead(x.Context, store.FeedbackBranchHead{Repository: "acme/app", Branch: "main", Commit: second, ObservedAt: feedbackTime(0)}))
	requireOK(t, x.Backend.RecordFeedbackBranchHead(x.Context, store.FeedbackBranchHead{Repository: "acme/app", Branch: "release", Commit: second, ObservedAt: feedbackTime(0)}))
	heads, err := x.Backend.ListFeedbackBranchHeads(x.Context)
	requireOK(t, err)
	if len(heads) != 2 || heads[0].Branch != "main" || heads[0].Commit != first || heads[0].Repository != "acme/app" || !heads[0].ObservedAt.Equal(feedbackTime(time.Minute)) || heads[1].Branch != "release" {
		t.Fatalf("heads=%+v", heads)
	}
	requireOK(t, x.Backend.RecordFeedbackBranchHead(x.Context, store.FeedbackBranchHead{Repository: "acme/app", Branch: "main", Commit: second, ObservedAt: feedbackTime(2 * time.Minute)}))
	heads, err = x.Backend.ListFeedbackBranchHeads(x.Context)
	requireOK(t, err)
	if heads[0].Commit != second {
		t.Fatalf("heads=%+v", heads)
	}
	if err := x.Backend.RecordFeedbackBranchHead(x.Context, store.FeedbackBranchHead{Repository: "acme/app", Branch: "main", Commit: "abc", ObservedAt: feedbackTime(0)}); err == nil {
		t.Fatal("a short commit was recorded")
	}
}

// feedbackMergedTask creates a merged task with the given merge events.
func feedbackMergedTask(t *testing.T, x Fixture, repository, crateID string, events ...core.Event) core.Task {
	t.Helper()
	id := fixtureTaskID()
	task := core.Task{ID: id, Workspace: x.Workspace, Repo: repository, Title: "Merged " + id, Body: "Merged.", State: core.TaskMerged, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(), Branch: "conveyor/task-" + id}
	if crateID != "" {
		task.Crate = &core.TaskCrate{ID: crateID}
	}
	requireOK(t, x.Backend.CreateTask(x.Context, task))
	for _, event := range events {
		event.TaskID = id
		requireOK(t, x.Backend.AppendEvent(x.Context, event))
	}
	return task
}

func feedbackMergeEvent(kind, sha string) core.Event {
	return core.Event{Kind: kind, Payload: core.JSONPayload(map[string]any{"repository": "acme/conveyor", "pull_request": 1, "head_sha": "head", "merge_commit_sha": sha})}
}

// runFeedbackMergedTasks proves the merged-task read: both merge kinds, the
// latest event's commit and time, a merged task without a usable commit kept,
// tasks that are not merged left out, and each filter.
func runFeedbackMergedTasks(t *testing.T, x Fixture) {
	backend, ok := x.Backend.(store.CrateStore)
	if !ok {
		t.Fatal("backend keeps no crates")
	}
	crate, err := backend.CreateCrate(x.Context, store.CrateInput{Name: "Wave"})
	requireOK(t, err)
	confirmed := feedbackMergedTask(t, x, "conveyor", crate.ID, feedbackMergeEvent("merge.confirmed", strings.ToUpper(feedbackCommit("a1"))))
	reconciled := feedbackMergedTask(t, x, "app", "", feedbackMergeEvent("merge.confirmed", feedbackCommit("b1")), feedbackMergeEvent("merge.reconciled", feedbackCommit("b2")))
	// A later merge event without a usable commit does not hide the commit an
	// earlier one recorded.
	kept := feedbackMergedTask(t, x, "app", "", feedbackMergeEvent("merge.confirmed", feedbackCommit("d1")), feedbackMergeEvent("merge.reconciled", ""))
	unusable := feedbackMergedTask(t, x, "conveyor", crate.ID, feedbackMergeEvent("merge.confirmed", "landed-sha"))
	bare := feedbackMergedTask(t, x, "conveyor", crate.ID)
	open := crateTask(t, x, core.TaskRunning, crate.ID)
	requireOK(t, x.Backend.AppendEvent(x.Context, core.Event{TaskID: open.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"merge_commit_sha": feedbackCommit("c1")})}))
	all, err := x.Backend.ListFeedbackMergedTasks(x.Context, store.FeedbackMergedTaskQuery{})
	requireOK(t, err)
	byID := map[string]store.FeedbackMergedTask{}
	for _, task := range all {
		byID[task.TaskID] = task
	}
	if len(all) != 5 || byID[open.ID].TaskID != "" {
		t.Fatalf("merged tasks=%+v", all)
	}
	if got := byID[confirmed.ID]; got.Repository != "conveyor" || got.MergeCommit != feedbackCommit("a1") || got.MergedAt.IsZero() || got.EventID == 0 {
		t.Fatalf("confirmed=%+v", got)
	}
	if got := byID[reconciled.ID]; got.Repository != "app" || got.MergeCommit != feedbackCommit("b2") {
		t.Fatalf("reconciled=%+v", got)
	}
	if got := byID[kept.ID]; got.MergeCommit != feedbackCommit("d1") || got.MergedAt.IsZero() {
		t.Fatalf("kept=%+v", got)
	}
	for _, id := range []string{unusable.ID, bare.ID} {
		if got := byID[id]; got.MergeCommit != "" || !got.MergedAt.IsZero() {
			t.Fatalf("task without a usable merge commit=%+v", got)
		}
	}
	// Newest merge first; tasks without a commit last.
	if all[0].TaskID != kept.ID || all[1].TaskID != reconciled.ID || all[2].TaskID != confirmed.ID {
		t.Fatalf("order=%+v", all)
	}
	inCrate, err := x.Backend.ListFeedbackMergedTasks(x.Context, store.FeedbackMergedTaskQuery{CrateID: crate.ID})
	requireOK(t, err)
	if len(inCrate) != 3 || slices.ContainsFunc(inCrate, func(task store.FeedbackMergedTask) bool { return task.TaskID == reconciled.ID }) {
		t.Fatalf("crate members=%+v", inCrate)
	}
	inApp, err := x.Backend.ListFeedbackMergedTasks(x.Context, store.FeedbackMergedTaskQuery{Repository: "app"})
	requireOK(t, err)
	if len(inApp) != 2 || inApp[1].TaskID != reconciled.ID {
		t.Fatalf("app tasks=%+v", inApp)
	}
	named, err := x.Backend.ListFeedbackMergedTasks(x.Context, store.FeedbackMergedTaskQuery{TaskIDs: []string{confirmed.ID, open.ID, "task-missing"}})
	requireOK(t, err)
	if len(named) != 1 || named[0].TaskID != confirmed.ID {
		t.Fatalf("named tasks=%+v", named)
	}
}

// runFeedbackStatusIsolation proves every operation stays in its workspace.
func runFeedbackStatusIsolation(t *testing.T, x Fixture) {
	other := x.Workspace + "-status"
	y := x
	y.Workspace, y.Context = other, store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), other)
	_, err := x.Backend.BootstrapWorkspaceConfig(y.Context, &config.Config{Workspace: other, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}})
	requireOK(t, err)
	ours := newFeedbackEnvironment(t, x, "Production", "watcher")
	theirs := newFeedbackEnvironment(t, y, "Production", "watcher")
	ours.send(t, 0, feedbackService("api", "abc1234"))
	theirs.send(t, 0, feedbackService("api", "abc1234"))
	ref := feedbackRef(theirs.id, "api", "abc1234")
	claimed, err := y.Backend.ClaimFeedbackResolution(y.Context, ref, feedbackTime(0), feedbackTime(0))
	requireOK(t, err)
	if !claimed {
		t.Fatal("their claim refused")
	}
	requireOK(t, y.Backend.RecordFeedbackResolution(y.Context, store.FeedbackResolution{FeedbackVersionRef: ref, ResolvedRepository: "conveyor", ResolvedCommit: feedbackCommit("abc1234"), AttemptedAt: feedbackTime(0)}))
	if inputs, err := x.Backend.ReadFeedbackInputs(x.Context, []string{theirs.id}); err != nil || len(inputs.Resolutions) != 0 || len(inputs.Running) != 0 || len(inputs.Signals) != 0 {
		t.Fatalf("foreign inputs=%+v err=%v", inputs, err)
	}
	key := store.FeedbackComparisonKey{Repository: "acme/conveyor", Base: feedbackCommit("a"), Head: feedbackCommit("b")}
	requireOK(t, y.Backend.RecordFeedbackComparisons(y.Context, []store.FeedbackComparison{{FeedbackComparisonKey: key, Status: store.FeedbackCompareAhead, ComparedAt: feedbackTime(0)}}))
	requireOK(t, y.Backend.RecordFeedbackBranchHead(y.Context, store.FeedbackBranchHead{Repository: "acme/conveyor", Branch: "main", Commit: feedbackCommit("b"), ObservedAt: feedbackTime(0)}))
	if got, err := x.Backend.ListFeedbackComparisons(x.Context, []store.FeedbackComparisonKey{key}); err != nil || len(got) != 0 {
		t.Fatalf("foreign comparisons=%+v err=%v", got, err)
	}
	if heads, err := x.Backend.ListFeedbackBranchHeads(x.Context); err != nil || len(heads) != 0 {
		t.Fatalf("foreign heads=%+v err=%v", heads, err)
	}
	// Their admissions spend none of our allowance, and our stored answer
	// set is our own.
	if admitted, err := y.Backend.AdmitFeedbackComparisons(y.Context, []store.FeedbackComparisonKey{{Repository: "acme/conveyor", Base: feedbackCommit("c"), Head: feedbackCommit("d")}}, feedbackTime(0), 0); err != nil || len(admitted) != 1 {
		t.Fatalf("their admission=%v err=%v", admitted, err)
	}
	if admitted, err := x.Backend.AdmitFeedbackComparisons(x.Context, []store.FeedbackComparisonKey{key, {Repository: "acme/conveyor", Base: feedbackCommit("c"), Head: feedbackCommit("d")}}, feedbackTime(0), 0); err != nil || len(admitted) != 2 {
		t.Fatalf("our admission=%v err=%v", admitted, err)
	}
	feedbackMergedTask(t, y, "conveyor", "", feedbackMergeEvent("merge.confirmed", feedbackCommit("e")))
	if merged, err := x.Backend.ListFeedbackMergedTasks(x.Context, store.FeedbackMergedTaskQuery{}); err != nil || len(merged) != 0 {
		t.Fatalf("foreign merged tasks=%+v err=%v", merged, err)
	}
}

// runFeedbackStatusObservesOnly proves that the new writes append no event
// and change no task or work order (component-production-feedback invariant
// 1; req-production-feedback AC-11.1).
func runFeedbackStatusObservesOnly(t *testing.T, x Fixture) {
	environment := newFeedbackEnvironment(t, x, "Production", "watcher")
	environment.send(t, 0, feedbackService("api", "abc1234"))
	task := feedbackMergedTask(t, x, "conveyor", "", feedbackMergeEvent("merge.confirmed", feedbackCommit("a1")))
	before, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	beforeEvents, err := x.Backend.ListEvents(x.Context, task.ID)
	requireOK(t, err)
	beforeOrders, err := x.Backend.ListWorkOrders(x.Context)
	requireOK(t, err)
	ref := feedbackRef(environment.id, "api", "abc1234")
	_, err = x.Backend.ClaimFeedbackResolution(x.Context, ref, feedbackTime(0), feedbackTime(0))
	requireOK(t, err)
	requireOK(t, x.Backend.RecordFeedbackResolution(x.Context, store.FeedbackResolution{FeedbackVersionRef: ref, ResolvedRepository: "conveyor", ResolvedCommit: feedbackCommit("abc1234"), AttemptedAt: feedbackTime(0)}))
	key := store.FeedbackComparisonKey{Repository: "acme/conveyor", Base: feedbackCommit("a1"), Head: feedbackCommit("abc1234")}
	_, err = x.Backend.AdmitFeedbackComparisons(x.Context, []store.FeedbackComparisonKey{key}, feedbackTime(0), 0)
	requireOK(t, err)
	requireOK(t, x.Backend.RecordFeedbackComparisons(x.Context, []store.FeedbackComparison{{FeedbackComparisonKey: key, Status: store.FeedbackCompareAhead, ComparedAt: feedbackTime(0)}}))
	requireOK(t, x.Backend.RecordFeedbackBranchHead(x.Context, store.FeedbackBranchHead{Repository: "acme/conveyor", Branch: "main", Commit: feedbackCommit("abc1234"), ObservedAt: feedbackTime(0)}))
	after, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	afterEvents, err := x.Backend.ListEvents(x.Context, task.ID)
	requireOK(t, err)
	afterOrders, err := x.Backend.ListWorkOrders(x.Context)
	requireOK(t, err)
	if after.State != before.State || after.PlanState != before.PlanState || after.GateKind != before.GateKind || after.NextStage != before.NextStage || len(afterEvents) != len(beforeEvents) || len(afterOrders) != len(beforeOrders) {
		t.Fatalf("task changed: before=%+v after=%+v events %d->%d orders %d->%d", before, after, len(beforeEvents), len(afterEvents), len(beforeOrders), len(afterOrders))
	}
}

// runFeedbackStatusConcurrent proves that concurrent signals or replicas
// never both run one attempt, and that concurrent admissions never pass the
// workspace allowance or admit one pair twice.
func runFeedbackStatusConcurrent(t *testing.T, x Fixture) {
	environment := newFeedbackEnvironment(t, x, "Production", "watcher")
	ref := feedbackRef(environment.id, "api", "abc1234")
	const claimers = 8
	var wg sync.WaitGroup
	wins := make(chan bool, claimers)
	failures := make(chan error, claimers)
	for range claimers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := x.Backend.ClaimFeedbackResolution(x.Context, ref, feedbackTime(0), feedbackTime(0))
			if err != nil && !errors.Is(err, store.ErrRetryable) {
				failures <- err
				return
			}
			wins <- claimed && err == nil
		}()
	}
	wg.Wait()
	close(wins)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	won := 0
	for claimed := range wins {
		if claimed {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d concurrent claims won, want one", won)
	}
	const admitters = 4
	head := feedbackCommit("ee")
	admitted := make(chan []store.FeedbackComparisonKey, admitters)
	errs := make(chan error, admitters)
	for i := range admitters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys := make([]store.FeedbackComparisonKey, 30)
			for j := range keys {
				// Each admitter asks for 10 pairs the next one asks for too.
				keys[j] = store.FeedbackComparisonKey{Repository: "acme/app", Base: feedbackCommit(fmt.Sprintf("%02x", i*20+j)), Head: head}
			}
			for attempt := 0; ; attempt++ {
				got, err := x.Backend.AdmitFeedbackComparisons(x.Context, keys, feedbackTime(0), 0)
				if errors.Is(err, store.ErrRetryable) && attempt < 20 {
					continue
				}
				if err != nil {
					errs <- err
					return
				}
				admitted <- got
				return
			}
		}()
	}
	wg.Wait()
	close(admitted)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := map[store.FeedbackComparisonKey]bool{}
	total := 0
	for keys := range admitted {
		for _, key := range keys {
			if seen[key] {
				t.Fatalf("pair %v admitted twice", key)
			}
			seen[key] = true
			total++
		}
	}
	if total != store.FeedbackComparisonsPerMinute {
		t.Fatalf("%d pairs admitted, want the allowance of %d", total, store.FeedbackComparisonsPerMinute)
	}
}

// runFeedbackAdmissionFairness proves that pairs tried before, which have no
// stored answer, go after pairs never tried, oldest attempt first, so pairs
// that keep failing never hold back the others across reads more than the
// in-flight lease apart (feature-production-feedback-live FB-3).
func runFeedbackAdmissionFairness(t *testing.T, x Fixture) {
	head := feedbackCommit("fa")
	keys := make([]store.FeedbackComparisonKey, 70)
	for i := range keys {
		keys[i] = store.FeedbackComparisonKey{Repository: "acme/app", Base: feedbackCommit(fmt.Sprintf("%02x", i)), Head: head}
	}
	first, err := x.Backend.AdmitFeedbackComparisons(x.Context, keys, feedbackTime(0), 0)
	requireOK(t, err)
	if len(first) != 60 || first[0] != keys[0] || first[59] != keys[59] {
		t.Fatalf("first admission=%d", len(first))
	}
	// None of the 60 is answered. Three minutes later the lease is over, and
	// the 10 pairs never tried come first.
	later, err := x.Backend.AdmitFeedbackComparisons(x.Context, keys, feedbackTime(3*time.Minute), 0)
	requireOK(t, err)
	if len(later) != 60 || !slices.Equal(later[:10], keys[60:]) || later[10] != keys[0] {
		t.Fatalf("later admission=%v", later[:11])
	}
	// The pairs tried at 0 go before the pairs tried at three minutes.
	requireOK(t, x.Backend.RecordFeedbackComparisons(x.Context, []store.FeedbackComparison{{FeedbackComparisonKey: keys[60], Status: store.FeedbackCompareAhead, ComparedAt: feedbackTime(0)}}))
	next, err := x.Backend.AdmitFeedbackComparisons(x.Context, keys, feedbackTime(6*time.Minute), 3)
	requireOK(t, err)
	if !slices.Equal(next, []store.FeedbackComparisonKey{keys[50], keys[51], keys[52]}) {
		t.Fatalf("oldest attempts first=%v", next)
	}
}
