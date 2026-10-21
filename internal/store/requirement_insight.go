package store

// Requirement insight inputs and headline storage (feature-requirement-insights
// RI-2, RI-5; req-requirement-insights REQ-2, REQ-3, REQ-4; DEC-87). The
// inputs read is a pure, bounded read; the headline commands touch only the
// requirement_insight_headlines table and append no event (DEC-87 (2)).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/insight"
)

// RequirementInsightInputs is RI-2's read result, the pure input of the
// internal/insight computations.
type RequirementInsightInputs = insight.Inputs

// RequirementInsightTask is one serving task of RequirementInsightInputs.
type RequirementInsightTask = insight.ServingTask

// ErrInsightHeadlineCurrent refuses a headline whose fingerprint equals the
// stored row's; nothing is written (RI-5; AC-2.6).
var ErrInsightHeadlineCurrent = errors.New("insight_headline_current")

// RequirementInsightHeadline is the one stored headline of a requirement
// (RI-5). WorkspaceID is always the bound workspace, never a caller field.
// GeneratedBy is the credential-derived requesting actor the caller supplies.
// GeneratedAt is set by the store to its UTC storage time.
type RequirementInsightHeadline struct {
	WorkspaceID   string
	RequirementID string
	Fingerprint   string
	Headline      string
	Model         string
	TokensIn      int64
	TokensOut     int64
	GeneratedBy   string
	GeneratedAt   time.Time
}

// RequirementInsightStore is the requirement insight capability every
// backend implements (component-persistence; feature-requirement-insights).
type RequirementInsightStore interface {
	// ReadRequirementInsightInputs returns the requirement at its current
	// confirmed version and its serving tasks in ID order, in a fixed number
	// of set-based queries whatever the task count. A task serves when its
	// latest task.context_requirement_added or _removed event for the
	// requirement, in (at, id) order, is an add: the task map's rule. An
	// unknown, archived, or unconfirmed requirement is ErrNotFound.
	ReadRequirementInsightInputs(ctx context.Context, requirementID string) (RequirementInsightInputs, error)
	// GetRequirementInsightHeadline returns the stored headline, or
	// ErrNotFound when the requirement has none.
	GetRequirementInsightHeadline(ctx context.Context, requirementID string) (RequirementInsightHeadline, error)
	// RecordRequirementInsightHeadline inserts or replaces the headline in one
	// transaction that holds the row, or the key of an absent row. A stored
	// row with an equal fingerprint refuses with ErrInsightHeadlineCurrent and
	// nothing changes. It returns the stored row and appends no event.
	RecordRequirementInsightHeadline(ctx context.Context, headline RequirementInsightHeadline) (RequirementInsightHeadline, error)
}

// ValidateRequirementInsightHeadline checks the caller-supplied fields every
// backend refuses before any write.
func ValidateRequirementInsightHeadline(headline RequirementInsightHeadline) error {
	switch {
	case strings.TrimSpace(headline.RequirementID) == "":
		return errors.New("requirement insight headline requires a requirement ID")
	case strings.TrimSpace(headline.Fingerprint) == "":
		return errors.New("requirement insight headline requires a fingerprint")
	case headline.TokensIn < 0 || headline.TokensOut < 0:
		return errors.New("requirement insight headline token counts must not be negative")
	}
	return nil
}

// RequirementInsightNotFound wraps ErrNotFound for an unknown, archived, or
// unconfirmed requirement, the three cases RI-2 refuses.
func RequirementInsightNotFound(requirementID string) error {
	return fmt.Errorf("%w: requirement %s has no confirmed, unarchived version", ErrNotFound, requirementID)
}

// RequirementInsightServing is one serving attachment a backend's set-based
// read found: the task and the time of its latest add event.
type RequirementInsightServing struct {
	TaskID       string
	ServingSince time.Time
}

