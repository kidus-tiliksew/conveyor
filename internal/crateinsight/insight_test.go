package crateinsight

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/insight"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

// now is the read instant every case uses: a Wednesday, so the ISO week
// holding it started on Monday 2026-11-16.
var now = time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return now.Add(-d) }

const hour = time.Hour

var (
	working      = nextaction.Compact{Actor: nextaction.ActorAgent, Kind: nextaction.KindWorking, Label: "Agent is working"}
	queued       = nextaction.Compact{Actor: nextaction.ActorAgent, Kind: nextaction.KindQueued, Label: "Waiting for an agent"}
	factory      = nextaction.Compact{Actor: nextaction.ActorFactory, Kind: nextaction.KindMerging, Label: "Merging"}
	planApproval = nextaction.Compact{Actor: nextaction.ActorOperator, Kind: nextaction.KindPlanApproval, Judgment: true, Label: "Approve the plan"}
	mergeGate    = nextaction.Compact{Actor: nextaction.ActorOperator, Kind: nextaction.KindMergeApproval, Judgment: true, Label: "Approve the merge"}
	dependency   = nextaction.Compact{Actor: nextaction.ActorDependency, Kind: nextaction.KindBlocked, Label: "Waiting on a dependency"}
	terminal     = nextaction.Compact{Actor: nextaction.ActorNone, Kind: nextaction.KindTerminal, Label: "Done"}
)

func task(id string, state core.TaskState, stage core.Stage, created time.Time) core.Task {
	return core.Task{ID: id, Title: "Task " + id, State: state, NextStage: stage, CreatedAt: created, BaseBranch: "main"}
}

func event(id int64, kind string, at time.Time, payload any) Event {
	e := Event{ID: id, Kind: kind, At: at}
	if payload != nil {
		e.Payload = core.JSONPayload(payload)
	}
	return e
}

func claim(id int64, stage core.Stage, at time.Time) Event {
	return Event{ID: id, Kind: kindClaimed, At: at, Stage: stage}
}

func stateChanged(id int64, to core.TaskState, at time.Time) Event {
	return event(id, kindStateChanged, at, map[string]any{"from": "running", "to": to})
}

// transition is a task.state_changed event as every store records a
// lifecycle command: from, to, and the command.
func transition(id int64, command core.TaskCommand, to core.TaskState, at time.Time) Event {
	return event(id, kindStateChanged, at, map[string]any{"from": core.TaskRunning, "to": to, "command": command})
}

func review(id int64, verdict string, at time.Time) Event {
	return event(id, kindReviewRound, at, map[string]any{"verdict": verdict, "review_round": id})
}

func opened(id int64, at time.Time) Event {
	return event(id, kindPullRequestOpened, at, map[string]any{"number": id})
}

func planWaits(id int64, at time.Time) Event {
	return event(id, core.EventTaskPlanStateChanged, at, core.PlanStateChange{To: core.PlanAwaitingApproval, Cause: core.PlanCausePlanSubmitted})
}

// TestPhaseAndStationRules walks invariants 23 and 24 case by case,
// including a member with no next stage, a judgment, a dependency block,
// and the two approval kinds.
func TestPhaseAndStationRules(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       core.TaskState
		stage       core.Stage
		action      nextaction.Compact
		pullRequest bool
		phase       Phase
		station     Station
	}{
		{"merged member", core.TaskMerged, "", terminal, true, PhaseMerged, StationCrate},
		{"closed member", core.TaskClosed, core.StageImplement, terminal, false, PhaseClosed, StationAside},
		{"dependency blocks before judgment", core.TaskQueued, core.StageSpec, nextaction.Compact{Actor: nextaction.ActorDependency, Judgment: true}, false, PhaseBlocked, StationIntake},
		{"dependency blocks implement", core.TaskQueued, core.StageImplement, dependency, false, PhaseBlocked, StationBuild},
		{"plan approval waits on a person", core.TaskAwaiting, core.StageImplement, planApproval, false, PhaseNeedsYou, StationPlan},
		{"merge approval waits on a person", core.TaskAwaiting, core.StageGate, mergeGate, true, PhaseNeedsYou, StationGate},
		{"spec queued", core.TaskQueued, core.StageSpec, queued, false, PhaseQueued, StationIntake},
		{"spec working", core.TaskRunning, core.StageSpec, working, false, PhasePlanning, StationPlan},
		{"triage working", core.TaskRunning, core.StageTriage, working, false, PhasePlanning, StationPlan},
		{"implement queued", core.TaskQueued, core.StageImplement, queued, false, PhaseQueued, StationBuild},
		{"implement working", core.TaskRunning, core.StageImplement, working, false, PhaseBuilding, StationBuild},
		{"implement after a pull request", core.TaskQueued, core.StageImplement, queued, true, PhaseBuilding, StationBuild},
		{"review stage", core.TaskQueued, core.StageReview, queued, true, PhaseReview, StationReview},
		{"verify stage", core.TaskRunning, core.StageVerify, working, true, PhaseReview, StationReview},
		{"risk stage", core.TaskQueued, core.StageRisk, queued, true, PhaseReview, StationReview},
		{"merge stage", core.TaskApproved, core.StageMerge, factory, true, PhaseReview, StationGate},
		{"no next stage with a pull request", core.TaskRunning, "", factory, true, PhaseReview, StationReview},
		{"no next stage working", core.TaskRunning, "", working, false, PhaseBuilding, StationBuild},
		{"no next stage waiting", core.TaskQueued, "", queued, false, PhaseQueued, StationIntake},
		{"no next stage blocked", core.TaskQueued, "", dependency, false, PhaseBlocked, StationIntake},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member := task("t", tc.state, tc.stage, ago(10*hour))
			phase := PhaseOf(member, tc.action, tc.pullRequest)
			station := StationOf(member, tc.action, phase, tc.pullRequest)
			if phase != tc.phase || station != tc.station {
				t.Fatalf("got %s at %s, want %s at %s", phase, station, tc.phase, tc.station)
			}
		})
	}
}

