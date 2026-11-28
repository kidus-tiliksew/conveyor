package feedback

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

var statusBase = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return statusBase.Add(time.Duration(minutes) * time.Minute) }

func sha(seed string) string { return (seed + strings.Repeat("0", 40))[:40] }

// statusFixture builds stored inputs for the pure status tests.
type statusFixture struct {
	in Inputs
}

func newStatusFixture() *statusFixture {
	return &statusFixture{in: Inputs{
		Now:   at(100),
		Slugs: map[string]string{"conveyor": "Acme/Conveyor", "app": "acme/app"},
		State: store.FeedbackInputs{Signals: map[string]store.FeedbackSignalState{}, Running: map[string][]store.FeedbackRunningVersion{}, Deploys: map[string][]store.FeedbackDeploy{}},
	}}
}

func (f *statusFixture) environment(id, name string, branches map[string]string) {
	f.in.Environments = append(f.in.Environments, store.Environment{ID: id, Name: name, TrackedBranches: branches})
}

// deploy records a deploy of commit for service, resolved in repository
// when repository is not empty, running when stopped is negative.
func (f *statusFixture) deploy(environmentID, service, commit, repository string, deployed, stopped int) {
	version := store.FeedbackVersion{Commit: commit[:7]}
	deploy := store.FeedbackDeploy{ID: environmentID + service + commit, EnvironmentID: environmentID, Service: service, FeedbackVersion: version, DeployedAt: at(deployed), Origin: store.FeedbackOrigin{Source: store.FeedbackSourceSignal, Report: int64(deployed + 1)}}
	if stopped >= 0 {
		stop := at(stopped)
		deploy.StoppedAt, deploy.StopOrigin = &stop, &store.FeedbackOrigin{Source: store.FeedbackSourceSignal, Report: int64(stopped + 1)}
	} else {
		f.in.State.Running[environmentID] = append(f.in.State.Running[environmentID], store.FeedbackRunningVersion{EnvironmentID: environmentID, Service: service, FeedbackVersion: version, DeployID: deploy.ID, DeployedAt: at(deployed)})
	}
	f.in.State.Deploys[environmentID] = append(f.in.State.Deploys[environmentID], deploy)
	ref := store.FeedbackVersionRef{EnvironmentID: environmentID, Service: service, FeedbackVersion: version}
	if repository != "" {
		f.in.State.Resolutions = append(f.in.State.Resolutions, store.FeedbackResolution{FeedbackVersionRef: ref, ResolvedRepository: repository, ResolvedCommit: commit})
	} else {
		f.in.State.Resolutions = append(f.in.State.Resolutions, store.FeedbackResolution{FeedbackVersionRef: ref, Reason: store.FeedbackReasonNoMatch})
	}
}

func (f *statusFixture) compare(repository, base, head, status string) {
	f.in.Comparisons = append(f.in.Comparisons, store.FeedbackComparison{FeedbackComparisonKey: store.FeedbackComparisonKey{Repository: store.FeedbackRepository(f.in.Slugs[repository]), Base: base, Head: head}, Status: status})
}

func (f *statusFixture) signal(environmentID string, minutes, interval int) {
	f.in.State.Signals[environmentID] = store.FeedbackSignalState{EnvironmentID: environmentID, LastSignalAt: at(minutes), NextSignalSeconds: interval}
}

func task(id, repository, merge string) store.FeedbackMergedTask {
	return store.FeedbackMergedTask{TaskID: id, Repository: repository, MergeCommit: merge, MergedAt: at(0)}
}

func only(t *testing.T, statuses []TaskStatus, environment string) TaskStatus {
	t.Helper()
	for _, status := range statuses {
		if status.Environment.ID == environment {
			return status
		}
	}
	t.Fatalf("no status for %s in %+v", environment, statuses)
	return TaskStatus{}
}

