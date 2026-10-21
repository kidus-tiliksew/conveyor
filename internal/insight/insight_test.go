package insight

import (
	"crypto/sha256"
	"encoding/hex"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

// fixedNow is a Wednesday: 2026-10-21 is in ISO week 2026-W43.
var fixedNow = time.Date(2026, 10, 21, 15, 30, 0, 0, time.UTC)

func at(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func serving(id string, state core.TaskState, since time.Time, dependsOn ...string) ServingTask {
	return ServingTask{Task: core.Task{ID: id, Title: "Task " + id, State: state, Body: "body of " + id, CreatedAt: since.Add(-time.Hour)}, ServingSince: since, DependsOn: dependsOn}
}

func compact(actor, label string) nextaction.Compact {
	deadline := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	return nextaction.Compact{Label: label, Actor: actor, Kind: "kind-" + actor, Judgment: actor == nextaction.ActorOperator, Summary: "summary " + label, Deadline: &deadline, Blocks: []nextaction.BlockerRef{}}
}

func ids(refs []TaskRef) []string {
	out := []string{}
	for _, ref := range refs {
		out = append(out, ref.ID)
	}
	return out
}

func TestGroupsActorOrderAndVerbatimActions(t *testing.T) {
	since := at("2026-10-01T00:00:00Z")
	inputs := Inputs{Requirement: Requirement{ID: "req-groups"}, Tasks: []ServingTask{
		serving("t-op-2", core.TaskAwaiting, since),
		serving("t-agent", core.TaskRunning, since),
		serving("t-op-1", core.TaskQueued, since),
		serving("t-missing", core.TaskQueued, since),
		serving("t-odd", core.TaskQueued, since),
		serving("t-dep", core.TaskQueued, since),
		serving("t-factory", core.TaskRunning, since),
		serving("t-merged", core.TaskMerged, since),
		serving("t-closed", core.TaskClosed, since),
	}}
	actions := map[string]nextaction.Compact{
		"t-op-2":    compact(nextaction.ActorOperator, "Approve plan"),
		"t-op-1":    compact(nextaction.ActorOperator, "Merge"),
		"t-agent":   compact(nextaction.ActorAgent, "Implementing"),
		"t-factory": compact(nextaction.ActorFactory, "Retrying"),
		"t-dep":     compact(nextaction.ActorDependency, "Waiting on dependency"),
		"t-odd":     compact("someone-else", "Unknown actor"),
		"t-merged":  compact(nextaction.ActorOperator, "Should not appear"),
		"t-closed":  compact(nextaction.ActorOperator, "Should not appear"),
	}
	groups := Groups(inputs, actions, fixedNow)
	var actors []string
	for _, group := range groups {
		actors = append(actors, group.Actor)
		if group.Count != len(group.Tasks) || group.Tasks == nil {
			t.Fatalf("group %s count=%d tasks=%v", group.Actor, group.Count, group.Tasks)
		}
		for _, ref := range group.Tasks {
			if ref.Title != "Task "+ref.ID {
				t.Fatalf("task ref %+v lost its title", ref)
			}
			if !reflect.DeepEqual(ref.NextAction, actions[ref.ID]) {
				t.Fatalf("task %s next action %+v, want verbatim %+v", ref.ID, ref.NextAction, actions[ref.ID])
			}
		}
	}
	if want := []string{"operator", "agent", "factory", "dependency", "none"}; !slices.Equal(actors, want) {
		t.Fatalf("actor order %v, want %v", actors, want)
	}
	want := map[string][]string{
		"operator":   {"t-op-1", "t-op-2"},
		"agent":      {"t-agent"},
		"factory":    {"t-factory"},
		"dependency": {"t-dep"},
		"none":       {"t-missing", "t-odd"},
	}
	for _, group := range groups {
		if got := ids(group.Tasks); !slices.Equal(got, want[group.Actor]) {
			t.Fatalf("group %s tasks %v, want %v", group.Actor, got, want[group.Actor])
		}
	}
	// A missing next action stays the zero compact: no label is invented.
	if none := groups[4].Tasks[0]; none.ID != "t-missing" || !reflect.DeepEqual(none.NextAction, nextaction.Compact{}) {
		t.Fatalf("missing next action became %+v", none.NextAction)
	}
	empty := Groups(Inputs{}, nil, fixedNow)
	if len(empty) != 5 {
		t.Fatalf("empty groups %+v", empty)
	}
	for _, group := range empty {
		if group.Count != 0 || group.Tasks == nil || len(group.Tasks) != 0 {
			t.Fatalf("empty group %+v", group)
		}
	}
}

func TestCriticalPathDependencyOrderAndTieBreak(t *testing.T) {
	since := at("2026-10-01T00:00:00Z")
	t.Run("longest chain listed from the first dependency", func(t *testing.T) {
		inputs := Inputs{Tasks: []ServingTask{
			serving("z-last", core.TaskQueued, since, "m-middle"),
			serving("m-middle", core.TaskRunning, since, "a-first"),
			serving("a-first", core.TaskQueued, since, "done", "outside"),
			serving("done", core.TaskMerged, since),
			serving("b-alone", core.TaskQueued, since),
			serving("c-pair", core.TaskQueued, since, "b-alone"),
		}}
		actions := map[string]nextaction.Compact{"m-middle": compact(nextaction.ActorAgent, "Implementing")}
		path := CriticalPath(inputs, actions, fixedNow)
		if got := ids(path.Tasks); !slices.Equal(got, []string{"a-first", "m-middle", "z-last"}) || path.Length != 3 {
			t.Fatalf("critical path %v length %d", got, path.Length)
		}
		if !reflect.DeepEqual(path.Tasks[1].NextAction, actions["m-middle"]) || path.Tasks[1].Title != "Task m-middle" {
			t.Fatalf("path task ref %+v", path.Tasks[1])
		}
	})
	t.Run("equal lengths take the smaller ID sequence", func(t *testing.T) {
		inputs := Inputs{Tasks: []ServingTask{
			serving("d", core.TaskQueued, since, "c"),
			serving("c", core.TaskQueued, since),
			serving("b", core.TaskQueued, since, "a"),
			serving("a", core.TaskQueued, since),
		}}
		if got := ids(CriticalPath(inputs, nil, fixedNow).Tasks); !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("tie chain %v, want [a b]", got)
		}
		// One dependent of two equal-length chains takes the smaller
		// dependency, whatever the DependsOn order.
		merged := Inputs{Tasks: []ServingTask{
			serving("y", core.TaskQueued, since, "q", "p"),
			serving("q", core.TaskQueued, since),
			serving("p", core.TaskQueued, since),
		}}
		if got := ids(CriticalPath(merged, nil, fixedNow).Tasks); !slices.Equal(got, []string{"p", "y"}) {
			t.Fatalf("merged tie chain %v, want [p y]", got)
		}
	})
	t.Run("terminal tasks and external dependencies never enter", func(t *testing.T) {
		inputs := Inputs{Tasks: []ServingTask{
			serving("x", core.TaskQueued, since, "merged", "closed", "foreign"),
			serving("merged", core.TaskMerged, since),
			serving("closed", core.TaskClosed, since, "merged"),
		}}
		path := CriticalPath(inputs, nil, fixedNow)
		if got := ids(path.Tasks); !slices.Equal(got, []string{"x"}) || path.Length != 1 {
			t.Fatalf("chain %v length %d", got, path.Length)
		}
		if empty := CriticalPath(Inputs{Tasks: []ServingTask{serving("m", core.TaskMerged, since)}}, nil, fixedNow); empty.Length != 0 || empty.Tasks == nil || len(empty.Tasks) != 0 {
			t.Fatalf("no open tasks gave %+v", empty)
		}
	})
}

func TestCriticalPathSharedDependenciesAndCycleBound(t *testing.T) {
	since := at("2026-10-01T00:00:00Z")
	// A diamond: the shared dependency a is reached through b and c, and its
	// chain must still extend through both.
	diamond := Inputs{Tasks: []ServingTask{
		serving("e", core.TaskQueued, since, "d"),
		serving("d", core.TaskQueued, since, "c", "b"),
		serving("c", core.TaskQueued, since, "a"),
		serving("b", core.TaskQueued, since, "a"),
		serving("a", core.TaskQueued, since),
		serving("f", core.TaskQueued, since, "a"),
	}}
	path := CriticalPath(diamond, nil, fixedNow)
	if got := ids(path.Tasks); !slices.Equal(got, []string{"a", "b", "d", "e"}) || path.Length != 4 {
		t.Fatalf("diamond chain %v length %d", got, path.Length)
	}
	// Defensive: a cycle in corrupt input terminates with a simple chain.
	cyclic := Inputs{Tasks: []ServingTask{
		serving("p", core.TaskQueued, since, "q"),
		serving("q", core.TaskQueued, since, "r"),
		serving("r", core.TaskQueued, since, "p"),
		serving("s", core.TaskQueued, since, "p"),
		serving("self", core.TaskQueued, since, "self"),
	}}
	cycle := CriticalPath(cyclic, nil, fixedNow)
	got := ids(cycle.Tasks)
	if cycle.Length != len(got) || cycle.Length == 0 || cycle.Length > 4 {
		t.Fatalf("cyclic chain %v length %d", got, cycle.Length)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("cyclic chain repeats %s: %v", id, got)
		}
		seen[id] = true
	}
}

