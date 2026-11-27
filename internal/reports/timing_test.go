package reports

import (
	"fmt"
	"slices"
	"strings"
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
		resolution := store.FeedbackResolution{FeedbackVersionRef: ref, ResolvedRepository: "app", ResolvedGitHubRepository: "acme/app", ResolvedCommit: commit}
		if commit == "" {
			resolution.ResolvedRepository, resolution.ResolvedGitHubRepository, resolution.Reason = "", "", store.FeedbackReasonNoMatch
		}
		f.resolutions = append(f.resolutions, resolution)
	}
	return deploy
}

// task adds a merged task of app whose merge commit is held by the listed
// heads and by no other head the fixture compares.
func (f *timingFixture) task(id, merge string, heldBy ...string) {
	f.merged = append(f.merged, store.FeedbackMergedTask{TaskID: id, Repository: "app", GitHubRepository: "acme/app", MergeCommit: merge})
	for _, head := range []string{shaA, shaB, shaC, shaD, shaE, shaF, shaG} {
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
	return TimingInput{Report: report, TimingWindowMinutes: 60, Deploys: f.deploys, Resolutions: f.resolutions,
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
// reader looks up: those of the service in the report's environment recorded
// from a signal at or after first_seen, in order, with the deploys of one
// signal next to each other.
func TestTimingSignalCandidates(t *testing.T) {
	f := &timingFixture{}
	f.deploy("d-early", "1", shaA, base, store.FeedbackSourceSignal)
	f.deploy("d-github", "2", shaB, base.Add(2*time.Hour), store.FeedbackSourceGitHub)
	f.deploy("d-at", "3", shaC, base.Add(time.Hour), store.FeedbackSourceSignal)
	f.deploy("d-late", "4", shaD, base.Add(3*time.Hour), store.FeedbackSourceSignal)
	f.sameSignal(f.deploy("d-late-too", "5", shaE, base.Add(3*time.Hour), store.FeedbackSourceSignal), "d-late")
	f.deploy("d-other-environment", "6", shaF, base.Add(2*time.Hour), store.FeedbackSourceSignal)
	f.deploys[len(f.deploys)-1].EnvironmentID = "env-2"
	f.deploy("d-web", "7", shaG, base.Add(2*time.Hour), store.FeedbackSourceSignal)
	f.deploys[len(f.deploys)-1].Service = "web"
	report := store.FeedbackReport{EnvironmentID: "env-1", Service: "api", FirstEntrySeen: base.Add(time.Hour)}
	got := []string{}
	for _, deploy := range SignalCandidates(report, f.deploys) {
		got = append(got, deploy.ID)
	}
	if !slices.Equal(got, []string{"d-at", "d-late", "d-late-too"}) {
		t.Fatalf("candidates %v", got)
	}
}

// sameSignal records the deploy as recorded by the same signal as another
// deploy: the two share one place.
func (f *timingFixture) sameSignal(deploy store.FeedbackDeploy, with string) {
	for i := range f.deploys {
		if f.deploys[i].ID == with {
			for j := range f.deploys {
				if f.deploys[j].ID == deploy.ID {
					f.deploys[j].Origin.Report = f.deploys[i].Origin.Report
					f.deploys[j].DeployedAt = f.deploys[i].DeployedAt
				}
			}
		}
	}
}

// decisions renders what an ended timing outcome decided for each deploy:
// each link with its task, elapsed_minutes, and prior_signal_at, and each
// note with its kind and task count.
func decisions(t *testing.T, outcome Outcome) map[string][]string {
	t.Helper()
	if !outcome.Ended {
		t.Fatalf("outcome %+v, want ended", outcome)
	}
	out := map[string][]string{}
	for _, link := range outcome.Links {
		for _, piece := range link.Evidence {
			if piece.Kind != store.FeedbackEvidenceTiming || piece.Deploy == nil || piece.ElapsedMinutes == nil {
				t.Fatalf("evidence %+v", piece)
			}
			entry := fmt.Sprintf("link %s %d", link.TaskID, *piece.ElapsedMinutes)
			if piece.PriorSignalAt != nil {
				entry += " prior " + piece.PriorSignalAt.Format(time.RFC3339Nano)
			}
			out[piece.Deploy.ID] = append(out[piece.Deploy.ID], entry)
		}
	}
	for _, note := range outcome.Notes {
		entry := "note " + note.Kind
		if note.TaskCount != 0 {
			entry += fmt.Sprintf(" %d", note.TaskCount)
		}
		out[note.Deploy.ID] = append(out[note.Deploy.ID], entry)
	}
	for id := range out {
		slices.Sort(out[id])
	}
	return out
}

// TestTimingDecidesEveryDeploy is the table of review round 3's timing class
// rule: the rule walks every deploy of the service in the report's
// environment and decides each one on its own, with no early end after a
// match and no deploy skipped because another qualified. Each row states
// what every deploy of the history gets; a deploy with no entry gets no link
// and no note (component-feedback-reports invariants 13 and 14;
// req-production-feedback AC-5.2 to AC-5.4).
func TestTimingDecidesEveryDeploy(t *testing.T) {
	prior := store.FeedbackBoundary{At: base.Add(time.Hour), Report: 99}
	priorText := prior.At.Format(time.RFC3339Nano)
	for _, tc := range []struct {
		name      string
		build     func(f *timingFixture) (firstSeen time.Time, priors map[string]store.FeedbackBoundary)
		decisions map[string][]string
	}{{
		// d-1 has no previous version. d-2 newly holds t-2. d-3 newly holds
		// nothing. Both are in the window.
		name: "several deploys in the window",
		build: func(f *timingFixture) (time.Time, map[string]store.FeedbackBoundary) {
			f.deploy("d-1", "1", shaA, base, store.FeedbackSourceGitHub)
			f.deploy("d-2", "2", shaB, base.Add(10*time.Minute), store.FeedbackSourceGitHub)
			f.deploy("d-3", "3", shaC, base.Add(20*time.Minute), store.FeedbackSourceGitHub)
			f.task("t-2", shaE, shaB, shaC)
			return base.Add(30 * time.Minute), nil
		},
		decisions: map[string][]string{"d-2": {"link t-2 20"}, "d-3": {"note no_task_deploy"}},
	}, {
		// One signal records d-2 and d-3 at one place after first_seen, and
		// first_seen is after the signal before it. Both get the prior signal
		// and are decided on their own.
		name: "several deploys from one signal at one boundary",
		build: func(f *timingFixture) (time.Time, map[string]store.FeedbackBoundary) {
			f.deploy("d-1", "1", shaA, base, store.FeedbackSourceSignal)
			f.deploy("d-2", "2", shaB, base.Add(2*time.Hour), store.FeedbackSourceSignal)
			f.sameSignal(f.deploy("d-3", "3", shaC, base.Add(2*time.Hour), store.FeedbackSourceSignal), "d-2")
			f.task("t-both", shaE, shaB, shaC)
			f.task("t-3", shaF, shaC)
			return base.Add(90 * time.Minute), map[string]store.FeedbackBoundary{"d-2": prior, "d-3": prior}
		},
		decisions: map[string][]string{"d-2": {"link t-both 0 prior " + priorText}, "d-3": {"link t-3 0 prior " + priorText, "link t-both 0 prior " + priorText}},
	}, {
		// d-2 is past its window and does not qualify, while d-3 after it
		// qualifies in the second case.
		name: "a deploy past its window beside a deploy in the second case",
		build: func(f *timingFixture) (time.Time, map[string]store.FeedbackBoundary) {
			f.deploy("d-1", "1", shaA, base.Add(-time.Hour), store.FeedbackSourceSignal)
			f.deploy("d-2", "2", shaB, base, store.FeedbackSourceGitHub)
			f.deploy("d-3", "3", shaC, base.Add(3*time.Hour), store.FeedbackSourceSignal)
			f.task("t-2", shaE, shaB, shaC)
			f.task("t-3", shaF, shaC)
			return base.Add(80 * time.Minute), map[string]store.FeedbackBoundary{"d-3": prior}
		},
		decisions: map[string][]string{"d-3": {"link t-3 0 prior " + priorText}},
	}, {
		// The same history with first_seen in the window of d-2 and after the
		// prior signal of d-3: both qualify, each in its own case.
		name: "one deploy in each case at once",
		build: func(f *timingFixture) (time.Time, map[string]store.FeedbackBoundary) {
			f.deploy("d-1", "1", shaA, base.Add(-time.Hour), store.FeedbackSourceSignal)
			f.deploy("d-2", "2", shaB, base.Add(10*time.Minute), store.FeedbackSourceGitHub)
			f.deploy("d-3", "3", shaC, base.Add(3*time.Hour), store.FeedbackSourceSignal)
			f.task("t-2", shaE, shaB, shaC)
			f.task("t-3", shaF, shaC)
			return base.Add(70 * time.Minute), map[string]store.FeedbackBoundary{"d-3": prior}
		},
		decisions: map[string][]string{"d-2": {"link t-2 60"}, "d-3": {"link t-3 0 prior " + priorText}},
	}, {
		// d-end is exactly a window before first_seen and d-start exactly at
		// it. d-out is one nanosecond earlier than d-end, and d-after one
		// nanosecond later than first_seen.
		name: "a deploy exactly at each end of the window",
		build: func(f *timingFixture) (time.Time, map[string]store.FeedbackBoundary) {
			firstSeen := base.Add(2 * time.Hour)
			f.deploy("d-0", "0", shaA, base, store.FeedbackSourceGitHub)
			f.deploy("d-out", "1", shaB, firstSeen.Add(-time.Hour-time.Nanosecond), store.FeedbackSourceGitHub)
			f.deploy("d-end", "2", shaC, firstSeen.Add(-time.Hour), store.FeedbackSourceGitHub)
			f.deploy("d-start", "3", shaD, firstSeen, store.FeedbackSourceGitHub)
			f.deploy("d-after", "4", shaE, firstSeen.Add(time.Nanosecond), store.FeedbackSourceGitHub)
			f.task("t-end", shaF, shaC, shaD, shaE)
			f.task("t-start", shaG, shaD, shaE)
			return firstSeen, nil
		},
		decisions: map[string][]string{"d-end": {"link t-end 60"}, "d-start": {"link t-start 0"}},
	}, {
		// Deploys of web and of api in env-2 sit beside the history. They are
		// not decided, and the env-2 deploy, which has no resolution, is not
		// the previous version of d-2.
		name: "deploys of another service or environment beside them",
		build: func(f *timingFixture) (time.Time, map[string]store.FeedbackBoundary) {
			f.deploy("d-1", "1", shaA, base, store.FeedbackSourceGitHub)
			f.deploys = append(f.deploys,
				store.FeedbackDeploy{ID: "d-env-2", EnvironmentID: "env-2", Service: "api", FeedbackVersion: store.FeedbackVersion{Version: "9"}, DeployedAt: base.Add(5 * time.Minute), Origin: store.FeedbackOrigin{Source: store.FeedbackSourceGitHub, Report: 50}},
				store.FeedbackDeploy{ID: "d-web", EnvironmentID: "env-1", Service: "web", FeedbackVersion: store.FeedbackVersion{Version: "2"}, DeployedAt: base.Add(10 * time.Minute), Origin: store.FeedbackOrigin{Source: store.FeedbackSourceGitHub, Report: 51}})
			f.deploy("d-2", "2", shaB, base.Add(10*time.Minute), store.FeedbackSourceGitHub)
			f.task("t-2", shaE, shaB)
			return base.Add(20 * time.Minute), map[string]store.FeedbackBoundary{"d-env-2": prior, "d-web": prior}
		},
		decisions: map[string][]string{"d-2": {"link t-2 10"}},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &timingFixture{}
			firstSeen, priors := tc.build(f)
			input := f.input(firstSeen)
			if priors != nil {
				input.PriorSignals = priors
			}
			got := decisions(t, Timing(input))
			if fmt.Sprint(got) != fmt.Sprint(tc.decisions) {
				t.Fatalf("decisions %v\nwant      %v", got, tc.decisions)
			}
		})
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
		f.resolutions[1].ResolvedRepository, f.resolutions[1].ResolvedGitHubRepository = "web", "acme/web"
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
				f.merged = append(f.merged, store.FeedbackMergedTask{TaskID: fmt.Sprintf("t-%02d", i), Repository: "app", GitHubRepository: "acme/app", MergeCommit: merge})
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

// TestTimingComparesGitHubRepositories is the timing rule's part of review
// round 4: "the merged tasks of the deploy's repository" and "two commits in
// different repositories" are answered with GitHub repositories, never with
// configured names. Both versions recorded acme/new when they resolved
// (component-feedback-reports invariants 13 and 14; approved plan version 4).
func TestTimingComparesGitHubRepositories(t *testing.T) {
	newRepo := func(f *timingFixture) {
		f.deploy("d-1", "1", shaA, base, store.FeedbackSourceGitHub)
		f.deploy("d-2", "2", shaB, base.Add(10*time.Minute), store.FeedbackSourceGitHub)
		for i := range f.resolutions {
			f.resolutions[i].ResolvedGitHubRepository = "acme/new"
		}
	}
	// held stores, in repository, that head holds merge or does not.
	held := func(f *timingFixture, repository, merge string, heads map[string]bool) {
		for head, contains := range heads {
			status := store.FeedbackCompareBehind
			if contains {
				status = store.FeedbackCompareAhead
			}
			f.comparisons = append(f.comparisons, store.FeedbackComparison{FeedbackComparisonKey: store.FeedbackComparisonKey{Repository: repository, Base: merge, Head: head}, Status: status})
		}
	}
	t.Run("a task merged in the old repository is not newly introduced", func(t *testing.T) {
		f := &timingFixture{}
		newRepo(f)
		// t-old was merged under the name app while it pointed at acme/old.
		// Even a stored answer that version 2 holds its commit does not make
		// it a task of acme/new.
		f.merged = append(f.merged,
			store.FeedbackMergedTask{TaskID: "t-old", Repository: "app", GitHubRepository: "acme/old", MergeCommit: shaC},
			store.FeedbackMergedTask{TaskID: "t-unknown", Repository: "app", MergeCommit: shaE},
			store.FeedbackMergedTask{TaskID: "t-new", Repository: "app", GitHubRepository: "acme/new", MergeCommit: shaD})
		held(f, "acme/new", shaC, map[string]bool{shaA: false, shaB: true})
		held(f, "acme/new", shaE, map[string]bool{shaA: false, shaB: true})
		held(f, "acme/new", shaD, map[string]bool{shaA: false, shaB: true})
		input := f.input(base.Add(20 * time.Minute))
		got := decisions(t, Timing(input))
		if fmt.Sprint(got) != fmt.Sprint(map[string][]string{"d-2": {"link t-new 10"}}) {
			t.Fatalf("decisions %v, want only t-new from d-2", got)
		}
	})
	t.Run("only old-repository tasks", func(t *testing.T) {
		f := &timingFixture{}
		newRepo(f)
		f.merged = append(f.merged, store.FeedbackMergedTask{TaskID: "t-old", Repository: "app", GitHubRepository: "acme/old", MergeCommit: shaC})
		input := f.input(base.Add(20 * time.Minute))
		// No comparison is asked for t-old, and d-2 newly holds no task.
		outcome := Timing(input)
		if got := decisions(t, outcome); len(outcome.Missing) != 0 || fmt.Sprint(got) != fmt.Sprint(map[string][]string{"d-2": {"note no_task_deploy"}}) {
			t.Fatalf("decisions %v missing %v, want no_task_deploy and no comparison", got, outcome.Missing)
		}
	})
	for _, tc := range []struct {
		name           string
		previousName   string
		previousGitHub string
		want           string
	}{
		{"two names on one GitHub repository are compared", "mirror", "Acme/New", "link t-new 10"},
		{"two GitHub repositories are not compared", "web", "acme/web", "note deploy_not_compared"},
		{"a version with no recorded repository proves none", "app", "", "note deploy_not_compared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &timingFixture{}
			newRepo(f)
			f.resolutions[0].ResolvedRepository, f.resolutions[0].ResolvedGitHubRepository = tc.previousName, tc.previousGitHub
			f.merged = append(f.merged, store.FeedbackMergedTask{TaskID: "t-new", Repository: "app", GitHubRepository: "acme/new", MergeCommit: shaD})
			held(f, "acme/new", shaD, map[string]bool{shaA: false, shaB: true})
			input := f.input(base.Add(20 * time.Minute))
			if got := decisions(t, Timing(input)); fmt.Sprint(got) != fmt.Sprint(map[string][]string{"d-2": {tc.want}}) {
				t.Fatalf("decisions %v, want d-2: %s", got, tc.want)
			}
		})
	}
}

// TestTimingUsesRecordedRepositories is the regression test of review round
// 5 and approved plan version 4. Both versions resolved while the name app
// pointed at acme/old, and their stored resolutions record acme/old. The name
// app now points at acme/new, and the two repositories share commits. A task
// merged in acme/new at the current version's own commit, with a stored
// answer in acme/new that the previous version does not hold it, does not
// link: the deploy is acme/old's. A task merged in acme/old with stored
// answers in acme/old still links, and a contrary answer stored under
// acme/new is never read for it. A resolution without the recorded field on
// either version gives deploy_not_compared and links no task
// (req-production-feedback AC-5.2; component-feedback-reports invariant 13;
// component-production-feedback invariant 14).
func TestTimingUsesRecordedRepositories(t *testing.T) {
	setup := func(current, previous string) TimingInput {
		f := &timingFixture{}
		f.deploy("d-1", "1", shaA, base, store.FeedbackSourceGitHub)
		f.deploy("d-2", "2", shaB, base.Add(10*time.Minute), store.FeedbackSourceGitHub)
		f.resolutions[0].ResolvedGitHubRepository = previous
		f.resolutions[1].ResolvedGitHubRepository = current
		f.merged = append(f.merged,
			// Merged in acme/new at shaB, the current version's commit, which
			// acme/new shares: SHA equality would skip the current comparison.
			store.FeedbackMergedTask{TaskID: "t-new-shared", Repository: "app", GitHubRepository: "acme/new", MergeCommit: shaB},
			// Merged in acme/old.
			store.FeedbackMergedTask{TaskID: "t-old", Repository: "app", GitHubRepository: "acme/old", MergeCommit: shaC})
		answer := func(repository, merge, head, status string) {
			f.comparisons = append(f.comparisons, store.FeedbackComparison{FeedbackComparisonKey: store.FeedbackComparisonKey{Repository: repository, Base: merge, Head: head}, Status: status})
		}
		// acme/new: the previous commit does not hold shaB.
		answer("acme/new", shaB, shaA, store.FeedbackCompareBehind)
		// acme/old: version 2 holds t-old's commit and version 1 does not.
		answer("acme/old", shaC, shaB, store.FeedbackCompareAhead)
		answer("acme/old", shaC, shaA, store.FeedbackCompareBehind)
		// A contrary answer under acme/new is never read for acme/old's deploy.
		answer("acme/new", shaC, shaB, store.FeedbackCompareBehind)
		return f.input(base.Add(20 * time.Minute))
	}
	for _, tc := range []struct {
		name              string
		current, previous string
		want              map[string][]string
	}{
		{"both versions recorded acme/old", "acme/old", "Acme/Old", map[string][]string{"d-2": {"link t-old 10"}}},
		{"the current version has no recorded repository", "", "acme/old", map[string][]string{"d-2": {"note deploy_not_compared"}}},
		{"the previous version has no recorded repository", "acme/old", "", map[string][]string{"d-2": {"note deploy_not_compared"}}},
		{"neither version has a recorded repository", "", "", map[string][]string{"d-2": {"note deploy_not_compared"}}},
		{"a recorded repository with quote characters", `"acme/old"`, `"acme/old"`, map[string][]string{"d-2": {"note deploy_not_compared"}}},
		{"a recorded repository with a further part", "acme/old/x", "acme/old/x", map[string][]string{"d-2": {"note deploy_not_compared"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome := Timing(setup(tc.current, tc.previous))
			got := decisions(t, outcome)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) || len(outcome.Missing) != 0 {
				t.Fatalf("decisions %v missing %v, want %v", got, outcome.Missing, tc.want)
			}
		})
	}
}

// TestTimingUnknownComparisonStatus proves that a stored comparison with a
// status the store never writes proves nothing: the rule counts it as
// missing and waits, and never reads it as "does not hold"
// (component-production-feedback invariant 14).
func TestTimingUnknownComparisonStatus(t *testing.T) {
	f := &timingFixture{}
	f.deploy("d-1", "1", shaA, base, store.FeedbackSourceGitHub)
	f.deploy("d-2", "2", shaB, base.Add(10*time.Minute), store.FeedbackSourceGitHub)
	f.merged = append(f.merged, store.FeedbackMergedTask{TaskID: "t-1", Repository: "app", GitHubRepository: "acme/app", MergeCommit: shaC})
	f.comparisons = append(f.comparisons,
		store.FeedbackComparison{FeedbackComparisonKey: store.FeedbackComparisonKey{Repository: "acme/app", Base: shaC, Head: shaB}, Status: store.FeedbackCompareAhead},
		store.FeedbackComparison{FeedbackComparisonKey: store.FeedbackComparisonKey{Repository: "acme/app", Base: shaC, Head: shaA}, Status: "maybe"})
	outcome := Timing(f.input(base.Add(20 * time.Minute)))
	want := []store.FeedbackComparisonKey{{Repository: "acme/app", Base: shaC, Head: shaA}}
	if outcome.Ended || outcome.Waiting != WaitingForComparisons || !slices.Equal(outcome.Missing, want) {
		t.Fatalf("outcome %+v, want waiting for the unknown answer", outcome)
	}
}

// TestTimingMalformedRecordedCommit proves that a stored resolution whose
// commit is not a full commit ID proves nothing: the deploy gives
// deploy_not_compared and links no task.
func TestTimingMalformedRecordedCommit(t *testing.T) {
	for _, bad := range []string{"abc1234", strings.ToUpper(shaB), " " + shaB} {
		f := &timingFixture{}
		f.deploy("d-1", "1", shaA, base, store.FeedbackSourceGitHub)
		f.deploy("d-2", "2", shaB, base.Add(10*time.Minute), store.FeedbackSourceGitHub)
		f.task("t-new", shaC, shaB)
		f.resolutions[1].ResolvedCommit = bad
		if got := decisions(t, Timing(f.input(base.Add(20*time.Minute)))); fmt.Sprint(got) != fmt.Sprint(map[string][]string{"d-2": {"note deploy_not_compared"}}) {
			t.Fatalf("commit %q: decisions %v, want deploy_not_compared", bad, got)
		}
	}
}