// TestTaskStatuses covers component-production-feedback invariants 14 to 17
// and 20 (req-production-feedback AC-3.1, AC-3.2, AC-3.6).
func TestTaskStatuses(t *testing.T) {
	merge, old, current, other := sha("aa"), sha("b1"), sha("b2"), sha("c1")
	t.Run("live with two services on different versions", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", nil)
		f.signal("env-prod", 90, 60)
		f.deploy("env-prod", "api", current, "conveyor", 10, -1)
		f.deploy("env-prod", "worker", old, "conveyor", 5, -1)
		f.compare("conveyor", merge, current, store.FeedbackCompareAhead)
		f.compare("conveyor", merge, old, store.FeedbackCompareAhead)
		statuses, _, missing := TaskStatuses(f.in, task("t1", "conveyor", merge))
		status := only(t, statuses, "env-prod")
		if status.Status != StatusLive || strings.Join(status.Services, ",") != "api,worker" || !status.LiveSince.Equal(at(5)) || status.AsOf == nil || !status.AsOf.Equal(at(90)) || !missing.Empty() {
			t.Fatalf("status=%+v missing=%+v", status, missing)
		}
	})
	t.Run("live names only the services that hold it", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", nil)
		f.deploy("env-prod", "api", current, "conveyor", 10, -1)
		f.deploy("env-prod", "worker", old, "conveyor", 5, -1)
		f.compare("conveyor", merge, current, store.FeedbackCompareIdentical)
		f.compare("conveyor", merge, old, store.FeedbackCompareBehind)
		status := only(t, first(TaskStatuses(f.in, task("t1", "conveyor", merge))), "env-prod")
		if status.Status != StatusLive || strings.Join(status.Services, ",") != "api" || !status.LiveSince.Equal(at(10)) || status.AsOf != nil {
			t.Fatalf("status=%+v", status)
		}
	})
	t.Run("a proven live service is served while a stopped version's comparison is missing", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", nil)
		f.deploy("env-prod", "api", old, "conveyor", 5, 10)
		f.deploy("env-prod", "api", current, "conveyor", 10, -1)
		f.compare("conveyor", merge, current, store.FeedbackCompareAhead)
		// No answer is stored for the stopped version, and none is asked for.
		statuses, _, missing := TaskStatuses(f.in, task("t1", "conveyor", merge))
		status := only(t, statuses, "env-prod")
		if status.Status != StatusLive || strings.Join(status.Services, ",") != "api" || !status.LiveSince.Equal(at(10)) || !missing.Empty() {
			t.Fatalf("status=%+v missing=%+v", status, missing)
		}
	})
	t.Run("a missing answer for a running version still waits", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", nil)
		f.deploy("env-prod", "api", current, "conveyor", 10, -1)
		f.deploy("env-prod", "worker", old, "conveyor", 5, -1)
		f.compare("conveyor", merge, current, store.FeedbackCompareAhead)
		statuses, _, missing := TaskStatuses(f.in, task("t1", "conveyor", merge))
		want := store.FeedbackComparisonKey{Repository: "acme/conveyor", Base: merge, Head: old}
		if len(statuses) != 0 || len(missing.Comparisons) != 1 || missing.Comparisons[0] != want {
			t.Fatalf("statuses=%+v missing=%+v", statuses, missing)
		}
	})
	t.Run("rollback is no longer live", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", nil)
		f.deploy("env-prod", "api", current, "conveyor", 10, 20)
		f.deploy("env-prod", "api", old, "conveyor", 20, -1)
		f.compare("conveyor", merge, current, store.FeedbackCompareAhead)
		f.compare("conveyor", merge, old, store.FeedbackCompareDiverged)
		status := only(t, first(TaskStatuses(f.in, task("t1", "conveyor", merge))), "env-prod")
		if status.Status != StatusNoLongerLive || len(status.Services) != 0 || status.LiveSince != nil {
			t.Fatalf("status=%+v", status)
		}
	})
	t.Run("not live yet with the tracked branch", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", map[string]string{"conveyor": "main"})
		f.deploy("env-prod", "api", old, "conveyor", 10, -1)
		f.compare("conveyor", merge, old, store.FeedbackCompareBehind)
		f.in.BranchHeads = []store.FeedbackBranchHead{{Repository: "acme/conveyor", Branch: "main", Commit: current, ObservedAt: at(99)}}
		f.compare("conveyor", merge, current, store.FeedbackCompareAhead)
		statuses, _, missing := TaskStatuses(f.in, task("t1", "conveyor", merge))
		status := only(t, statuses, "env-prod")
		if status.Status != StatusNotLiveYet || !status.OnTrackedBranch || !missing.Empty() {
			t.Fatalf("status=%+v missing=%+v", status, missing)
		}
	})
	t.Run("a tracked branch head that is not stored waits", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", map[string]string{"conveyor": "main"})
		statuses, _, missing := TaskStatuses(f.in, task("t1", "conveyor", merge))
		if len(statuses) != 0 || len(missing.Branches) != 1 || missing.Branches[0] != (BranchRef{Slug: "acme/conveyor", Branch: "main"}) {
			t.Fatalf("statuses=%+v missing=%+v", statuses, missing)
		}
	})
	t.Run("an old tracked branch head is served and read again", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", map[string]string{"conveyor": "main"})
		f.in.BranchHeads = []store.FeedbackBranchHead{{Repository: "acme/conveyor", Branch: "main", Commit: current, ObservedAt: at(80)}}
		f.compare("conveyor", merge, current, store.FeedbackCompareBehind)
		statuses, _, missing := TaskStatuses(f.in, task("t1", "conveyor", merge))
		if status := only(t, statuses, "env-prod"); status.Status != StatusNotLiveYet || status.OnTrackedBranch || len(missing.Branches) != 1 {
			t.Fatalf("status=%+v missing=%+v", status, missing)
		}
	})
	t.Run("a tracked branch that GitHub confirmed is gone holds no task", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", map[string]string{"conveyor": "main"})
		f.in.BranchHeads = []store.FeedbackBranchHead{{Repository: "acme/conveyor", Branch: "main", Commit: store.FeedbackBranchAbsent, ObservedAt: at(99)}}
		statuses, _, missing := TaskStatuses(f.in, task("t1", "conveyor", merge))
		if status := only(t, statuses, "env-prod"); status.Status != StatusNotLiveYet || status.OnTrackedBranch || !missing.Empty() {
			t.Fatalf("status=%+v missing=%+v", status, missing)
		}
		// An old absence is still served and read again.
		f.in.BranchHeads[0].ObservedAt = at(80)
		statuses, _, missing = TaskStatuses(f.in, task("t1", "conveyor", merge))
		if status := only(t, statuses, "env-prod"); status.Status != StatusNotLiveYet || status.OnTrackedBranch || len(missing.Branches) != 1 || len(missing.Comparisons) != 0 {
			t.Fatalf("status=%+v missing=%+v", status, missing)
		}
	})
	t.Run("a missing merge commit is unknown", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", nil)
		f.deploy("env-prod", "api", current, "conveyor", 10, -1)
		f.compare("conveyor", merge, current, store.FeedbackCompareMissing)
		if status := only(t, first(TaskStatuses(f.in, task("t1", "conveyor", merge))), "env-prod"); status.Status != StatusUnknown {
			t.Fatalf("status=%+v", status)
		}
	})
	t.Run("an unresolved version contains no task", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", nil)
		f.deploy("env-prod", "api", current, "conveyor", 10, -1)
		f.deploy("env-prod", "web", other, "", 10, -1)
		f.in.State.Deploys["env-prod"][1].Repository = "conveyor"
		f.in.State.Running["env-prod"][1].Repository = "conveyor"
		f.in.State.Resolutions[1].Repository = "conveyor"
		f.compare("conveyor", merge, current, store.FeedbackCompareBehind)
		statuses, _, missing := TaskStatuses(f.in, task("t1", "conveyor", merge))
		if status := only(t, statuses, "env-prod"); status.Status != StatusNotLiveYet || !missing.Empty() {
			t.Fatalf("status=%+v missing=%+v", status, missing)
		}
	})
	t.Run("a missing comparison leaves the status out and asks for it", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", nil)
		f.environment("env-stage", "Staging", nil)
		f.deploy("env-prod", "api", current, "conveyor", 10, -1)
		f.deploy("env-stage", "api", current, "conveyor", 10, -1)
		f.compare("conveyor", merge, old, store.FeedbackCompareAhead)
		statuses, _, missing := TaskStatuses(f.in, task("t1", "conveyor", merge))
		want := store.FeedbackComparisonKey{Repository: "acme/conveyor", Base: merge, Head: current}
		if len(statuses) != 0 || len(missing.Comparisons) == 0 || missing.Comparisons[0] != want {
			t.Fatalf("statuses=%+v missing=%+v", statuses, missing)
		}
	})
	t.Run("relevance and archive", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", nil)
		f.environment("env-app", "App only", nil)
		f.environment("env-old", "Archived", nil)
		f.in.Environments[2].Archived = true
		f.deploy("env-prod", "api", current, "conveyor", 10, -1)
		f.deploy("env-app", "web", other, "app", 10, -1)
		f.deploy("env-old", "api", current, "conveyor", 10, -1)
		f.compare("conveyor", merge, current, store.FeedbackCompareAhead)
		statuses, _, _ := TaskStatuses(f.in, task("t1", "conveyor", merge))
		if len(statuses) != 1 || statuses[0].Environment != (EnvironmentRef{ID: "env-prod", Name: "Production"}) {
			t.Fatalf("statuses=%+v", statuses)
		}
	})
	t.Run("a merge commit equal to the version needs no comparison", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", nil)
		f.deploy("env-prod", "api", merge, "conveyor", 10, -1)
		statuses, _, missing := TaskStatuses(f.in, task("t1", "conveyor", merge))
		if status := only(t, statuses, "env-prod"); status.Status != StatusLive || !missing.Empty() {
			t.Fatalf("status=%+v missing=%+v", status, missing)
		}
	})
	t.Run("a GitHub deployment report dates the status", func(t *testing.T) {
		f := newStatusFixture()
		f.environment("env-prod", "Production", nil)
		f.signal("env-prod", 30, 60)
		f.deploy("env-prod", "conveyor", merge, "conveyor", 50, -1)
		f.in.State.Deploys["env-prod"][0].Origin.Source = "github"
		status := only(t, first(TaskStatuses(f.in, task("t1", "conveyor", merge))), "env-prod")
		if status.AsOf == nil || !status.AsOf.Equal(at(50)) {
			t.Fatalf("status=%+v", status)
		}
	})
}