func TestProgressDailyScopeAndClosedExclusion(t *testing.T) {
	a := serving("a", core.TaskRunning, at("2026-10-18T09:00:00Z"))
	b := serving("b", core.TaskMerged, at("2026-10-18T23:59:59Z"))
	b.MergedAt = at("2026-10-19T01:00:00Z")
	c := serving("c", core.TaskClosed, at("2026-10-19T12:00:00Z"))
	c.ClosedAt = at("2026-10-20T08:00:00Z")
	d := serving("d", core.TaskQueued, at("2026-10-21T00:00:00Z"))
	inputs := Inputs{Tasks: []ServingTask{d, c, b, a}}
	actions := map[string]nextaction.Compact{"a": compact(nextaction.ActorAgent, "Implementing"), "d": compact(nextaction.ActorOperator, "Approve plan")}
	progress := Progress(inputs, actions, fixedNow)
	want := []DailyPoint{
		{Day: at("2026-10-18T00:00:00Z"), Scope: 2, Open: 2, Delivered: 0},
		{Day: at("2026-10-19T00:00:00Z"), Scope: 3, Open: 2, Delivered: 1},
		// Closure removes c from scope on its own UTC day.
		{Day: at("2026-10-20T00:00:00Z"), Scope: 2, Open: 1, Delivered: 1},
		{Day: at("2026-10-21T00:00:00Z"), Scope: 3, Open: 2, Delivered: 1},
	}
	if !reflect.DeepEqual(progress.Daily, want) {
		t.Fatalf("daily %+v\nwant %+v", progress.Daily, want)
	}
	// Total excludes the closed task; delivered and open follow state.
	if progress.Delivered != 1 || progress.Total != 3 || progress.Open != 2 {
		t.Fatalf("delivered=%d total=%d open=%d", progress.Delivered, progress.Total, progress.Open)
	}
	wantMix := StatusMix{Delivered: 1, Closed: 1, Open: []ActorCount{{"operator", 1}, {"agent", 1}, {"factory", 0}, {"dependency", 0}, {"none", 0}}}
	if !reflect.DeepEqual(progress.StatusMix, wantMix) {
		t.Fatalf("status mix %+v want %+v", progress.StatusMix, wantMix)
	}
	sum := progress.StatusMix.Delivered + progress.StatusMix.Closed
	for _, count := range progress.StatusMix.Open {
		sum += count.Count
	}
	if sum != len(inputs.Tasks) {
		t.Fatalf("status mix sums to %d, want %d", sum, len(inputs.Tasks))
	}
	// Event times, not the current state, place a task in the series: a task
	// whose merge event lies in the future is merged now but not yet
	// delivered on any recorded day.
	late := serving("late", core.TaskMerged, at("2026-10-21T01:00:00Z"))
	late.MergedAt = fixedNow.Add(24 * time.Hour)
	lateProgress := Progress(Inputs{Tasks: []ServingTask{late}}, nil, fixedNow)
	if got := lateProgress.Daily; len(got) != 1 || got[0].Scope != 1 || got[0].Delivered != 0 || lateProgress.Delivered != 1 {
		t.Fatalf("late merge daily %+v delivered %d", got, lateProgress.Delivered)
	}
}

