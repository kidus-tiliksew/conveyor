package reports

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// timingFixture builds a deploy history of service api in env-1 with stored
// resolutions and comparisons.
type timingFixture struct {
	deploys     []store.FeedbackDeploy
	resolutions []store.FeedbackResolution
	comparisons []store.FeedbackComparison
	merged      []store.FeedbackMergedTask
	report      int64
}

func (f *timingFixture) deploy(id, version, commit string, at time.Time, source string) store.FeedbackDeploy {
	f.report++
	deploy := store.FeedbackDeploy{ID: id, EnvironmentID: "env-1", Service: "api", FeedbackVersion: store.FeedbackVersion{Version: version}, DeployedAt: at,
		Origin: store.FeedbackOrigin{Source: source, Report: f.report}}
	f.deploys = append(f.deploys, deploy)
	ref := store.FeedbackVersionRef{EnvironmentID: "env-1", Service: "api", FeedbackVersion: deploy.FeedbackVersion}
	if !slices.ContainsFunc(f.resolutions, func(r store.FeedbackResolution) bool { return r.FeedbackVersionRef == ref }) {
		resolution := store.FeedbackResolution{FeedbackVersionRef: ref, ResolvedRepository: "app", ResolvedCommit: commit}
		if commit == "" {
			resolution.ResolvedRepository, resolution.Reason = "", store.FeedbackReasonNoMatch
		}
		f.resolutions = append(f.resolutions, resolution)
	}
	return deploy
}

// task adds a merged task of app whose merge commit is held by the listed
// heads and by no other head the fixture compares.
func (f *timingFixture) task(id, merge string, heldBy ...string) {
	f.merged = append(f.merged, store.FeedbackMergedTask{TaskID: id, Repository: "app", MergeCommit: merge})
	for _, head := range []string{shaA, shaB, shaC, shaD} {
		if head == merge {
			continue
		}
		status := store.FeedbackCompareBehind
		if slices.Contains(heldBy, head) {
			status = store.FeedbackCompareAhead
		}
		f.comparisons = append(f.comparisons, store.FeedbackComparison{FeedbackComparisonKey: store.FeedbackComparisonKey{Repository: "acme/app", Base: merge, Head: head}, Status: status})
	}
}

func (f *timingFixture) input(firstSeen time.Time) TimingInput {
	report := store.FeedbackReport{ID: "rpt-261127-000002", EnvironmentID: "env-1", Service: "api", FirstEntrySeen: firstSeen, CreatedAt: firstSeen}
	return TimingInput{Report: report, TimingWindowMinutes: 60, Deploys: f.deploys, Resolutions: f.resolutions, Slugs: map[string]string{"app": "Acme/App"},
		Merged: f.merged, Comparisons: f.comparisons, SignalAfterReport: true, Listening: true, PriorSignals: map[string]store.FeedbackBoundary{}}
}

func linkedTasks(outcome Outcome) []string {
	ids := []string{}
	for _, link := range outcome.Links {
		ids = append(ids, link.TaskID)
	}
	return ids
}

// TestTimingWindow covers the first case of invariant 13 with exact times:
// first_seen at the deploy, at the end of the window, one nanosecond past
// it, and one nanosecond before the deploy; elapsed_minutes counts whole
// minutes (req-production-feedback AC-5.2; req-environments AC-1.3).
func TestTimingWindow(t *testing.T) {
	f := &timingFixture{}
	f.deploy("d-1", "1", shaA, base, store.FeedbackSourceSignal)
	deploy := f.deploy("d-2", "2", shaB, base.Add(time.Hour), store.FeedbackSourceGitHub)
	f.task("t-new", shaC, shaB)
	f.task("t-old", shaD, shaA, shaB)
	for _, tc := range []struct {
		name      string
		firstSeen time.Time
		linked    bool
		elapsed   int64
	}{
		{"at the deploy", deploy.DeployedAt, true, 0},
		{"59 minutes 59.999999999 seconds after", deploy.DeployedAt.Add(time.Hour - time.Nanosecond), true, 59},
		{"at the end of the window", deploy.DeployedAt.Add(time.Hour), true, 60},
		{"one nanosecond past the window", deploy.DeployedAt.Add(time.Hour + time.Nanosecond), false, 0},
		{"one nanosecond before the deploy", deploy.DeployedAt.Add(-time.Nanosecond), false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome := Timing(f.input(tc.firstSeen))
			if !outcome.Ended {
				t.Fatalf("outcome %+v, want ended", outcome)
			}
			if !tc.linked {
				if len(outcome.Links) != 0 || len(outcome.Notes) != 0 {
					t.Fatalf("outcome %+v, want nothing", outcome)
				}
				return
			}
			if got := linkedTasks(outcome); !slices.Equal(got, []string{"t-new"}) {
				t.Fatalf("links %v, want t-new", got)
			}
			evidence := outcome.Links[0].Evidence[0]
			if evidence.Kind != store.FeedbackEvidenceTiming || evidence.Deploy == nil || evidence.Deploy.ID != "d-2" || evidence.Deploy.Source != store.FeedbackSourceGitHub ||
				!evidence.Deploy.DeployedAt.Equal(deploy.DeployedAt) || evidence.Deploy.Version.Version != "2" || evidence.ElapsedMinutes == nil || *evidence.ElapsedMinutes != tc.elapsed || evidence.PriorSignalAt != nil {
				t.Fatalf("evidence %+v, want the deploy and %d minutes", evidence, tc.elapsed)
			}
		})
	}
}