func first(statuses []TaskStatus, _ bool, _ Missing) []TaskStatus { return statuses }

// TestCrateStatus covers invariant 18 (req-production-feedback AC-3.4).
func TestCrateStatus(t *testing.T) {
	mergeA, mergeB, version := sha("a1"), sha("a2"), sha("b1")
	f := newStatusFixture()
	f.environment("env-prod", "Production", nil)
	f.environment("env-stage", "Staging", nil)
	f.environment("env-app", "App", nil)
	f.signal("env-prod", 99, 60)
	f.signal("env-stage", 10, 60)
	f.deploy("env-prod", "api", version, "conveyor", 20, -1)
	f.deploy("env-stage", "api", version, "conveyor", 30, -1)
	f.deploy("env-app", "web", sha("c1"), "app", 30, -1)
	f.compare("conveyor", mergeA, version, store.FeedbackCompareAhead)
	f.compare("conveyor", mergeB, version, store.FeedbackCompareDiverged)
	members := []store.FeedbackMergedTask{task("ta", "conveyor", mergeA), task("tb", "conveyor", mergeB), task("tc", "conveyor", "")}
	lines, liveIn, _, missing := CrateStatus(f.in, members, nil)
	if len(lines) != 2 || !missing.Empty() {
		t.Fatalf("lines=%+v missing=%+v", lines, missing)
	}
	prod, stage := lines[0], lines[1]
	if prod.Environment.Name != "Production" || prod.LiveTaskCount != 1 || prod.MergedTaskCount != 3 || !prod.NewestLiveAt.Equal(at(20)) || !prod.Listening || !prod.LastSignalAt.Equal(at(99)) || !prod.AsOf.Equal(at(99)) {
		t.Fatalf("production=%+v", prod)
	}
	if stage.Environment.Name != "Staging" || stage.LiveTaskCount != 1 || stage.Listening || !stage.LastSignalAt.Equal(at(10)) {
		t.Fatalf("staging=%+v", stage)
	}
	if strings.Join(liveIn["ta"], ",") != "Production,Staging" || len(liveIn["tb"]) != 0 || len(liveIn["tc"]) != 0 {
		t.Fatalf("live_in=%+v", liveIn)
	}
	// A stopped version whose answer stays missing does not hide a member
	// that a running version proves live.
	f.deploy("env-prod", "api", sha("b0"), "conveyor", 5, 20)
	lines, liveIn, _, missing = CrateStatus(f.in, members, nil)
	if len(lines) != 2 || lines[0].LiveTaskCount != 1 || strings.Join(liveIn["ta"], ",") != "Production,Staging" {
		t.Fatalf("lines with a stopped version=%+v live_in=%+v", lines, liveIn)
	}
	// Member tb is not live, so its answer for the stopped version is asked for.
	if len(missing.Comparisons) != 1 || missing.Comparisons[0].Base != mergeB || missing.Comparisons[0].Head != sha("b0") {
		t.Fatalf("missing with a stopped version=%+v", missing)
	}
	if lines, _, _, _ := CrateStatus(f.in, nil, nil); len(lines) != 0 {
		t.Fatalf("lines without members=%+v", lines)
	}
}