func TestProgressForecastWindowsAndNoMerges(t *testing.T) {
	since := at("2026-08-01T00:00:00Z")
	merged := func(id string, mergedAt time.Time) ServingTask {
		task := serving(id, core.TaskMerged, since)
		task.MergedAt = mergedAt
		return task
	}
	inputs := Inputs{Tasks: []ServingTask{
		merged("m-14-edge", fixedNow.Add(-14*24*time.Hour)),
		merged("m-recent", fixedNow.Add(-time.Hour)),
		merged("m-14-out", fixedNow.Add(-14*24*time.Hour-time.Second)),
		merged("m-28-edge", fixedNow.Add(-28*24*time.Hour)),
		merged("m-28-out", fixedNow.Add(-28*24*time.Hour-time.Second)),
		merged("m-future", fixedNow.Add(time.Hour)),
		serving("o-1", core.TaskQueued, since),
		serving("o-2", core.TaskRunning, since),
		serving("o-3", core.TaskAwaiting, since),
	}}
	progress := Progress(inputs, nil, fixedNow)
	// Recent 14 days: the inclusive edge and the recent merge, so the rate is
	// 2/14 per day and three open tasks need ceil(3*14/2) = 21 days.
	if progress.Forecast == nil || !progress.Forecast.Day.Equal(at("2026-11-11T00:00:00Z")) || progress.Forecast.RatePerDay != float64(2)/14 {
		t.Fatalf("forecast %+v", progress.Forecast)
	}
	// Recent 28 days: both 14-day merges, the 14-day outlier, and the 28-day
	// inclusive edge.
	if progress.ThroughputPerWeek != 1 {
		t.Fatalf("throughput %v want 1", progress.ThroughputPerWeek)
	}
	// One merge rounds the forecast day up.
	single := Progress(Inputs{Tasks: []ServingTask{merged("m", fixedNow.Add(-time.Hour)), serving("o", core.TaskQueued, since)}}, nil, fixedNow)
	if single.Forecast == nil || !single.Forecast.Day.Equal(at("2026-11-04T00:00:00Z")) || single.ThroughputPerWeek != 0.25 {
		t.Fatalf("single merge forecast %+v throughput %v", single.Forecast, single.ThroughputPerWeek)
	}
	// Nothing open forecasts today.
	done := Progress(Inputs{Tasks: []ServingTask{merged("m", fixedNow.Add(-time.Hour))}}, nil, fixedNow)
	if done.Forecast == nil || !done.Forecast.Day.Equal(at("2026-10-21T00:00:00Z")) {
		t.Fatalf("all delivered forecast %+v", done.Forecast)
	}
	none := Progress(Inputs{Tasks: []ServingTask{serving("o", core.TaskQueued, since), merged("old", fixedNow.Add(-30*24*time.Hour))}}, nil, fixedNow)
	if none.Forecast != nil {
		t.Fatalf("no recent merge produced forecast %+v", none.Forecast)
	}
	if none.ThroughputPerWeek != 0 {
		t.Fatalf("no recent merge throughput %v", none.ThroughputPerWeek)
	}
	unmerged := Progress(Inputs{Tasks: []ServingTask{serving("o", core.TaskQueued, since)}}, nil, fixedNow)
	if unmerged.Forecast != nil || unmerged.MedianCycleSeconds != nil || unmerged.Delivered != 0 {
		t.Fatalf("nothing merged gave forecast %+v median %v delivered %d", unmerged.Forecast, unmerged.MedianCycleSeconds, unmerged.Delivered)
	}
}