// TestReplayRules covers invariant 25: creation, claims by stage, a plan
// that waits for approval, a review round that asks for changes and a new
// pull request after it, an approval whose merge waits on a person, merge,
// close, and the closing step at the read instant.
func TestReplayRules(t *testing.T) {
	created := ago(100 * hour)
	step := func(at time.Time, id string, phase Phase, station Station) Step {
		return Step{At: at, TaskID: id, Phase: phase, Station: station}
	}
	for _, tc := range []struct {
		name   string
		task   core.Task
		action nextaction.Compact
		events []Event
		want   []Step
	}{
		{
			name: "changes requested then a new pull request",
			task: task("a", core.TaskQueued, core.StageReview, created), action: queued,
			events: []Event{
				claim(1, core.StageSpec, ago(90*hour)), planWaits(2, ago(85*hour)), claim(3, core.StageImplement, ago(80*hour)),
				opened(4, ago(70*hour)), claim(5, core.StageReview, ago(65*hour)), review(6, verdictChanges, ago(60*hour)),
				claim(7, core.StageImplement, ago(55*hour)), opened(8, ago(50*hour)),
			},
			want: []Step{
				step(created, "a", PhaseQueued, StationIntake), step(ago(90*hour), "a", PhasePlanning, StationPlan),
				step(ago(85*hour), "a", PhaseNeedsYou, StationPlan), step(ago(80*hour), "a", PhaseBuilding, StationBuild),
				step(ago(70*hour), "a", PhaseReview, StationReview), step(ago(60*hour), "a", PhaseBuilding, StationBuild),
				step(ago(50*hour), "a", PhaseReview, StationReview),
			},
		},
		{
			name: "approved and waiting on merge approval",
			task: func() core.Task {
				waiting := task("b", core.TaskAwaiting, core.StageGate, created)
				waiting.MergeApproval = true
				return waiting
			}(), action: mergeGate,
			events: []Event{
				claim(1, core.StageImplement, ago(90*hour)), opened(2, ago(80*hour)), claim(3, core.StageVerify, ago(79*hour)),
				review(4, verdictApprove, ago(70*hour)), transition(5, core.TaskGateMerge, core.TaskAwaiting, ago(70*hour)),
			},
			want: []Step{
				step(created, "b", PhaseQueued, StationIntake), step(ago(90*hour), "b", PhaseBuilding, StationBuild),
				step(ago(80*hour), "b", PhaseReview, StationReview), step(ago(70*hour), "b", PhaseNeedsYou, StationGate),
			},
		},
		{
			// DEC-124(2): a risk hold sends the approved round to the merge
			// gate although the frozen merge_approval is off.
			name: "risk hold with the merge gate off still waits on a person",
			task: task("h", core.TaskAwaiting, core.StageGate, created), action: mergeGate,
			events: []Event{
				claim(1, core.StageImplement, ago(90*hour)), opened(2, ago(80*hour)),
				review(3, verdictApprove, ago(70*hour)), transition(4, core.TaskGateMerge, core.TaskAwaiting, ago(70*hour)),
			},
			want: []Step{
				step(created, "h", PhaseQueued, StationIntake), step(ago(90*hour), "h", PhaseBuilding, StationBuild),
				step(ago(80*hour), "h", PhaseReview, StationReview), step(ago(70*hour), "h", PhaseNeedsYou, StationGate),
			},
		},
		{
			// The first round enters the merge gate. The refresh round's
			// approval takes review.accept, so it adds no second wait even
			// though the frozen merge_approval is on.
			name: "refresh approval that takes review accept bypasses the gate",
			task: func() core.Task {
				merged := task("r", core.TaskMerged, "", created)
				merged.MergeApproval = true
				return merged
			}(), action: terminal,
			events: []Event{
				claim(1, core.StageImplement, ago(90*hour)), opened(2, ago(80*hour)),
				review(3, verdictApprove, ago(70*hour)), transition(4, core.TaskGateMerge, core.TaskAwaiting, ago(70*hour)),
				claim(5, core.StageReview, ago(60*hour)),
				review(6, verdictApprove, ago(55*hour)), transition(7, core.TaskReviewAccept, core.TaskApproved, ago(55*hour)),
				event(8, kindMergeConfirmed, ago(50*hour), map[string]any{"sha": "abc"}), stateChanged(9, core.TaskMerged, ago(50*hour)),
			},
			want: []Step{
				step(created, "r", PhaseQueued, StationIntake), step(ago(90*hour), "r", PhaseBuilding, StationBuild),
				step(ago(80*hour), "r", PhaseReview, StationReview), step(ago(70*hour), "r", PhaseNeedsYou, StationGate),
				step(ago(60*hour), "r", PhaseReview, StationReview), step(ago(50*hour), "r", PhaseMerged, StationCrate),
			},
		},
		{
			name: "approval without the merge gate moves nothing until the merge",
			task: task("c", core.TaskMerged, "", created), action: terminal,
			events: []Event{
				claim(1, core.StageImplement, ago(90*hour)), opened(2, ago(80*hour)), review(3, verdictApprove, ago(70*hour)),
				event(4, kindMergeConfirmed, ago(69*hour), map[string]any{"sha": "abc"}), stateChanged(5, core.TaskMerged, ago(69*hour)),
			},
			want: []Step{
				step(created, "c", PhaseQueued, StationIntake), step(ago(90*hour), "c", PhaseBuilding, StationBuild),
				step(ago(80*hour), "c", PhaseReview, StationReview), step(ago(69*hour), "c", PhaseMerged, StationCrate),
			},
		},
		{
			name: "closed member",
			task: task("d", core.TaskClosed, core.StageImplement, created), action: terminal,
			events: []Event{claim(1, core.StageImplement, ago(90*hour)), event(2, kindTaskCancelled, ago(80*hour), map[string]any{"reason": "dropped"}), stateChanged(3, core.TaskClosed, ago(80*hour))},
			want: []Step{
				step(created, "d", PhaseQueued, StationIntake), step(ago(90*hour), "d", PhaseBuilding, StationBuild),
				step(ago(80*hour), "d", PhaseClosed, StationAside),
			},
		},
		{
			name: "closing step at the read instant",
			task: task("e", core.TaskRunning, core.StageImplement, created), action: working,
			events: []Event{claim(1, core.StageSpec, ago(90*hour)), event(2, "task.crate_set", ago(89*hour), map[string]any{"crate_id": "crate-1"})},
			want: []Step{
				step(created, "e", PhaseQueued, StationIntake), step(ago(90*hour), "e", PhasePlanning, StationPlan),
				step(now, "e", PhaseBuilding, StationBuild),
			},
		},
		{
			name: "no closing step when the last step is the live position",
			task: task("f", core.TaskQueued, core.StageSpec, created), action: queued,
			events: []Event{event(1, "task.created", created, nil), stateChanged(2, core.TaskQueued, ago(99*hour))},
			want:   []Step{step(created, "f", PhaseQueued, StationIntake)},
		},
		{
			name: "merge reconciled and a later state change repeat no step",
			task: task("g", core.TaskMerged, "", created), action: terminal,
			events: []Event{event(1, kindMergeReconciled, ago(10*hour), map[string]any{"result": "merged_outside_conveyor"}), stateChanged(2, core.TaskMerged, ago(9*hour))},
			want:   []Step{step(created, "g", PhaseQueued, StationIntake), step(ago(10*hour), "g", PhaseMerged, StationCrate)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := MemberSteps(Member{Task: tc.task, Events: tc.events}, tc.action, now)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("steps:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// TestReplayOrdersStepsByTimeThenTask holds the replay's (at, task ID)
// order, event order for one member's steps at one time, and the start and
// end of the span.
func TestReplayOrdersStepsByTimeThenTask(t *testing.T) {
	tie := ago(50 * hour)
	inputs := Inputs{Members: []Member{
		{Task: task("b", core.TaskQueued, core.StageReview, ago(80*hour)), Events: []Event{
			// Unsorted on purpose: the fold reads (at, id) order.
			opened(9, tie), claim(8, core.StageImplement, tie),
		}},
		{Task: task("a", core.TaskQueued, core.StageReview, ago(70*hour)), Events: []Event{opened(3, tie)}},
	}}
	replay := ReplayOf(inputs, map[string]nextaction.Compact{"a": queued, "b": queued}, now)
	if !replay.Start.Equal(ago(80*hour)) || !replay.End.Equal(now) {
		t.Fatalf("span %s to %s", replay.Start, replay.End)
	}
	type key struct {
		id    string
		phase Phase
	}
	var got []key
	for _, step := range replay.Steps {
		got = append(got, key{step.TaskID, step.Phase})
	}
	want := []key{{"b", PhaseQueued}, {"a", PhaseQueued}, {"a", PhaseReview}, {"b", PhaseBuilding}, {"b", PhaseReview}}
	if !slices.Equal(got, want) {
		t.Fatalf("steps %+v, want %+v", got, want)
	}
}

// TestTaskOrderSharedByBothReads holds invariant 28's groups and its
// creation time and ID ties (req-crates AC-5.7).
func TestTaskOrderSharedByBothReads(t *testing.T) {
	base := ago(100 * hour)
	tasks := []core.Task{
		task("closed", core.TaskClosed, "", base),
		task("merged-late", core.TaskMerged, "", base.Add(2*hour)),
		task("merged-early", core.TaskMerged, "", base.Add(hour)),
		task("blocked", core.TaskQueued, core.StageImplement, base),
		task("other-b", core.TaskApproved, core.StageMerge, base),
		task("other-a", core.TaskApproved, core.StageMerge, base),
		task("agent-next", core.TaskQueued, core.StageImplement, base),
		task("agent-working-late", core.TaskRunning, core.StageImplement, base.Add(time.Minute)),
		task("agent-working", core.TaskRunning, core.StageImplement, base),
		task("judgment", core.TaskAwaiting, core.StageGate, base.Add(5*hour)),
	}
	actions := map[string]nextaction.Compact{
		"closed": terminal, "merged-late": terminal, "merged-early": terminal, "blocked": dependency,
		"other-b": factory, "other-a": factory, "agent-next": queued, "agent-working-late": working,
		"agent-working": working, "judgment": mergeGate,
	}
	SortTasks(tasks, actions)
	got := make([]string, len(tasks))
	for i, task := range tasks {
		got[i] = task.ID
	}
	want := []string{"judgment", "agent-working", "agent-working-late", "agent-next", "other-a", "other-b", "blocked", "merged-early", "merged-late", "closed"}
	if !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	members := make([]Member, len(tasks))
	for i := range tasks {
		members[len(tasks)-1-i] = Member{Task: tasks[i]}
	}
	view := Compute(Inputs{Members: members}, actions, now)
	for i, entry := range view.Tasks {
		if entry.ID != want[i] {
			t.Fatalf("insight view order %d = %s, want %s", i, entry.ID, want[i])
		}
	}
}

// TestBranchesAndWhereabouts holds invariant 26: lane order, the
// not_started and task_branch groups, and one merged entry per branch in
// reverse lane order, kept when empty.
func TestBranchesAndWhereabouts(t *testing.T) {
	member := func(id, base string, state core.TaskState, stage core.Stage) Member {
		m := task(id, state, stage, ago(10*hour))
		m.BaseBranch = base
		return Member{Task: m}
	}
	inputs := Inputs{Members: []Member{
		member("m1", "feature/x", core.TaskMerged, ""),
		member("m2", "develop", core.TaskMerged, ""),
		member("m3", "main", core.TaskMerged, ""),
		member("q1", "main", core.TaskQueued, core.StageSpec),
		member("b1", "release", core.TaskRunning, core.StageImplement),
		member("c1", "dev", core.TaskClosed, ""),
		member("p1", "production", core.TaskQueued, core.StageImplement),
		member("x1", "master", core.TaskQueued, core.StageSpec),
	}}
	actions := map[string]nextaction.Compact{"m1": terminal, "m2": terminal, "m3": terminal, "q1": queued, "b1": working, "c1": terminal, "p1": queued, "x1": queued}
	if got, want := Branches(inputs), []string{"master", "main", "production", "release", "develop", "dev", "feature/x"}; !slices.Equal(got, want) {
		t.Fatalf("branches %v, want %v", got, want)
	}
	view := Compute(inputs, actions, now)
	type group struct {
		key, branch string
		ids         []string
	}
	var got []group
	for _, entry := range view.Whereabouts {
		branch := "<null>"
		if entry.Branch != nil {
			branch = *entry.Branch
		}
		got = append(got, group{entry.Key, branch, entry.TaskIDs})
	}
	want := []group{
		{WhereaboutsNotStarted, "<null>", []string{"q1", "x1"}},
		{WhereaboutsTaskBranch, "<null>", []string{"b1", "p1"}},
		{WhereaboutsMerged, "feature/x", []string{"m1"}},
		{WhereaboutsMerged, "dev", []string{}},
		{WhereaboutsMerged, "develop", []string{"m2"}},
		{WhereaboutsMerged, "release", []string{}},
		{WhereaboutsMerged, "production", []string{}},
		{WhereaboutsMerged, "main", []string{"m3"}},
		{WhereaboutsMerged, "master", []string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("whereabouts:\n got %+v\nwant %+v", got, want)
	}
	// Only merged members: both open groups are left out.
	merged := Compute(Inputs{Members: inputs.Members[:1]}, actions, now)
	if len(merged.Whereabouts) != 1 || merged.Whereabouts[0].Key != WhereaboutsMerged {
		t.Fatalf("merged-only whereabouts %+v", merged.Whereabouts)
	}
}

// TestEmptyCrate holds an empty crate's view: empty lists, no forecast, and
// a replay span that starts and ends at the read instant.
func TestEmptyCrate(t *testing.T) {
	target := "2026-12-01"
	view := Compute(Inputs{Crate: Crate{ID: "crate-1", TargetDate: &target}}, nil, now)
	if len(view.Tasks) != 0 || len(view.Branches) != 0 || len(view.Whereabouts) != 0 || len(view.Replay.Steps) != 0 {
		t.Fatalf("empty crate view %+v", view)
	}
	if !view.Replay.Start.Equal(now) || !view.Replay.End.Equal(now) {
		t.Fatalf("empty replay span %s to %s", view.Replay.Start, view.Replay.End)
	}
	if view.Progress.Forecast != nil || view.ForecastDaysBeforeTarget != nil || view.FirstReview != (FirstReview{}) {
		t.Fatalf("empty crate numbers %+v", view)
	}
	if view.Progress.Total != 0 || view.Progress.MedianCycleSeconds != nil || len(view.Progress.Weekly) != 8 || len(view.Progress.Daily) != 0 {
		t.Fatalf("empty progress %+v", view.Progress)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"tasks":[]`, `"branches":[]`, `"whereabouts":[]`, `"steps":[]`, `"forecast_days_before_target":null`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("empty view JSON %s lacks %s", raw, field)
		}
	}
}

// TestTaskEntriesFromRecordedHistory holds each member entry's times and
// round count, taken from its events rather than its current row.
func TestTaskEntriesFromRecordedHistory(t *testing.T) {
	member := Member{Task: task("a", core.TaskMerged, "", ago(100*hour)), Events: []Event{
		opened(1, ago(90*hour)), review(2, verdictChanges, ago(80*hour)), opened(3, ago(70*hour)),
		review(4, verdictChanges, ago(60*hour)), review(5, verdictApprove, ago(50*hour)),
		stateChanged(6, core.TaskMerged, ago(40*hour)), stateChanged(7, core.TaskMerged, ago(30*hour)),
	}}
	got := TaskOf(member, terminal)
	if got.ReviewRounds != 3 || !slices.Equal(got.PullRequestOpenedAt, []time.Time{ago(90 * hour), ago(70 * hour)}) ||
		!slices.Equal(got.ChangesRequestedAt, []time.Time{ago(80 * hour), ago(60 * hour)}) {
		t.Fatalf("entry %+v", got)
	}
	if got.MergedAt == nil || !got.MergedAt.Equal(ago(40*hour)) || got.ClosedAt != nil || got.BaseBranch != "main" || !got.CreatedAt.Equal(ago(100*hour)) {
		t.Fatalf("entry times %+v", got)
	}
	fresh := TaskOf(Member{Task: task("b", core.TaskQueued, core.StageSpec, ago(time.Hour))}, queued)
	raw, err := json.Marshal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"pull_request_opened_at":[]`) || !strings.Contains(string(raw), `"changes_requested_at":[]`) || !strings.Contains(string(raw), `"merged_at":null`) {
		t.Fatalf("fresh entry JSON %s", raw)
	}
}

// TestDeliveryNumbers holds invariant 27: terminal times, counts, the
// trailing 14-day window, daily and weekly values, the median, first review,
// the forecast, and the signed target difference.
func TestDeliveryNumbers(t *testing.T) {
	day := 24 * hour
	member := func(id string, state core.TaskState, created time.Time, events ...Event) Member {
		return Member{Task: task(id, state, core.StageImplement, created), Events: events}
	}
	// Merged 14 days before now exactly: the window's inclusive lower bound.
	edge := member("edge", core.TaskMerged, ago(20*day), stateChanged(1, core.TaskMerged, ago(14*day)))
	// Merged 15 days ago: outside the 14-day window.
	old := member("old", core.TaskMerged, ago(20*day), review(1, verdictChanges, ago(17*day)), review(2, verdictApprove, ago(16*day)), stateChanged(3, core.TaskMerged, ago(15*day)))
	recent := member("recent", core.TaskMerged, ago(3*day), review(1, verdictApprove, ago(2*day)), stateChanged(2, core.TaskMerged, ago(day)), stateChanged(3, core.TaskMerged, ago(12*hour)))
	closed := member("closed", core.TaskClosed, ago(10*day), stateChanged(1, core.TaskClosed, ago(5*day)))
	open1 := member("open1", core.TaskRunning, ago(2*day), review(1, verdictChanges, ago(day)))
	open2 := member("open2", core.TaskQueued, ago(day))
	inputs := Inputs{Crate: Crate{ID: "crate-1"}, Members: []Member{edge, old, recent, closed, open1, open2}}
	actions := map[string]nextaction.Compact{"edge": terminal, "old": terminal, "recent": terminal, "closed": terminal, "open1": working, "open2": dependency}

	progress := Progress(inputs, actions, now)
	if progress.Delivered != 3 || progress.Open != 2 || progress.Total != 5 {
		t.Fatalf("counts delivered=%d open=%d total=%d", progress.Delivered, progress.Open, progress.Total)
	}
	if progress.StatusMix.Delivered != 3 || progress.StatusMix.Closed != 1 {
		t.Fatalf("status mix %+v", progress.StatusMix)
	}
	// Two merges in [now-14d, now]: 2 merged tasks over two weeks.
	if progress.ThroughputPerWeek != 1 {
		t.Fatalf("throughput %v, want 1 per week", progress.ThroughputPerWeek)
	}
	if progress.Forecast == nil || progress.Forecast.RatePerDay != 2.0/14 || !progress.Forecast.Day.Equal(time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("forecast %+v, want 2026-12-02 at 2/14 per day", progress.Forecast)
	}
	// Cycle times 6, 5, and 2 days: the median is 5 days.
	if progress.MedianCycleSeconds == nil || *progress.MedianCycleSeconds != (5*day).Seconds() {
		t.Fatalf("median %v", progress.MedianCycleSeconds)
	}
	if len(progress.Weekly) != 8 || !progress.Weekly[7].InProgress || progress.Weekly[7].Delivered != 1 || !progress.Weekly[7].WeekStart.Equal(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekly %+v", progress.Weekly)
	}
	weekly := 0
	for _, week := range progress.Weekly {
		weekly += week.Delivered
	}
	if weekly != 3 {
		t.Fatalf("weekly merges %d, want 3", weekly)
	}
	first := progress.Daily[0]
	if !first.Day.Equal(time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)) || first.Scope != 2 || first.Open != 2 || first.Delivered != 0 {
		t.Fatalf("first day %+v", first)
	}
	last := progress.Daily[len(progress.Daily)-1]
	if !last.Day.Equal(time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)) || last.Scope != 5 || last.Delivered != 3 || last.Open != 2 {
		t.Fatalf("last day %+v", last)
	}

	view := Compute(inputs, actions, now)
	if view.FirstReview != (FirstReview{Approved: 1, Reviewed: 3}) {
		t.Fatalf("first review %+v, want 1 of 3", view.FirstReview)
	}
	if view.ForecastDaysBeforeTarget != nil {
		t.Fatalf("difference without a target %v", *view.ForecastDaysBeforeTarget)
	}
	for _, tc := range []struct {
		target string
		want   int
	}{{"2026-12-10", 8}, {"2026-12-02", 0}, {"2026-11-25", -7}} {
		target := tc.target
		inputs.Crate.TargetDate = &target
		got := Compute(inputs, actions, now).ForecastDaysBeforeTarget
		if got == nil || *got != tc.want {
			t.Fatalf("target %s difference %v, want %d", tc.target, got, tc.want)
		}
	}
}

// TestForecastNeedsOpenWorkAndRecentMerges holds the crate's forecast
// conditions (req-crate-insights AC-4.2, AC-4.3): no forecast once every
// member is merged or closed, and none without a merge in 14 days.
func TestForecastNeedsOpenWorkAndRecentMerges(t *testing.T) {
	target := "2026-12-01"
	done := Inputs{Crate: Crate{TargetDate: &target}, Members: []Member{
		{Task: task("a", core.TaskMerged, "", ago(48*hour)), Events: []Event{stateChanged(1, core.TaskMerged, ago(24*hour))}},
	}}
	view := Compute(done, map[string]nextaction.Compact{"a": terminal}, now)
	if view.Progress.Forecast != nil || view.ForecastDaysBeforeTarget != nil || view.Progress.ThroughputPerWeek != 0.5 {
		t.Fatalf("all merged: forecast %+v difference %v throughput %v", view.Progress.Forecast, view.ForecastDaysBeforeTarget, view.Progress.ThroughputPerWeek)
	}
	stale := Inputs{Crate: Crate{TargetDate: &target}, Members: []Member{
		{Task: task("a", core.TaskMerged, "", ago(40*24*hour)), Events: []Event{stateChanged(1, core.TaskMerged, ago(30*24*hour))}},
		{Task: task("b", core.TaskQueued, core.StageImplement, ago(40*24*hour))},
	}}
	view = Compute(stale, map[string]nextaction.Compact{"a": terminal, "b": queued}, now)
	if view.Progress.Forecast != nil || view.ForecastDaysBeforeTarget != nil || view.Progress.ThroughputPerWeek != 0 {
		t.Fatalf("no recent merge: forecast %+v difference %v throughput %v", view.Progress.Forecast, view.ForecastDaysBeforeTarget, view.Progress.ThroughputPerWeek)
	}
	// A merge recorded after the read instant counts in no window.
	future := Inputs{Members: []Member{
		{Task: task("a", core.TaskMerged, "", ago(48*hour)), Events: []Event{stateChanged(1, core.TaskMerged, now.Add(hour))}},
		{Task: task("b", core.TaskQueued, core.StageImplement, ago(48*hour))},
	}}
	if got := Progress(future, nil, now); got.Forecast != nil || got.ThroughputPerWeek != 0 {
		t.Fatalf("future merge counted: %+v", got)
	}
}

// TestProgressKeepsRequirementShapeAndResults holds that the crate adapter
// calls the requirement computation as it stands: every value other than
// the two crate differences equals insight.Progress over the same tasks.
func TestProgressKeepsRequirementShapeAndResults(t *testing.T) {
	inputs := Inputs{Members: []Member{
		{Task: task("a", core.TaskMerged, "", ago(30*24*hour)), Events: []Event{stateChanged(1, core.TaskMerged, ago(20*24*hour))}},
		{Task: task("b", core.TaskMerged, "", ago(10*24*hour)), Events: []Event{stateChanged(1, core.TaskMerged, ago(2*24*hour))}},
		{Task: task("c", core.TaskRunning, core.StageImplement, ago(5*24*hour))},
	}}
	actions := map[string]nextaction.Compact{"a": terminal, "b": terminal, "c": working}
	crate := Progress(inputs, actions, now)
	requirement := insight.Progress(ServingTasks(inputs), actions, now)
	if requirement.ThroughputPerWeek != 0.5 || crate.ThroughputPerWeek != 0.5 {
		t.Fatalf("throughput requirement=%v crate=%v", requirement.ThroughputPerWeek, crate.ThroughputPerWeek)
	}
	crate.ThroughputPerWeek, requirement.ThroughputPerWeek = 0, 0
	if !reflect.DeepEqual(crate, requirement) {
		t.Fatalf("crate progress differs beyond throughput:\n crate %+v\n requirement %+v", crate, requirement)
	}
	var shape map[string]json.RawMessage
	raw, _ := json.Marshal(crate)
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(shape))
	for key := range shape {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"daily", "delivered", "forecast", "median_cycle_seconds", "open", "status_mix", "throughput_per_week", "total", "weekly"}; !slices.Equal(keys, want) {
		t.Fatalf("progress keys %v, want %v", keys, want)
	}
}

// TestViewFieldsAreExactlyTheInsightView holds the JSON field set of the
// insight view and of each nested entry (component-crates, Insight view).
func TestViewFieldsAreExactlyTheInsightView(t *testing.T) {
	target := "2026-12-10"
	inputs := Inputs{Crate: Crate{TargetDate: &target}, Members: []Member{
		{Task: task("a", core.TaskMerged, "", ago(48*hour)), Events: []Event{opened(1, ago(40*hour)), review(2, verdictApprove, ago(30*hour)), stateChanged(3, core.TaskMerged, ago(24*hour))}},
		{Task: task("b", core.TaskQueued, core.StageSpec, ago(24*hour))},
	}}
	raw, err := json.Marshal(Compute(inputs, map[string]nextaction.Compact{"a": terminal, "b": queued}, now))
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Top         map[string]json.RawMessage
		Tasks       []map[string]json.RawMessage `json:"tasks"`
		Whereabouts []map[string]json.RawMessage `json:"whereabouts"`
		FirstReview map[string]json.RawMessage   `json:"first_review"`
		Replay      struct {
			Steps []map[string]json.RawMessage `json:"steps"`
		} `json:"replay"`
		ReplayKeys map[string]json.RawMessage
	}
	requireKeys := func(name string, got map[string]json.RawMessage, want ...string) {
		t.Helper()
		keys := make([]string, 0, len(got))
		for key := range got {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		sort.Strings(want)
		if !slices.Equal(keys, want) {
			t.Fatalf("%s keys %v, want %v", name, keys, want)
		}
	}
	if err := json.Unmarshal(raw, &view.Top); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(view.Top["replay"], &view.ReplayKeys); err != nil {
		t.Fatal(err)
	}
	requireKeys("view", view.Top, "tasks", "branches", "whereabouts", "progress", "first_review", "forecast_days_before_target", "replay")
	requireKeys("task", view.Tasks[0], "id", "phase", "station", "base_branch", "created_at", "pull_request_opened_at", "changes_requested_at", "review_rounds", "merged_at", "closed_at")
	requireKeys("whereabouts", view.Whereabouts[0], "key", "branch", "task_ids")
	requireKeys("first_review", view.FirstReview, "approved", "reviewed")
	requireKeys("replay", view.ReplayKeys, "start", "end", "steps")
	requireKeys("step", view.Replay.Steps[0], "at", "task_id", "phase", "station")
}

// TestComputeDoesNotMutateInputs holds the purity rule of invariant 21 for
// the arguments: a call leaves its inputs and actions as they were.
func TestComputeDoesNotMutateInputs(t *testing.T) {
	inputs := Inputs{Members: []Member{
		{Task: task("b", core.TaskQueued, core.StageImplement, ago(10*hour)), Events: []Event{opened(2, ago(5*hour)), claim(1, core.StageImplement, ago(6*hour))}},
		{Task: task("a", core.TaskMerged, "", ago(20*hour)), Events: []Event{stateChanged(1, core.TaskMerged, ago(2*hour))}},
	}}
	actions := map[string]nextaction.Compact{"a": terminal, "b": queued}
	before, _ := json.Marshal(inputs)
	actionsBefore, _ := json.Marshal(actions)
	first := Compute(inputs, actions, now)
	after, _ := json.Marshal(inputs)
	actionsAfter, _ := json.Marshal(actions)
	if string(before) != string(after) || string(actionsBefore) != string(actionsAfter) {
		t.Fatalf("Compute mutated its inputs:\n%s\n%s", before, after)
	}
	if second := Compute(inputs, actions, now); !reflect.DeepEqual(first, second) {
		t.Fatal("Compute is not deterministic for one input and instant")
	}
}

// TestPackageImportsNoStoreHTTPOrClock holds invariant 21's boundary: the
// package imports no store, HTTP, database, or model client, and no source
// file reads the clock.
func TestPackageImportsNoStoreHTTPOrClock(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range parsed.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			for _, banned := range []string{"/internal/store", "/internal/httpapi", "net/http", "database/sql", "/internal/llm", "/internal/dispatch", "github.com/jackc"} {
				if strings.Contains(path, banned) {
					t.Fatalf("%s imports %s", name, path)
				}
			}
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(source), "time.Now(") || strings.Contains(string(source), "time.Since(") {
			t.Fatalf("%s reads the clock", name)
		}
	}
}
