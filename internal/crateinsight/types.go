// Package crateinsight computes a crate's insight view from its insight
// inputs: each member's phase and station, the replay steps, the branch
// lanes and whereabouts, the delivery numbers, the member order, and the
// dependency values (component-crates invariants 21, 23 to 28, and 38;
// feature-crate-insights CI-1; req-crate-insights REQ-1, REQ-2, REQ-4,
// REQ-6). Every function is pure: it reads
// only its arguments, performs no I/O, reads no clock, and never mutates an
// input. The package imports no store, HTTP, database, or model client, so
// the crate briefing and the crate page parts reuse it as it stands.
package crateinsight

import (
	"encoding/json"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/insight"
)

// Crate is the crate row the insight read returns (component-crates
// invariant 22). TargetDate is a calendar date written YYYY-MM-DD, or nil.
// The counts are the crate row's counts of invariant 9.
type Crate struct {
	// ID is the crate's ID, such as crate-261119-d9973f.
	ID string `json:"id"`
	// Name is the crate's name.
	Name string `json:"name"`
	// TargetDate is the crate's target date written YYYY-MM-DD, or null.
	TargetDate *string `json:"target_date"`
	// State is open or closed.
	State string `json:"state"`
	// Shipped is true for an open crate with a member and no open member.
	Shipped bool `json:"shipped"`
	// CreatedBy is the actor ID that created the crate.
	CreatedBy string `json:"created_by"`
	// CreatedAt is the crate's creation time in UTC.
	CreatedAt time.Time `json:"created_at"`
	// ClosedAt is the time the crate closed, or null while it is open.
	ClosedAt *time.Time `json:"closed_at"`
	// OpenTaskCount counts members in neither merged nor closed.
	OpenTaskCount int `json:"open_task_count"`
	// MergedTaskCount counts members in state merged.
	MergedTaskCount int `json:"merged_task_count"`
	// TotalTaskCount counts every member.
	TotalTaskCount int `json:"total_task_count"`
}