func TestProgressISOWeeksAndMedian(t *testing.T) {
	now := at("2027-01-06T10:00:00Z") // Wednesday of 2027-W01; 2026 has 53 ISO weeks
	since := at("2026-11-01T00:00:00Z")
	merged := func(id string, created, mergedAt time.Time) ServingTask {
		task := serving(id, core.TaskMerged, since)
		task.Task.CreatedAt, task.MergedAt = created, mergedAt
		return task
	}
	base := at("2026-11-01T00:00:00Z")
	inputs := Inputs{Tasks: []ServingTask{
		merged("w01-monday", base, at("2027-01-04T00:00:00Z")),
		merged("w53-sunday", base, at("2027-01-03T23:59:59Z")),
		merged("w47-start", base, at("2026-11-16T00:00:00Z")),
		merged("w46-before", base, at("2026-11-15T23:59:59Z")),
		merged("w01-future", base, at("2027-01-07T00:00:00Z")),
	}}
	progress := Progress(inputs, nil, now)
	if len(progress.Weekly) != 8 {
		t.Fatalf("weekly buckets %d", len(progress.Weekly))
	}
	wantWeeks := []struct{ year, week, delivered int }{{2026, 47, 1}, {2026, 48, 0}, {2026, 49, 0}, {2026, 50, 0}, {2026, 51, 0}, {2026, 52, 0}, {2026, 53, 1}, {2027, 1, 1}}
	for i, bucket := range progress.Weekly {
		want := wantWeeks[i]
		if bucket.ISOYear != want.year || bucket.ISOWeek != want.week || bucket.Delivered != want.delivered || bucket.InProgress != (i == 7) {
			t.Fatalf("week %d = %+v, want %+v", i, bucket, want)
		}
		if bucket.WeekStart.Weekday() != time.Monday || bucket.WeekStart.Location() != time.UTC || bucket.WeekStart.Hour() != 0 {
			t.Fatalf("week %d start %s is not a UTC Monday midnight", i, bucket.WeekStart)
		}
	}
	if !progress.Weekly[0].WeekStart.Equal(at("2026-11-16T00:00:00Z")) || !progress.Weekly[7].WeekStart.Equal(at("2027-01-04T00:00:00Z")) {
		t.Fatalf("week range %s..%s", progress.Weekly[0].WeekStart, progress.Weekly[7].WeekStart)
	}

	created := at("2026-12-01T00:00:00Z")
	cycle := func(id string, hours int) ServingTask {
		return merged(id, created, created.Add(time.Duration(hours)*time.Hour))
	}
	even := Progress(Inputs{Tasks: []ServingTask{cycle("c4", 4), cycle("c1", 1), cycle("c3", 3), cycle("c2", 2), serving("open", core.TaskQueued, since)}}, nil, now)
	if even.MedianCycleSeconds == nil || *even.MedianCycleSeconds != 9000 {
		t.Fatalf("even median %v want 9000", even.MedianCycleSeconds)
	}
	odd := Progress(Inputs{Tasks: []ServingTask{cycle("c5", 5), cycle("c1", 1), cycle("c3", 3)}}, nil, now)
	if odd.MedianCycleSeconds == nil || *odd.MedianCycleSeconds != 3*3600 {
		t.Fatalf("odd median %v want 10800", odd.MedianCycleSeconds)
	}
}

func TestProgressEmptyInputs(t *testing.T) {
	progress := Progress(Inputs{Requirement: Requirement{ID: "req-empty", CurrentVersion: 1}}, nil, fixedNow)
	if progress.Daily == nil || len(progress.Daily) != 0 {
		t.Fatalf("empty daily %+v", progress.Daily)
	}
	if progress.Forecast != nil || progress.MedianCycleSeconds != nil || progress.Delivered != 0 || progress.Total != 0 || progress.Open != 0 || progress.ThroughputPerWeek != 0 {
		t.Fatalf("empty progress %+v", progress)
	}
	if len(progress.Weekly) != 8 {
		t.Fatalf("empty weekly %+v", progress.Weekly)
	}
	for i, bucket := range progress.Weekly {
		if bucket.Delivered != 0 || bucket.InProgress != (i == 7) {
			t.Fatalf("empty week %d %+v", i, bucket)
		}
	}
	if !progress.Weekly[7].WeekStart.Equal(at("2026-10-19T00:00:00Z")) || progress.Weekly[7].ISOWeek != 43 {
		t.Fatalf("current week %+v", progress.Weekly[7])
	}
	if progress.StatusMix.Delivered != 0 || progress.StatusMix.Closed != 0 || len(progress.StatusMix.Open) != 5 {
		t.Fatalf("empty status mix %+v", progress.StatusMix)
	}
}