// TestTimingPriorSignal covers the second case of invariant 13: a deploy
// recorded from a signal qualifies when first_seen falls between the prior
// watcher signal that named the service and the deploy, both included, with
// elapsed_minutes 0 and prior_signal_at. A GitHub deploy never qualifies
// that way.
func TestTimingPriorSignal(t *testing.T) {
	f := &timingFixture{}
	f.deploy("d-1", "1", shaA, base, store.FeedbackSourceSignal)
	signalDeploy := f.deploy("d-2", "2", shaB, base.Add(3*time.Hour), store.FeedbackSourceSignal)
	f.task("t-new", shaC, shaB)
	prior := store.FeedbackBoundary{At: base.Add(2 * time.Hour), Report: 7}
	for _, tc := range []struct {
		name      string
		firstSeen time.Time
		linked    bool
	}{
		{"at the prior signal", prior.At, true},
		{"between", prior.At.Add(30 * time.Minute), true},
		{"one nanosecond before the prior signal", prior.At.Add(-time.Nanosecond), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := f.input(tc.firstSeen)
			input.PriorSignals = map[string]store.FeedbackBoundary{"d-2": prior}
			outcome := Timing(input)
			if !outcome.Ended {
				t.Fatalf("outcome %+v", outcome)
			}
			if !tc.linked {
				if len(outcome.Links) != 0 {
					t.Fatalf("links %v, want none", linkedTasks(outcome))
				}
				return
			}
			evidence := outcome.Links[0].Evidence[0]
			if evidence.Deploy.ID != signalDeploy.ID || *evidence.ElapsedMinutes != 0 || evidence.PriorSignalAt == nil || !evidence.PriorSignalAt.Equal(prior.At) {
				t.Fatalf("evidence %+v, want 0 minutes and the prior signal", evidence)
			}
		})
	}
	// The same history from GitHub does not qualify by a prior signal.
	g := &timingFixture{}
	g.deploy("d-1", "1", shaA, base, store.FeedbackSourceSignal)
	g.deploy("d-2", "2", shaB, base.Add(3*time.Hour), store.FeedbackSourceGitHub)
	g.task("t-new", shaC, shaB)
	input := g.input(prior.At.Add(time.Minute))
	input.PriorSignals = map[string]store.FeedbackBoundary{"d-2": prior}
	if outcome := Timing(input); !outcome.Ended || len(outcome.Links) != 0 {
		t.Fatalf("a GitHub deploy: %+v, want no link", outcome)
	}
}

// TestTimingSignalCandidates covers the deploys whose prior signal the
// reader looks up: those of the service recorded from a signal at or after
// first_seen, in order.
func TestTimingSignalCandidates(t *testing.T) {
	f := &timingFixture{}
	f.deploy("d-early", "1", shaA, base, store.FeedbackSourceSignal)
	f.deploy("d-github", "2", shaB, base.Add(2*time.Hour), store.FeedbackSourceGitHub)
	f.deploy("d-at", "3", shaC, base.Add(time.Hour), store.FeedbackSourceSignal)
	f.deploy("d-late", "4", shaD, base.Add(3*time.Hour), store.FeedbackSourceSignal)
	report := store.FeedbackReport{Service: "api", FirstEntrySeen: base.Add(time.Hour)}
	got := []string{}
	for _, deploy := range SignalCandidates(report, f.deploys) {
		got = append(got, deploy.ID)
	}
	if !slices.Equal(got, []string{"d-at", "d-late"}) {
		t.Fatalf("candidates %v", got)
	}
}