// TestCrateStatusOpenReportCounts proves the count on each environment line
// at the crate projection boundary: a line carries its environment's count
// and 0 when it has none; a count with no environment, and a count of an
// environment that has no line, make no line and reach no line
// (component-feedback-reports invariant 23; feature-production-feedback-listening
// approved plan: environment-less reports stay out of the counts).
func TestCrateStatusOpenReportCounts(t *testing.T) {
	merge, version := sha("a1"), sha("b1")
	f := newStatusFixture()
	f.environment("env-prod", "Production", nil)
	f.environment("env-stage", "Staging", nil)
	f.environment("env-app", "App", nil)
	f.deploy("env-prod", "api", version, "conveyor", 20, -1)
	f.deploy("env-stage", "api", version, "conveyor", 30, -1)
	f.deploy("env-app", "web", sha("c1"), "app", 30, -1)
	f.compare("conveyor", merge, version, store.FeedbackCompareAhead)
	members := []store.FeedbackMergedTask{task("ta", "conveyor", merge)}
	counts := map[string]int{"env-prod": 3, "": 7, "env-app": 5, "env-unknown": 9}
	lines, _, _, _ := CrateStatus(f.in, members, counts)
	if len(lines) != 2 {
		t.Fatalf("lines=%+v, want only the two relevant environments", lines)
	}
	got := map[string]int{}
	for _, line := range lines {
		got[line.Environment.ID] = line.OpenReportCount
	}
	if want := map[string]int{"env-prod": 3, "env-stage": 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("counts on the lines=%v, want %v", got, want)
	}
	total := 0
	for _, line := range lines {
		total += line.OpenReportCount
	}
	if total != 3 {
		t.Fatalf("the lines count %d reports, want 3: no environment-less count reaches a line", total)
	}
	// Without counts every line shows 0, and the lines are the same.
	plain, _, _, _ := CrateStatus(f.in, members, nil)
	if len(plain) != len(lines) || plain[0].OpenReportCount != 0 || plain[1].OpenReportCount != 0 {
		t.Fatalf("lines without counts=%+v", plain)
	}
}