func associationFixture() (Inputs, []DesignInput, []core.Decision) {
	since := at("2026-10-01T00:00:00Z")
	ta := serving("t-a", core.TaskRunning, since)
	ta.Task.Body = "Implements AC-1.1 and DEC-7. See AC-1.10 (DEC-70 is unrelated)."
	ta.Designs = []string{"design-b", "design-a"}
	tb := serving("t-b", core.TaskMerged, since)
	tb.Task.Body = "Covers AC-1.1.2 only, plus DEC-7 and xDEC-1."
	tb.Designs = []string{"design-a", "design-a"}
	tc := serving("t-c", core.TaskClosed, since)
	tc.Task.Body = "AC-2.1; DEC-1"
	inputs := Inputs{
		Requirement: Requirement{
			ID: "req-insight-demo", Title: "Insight demo", CurrentVersion: 3,
			Content: "Governed by DEC-2 and DEC-10. Not DEC-1x or DEC-3-draft.",
			Statements: []core.RequirementStatement{
				{ID: "REQ-1", Statement: "One.", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "first"}, {ID: "AC-1.2", Statement: "second"}, {ID: "AC-1.10", Statement: "tenth"}}},
				{ID: "REQ-2", Statement: "Two.", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-2.1", Statement: "other"}}},
			},
		},
		Tasks: []ServingTask{tc, ta, tb},
	}
	designs := []DesignInput{
		{ID: "design-e", Title: "Underscore", CurrentVersion: 1, Content: "req-insight-demo_x"},
		{ID: "design-c", Title: "Names it", CurrentVersion: 4, Content: "See req-insight-demo."},
		{ID: "design-b", Title: "Attached once", CurrentVersion: 2, Content: "Implements (req-insight-demo)."},
		{ID: "design-d", Title: "Suffix", CurrentVersion: 1, Content: "req-insight-demo-v2 and xreq-insight-demo"},
		{ID: "design-a", Title: "Attached twice", CurrentVersion: 7, Content: "No mention.", PendingVersionCount: 1},
	}
	decisions := []core.Decision{
		{ID: "DEC-10", Statement: "ten", Status: core.DecisionConfirmed},
		{ID: "DEC-7", Statement: "seven", Status: core.DecisionProposed},
		{ID: "DEC-6", Statement: "Applies to req-insight-demo.", Status: core.DecisionDismissed},
		{ID: "DEC-5", Statement: "five", Context: "Context for req-insight-demo", Status: core.DecisionSuperseded},
		{ID: "DEC-3", Statement: "three", Status: core.DecisionProposed},
		{ID: "DEC-2", Statement: "two", Status: core.DecisionConfirmed},
		{ID: "DEC-1", Statement: "one", Status: core.DecisionConfirmed},
		{ID: "DEC-8", Statement: "eight", AlternativesRejected: "Rejected: req-insight-demo, alone.", Status: core.DecisionConfirmed},
	}
	return inputs, designs, decisions
}

func TestAssociationsEvidenceAndOrdering(t *testing.T) {
	inputs, designs, decisions := associationFixture()
	result := Associations(inputs, nil, fixedNow, designs, decisions)
	wantDesigns := []DesignAssociation{
		{ID: "design-a", Title: "Attached twice", CurrentVersion: 7, RevisionPending: true, AttachedTaskCount: 2, NamesRequirement: false, AttachedTaskIDs: []string{"t-a", "t-b"}},
		{ID: "design-b", Title: "Attached once", CurrentVersion: 2, AttachedTaskCount: 1, NamesRequirement: true, AttachedTaskIDs: []string{"t-a"}},
		{ID: "design-c", Title: "Names it", CurrentVersion: 4, AttachedTaskCount: 0, NamesRequirement: true, AttachedTaskIDs: []string{}},
	}
	if !reflect.DeepEqual(result.Designs, wantDesigns) {
		t.Fatalf("designs %+v\nwant %+v", result.Designs, wantDesigns)
	}
	var order []string
	for _, decision := range result.Decisions {
		order = append(order, decision.ID)
	}
	if want := []string{"DEC-5", "DEC-8", "DEC-2", "DEC-10", "DEC-7", "DEC-1"}; !slices.Equal(order, want) {
		t.Fatalf("decision order %v, want %v", order, want)
	}
	byID := map[string]DecisionAssociation{}
	for _, decision := range result.Decisions {
		byID[decision.ID] = decision
	}
	checks := []struct {
		id            string
		names, cited  bool
		citing        []string
		status        core.DecisionStatus
		statement     string
		contextPrefix string
	}{
		{"DEC-5", true, false, []string{}, core.DecisionSuperseded, "five", "Context for"},
		{"DEC-8", true, false, []string{}, core.DecisionConfirmed, "eight", ""},
		{"DEC-2", false, true, []string{}, core.DecisionConfirmed, "two", ""},
		{"DEC-10", false, true, []string{}, core.DecisionConfirmed, "ten", ""},
		{"DEC-7", false, false, []string{"t-a", "t-b"}, core.DecisionProposed, "seven", ""},
		{"DEC-1", false, false, []string{"t-c"}, core.DecisionConfirmed, "one", ""},
	}
	for _, check := range checks {
		got := byID[check.id]
		if got.NamesRequirement != check.names || got.CitedByRequirement != check.cited || !slices.Equal(got.CitingTaskIDs, check.citing) || got.Status != check.status || got.Statement != check.statement || !strings.HasPrefix(got.Context, check.contextPrefix) {
			t.Fatalf("decision %s = %+v", check.id, got)
		}
	}
	wantCriteria := []CriterionAssociation{
		{ID: "AC-1.1", Statement: "first", CitingTaskIDs: []string{"t-a"}},
		{ID: "AC-1.2", Statement: "second", CitingTaskIDs: []string{}},
		{ID: "AC-1.10", Statement: "tenth", CitingTaskIDs: []string{"t-a"}},
		{ID: "AC-2.1", Statement: "other", CitingTaskIDs: []string{"t-c"}},
	}
	if !reflect.DeepEqual(result.AcceptanceCriteria, wantCriteria) || result.CitedCount != 3 || result.Total != 4 {
		t.Fatalf("criteria %+v cited=%d total=%d", result.AcceptanceCriteria, result.CitedCount, result.Total)
	}
	empty := Associations(Inputs{Requirement: Requirement{ID: "req-none"}}, nil, fixedNow, nil, nil)
	if empty.Designs == nil || empty.Decisions == nil || empty.AcceptanceCriteria == nil || empty.CitedCount != 0 || empty.Total != 0 {
		t.Fatalf("empty associations %+v", empty)
	}
}