// LatestRequirementServing folds requirement add/remove events, ordered by
// (at, id) within each task, into the serving set for one requirement: the
// tasks whose latest event for it is an add, with that add's time. It is the
// fold ActiveTaskContextReferences applies, restricted to one requirement.
func LatestRequirementServing(events []core.Event, requirementID string) []RequirementInsightServing {
	latest := map[string]core.Event{}
	for _, event := range events {
		if event.Kind != TaskContextRequirementAdded && event.Kind != TaskContextRequirementRemoved {
			continue
		}
		var payload struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.ID != requirementID {
			continue
		}
		latest[event.TaskID] = event
	}
	out := make([]RequirementInsightServing, 0, len(latest))
	for taskID, event := range latest {
		if event.Kind == TaskContextRequirementAdded {
			out = append(out, RequirementInsightServing{TaskID: taskID, ServingSince: event.At.UTC()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out
}

// SortedEventsByTask orders events by task, then (at, id), the order the
// insight folds read.
func SortedEventsByTask(events []core.Event) []core.Event {
	out := slices.Clone(events)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TaskID != out[j].TaskID {
			return out[i].TaskID < out[j].TaskID
		}
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// RequirementInsightTerminalTimes returns, per task, the earliest
// task.state_changed time whose payload `to` is merged and the earliest whose
// `to` is closed.
func RequirementInsightTerminalTimes(events []core.Event) (merged, closed map[string]time.Time) {
	merged, closed = map[string]time.Time{}, map[string]time.Time{}
	for _, event := range events {
		if event.Kind != "task.state_changed" {
			continue
		}
		var payload struct {
			To core.TaskState `json:"to"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		target := map[core.TaskState]map[string]time.Time{core.TaskMerged: merged, core.TaskClosed: closed}[payload.To]
		if target == nil {
			continue
		}
		if current, ok := target[event.TaskID]; !ok || event.At.Before(current) {
			target[event.TaskID] = event.At.UTC()
		}
	}
	return merged, closed
}

// AssembleRequirementInsightInputs joins a backend's batched rows into RI-2's
// result. tasks holds the serving task rows; a serving attachment whose task
// row is missing is skipped. Slices are copies sorted for determinism.
func AssembleRequirementInsightInputs(requirement core.Requirement, version core.RequirementVersion, serving []RequirementInsightServing, tasks map[string]core.Task, dependencies map[string][]string, designs map[string][]string, merged, closed map[string]time.Time) RequirementInsightInputs {
	result := RequirementInsightInputs{
		Requirement: insight.Requirement{ID: requirement.ID, Title: requirement.Title, CurrentVersion: requirement.CurrentVersion, Content: version.Content, Statements: slices.Clone(version.Statements)},
		Tasks:       []RequirementInsightTask{},
	}
	if result.Requirement.Statements == nil {
		result.Requirement.Statements = []core.RequirementStatement{}
	}
	for _, attachment := range serving {
		task, ok := tasks[attachment.TaskID]
		if !ok {
			continue
		}
		dependsOn := slices.Clone(dependencies[task.ID])
		sort.Strings(dependsOn)
		dependsOn = slices.Compact(dependsOn)
		if dependsOn == nil {
			dependsOn = []string{}
		}
		designIDs := slices.Clone(designs[task.ID])
		sort.Strings(designIDs)
		designIDs = slices.Compact(designIDs)
		if designIDs == nil {
			designIDs = []string{}
		}
		result.Tasks = append(result.Tasks, RequirementInsightTask{
			Task: task, ServingSince: attachment.ServingSince.UTC(),
			MergedAt: merged[task.ID], ClosedAt: closed[task.ID],
			DependsOn: dependsOn, Designs: designIDs,
		})
	}
	sort.Slice(result.Tasks, func(i, j int) bool { return result.Tasks[i].Task.ID < result.Tasks[j].Task.ID })
	return result
}

// ActiveDesignAttachments folds one task's design add/remove events, in
// (at, id) order, into the designs whose latest event is an add.
func ActiveDesignAttachments(events []core.Event) []string {
	_, designs := ActiveTaskContextReferences(events)
	out := make([]string, 0, len(designs))
	for id := range designs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ReadRequirementInsightInputs reads the volatile store under one read lock
// with the same folds the SQL backends apply.
func (m *memory) ReadRequirementInsightInputs(ctx context.Context, requirementID string) (RequirementInsightInputs, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	key := memoryScopedKey{workspace: workspace, id: requirementID}
	requirement, ok := m.requirements[key]
	if !ok || requirement.Archived || requirement.CurrentVersion <= 0 {
		return RequirementInsightInputs{}, RequirementInsightNotFound(requirementID)
	}
	versions := m.requirementVersions[key]
	if requirement.CurrentVersion > len(versions) {
		return RequirementInsightInputs{}, RequirementInsightNotFound(requirementID)
	}
	version := versions[requirement.CurrentVersion-1]
	requirement.SupersededBy = slices.Clone(requirement.SupersededBy)

	var attachmentEvents []core.Event
	for id, task := range m.tasks {
		if task.Workspace != workspace {
			continue
		}
		for _, event := range m.events[id] {
			if event.Kind == TaskContextRequirementAdded || event.Kind == TaskContextRequirementRemoved {
				attachmentEvents = append(attachmentEvents, event)
			}
		}
	}
	serving := LatestRequirementServing(SortedEventsByTask(attachmentEvents), requirementID)
	tasks := map[string]core.Task{}
	dependencies := map[string][]string{}
	designs := map[string][]string{}
	var terminalEvents []core.Event
	for _, attachment := range serving {
		task := m.tasks[attachment.TaskID]
		task.Dependencies, task.BlockingTaskIDs, task.Children = nil, nil, nil
		if task.Assignee != nil {
			assignee := *task.Assignee
			task.Assignee = &assignee
		}
		tasks[task.ID] = task
		for dependency := range m.dependencies[task.ID] {
			dependencies[task.ID] = append(dependencies[task.ID], dependency)
		}
		var designEvents []core.Event
		for _, event := range m.sortedEventsLocked(task.ID) {
			switch event.Kind {
			case TaskContextDesignAdded, TaskContextDesignRemoved:
				designEvents = append(designEvents, event)
			case "task.state_changed":
				terminalEvents = append(terminalEvents, event)
			}
		}
		for _, id := range ActiveDesignAttachments(designEvents) {
			if _, exists := m.systemDesigns[memoryScopedKey{workspace: workspace, id: id}]; exists {
				designs[task.ID] = append(designs[task.ID], id)
			}
		}
	}
	merged, closed := RequirementInsightTerminalTimes(terminalEvents)
	return AssembleRequirementInsightInputs(requirement, version, serving, tasks, dependencies, designs, merged, closed), nil
}

// GetRequirementInsightHeadline returns the volatile stored headline.
func (m *memory) GetRequirementInsightHeadline(ctx context.Context, requirementID string) (RequirementInsightHeadline, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	headline, ok := m.requirementInsightHeadlines[memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: requirementID}]
	if !ok {
		return RequirementInsightHeadline{}, fmt.Errorf("%w: requirement insight headline %s", ErrNotFound, requirementID)
	}
	return headline, nil
}

// RecordRequirementInsightHeadline compares and replaces under the store
// mutex, the volatile backend's single serialization point.
func (m *memory) RecordRequirementInsightHeadline(ctx context.Context, headline RequirementInsightHeadline) (RequirementInsightHeadline, error) {
	if err := ValidateRequirementInsightHeadline(headline); err != nil {
		return RequirementInsightHeadline{}, err
	}
	workspace := workspaceOrDefault(ctx, "")
	if workspace == "" {
		return RequirementInsightHeadline{}, ErrWorkspaceRequired
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memoryScopedKey{workspace: workspace, id: headline.RequirementID}
	if stored, ok := m.requirementInsightHeadlines[key]; ok && stored.Fingerprint == headline.Fingerprint {
		return RequirementInsightHeadline{}, ErrInsightHeadlineCurrent
	}
	headline.WorkspaceID = workspace
	headline.GeneratedAt = time.Now().UTC().Truncate(time.Microsecond)
	if m.requirementInsightHeadlines == nil {
		m.requirementInsightHeadlines = map[memoryScopedKey]RequirementInsightHeadline{}
	}
	m.requirementInsightHeadlines[key] = headline
	return headline, nil
}