// TestTimingPredecessor covers the previous version of a qualifying deploy:
// anchored to that deploy, with a version that returns after a stop taking
// the version before its return, and a first deploy skipped.
func TestTimingPredecessor(t *testing.T) {
	f := &timingFixture{}
	f.deploy("d-1", "1", shaA, base, store.FeedbackSourceSignal)
	f.deploy("d-2", "2", shaB, base.Add(time.Hour), store.FeedbackSourceSignal)
	// Version 1 returns: its predecessor is version 2, not nothing.
	f.deploy("d-3", "1", shaA, base.Add(2*time.Hour), store.FeedbackSourceSignal)
	f.task("t-in-2", shaC, shaB)
	f.task("t-in-1", shaD, shaA)
	// first_seen after the return: version 1 newly holds t-in-1 against 2.
	outcome := Timing(f.input(base.Add(2*time.Hour + time.Minute)))
	if !outcome.Ended || !slices.Equal(linkedTasks(outcome), []string{"t-in-1"}) || outcome.Links[0].Evidence[0].Deploy.ID != "d-3" {
		t.Fatalf("after the return: %+v, want t-in-1 from d-3", outcome)
	}
	// first_seen after the first deploy only: no previous version, skipped.
	first := Timing(f.input(base.Add(time.Minute)))
	if !first.Ended || len(first.Links) != 0 || len(first.Notes) != 0 {
		t.Fatalf("a first deploy: %+v, want skipped", first)
	}
}

// TestTimingNotes covers deploy_not_compared for an unresolved version and
// for two repositories, no_task_deploy, and too_many_tasks at exactly ten
// against eleven tasks (component-feedback-reports invariants 13 and 14;
// req-production-feedback AC-5.3, AC-5.4).
func TestTimingNotes(t *testing.T) {
	t.Run("unresolved", func(t *testing.T) {
		f := &timingFixture{}
		f.deploy("d-1", "1", shaA, base, store.FeedbackSourceSignal)
		f.deploy("d-2", "2", "", base.Add(time.Hour), store.FeedbackSourceSignal)
		outcome := Timing(f.input(base.Add(time.Hour)))
		if !outcome.Ended || len(outcome.Notes) != 1 || outcome.Notes[0].Kind != store.FeedbackNoteDeployNotCompare || outcome.Notes[0].Deploy.ID != "d-2" {
			t.Fatalf("outcome %+v", outcome)
		}
	})
	t.Run("two repositories", func(t *testing.T) {
		f := &timingFixture{}
		f.deploy("d-1", "1", shaA, base, store.FeedbackSourceSignal)
		f.deploy("d-2", "2", shaB, base.Add(time.Hour), store.FeedbackSourceSignal)
		f.resolutions[1].ResolvedRepository = "web"
		outcome := Timing(f.input(base.Add(time.Hour)))
		if !outcome.Ended || len(outcome.Notes) != 1 || outcome.Notes[0].Kind != store.FeedbackNoteDeployNotCompare {
			t.Fatalf("outcome %+v", outcome)
		}
	})
	t.Run("no task", func(t *testing.T) {
		f := &timingFixture{}
		f.deploy("d-1", "1", shaA, base, store.FeedbackSourceSignal)
		f.deploy("d-2", "2", shaB, base.Add(time.Hour), store.FeedbackSourceSignal)
		f.task("t-both", shaC, shaA, shaB)
		outcome := Timing(f.input(base.Add(time.Hour)))
		if !outcome.Ended || len(outcome.Links) != 0 || len(outcome.Notes) != 1 || outcome.Notes[0].Kind != store.FeedbackNoteNoTaskDeploy || outcome.Notes[0].Deploy.ID != "d-2" {
			t.Fatalf("outcome %+v", outcome)
		}
	})
	for _, count := range []int{MaxTimingTasks, MaxTimingTasks + 1} {
		t.Run(fmt.Sprintf("%d tasks", count), func(t *testing.T) {
			f := &timingFixture{}
			f.deploy("d-1", "1", shaA, base, store.FeedbackSourceSignal)
			f.deploy("d-2", "2", shaB, base.Add(time.Hour), store.FeedbackSourceSignal)
			for i := range count {
				merge := fmt.Sprintf("%040x", i+1)
				f.merged = append(f.merged, store.FeedbackMergedTask{TaskID: fmt.Sprintf("t-%02d", i), Repository: "app", MergeCommit: merge})
				f.comparisons = append(f.comparisons,
					store.FeedbackComparison{FeedbackComparisonKey: store.FeedbackComparisonKey{Repository: "acme/app", Base: merge, Head: shaB}, Status: store.FeedbackCompareAhead},
					store.FeedbackComparison{FeedbackComparisonKey: store.FeedbackComparisonKey{Repository: "acme/app", Base: merge, Head: shaA}, Status: store.FeedbackCompareDiverged})
			}
			outcome := Timing(f.input(base.Add(time.Hour)))
			if !outcome.Ended {
				t.Fatalf("outcome %+v", outcome)
			}
			if count <= MaxTimingTasks {
				if len(outcome.Links) != count || len(outcome.Notes) != 0 {
					t.Fatalf("links=%d notes=%v, want %d links", len(outcome.Links), outcome.Notes, count)
				}
				return
			}
			if len(outcome.Links) != 0 || len(outcome.Notes) != 1 || outcome.Notes[0].Kind != store.FeedbackNoteTooManyTasks || outcome.Notes[0].TaskCount != count || outcome.Notes[0].Deploy.ID != "d-2" {
				t.Fatalf("outcome %+v, want too_many_tasks with %d", outcome, count)
			}
		})
	}
}