func TestContainsTokenBoundaries(t *testing.T) {
	for _, tc := range []struct {
		content, token string
		want           bool
	}{
		{"req-x", "req-x", true},
		{"see req-x.", "req-x", true},
		{"(req-x)", "req-x", true},
		{"req-x, req-y", "req-y", true},
		{"`req-x`", "req-x", true},
		{"req-x-v2", "req-x", false},
		{"pre-req-x", "req-x", false},
		{"req-x_y", "req-x", false},
		{"_req-x", "req-x", false},
		{"areq-x", "req-x", false},
		{"req-x1", "req-x", false},
		{"éreq-x", "req-x", false},
		{"req-xé", "req-x", false},
		{"req-x٣", "req-x", false},
		{"req-x-v2 then req-x", "req-x", true},
		{"", "req-x", false},
		{"req-x", "", false},
		{"DEC-1", "DEC-1", true},
		{"DEC-10", "DEC-1", false},
	} {
		if got := ContainsToken(tc.content, tc.token); got != tc.want {
			t.Errorf("ContainsToken(%q, %q) = %v, want %v", tc.content, tc.token, got, tc.want)
		}
	}
	for _, tc := range []struct {
		content, id string
		want        bool
	}{
		{"DEC-1", "DEC-1", true},
		{"see DEC-1.", "DEC-1", true},
		{"DEC-10", "DEC-1", false},
		{"DEC-1-draft", "DEC-1", false},
		{"pre-DEC-1", "DEC-1", false},
		{"DEC-1_x", "DEC-1", false},
		{"xDEC-1", "DEC-1", false},
		{"DEC-1x", "DEC-1", false},
		{"DEC-1", "DEC-01", false},
		{"DEC-1", "not-a-decision", false},
	} {
		if got := containsDecisionToken(tc.content, tc.id); got != tc.want {
			t.Errorf("containsDecisionToken(%q, %q) = %v, want %v", tc.content, tc.id, got, tc.want)
		}
	}
	for _, tc := range []struct {
		content, id string
		want        bool
	}{
		{"AC-1.1", "AC-1.1", true},
		{"AC-1.1.", "AC-1.1", true},
		{"AC-1.1, AC-1.2", "AC-1.2", true},
		{"AC-1.10", "AC-1.1", false},
		{"AC-1.1.2", "AC-1.1", false},
		{"AC-1.1-draft", "AC-1.1", false},
		{"XAC-1.1", "AC-1.1", false},
		{"AC-1.1.2 then AC-1.1;", "AC-1.1", true},
		{"AC-1.1", "AC-1", false},
	} {
		if got := containsCriterionToken(tc.content, tc.id); got != tc.want {
			t.Errorf("containsCriterionToken(%q, %q) = %v, want %v", tc.content, tc.id, got, tc.want)
		}
	}
}

func fingerprintFixture() (Inputs, map[string]nextaction.Compact) {
	inputs := Inputs{
		Requirement: Requirement{ID: "req-fp", Title: "Fingerprint", CurrentVersion: 4, Content: "content", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "s"}}},
		Tasks: []ServingTask{
			{Task: core.Task{ID: "t-2", Title: "Second <&>", State: core.TaskRunning, Body: "body two", CreatedAt: at("2026-09-01T00:00:00Z")}, ServingSince: time.Date(2026, 10, 2, 3, 4, 5, 600, time.FixedZone("plus2", 2*3600)), DependsOn: []string{"t-9", "t-1"}, Designs: []string{"design-a"}},
			{Task: core.Task{ID: "t-1", Title: "First", State: core.TaskMerged, Body: "body one", CreatedAt: at("2026-09-01T00:00:00Z")}, ServingSince: at("2026-10-01T00:00:00Z"), MergedAt: at("2026-10-05T00:00:00Z")},
		},
	}
	deadline := at("2026-10-30T00:00:00Z")
	actions := map[string]nextaction.Compact{
		"t-2": {Label: "Implementing", Actor: "agent", Kind: "implementing", Judgment: false, Summary: "agent at work", Deadline: &deadline, Blocks: []nextaction.BlockerRef{{TaskID: "t-3"}}},
		"t-1": {Label: "Merged", Actor: "none", Kind: "terminal", Blocks: []nextaction.BlockerRef{}},
	}
	return inputs, actions
}