// Event is one recorded event of a member that the insight read returns:
// the kinds of component-crates invariant 13, task.state_changed,
// task.plan_state_changed, and work_order.claimed (invariant 22).
type Event struct {
	// ID is the event's immutable ID. It orders events that share a time.
	ID int64 `json:"id"`
	// Kind is the event kind, such as pull_request.opened.
	Kind string `json:"kind"`
	// At is the time the event was recorded, in UTC.
	At time.Time `json:"at"`
	// Stage is the stage of the work order a work_order.claimed event
	// names, and empty for every other kind.
	Stage core.Stage `json:"stage,omitempty"`
	// Payload is the event's recorded payload. A work_order.claimed event
	// carries none: the read returns only its Stage.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Dependency is one task a member depends on, as the insight read returns
// it (component-crates invariant 22): the task's ID, title, and state. The
// task may be outside the crate, merged, or closed.
type Dependency struct {
	// ID is the task ID of the task the member depends on.
	ID string `json:"id"`
	// Title is that task's title.
	Title string `json:"title"`
	// State is that task's state.
	State core.TaskState `json:"state"`
}

// Member is one current member of the crate with its task row, its
// dependencies in ID order, and its events in (at, id) order. Events
// recorded before the task joined the crate are included.
type Member struct {
	// Task is the member's task row, usable by the shared next-action
	// projection. Its CreatedAt, BaseBranch, NextStage, and State fields
	// feed the computation. The dependencies are carried in Dependencies,
	// not in the task row, so the projection reads the same row it reads
	// for every other task.
	Task core.Task `json:"task"`
	// Dependencies are the tasks the member depends on, in ID order.
	Dependencies []Dependency `json:"dependencies"`
	// Events are the member's insight events in (at, id) order.
	Events []Event `json:"events"`
}

// Inputs is the insight read's result (component-crates invariant 22): the
// crate row and its current members ordered by creation time and then ID.
type Inputs struct {
	Crate   Crate    `json:"crate"`
	Members []Member `json:"members"`
}

// Phase is what a member is doing (component-crates, Phase).
type Phase string

// The eight phases of component-crates Public surfaces.
const (
	PhaseQueued   Phase = "queued"
	PhasePlanning Phase = "planning"
	PhaseBuilding Phase = "building"
	PhaseReview   Phase = "review"
	PhaseNeedsYou Phase = "needs_you"
	PhaseBlocked  Phase = "blocked"
	PhaseMerged   Phase = "merged"
	PhaseClosed   Phase = "closed"
)

// Station is where a member sits on the crate's line (component-crates,
// Station).
type Station string

// The seven stations of component-crates Public surfaces. The line shows
// them in this order. Aside holds closed members off the line.
const (
	StationIntake Station = "intake"
	StationPlan   Station = "plan"
	StationBuild  Station = "build"
	StationReview Station = "review"
	StationGate   Station = "gate"
	StationCrate  Station = "crate"
	StationAside  Station = "aside"
)

// Whereabouts keys (component-crates invariant 26).
const (
	// WhereaboutsNotStarted holds the open members at the intake station.
	WhereaboutsNotStarted = "not_started"
	// WhereaboutsTaskBranch holds the other open members.
	WhereaboutsTaskBranch = "task_branch"
	// WhereaboutsMerged holds the members merged into one branch.
	WhereaboutsMerged = "merged"
)

// Task is one member on the insight view (component-crates, Insight view).
type Task struct {
	// ID is the member's task ID.
	ID string `json:"id"`
	// Phase is what the member is doing now (invariant 23).
	Phase Phase `json:"phase"`
	// Station is where the member sits on the line now (invariant 24).
	Station Station `json:"station"`
	// BaseBranch is the branch the member's work leaves and merges into.
	BaseBranch string `json:"base_branch"`
	// CreatedAt is the member's creation time.
	CreatedAt time.Time `json:"created_at"`
	// PullRequestOpenedAt holds the time of each pull_request.opened event,
	// oldest first. It is empty when no pull request was opened.
	PullRequestOpenedAt []time.Time `json:"pull_request_opened_at"`
	// ChangesRequestedAt holds the time of each review round that asked for
	// changes, oldest first (req-crate-insights AC-2.3).
	ChangesRequestedAt []time.Time `json:"changes_requested_at"`
	// ReviewRounds counts the member's completed review rounds.
	ReviewRounds int `json:"review_rounds"`
	// MergedAt is the member's earliest move into merged, or null.
	MergedAt *time.Time `json:"merged_at"`
	// ClosedAt is the member's earliest move into closed, or null.
	ClosedAt *time.Time `json:"closed_at"`
	// DependsOn holds one entry for each task the member depends on, in ID
	// order, a task outside the crate included (invariant 38).
	DependsOn []DependsOn `json:"depends_on"`
	// WaitDepth is 0 when no DependsOn entry is both waiting and member,
	// and otherwise one more than the highest WaitDepth among the members
	// those entries name (invariant 38).
	WaitDepth int `json:"wait_depth"`
	// WaitedOnBy holds, in ID order, the IDs of the members with a waiting
	// DependsOn entry that names this member (invariant 38).
	WaitedOnBy []string `json:"waited_on_by"`
}

// DependsOn is one task a member depends on, on the insight view
// (component-crates invariant 38; req-crate-insights AC-6.1).
type DependsOn struct {
	// ID is the task ID of the task the member depends on.
	ID string `json:"id"`
	// Title is that task's title.
	Title string `json:"title"`
	// State is that task's state. closed marks a task that closed without
	// merging.
	State core.TaskState `json:"state"`
	// Member is true when that task is a current member of the crate.
	Member bool `json:"member"`
	// Waiting is true when the member is neither merged nor closed and
	// that task is not merged.
	Waiting bool `json:"waiting"`
}

// Whereabouts is one group of the branch map's counts (component-crates
// invariant 26; req-crate-insights AC-2.4).
type Whereabouts struct {
	// Key is not_started, task_branch, or merged.
	Key string `json:"key"`
	// Branch names the branch a merged group's members merged into. It is
	// null for not_started and task_branch.
	Branch *string `json:"branch"`
	// TaskIDs holds the group's members in the view's task order.
	TaskIDs []string `json:"task_ids"`
}

// FirstReview is the first-review approval count (component-crates
// invariant 27; req-crate-insights AC-4.1).
type FirstReview struct {
	// Approved counts the reviewed members whose earliest completed review
	// round approved.
	Approved int `json:"approved"`
	// Reviewed counts the members with a completed review round.
	Reviewed int `json:"reviewed"`
}

// Step is one replay step: a change of one member's phase or station at one
// time (component-crates invariant 25).
type Step struct {
	// At is the time of the change.
	At time.Time `json:"at"`
	// TaskID names the member that changed.
	TaskID string `json:"task_id"`
	// Phase is the member's phase from this time.
	Phase Phase `json:"phase"`
	// Station is the member's station from this time.
	Station Station `json:"station"`
}

// Replay is the line's history from the first member's creation to the read
// instant (req-crate-insights AC-1.4).
type Replay struct {
	// Start is the earliest member creation. With no member it equals End.
	Start time.Time `json:"start"`
	// End is the read instant.
	End time.Time `json:"end"`
	// Steps hold every change in (at, task ID) order. Steps of one member
	// that share a time keep their event order.
	Steps []Step `json:"steps"`
}

// View is the insight view GET /v1/crates/{id}/insight serves
// (component-crates, Insight view). It holds exactly the fields that
// section names.
type View struct {
	// Tasks holds one entry per member in the order of invariant 28.
	Tasks []Task `json:"tasks"`
	// Branches lists each member's base branch once, in lane order.
	Branches []string `json:"branches"`
	// Whereabouts holds the branch map's groups in invariant 26's order.
	Whereabouts []Whereabouts `json:"whereabouts"`
	// Progress is the delivery numbers, with the shape of the requirement
	// insight progress view.
	Progress insight.ProgressResult `json:"progress"`
	// FirstReview is the first-review approval count.
	FirstReview FirstReview `json:"first_review"`
	// ForecastDaysBeforeTarget is the target date minus the forecast day in
	// whole days: positive when the forecast falls before the target date,
	// negative when after. It is null without both.
	ForecastDaysBeforeTarget *int `json:"forecast_days_before_target"`
	// Replay is the line's history.
	Replay Replay `json:"replay"`
}