// TestTimingWaits covers what the rule waits for: a missing comparison,
// which is never taken as an answer; a pending resolution; a watcher signal
// after the report; and the exception when no watcher is listening
// (component-feedback-reports invariant 15).
func TestTimingWaits(t *testing.T) {
	f := &timingFixture{}
	f.deploy("d-1", "1", shaA, base, store.FeedbackSourceSignal)
	f.deploy("d-2", "2", shaB, base.Add(time.Hour), store.FeedbackSourceSignal)
	f.task("t-new", shaC, shaB)
	complete := f.comparisons
	input := f.input(base.Add(time.Hour))
	input.Comparisons = nil
	outcome := Timing(input)
	wantMissing := []store.FeedbackComparisonKey{{Repository: "acme/app", Base: shaC, Head: shaA}, {Repository: "acme/app", Base: shaC, Head: shaB}}
	if outcome.Ended || outcome.Waiting != WaitingForComparisons || !slices.Equal(outcome.Missing, wantMissing) || len(outcome.Links) != 0 {
		t.Fatalf("no comparisons: %+v, want waiting for %v", outcome, wantMissing)
	}
	input.Comparisons = complete
	if outcome := Timing(input); !outcome.Ended || !slices.Equal(linkedTasks(outcome), []string{"t-new"}) {
		t.Fatalf("with comparisons: %+v", outcome)
	}
	pending := f.input(base.Add(time.Hour))
	pending.Resolutions = slices.Clone(pending.Resolutions)
	pending.Resolutions[1] = store.FeedbackResolution{FeedbackVersionRef: pending.Resolutions[1].FeedbackVersionRef, Reason: store.FeedbackReasonPending}
	if outcome := Timing(pending); outcome.Ended || outcome.Waiting != WaitingForResolution {
		t.Fatalf("pending: %+v, want waiting", outcome)
	}
	noSignal := f.input(base.Add(time.Hour))
	noSignal.SignalAfterReport = false
	if outcome := Timing(noSignal); outcome.Ended || outcome.Waiting != WaitingForSignal {
		t.Fatalf("no signal after the report: %+v, want waiting", outcome)
	}
	noSignal.Listening = false
	if outcome := Timing(noSignal); !outcome.Ended || !slices.Equal(linkedTasks(outcome), []string{"t-new"}) {
		t.Fatalf("no watcher listening: %+v, want the answer", outcome)
	}
	other := f.input(base.Add(time.Hour))
	other.Report.Service = ""
	if outcome := Timing(other); !outcome.Ended || len(outcome.Links) != 0 || len(outcome.Notes) != 0 {
		t.Fatalf("no service: %+v, want ended with nothing", outcome)
	}
	web := f.input(base.Add(time.Hour))
	web.Report.Service = "web"
	if outcome := Timing(web); !outcome.Ended || len(outcome.Links) != 0 || len(outcome.Notes) != 0 {
		t.Fatalf("another service's deploys: %+v, want nothing", outcome)
	}
}