const fingerprintGolden = `{"requirement_id":"req-fp","current_version":4,"tasks":[` +
	`{"id":"t-1","title":"First","state":"merged","next_action":{"label":"Merged","actor":"none","kind":"terminal"},"depends_on":[],"serving_since":"2026-10-01T00:00:00Z"},` +
	`{"id":"t-2","title":"Second <&>","state":"running","next_action":{"label":"Implementing","actor":"agent","kind":"implementing"},"depends_on":["t-1","t-9"],"serving_since":"2026-10-02T01:04:05Z"}]}`

func TestFingerprintCanonicalJSONAndFieldCoverage(t *testing.T) {
	inputs, actions := fingerprintFixture()
	document := FingerprintDocument(inputs, actions, fixedNow)
	if string(document) != fingerprintGolden {
		t.Fatalf("fingerprint document\n got %s\nwant %s", document, fingerprintGolden)
	}
	sum := sha256.Sum256([]byte(fingerprintGolden))
	golden := hex.EncodeToString(sum[:])
	fingerprint := Fingerprint(inputs, actions, fixedNow)
	if fingerprint != golden || fingerprint != strings.ToLower(fingerprint) || len(fingerprint) != 64 {
		t.Fatalf("fingerprint %s, want %s", fingerprint, golden)
	}
	if golden != fingerprintGoldenDigest {
		t.Fatalf("golden digest %s drifted from pinned %s", golden, fingerprintGoldenDigest)
	}

	// Permutations of task order, dependency order, and now change nothing.
	permuted, _ := fingerprintFixture()
	slices.Reverse(permuted.Tasks)
	permuted.Tasks[1].DependsOn = []string{"t-1", "t-9"}
	if got := Fingerprint(permuted, actions, fixedNow.Add(1000*time.Hour)); got != fingerprint {
		t.Fatalf("permuted fingerprint %s != %s", got, fingerprint)
	}
	empty := FingerprintDocument(Inputs{Requirement: Requirement{ID: "req-empty", CurrentVersion: 1}}, nil, fixedNow)
	if string(empty) != `{"requirement_id":"req-empty","current_version":1,"tasks":[]}` {
		t.Fatalf("empty document %s", empty)
	}

	mutate := func(name string, change func(*Inputs, map[string]nextaction.Compact)) (string, string) {
		in, acts := fingerprintFixture()
		change(&in, acts)
		return name, Fingerprint(in, acts, fixedNow)
	}
	included := [][2]string{}
	for _, item := range []struct {
		name   string
		change func(*Inputs, map[string]nextaction.Compact)
	}{
		{"requirement id", func(in *Inputs, _ map[string]nextaction.Compact) { in.Requirement.ID = "req-other" }},
		{"current version", func(in *Inputs, _ map[string]nextaction.Compact) { in.Requirement.CurrentVersion = 5 }},
		{"task id", func(in *Inputs, _ map[string]nextaction.Compact) { in.Tasks[1].Task.ID = "t-0" }},
		{"title", func(in *Inputs, _ map[string]nextaction.Compact) { in.Tasks[0].Task.Title = "Renamed" }},
		{"state", func(in *Inputs, _ map[string]nextaction.Compact) { in.Tasks[0].Task.State = core.TaskAwaiting }},
		{"label", func(_ *Inputs, acts map[string]nextaction.Compact) {
			a := acts["t-2"]
			a.Label = "Other"
			acts["t-2"] = a
		}},
		{"actor", func(_ *Inputs, acts map[string]nextaction.Compact) {
			a := acts["t-2"]
			a.Actor = "operator"
			acts["t-2"] = a
		}},
		{"kind", func(_ *Inputs, acts map[string]nextaction.Compact) {
			a := acts["t-2"]
			a.Kind = "parked"
			acts["t-2"] = a
		}},
		{"depends_on", func(in *Inputs, _ map[string]nextaction.Compact) { in.Tasks[0].DependsOn = []string{"t-1"} }},
		{"serving_since", func(in *Inputs, _ map[string]nextaction.Compact) {
			in.Tasks[1].ServingSince = at("2026-10-01T00:00:01Z")
		}},
		{"added task", func(in *Inputs, _ map[string]nextaction.Compact) {
			in.Tasks = append(in.Tasks, ServingTask{Task: core.Task{ID: "t-3"}, ServingSince: at("2026-10-03T00:00:00Z")})
		}},
	} {
		name, got := mutate(item.name, item.change)
		if got == fingerprint {
			t.Errorf("changing %s left the fingerprint unchanged", name)
		}
		included = append(included, [2]string{name, got})
	}
	for _, item := range []struct {
		name   string
		change func(*Inputs, map[string]nextaction.Compact)
	}{
		{"requirement title", func(in *Inputs, _ map[string]nextaction.Compact) { in.Requirement.Title = "x" }},
		{"requirement content", func(in *Inputs, _ map[string]nextaction.Compact) { in.Requirement.Content = "x" }},
		{"statements", func(in *Inputs, _ map[string]nextaction.Compact) { in.Requirement.Statements = nil }},
		{"body", func(in *Inputs, _ map[string]nextaction.Compact) { in.Tasks[0].Task.Body = "changed body" }},
		{"created_at", func(in *Inputs, _ map[string]nextaction.Compact) { in.Tasks[0].Task.CreatedAt = fixedNow }},
		{"merged_at", func(in *Inputs, _ map[string]nextaction.Compact) { in.Tasks[1].MergedAt = fixedNow }},
		{"closed_at", func(in *Inputs, _ map[string]nextaction.Compact) { in.Tasks[0].ClosedAt = fixedNow }},
		{"designs", func(in *Inputs, _ map[string]nextaction.Compact) { in.Tasks[0].Designs = []string{"design-z"} }},
		{"sub-second serving_since", func(in *Inputs, _ map[string]nextaction.Compact) {
			in.Tasks[1].ServingSince = in.Tasks[1].ServingSince.Add(500 * time.Millisecond)
		}},
		{"summary", func(_ *Inputs, acts map[string]nextaction.Compact) {
			a := acts["t-2"]
			a.Summary = "x"
			acts["t-2"] = a
		}},
		{"judgment", func(_ *Inputs, acts map[string]nextaction.Compact) {
			a := acts["t-2"]
			a.Judgment = true
			acts["t-2"] = a
		}},
		{"deadline", func(_ *Inputs, acts map[string]nextaction.Compact) {
			a := acts["t-2"]
			a.Deadline = nil
			acts["t-2"] = a
		}},
		{"blocks", func(_ *Inputs, acts map[string]nextaction.Compact) { a := acts["t-2"]; a.Blocks = nil; acts["t-2"] = a }},
		{"blocked_by", func(_ *Inputs, acts map[string]nextaction.Compact) {
			a := acts["t-2"]
			a.BlockedBy = []nextaction.BlockerRef{{TaskID: "t-7"}}
			acts["t-2"] = a
		}},
		{"unrelated action", func(_ *Inputs, acts map[string]nextaction.Compact) {
			acts["t-unrelated"] = nextaction.Compact{Label: "x"}
		}},
	} {
		if name, got := mutate(item.name, item.change); got != fingerprint {
			t.Errorf("changing excluded field %s changed the fingerprint", name)
		}
	}
	for i := range included {
		for j := i + 1; j < len(included); j++ {
			if included[i][1] == included[j][1] {
				t.Errorf("mutations %s and %s collide", included[i][0], included[j][0])
			}
		}
	}
}