// TestStatusPending covers environments_pending (component-production-feedback
// invariant 20; req-production-feedback AC-3.2): it is true only when a
// relevant status is left out because a comparison or a tracked branch head is
// not stored yet.
func TestStatusPending(t *testing.T) {
	merge, old, current := sha("aa"), sha("b1"), sha("b2")
	cases := []struct {
		name    string
		setup   func(f *statusFixture)
		task    store.FeedbackMergedTask
		pending bool
		served  int
		missing bool
	}{
		{name: "a missing comparison for a running version is pending", setup: func(f *statusFixture) {
			f.environment("env-prod", "Production", nil)
			f.deploy("env-prod", "api", current, "conveyor", 10, -1)
		}, pending: true, missing: true},
		{name: "a tracked branch head that is not stored is pending", setup: func(f *statusFixture) {
			f.environment("env-prod", "Production", map[string]string{"conveyor": "main"})
		}, pending: true, missing: true},
		{name: "a stored head without its comparison is pending", setup: func(f *statusFixture) {
			f.environment("env-prod", "Production", map[string]string{"conveyor": "main"})
			f.in.BranchHeads = []store.FeedbackBranchHead{{Repository: "acme/conveyor", Branch: "main", Commit: current, ObservedAt: at(80)}}
		}, pending: true, missing: true},
		{name: "a served stale head is not pending", setup: func(f *statusFixture) {
			f.environment("env-prod", "Production", map[string]string{"conveyor": "main"})
			f.in.BranchHeads = []store.FeedbackBranchHead{{Repository: "acme/conveyor", Branch: "main", Commit: current, ObservedAt: at(80)}}
			f.compare("conveyor", merge, current, store.FeedbackCompareBehind)
		}, served: 1, missing: true},
		{name: "a live task with a stopped version's comparison missing is not pending", setup: func(f *statusFixture) {
			f.environment("env-prod", "Production", nil)
			f.deploy("env-prod", "api", old, "conveyor", 5, 10)
			f.deploy("env-prod", "api", current, "conveyor", 10, -1)
			f.compare("conveyor", merge, current, store.FeedbackCompareAhead)
		}, served: 1},
		{name: "a missing merge commit stays unknown and is not pending", setup: func(f *statusFixture) {
			f.environment("env-prod", "Production", nil)
			f.deploy("env-prod", "api", current, "conveyor", 10, -1)
			f.deploy("env-prod", "worker", old, "conveyor", 5, -1)
			f.compare("conveyor", merge, current, store.FeedbackCompareMissing)
		}, served: 1},
		{name: "a workspace with no environment is not pending", setup: func(*statusFixture) {}},
		{name: "an archived environment is not pending", setup: func(f *statusFixture) {
			f.environment("env-old", "Archived", nil)
			f.in.Environments[0].Archived = true
			f.deploy("env-old", "api", current, "conveyor", 10, -1)
		}},
		{name: "a task without a merge commit is left out and not pending", setup: func(f *statusFixture) {
			f.environment("env-prod", "Production", nil)
			f.deploy("env-prod", "api", current, "conveyor", 10, -1)
		}, task: task("t1", "conveyor", "")},
		{name: "a repository without a slug is left out and not pending", setup: func(f *statusFixture) {
			f.environment("env-prod", "Production", nil)
			f.deploy("env-prod", "api", current, "conveyor", 10, -1)
			delete(f.in.Slugs, "conveyor")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStatusFixture()
			tc.setup(f)
			merged := tc.task
			if merged.TaskID == "" {
				merged = task("t1", "conveyor", merge)
			}
			statuses, pending, missing := TaskStatuses(f.in, merged)
			if pending != tc.pending || len(statuses) != tc.served || missing.Empty() == tc.missing {
				t.Fatalf("pending=%v statuses=%+v missing=%+v", pending, statuses, missing)
			}
		})
	}
}

