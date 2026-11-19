package crateinsight

import (
	"cmp"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/insight"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

// Event kinds the computation folds (component-crates invariants 13 and 22).
const (
	kindPullRequestOpened = "pull_request.opened"
	kindReviewRound       = "review.round_completed"
	kindMergeConfirmed    = "merge.confirmed"
	kindMergeReconciled   = "merge.reconciled"
	kindTaskCancelled     = "task.cancelled"
	kindStateChanged      = "task.state_changed"
	kindClaimed           = "work_order.claimed"
	verdictApprove        = "approve"
	verdictChanges        = "changes_requested"
	crateDateLayout       = "2006-01-02"
	day                   = 24 * time.Hour
	// throughputWindow is the crate's trailing delivery window: the panel
	// shows tasks merged per week over the trailing 14 days
	// (req-crate-insights AC-4.1).
	throughputWindow = 14 * day
)

// InsightEventKinds are the event kinds the insight read returns for each
// current member (component-crates invariant 22): the activity kinds of
// invariant 13, then task.state_changed, task.plan_state_changed, and
// work_order.claimed.
var InsightEventKinds = []string{
	"task.created", "task.crate_set", kindPullRequestOpened, kindReviewRound,
	kindMergeConfirmed, kindMergeReconciled, kindTaskCancelled,
	kindStateChanged, core.EventTaskPlanStateChanged, kindClaimed,
}

// Compute returns the insight view of the inputs at the read instant now
// (component-crates invariant 21). actions holds each member's compact next
// action from the shared projection, keyed by task ID; a member without an
// entry reads as the zero value. Compute reads no clock and performs no I/O.
func Compute(inputs Inputs, actions map[string]nextaction.Compact, now time.Time) View {
	now = now.UTC()
	members := OrderedMembers(inputs.Members, actions)
	view := View{
		Tasks:       make([]Task, 0, len(members)),
		Branches:    Branches(inputs),
		Whereabouts: []Whereabouts{},
		Progress:    Progress(inputs, actions, now),
		FirstReview: FirstReviewOf(inputs),
		Replay:      ReplayOf(inputs, actions, now),
	}
	view.ForecastDaysBeforeTarget = ForecastDaysBeforeTarget(inputs.Crate.TargetDate, view.Progress.Forecast)
	for _, member := range members {
		view.Tasks = append(view.Tasks, TaskOf(member, actions[member.Task.ID]))
	}
	view.Whereabouts = WhereaboutsOf(view.Tasks, view.Branches)
	return view
}

// pullRequestOpened reports whether the member's events record an opened
// pull request.
func pullRequestOpened(member Member) bool {
	for _, event := range member.Events {
		if event.Kind == kindPullRequestOpened {
			return true
		}
	}
	return false
}

// PhaseOf returns a member's phase (component-crates invariant 23;
// req-crate-insights AC-1.2). A next stage outside the stages invariant 23
// names, such as gate or merge, reads as no next stage.
func PhaseOf(task core.Task, action nextaction.Compact, pullRequest bool) Phase {
	switch task.State {
	case core.TaskMerged:
		return PhaseMerged
	case core.TaskClosed:
		return PhaseClosed
	}
	if action.Actor == nextaction.ActorDependency {
		return PhaseBlocked
	}
	if action.Judgment {
		return PhaseNeedsYou
	}
	working := action.Kind == nextaction.KindWorking
	switch task.NextStage {
	case core.StageReview, core.StageVerify, core.StageRisk:
		return PhaseReview
	case core.StageImplement:
		if working || pullRequest {
			return PhaseBuilding
		}
		return PhaseQueued
	case core.StageTriage, core.StageSpec:
		if working {
			return PhasePlanning
		}
		return PhaseQueued
	}
	switch {
	case pullRequest:
		return PhaseReview
	case working:
		return PhaseBuilding
	}
	return PhaseQueued
}

// StationOf returns a member's station (component-crates invariant 24;
// req-crate-insights AC-1.1).
func StationOf(task core.Task, action nextaction.Compact, phase Phase, pullRequest bool) Station {
	switch task.State {
	case core.TaskMerged:
		return StationCrate
	case core.TaskClosed:
		return StationAside
	}
	switch action.Kind {
	case nextaction.KindPlanApproval:
		return StationPlan
	case nextaction.KindMergeApproval:
		return StationGate
	}
	waiting := phase == PhaseQueued || phase == PhaseBlocked
	switch task.NextStage {
	case core.StageTriage, core.StageSpec:
		if waiting {
			return StationIntake
		}
		return StationPlan
	case core.StageImplement:
		return StationBuild
	case core.StageReview, core.StageVerify, core.StageRisk:
		return StationReview
	case core.StageGate, core.StageMerge:
		return StationGate
	}
	switch {
	case pullRequest:
		return StationReview
	case waiting:
		return StationIntake
	}
	return StationBuild
}

// TerminalTimes returns a member's earliest task.state_changed times whose
// payload `to` is merged and closed, zero when there is none
// (component-requirement-insights invariant 2).
func TerminalTimes(events []Event) (merged, closed time.Time) {
	for _, event := range events {
		if event.Kind != kindStateChanged {
			continue
		}
		var payload struct {
			To core.TaskState `json:"to"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		at := event.At.UTC()
		switch payload.To {
		case core.TaskMerged:
			if merged.IsZero() || at.Before(merged) {
				merged = at
			}
		case core.TaskClosed:
			if closed.IsZero() || at.Before(closed) {
				closed = at
			}
		}
	}
	return merged, closed
}

// reviewVerdict returns the verdict of a review.round_completed payload.
func reviewVerdict(event Event) string {
	var payload struct {
		Verdict string `json:"verdict"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil {
		return ""
	}
	return payload.Verdict
}

// sortedEvents returns a copy of events in (at, id) order.
func sortedEvents(events []Event) []Event {
	out := slices.Clone(events)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// TaskOf returns a member's entry on the insight view. Times come from the
// member's recorded events, not from its current branch or last pull
// request alone.
func TaskOf(member Member, action nextaction.Compact) Task {
	opened := pullRequestOpened(member)
	phase := PhaseOf(member.Task, action, opened)
	task := Task{
		ID: member.Task.ID, Phase: phase, Station: StationOf(member.Task, action, phase, opened),
		BaseBranch: member.Task.BaseBranch, CreatedAt: member.Task.CreatedAt.UTC(),
		PullRequestOpenedAt: []time.Time{}, ChangesRequestedAt: []time.Time{},
	}
	for _, event := range sortedEvents(member.Events) {
		switch event.Kind {
		case kindPullRequestOpened:
			task.PullRequestOpenedAt = append(task.PullRequestOpenedAt, event.At.UTC())
		case kindReviewRound:
			task.ReviewRounds++
			if reviewVerdict(event) == verdictChanges {
				task.ChangesRequestedAt = append(task.ChangesRequestedAt, event.At.UTC())
			}
		}
	}
	merged, closed := TerminalTimes(member.Events)
	if !merged.IsZero() {
		task.MergedAt = &merged
	}
	if !closed.IsZero() {
		task.ClosedAt = &closed
	}
	return task
}

// Rank returns the group of invariant 28 that a member belongs to, 0 first:
// a judgment; an agent at work; another agent next action; another open
// member that no dependency blocks; a member a dependency blocks; merged;
// closed (component-crates invariant 28; req-crates AC-5.7).
func Rank(task core.Task, action nextaction.Compact) int {
	switch {
	case task.State == core.TaskMerged:
		return 5
	case task.State == core.TaskClosed:
		return 6
	case action.Judgment:
		return 0
	case action.Actor == nextaction.ActorAgent && action.Kind == nextaction.KindWorking:
		return 1
	case action.Actor == nextaction.ActorAgent:
		return 2
	case action.Actor == nextaction.ActorDependency:
		return 4
	}
	return 3
}

// CompareTasks is the one task order the crate read and the insight view
// share: Rank, then creation time, then ID (component-crates invariant 28).
func CompareTasks(a, b core.Task, actions map[string]nextaction.Compact) int {
	if n := cmp.Compare(Rank(a, actions[a.ID]), Rank(b, actions[b.ID])); n != 0 {
		return n
	}
	if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
		return n
	}
	return strings.Compare(a.ID, b.ID)
}

// SortTasks orders tasks in place by CompareTasks.
func SortTasks(tasks []core.Task, actions map[string]nextaction.Compact) {
	slices.SortStableFunc(tasks, func(a, b core.Task) int { return CompareTasks(a, b, actions) })
}

// OrderedMembers returns a copy of members ordered by CompareTasks.
func OrderedMembers(members []Member, actions map[string]nextaction.Compact) []Member {
	out := slices.Clone(members)
	slices.SortStableFunc(out, func(a, b Member) int { return CompareTasks(a.Task, b.Task, actions) })
	return out
}

// branchRank places the trunk names first in the order invariant 26 lists
// them, then develop and dev, then every other branch.
func branchRank(name string) int {
	if i := slices.Index([]string{"master", "main", "production", "release", "develop", "dev"}, name); i >= 0 {
		return i
	}
	return 6
}

// Branches lists each member's base branch once in lane order: master,
// main, production, and release, then develop and dev, then the rest by
// name (component-crates invariant 26; req-crate-insights AC-2.1).
func Branches(inputs Inputs) []string {
	branches := []string{}
	for _, member := range inputs.Members {
		if !slices.Contains(branches, member.Task.BaseBranch) {
			branches = append(branches, member.Task.BaseBranch)
		}
	}
	slices.SortFunc(branches, func(a, b string) int {
		if n := cmp.Compare(branchRank(a), branchRank(b)); n != 0 {
			return n
		}
		return strings.Compare(a, b)
	})
	return branches
}

// WhereaboutsOf groups the view's tasks: not_started for open members at
// intake, task_branch for the other open members, and then one merged entry
// per branch in reverse lane order. An empty not_started or task_branch
// entry is left out, and a branch entry stays even when it is empty
// (component-crates invariant 26; req-crate-insights AC-2.4).
func WhereaboutsOf(tasks []Task, branches []string) []Whereabouts {
	notStarted := Whereabouts{Key: WhereaboutsNotStarted, TaskIDs: []string{}}
	onBranch := Whereabouts{Key: WhereaboutsTaskBranch, TaskIDs: []string{}}
	merged := map[string][]string{}
	for _, task := range tasks {
		switch {
		case task.Phase == PhaseMerged:
			merged[task.BaseBranch] = append(merged[task.BaseBranch], task.ID)
		case task.Phase == PhaseClosed:
		case task.Station == StationIntake:
			notStarted.TaskIDs = append(notStarted.TaskIDs, task.ID)
		default:
			onBranch.TaskIDs = append(onBranch.TaskIDs, task.ID)
		}
	}
	out := []Whereabouts{}
	for _, group := range []Whereabouts{notStarted, onBranch} {
		if len(group.TaskIDs) > 0 {
			out = append(out, group)
		}
	}
	for i := len(branches) - 1; i >= 0; i-- {
		branch := branches[i]
		ids := merged[branch]
		if ids == nil {
			ids = []string{}
		}
		out = append(out, Whereabouts{Key: WhereaboutsMerged, Branch: &branch, TaskIDs: ids})
	}
	return out
}

// position is one phase and station pair on the line.
type position struct {
	phase   Phase
	station Station
}

// replayPosition returns the position an event places a member at, and
// false for an event that places it nowhere (component-crates invariant 25).
func replayPosition(event Event) (position, bool) {
	switch event.Kind {
	case kindClaimed:
		switch event.Stage {
		case core.StageTriage, core.StageSpec:
			return position{PhasePlanning, StationPlan}, true
		case core.StageImplement:
			return position{PhaseBuilding, StationBuild}, true
		case core.StageReview, core.StageVerify, core.StageRisk:
			return position{PhaseReview, StationReview}, true
		}
	case core.EventTaskPlanStateChanged:
		var change core.PlanStateChange
		if json.Unmarshal(event.Payload, &change) == nil && change.To == core.PlanAwaitingApproval {
			return position{PhaseNeedsYou, StationPlan}, true
		}
	case kindPullRequestOpened:
		return position{PhaseReview, StationReview}, true
	case kindReviewRound:
		// An approving round places the member nowhere by itself: the
		// recorded gate transition below says whether the merge waits on a
		// person.
		if reviewVerdict(event) == verdictChanges {
			return position{PhaseBuilding, StationBuild}, true
		}
	case kindMergeConfirmed, kindMergeReconciled:
		return position{PhaseMerged, StationCrate}, true
	case kindTaskCancelled:
		return position{PhaseClosed, StationAside}, true
	case kindStateChanged:
		var payload struct {
			To      core.TaskState   `json:"to"`
			Command core.TaskCommand `json:"command"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil {
			// Every store records the round's entry to the merge gate as
			// gate.merge into awaiting_human, whether the frozen merge gate
			// or a risk hold requires the person (DEC-124(2)). An admitted
			// approval records review.accept and enters no gate.
			if payload.Command == core.TaskGateMerge && payload.To == core.TaskAwaiting {
				return position{PhaseNeedsYou, StationGate}, true
			}
			switch payload.To {
			case core.TaskMerged:
				return position{PhaseMerged, StationCrate}, true
			case core.TaskClosed:
				return position{PhaseClosed, StationAside}, true
			}
		}
	}
	return position{}, false
}

// MemberSteps returns one member's replay steps: queued at intake at its
// creation, then each change its events record in (at, id) order, then,
// when the last step differs from the live position, one step at the read
// instant that closes the difference (component-crates invariant 25).
func MemberSteps(member Member, action nextaction.Compact, now time.Time) []Step {
	task := member.Task
	current := position{PhaseQueued, StationIntake}
	steps := []Step{{At: task.CreatedAt.UTC(), TaskID: task.ID, Phase: current.phase, Station: current.station}}
	for _, event := range sortedEvents(member.Events) {
		next, ok := replayPosition(event)
		if !ok || next == current {
			continue
		}
		current = next
		steps = append(steps, Step{At: event.At.UTC(), TaskID: task.ID, Phase: next.phase, Station: next.station})
	}
	opened := pullRequestOpened(member)
	phase := PhaseOf(task, action, opened)
	live := position{phase, StationOf(task, action, phase, opened)}
	if live != current {
		steps = append(steps, Step{At: now.UTC(), TaskID: task.ID, Phase: live.phase, Station: live.station})
	}
	return steps
}

// ReplayOf returns the line's history: every member's steps in (at, task
// ID) order, keeping event order for one member's steps at one time.
// Start is the earliest member creation, or now with no member; End is now
// (component-crates invariant 25; req-crate-insights AC-1.4).
func ReplayOf(inputs Inputs, actions map[string]nextaction.Compact, now time.Time) Replay {
	now = now.UTC()
	replay := Replay{Start: now, End: now, Steps: []Step{}}
	for i, member := range inputs.Members {
		created := member.Task.CreatedAt.UTC()
		if i == 0 || created.Before(replay.Start) {
			replay.Start = created
		}
		replay.Steps = append(replay.Steps, MemberSteps(member, actions[member.Task.ID], now)...)
	}
	sort.SliceStable(replay.Steps, func(i, j int) bool {
		a, b := replay.Steps[i], replay.Steps[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return a.TaskID < b.TaskID
	})
	return replay
}

// ServingTasks adapts the members to the requirement insight inputs: a
// member's creation time takes the place of serving_since, and its terminal
// times are TerminalTimes (component-crates invariant 27).
func ServingTasks(inputs Inputs) insight.Inputs {
	adapted := insight.Inputs{Tasks: make([]insight.ServingTask, 0, len(inputs.Members))}
	for _, member := range inputs.Members {
		merged, closed := TerminalTimes(member.Events)
		adapted.Tasks = append(adapted.Tasks, insight.ServingTask{
			Task: member.Task, ServingSince: member.Task.CreatedAt.UTC(),
			MergedAt: merged, ClosedAt: closed, DependsOn: []string{}, Designs: []string{},
		})
	}
	return adapted
}

// Progress returns the crate's delivery numbers. It calls insight.Progress
// as it stands, then applies the two crate differences that
// req-crate-insights states: throughput is the tasks merged per week over
// the trailing 14 days (AC-4.1), and a forecast exists only while a member
// is open (AC-4.2, AC-4.3). The requirement insight results do not change.
func Progress(inputs Inputs, actions map[string]nextaction.Compact, now time.Time) insight.ProgressResult {
	now = now.UTC()
	adapted := ServingTasks(inputs)
	result := insight.Progress(adapted, actions, now)
	recent := 0
	for _, task := range adapted.Tasks {
		// The window is inclusive at both ends and ends at the read instant
		// (component-requirement-insights invariant 7).
		if merged := task.MergedAt; !merged.IsZero() && !merged.After(now) && !merged.Before(now.Add(-throughputWindow)) {
			recent++
		}
	}
	result.ThroughputPerWeek = float64(recent) / 2
	if result.Open == 0 {
		result.Forecast = nil
	}
	return result
}

// FirstReviewOf counts the members with a completed review round and those
// whose earliest completed round approved (component-crates invariant 27).
func FirstReviewOf(inputs Inputs) FirstReview {
	var result FirstReview
	for _, member := range inputs.Members {
		for _, event := range sortedEvents(member.Events) {
			if event.Kind != kindReviewRound {
				continue
			}
			result.Reviewed++
			if reviewVerdict(event) == verdictApprove {
				result.Approved++
			}
			break
		}
	}
	return result
}

// ForecastDaysBeforeTarget returns the target date minus the forecast day
// in whole UTC days, or nil when either is absent or the target date does
// not parse (component-crates invariant 27; req-crate-insights AC-4.3).
func ForecastDaysBeforeTarget(targetDate *string, forecast *insight.Forecast) *int {
	if targetDate == nil || forecast == nil {
		return nil
	}
	target, err := time.Parse(crateDateLayout, *targetDate)
	if err != nil {
		return nil
	}
	forecastDay := forecast.Day.UTC()
	forecastDay = time.Date(forecastDay.Year(), forecastDay.Month(), forecastDay.Day(), 0, 0, 0, 0, time.UTC)
	days := int(target.Sub(forecastDay) / day)
	return &days
}
