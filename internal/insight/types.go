// Package insight computes a requirement's insight sections from its serving
// tasks: next-action groups, the critical path, delivery progress, document
// associations, and the headline fingerprint (feature-requirement-insights
// RI-3; req-requirement-insights REQ-2, REQ-3, REQ-4). Every function is pure:
// it reads only its arguments, performs no I/O, and never mutates an input.
// The package imports no store, HTTP, database, or model client.
package insight

import (
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

// Requirement is the requirement an insight describes, at its current
// confirmed version (RI-2).
type Requirement struct {
	ID             string                      `json:"id"`
	Title          string                      `json:"title"`
	CurrentVersion int                         `json:"current_version"`
	Content        string                      `json:"content"`
	Statements     []core.RequirementStatement `json:"statements"`
}

// ServingTask is one task whose latest requirement add/remove event for the
// requirement is an add (RI-2). ServingSince is that add's time. MergedAt and
// ClosedAt are the earliest task.state_changed times entering merged or
// closed, zero when the task never did. DependsOn holds direct dependency
// IDs, and Designs the System Design IDs in the task's attached context.
type ServingTask struct {
	Task         core.Task `json:"task"`
	ServingSince time.Time `json:"serving_since"`
	MergedAt     time.Time `json:"merged_at"`
	ClosedAt     time.Time `json:"closed_at"`
	DependsOn    []string  `json:"depends_on"`
	Designs      []string  `json:"designs"`
}

// Inputs is everything RI-3 computes over, as RI-2's store read returns it.
type Inputs struct {
	Requirement Requirement   `json:"requirement"`
	Tasks       []ServingTask `json:"tasks"`
}

// TaskRef names one task with its compact next action, unchanged (DEC-61).
// It never carries the task body.
type TaskRef struct {
	ID         string             `json:"id"`
	Title      string             `json:"title"`
	NextAction nextaction.Compact `json:"next_action"`
}

// Group is the open serving tasks whose next action names one actor
// (AC-2.1).
type Group struct {
	Actor string    `json:"actor"`
	Count int       `json:"count"`
	Tasks []TaskRef `json:"tasks"`
}

// CriticalPathResult is the longest dependency chain of open serving tasks,
// listed from the first dependency to the last dependent (AC-2.2).
type CriticalPathResult struct {
	Tasks  []TaskRef `json:"tasks"`
	Length int       `json:"length"`
}

// DailyPoint is one UTC calendar day of the scope/open/delivered series
// (AC-3.1). Day is UTC midnight.
type DailyPoint struct {
	Day       time.Time `json:"day"`
	Scope     int       `json:"scope"`
	Open      int       `json:"open"`
	Delivered int       `json:"delivered"`
}

// Forecast is the projected completion day at the trailing 14-day delivery
// rate (AC-3.2). Day is UTC midnight.
type Forecast struct {
	Day        time.Time `json:"day"`
	RatePerDay float64   `json:"rate_per_day"`
}

// WeeklyDelivered is one ISO week's merged deliveries (AC-3.4). WeekStart is
// the week's Monday at UTC midnight.
type WeeklyDelivered struct {
	WeekStart  time.Time `json:"week_start"`
	ISOYear    int       `json:"iso_year"`
	ISOWeek    int       `json:"iso_week"`
	Delivered  int       `json:"delivered"`
	InProgress bool      `json:"in_progress"`
}

// ActorCount is the number of open serving tasks whose next action names one
// actor.
type ActorCount struct {
	Actor string `json:"actor"`
	Count int    `json:"count"`
}

// StatusMix partitions the serving tasks into delivered, closed, and open by
// next-action actor; its counts sum to the number of serving tasks (AC-3.3).
type StatusMix struct {
	Delivered int          `json:"delivered"`
	Closed    int          `json:"closed"`
	Open      []ActorCount `json:"open"`
}

// ProgressResult is the Charts tab's data (AC-3.1 to AC-3.5). Forecast and
// MedianCycleSeconds are nil when they do not exist.
type ProgressResult struct {
	Daily              []DailyPoint      `json:"daily"`
	Forecast           *Forecast         `json:"forecast"`
	Weekly             []WeeklyDelivered `json:"weekly"`
	Delivered          int               `json:"delivered"`
	Total              int               `json:"total"`
	Open               int               `json:"open"`
	ThroughputPerWeek  float64           `json:"throughput_per_week"`
	MedianCycleSeconds *float64          `json:"median_cycle_seconds"`
	StatusMix          StatusMix         `json:"status_mix"`
}

// DesignInput is one System Design candidate a caller supplies to
// Associations: its current confirmed content and its pending-version count.
type DesignInput struct {
	ID                  string `json:"id"`
	Title               string `json:"title"`
	CurrentVersion      int    `json:"current_version"`
	Content             string `json:"content"`
	PendingVersionCount int    `json:"pending_version_count"`
}

// DesignAssociation is one System Design document associated with the
// requirement (AC-4.1). AttachedTaskIDs are the distinct serving tasks whose
// Designs hold it, sorted by ID; AttachedTaskCount is their number.
type DesignAssociation struct {
	ID                string   `json:"id"`
	Title             string   `json:"title"`
	CurrentVersion    int      `json:"current_version"`
	RevisionPending   bool     `json:"revision_pending"`
	AttachedTaskCount int      `json:"attached_task_count"`
	NamesRequirement  bool     `json:"names_requirement"`
	AttachedTaskIDs   []string `json:"attached_task_ids"`
}

// DecisionAssociation is one decision that is not dismissed and meets at
// least one of AC-4.2's three tests.
type DecisionAssociation struct {
	ID                 string              `json:"id"`
	Statement          string              `json:"statement"`
	Context            string              `json:"context"`
	Status             core.DecisionStatus `json:"status"`
	NamesRequirement   bool                `json:"names_requirement"`
	CitedByRequirement bool                `json:"cited_by_requirement"`
	CitingTaskIDs      []string            `json:"citing_task_ids"`
}

// CriterionAssociation is one acceptance criterion of the current confirmed
// statements with the serving tasks whose bodies cite it (AC-4.3). A cite is
// never a satisfaction claim.
type CriterionAssociation struct {
	ID            string   `json:"id"`
	Statement     string   `json:"statement"`
	CitingTaskIDs []string `json:"citing_task_ids"`
}

// AssociationsResult is the Documents tab's data (AC-4.1 to AC-4.3).
// CitedCount counts criteria with at least one citing task; Total counts all.
type AssociationsResult struct {
	Designs            []DesignAssociation    `json:"designs"`
	Decisions          []DecisionAssociation  `json:"decisions"`
	AcceptanceCriteria []CriterionAssociation `json:"acceptance_criteria"`
	CitedCount         int                    `json:"cited_count"`
	Total              int                    `json:"total"`
}