// TestCrateStatusPending proves that a pending member sets the crate's flag
// even when its environment already has a line, and that the flag changes no
// line, count, or live_in entry (invariants 18 and 20).
func TestCrateStatusPending(t *testing.T) {
	mergeA, mergeB, version := sha("a1"), sha("a2"), sha("b1")
	f := newStatusFixture()
	f.environment("env-prod", "Production", nil)
	f.signal("env-prod", 99, 60)
	f.deploy("env-prod", "api", version, "conveyor", 20, -1)
	f.compare("conveyor", mergeA, version, store.FeedbackCompareAhead)
	members := []store.FeedbackMergedTask{task("ta", "conveyor", mergeA), task("tb", "conveyor", mergeB)}
	lines, liveIn, pending, missing := CrateStatus(f.in, members, nil)
	if !pending || len(missing.Comparisons) != 1 || missing.Comparisons[0].Base != mergeB {
		t.Fatalf("pending=%v missing=%+v", pending, missing)
	}
	if len(lines) != 1 || lines[0].LiveTaskCount != 1 || lines[0].MergedTaskCount != 2 || !lines[0].NewestLiveAt.Equal(at(20)) || strings.Join(liveIn["ta"], ",") != "Production" || len(liveIn["tb"]) != 0 {
		t.Fatalf("lines=%+v live_in=%+v", lines, liveIn)
	}
	// Once the answer is stored, the same lines are served and nothing is
	// pending.
	f.compare("conveyor", mergeB, version, store.FeedbackCompareDiverged)
	settled, settledLiveIn, pending, missing := CrateStatus(f.in, members, nil)
	if pending || !missing.Empty() || !reflect.DeepEqual(settled, lines) || strings.Join(settledLiveIn["ta"], ",") != "Production" {
		t.Fatalf("pending=%v missing=%+v lines=%+v", pending, missing, settled)
	}
	// A member without a merge commit is left out and is not pending.
	f = newStatusFixture()
	f.environment("env-prod", "Production", nil)
	f.deploy("env-prod", "api", version, "conveyor", 20, -1)
	if lines, _, pending, _ := CrateStatus(f.in, []store.FeedbackMergedTask{task("tc", "conveyor", "")}, nil); pending || len(lines) != 1 || lines[0].LiveTaskCount != 0 {
		t.Fatalf("pending=%v lines=%+v", pending, lines)
	}
	if lines, _, pending, _ := CrateStatus(newStatusFixture().in, members, nil); pending || len(lines) != 0 {
		t.Fatalf("no environment: pending=%v lines=%+v", pending, lines)
	}
}