// fingerprintGoldenDigest pins the SHA-256 of fingerprintGolden so a change to
// the canonical bytes fails here before any stored headline goes stale.
const fingerprintGoldenDigest = "be0f582eab5c85b0c2c3d5e3bff2b0d95cb0942f0bd8aafa7e104a1eaca1c10b"

func TestInsightFunctionsDoNotMutateInputs(t *testing.T) {
	build := func() (Inputs, map[string]nextaction.Compact, []DesignInput, []core.Decision) {
		inputs, designs, decisions := associationFixture()
		inputs.Tasks[1].DependsOn = []string{"t-c", "t-b"}
		inputs.Tasks[2].DependsOn = []string{"t-c"}
		actions := map[string]nextaction.Compact{"t-a": compact(nextaction.ActorAgent, "Implementing")}
		return inputs, actions, designs, decisions
	}
	inputs, actions, designs, decisions := build()
	Groups(inputs, actions, fixedNow)
	CriticalPath(inputs, actions, fixedNow)
	Progress(inputs, actions, fixedNow)
	Associations(inputs, actions, fixedNow, designs, decisions)
	FingerprintDocument(inputs, actions, fixedNow)
	Fingerprint(inputs, actions, fixedNow)
	wantInputs, wantActions, wantDesigns, wantDecisions := build()
	if !reflect.DeepEqual(inputs, wantInputs) || !reflect.DeepEqual(actions, wantActions) || !reflect.DeepEqual(designs, wantDesigns) || !reflect.DeepEqual(decisions, wantDecisions) {
		t.Fatal("an insight function mutated its inputs")
	}
}

// The package stays pure: no store, HTTP, database, dispatch, or model client.
func TestPackageImportsNoStoreHTTPOrModelClient(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"/internal/store", "/internal/httpapi", "/internal/dispatch", "/internal/inprocess", "/internal/agent", "net/http", "database/sql", "github.com/jackc/", "github.com/go-sql-driver/", "os/exec"}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range parsed.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			for _, prefix := range forbidden {
				if strings.Contains(path, prefix) {
					t.Errorf("%s imports %s", file, path)
				}
			}
		}
	}
}