// TestRequiredComparisons proves a read loads every answer a derivation can
// use: running and past versions and each observed tracked branch head.
func TestRequiredComparisons(t *testing.T) {
	merge := sha("aa")
	f := newStatusFixture()
	f.environment("env-prod", "Production", map[string]string{"conveyor": "main"})
	f.deploy("env-prod", "api", sha("b1"), "conveyor", 10, 20)
	f.deploy("env-prod", "api", sha("b2"), "conveyor", 20, -1)
	f.deploy("env-prod", "web", sha("c1"), "app", 20, -1)
	f.in.BranchHeads = []store.FeedbackBranchHead{{Repository: "acme/conveyor", Branch: "main", Commit: sha("b3"), ObservedAt: at(99)}}
	keys := RequiredComparisons(f.in, []store.FeedbackMergedTask{task("t1", "conveyor", merge), task("t2", "conveyor", "")})
	if len(keys) != 3 || keys[0].Head != sha("b1") || keys[1].Head != sha("b2") || keys[2].Head != sha("b3") {
		t.Fatalf("keys=%+v", keys)
	}
	// A confirmed absence of the branch names no commit to compare.
	f.in.BranchHeads[0].Commit = store.FeedbackBranchAbsent
	if keys := RequiredComparisons(f.in, []store.FeedbackMergedTask{task("t1", "conveyor", merge)}); len(keys) != 2 || keys[1].Head != sha("b2") {
		t.Fatalf("keys=%+v", keys)
	}
}

// TestNewestUnresolved covers invariant 21 (req-production-feedback AC-2.3).
func TestNewestUnresolved(t *testing.T) {
	f := newStatusFixture()
	f.deploy("env-prod", "api", sha("b1"), "conveyor", 10, -1)
	f.deploy("env-prod", "web", sha("c1"), "", 20, -1)
	f.deploy("env-prod", "worker", sha("d1"), "", 5, -1)
	version, reason, ok := NewestUnresolved(f.in.State, "env-prod")
	if !ok || version.Service != "web" || reason != store.FeedbackReasonNoMatch {
		t.Fatalf("version=%+v reason=%q ok=%v", version, reason, ok)
	}
	f.in.State.Running["env-prod"] = append(f.in.State.Running["env-prod"], store.FeedbackRunningVersion{EnvironmentID: "env-prod", Service: "new", FeedbackVersion: store.FeedbackVersion{Version: "v9"}, DeployedAt: at(30)})
	if version, reason, ok := NewestUnresolved(f.in.State, "env-prod"); !ok || version.Service != "new" || reason != store.FeedbackReasonPending {
		t.Fatalf("version=%+v reason=%q", version, reason)
	}
	if _, _, ok := NewestUnresolved(f.in.State, "env-none"); ok {
		t.Fatal("an environment without versions has an unresolved one")
	}
}

// TestStatusIgnoresResolvedGitHubRepository proves that live status reads the
// stored GitHub repository of a resolution nowhere: the same inputs with the
// field empty, with the matching repository, and with an unrelated one give
// the same task statuses, crate lines, and required comparisons (approved
// plan version 4).
func TestStatusIgnoresResolvedGitHubRepository(t *testing.T) {
	merge, old, current := sha("aa"), sha("b1"), sha("b2")
	build := func(github string) Inputs {
		f := newStatusFixture()
		f.environment("env-prod", "Production", nil)
		f.signal("env-prod", 90, 60)
		f.deploy("env-prod", "api", old, "conveyor", 5, 10)
		f.deploy("env-prod", "api", current, "conveyor", 10, -1)
		f.compare("conveyor", merge, current, store.FeedbackCompareAhead)
		for i := range f.in.State.Resolutions {
			if f.in.State.Resolutions[i].Resolved() {
				f.in.State.Resolutions[i].ResolvedGitHubRepository = github
			}
		}
		return f.in
	}
	type answer struct {
		Statuses []TaskStatus
		Pending  bool
		Missing  Missing
		Lines    []CrateLine
		LiveIn   map[string][]string
		Required []store.FeedbackComparisonKey
	}
	run := func(in Inputs) answer {
		tasks := []store.FeedbackMergedTask{task("t1", "conveyor", merge), task("t2", "conveyor", sha("cc"))}
		statuses, pending, missing := TaskStatuses(in, tasks[0])
		lines, liveIn, _, _ := CrateStatus(in, tasks, nil)
		return answer{statuses, pending, missing, lines, liveIn, RequiredComparisons(in, tasks)}
	}
	want := run(build(""))
	for _, github := range []string{"acme/conveyor", "someone/else"} {
		if got := run(build(github)); !reflect.DeepEqual(got, want) {
			t.Fatalf("with %q: %+v\nwant %+v", github, got, want)
		}
	}
}
