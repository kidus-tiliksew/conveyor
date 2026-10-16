package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
)

var ErrStartOverConfirmDocuments = errors.New("task has pending proposals: confirm_documents capability is required")
var ErrStartOverRequestConflict = errors.New("request_id was already used with a different reason or note")

const TaskStartOverContextDropped = "task.start_over_context_dropped"

type taskStartOverFaultKey struct{}

// WithTaskStartOverFault supplies deterministic transaction failures to backend
// conformance fixtures. It is an in-process test hook, never a request field.
func WithTaskStartOverFault(ctx context.Context, fn func(string) error) context.Context {
	return context.WithValue(ctx, taskStartOverFaultKey{}, fn)
}

func TaskStartOverFault(ctx context.Context, step string) error {
	if fn, ok := ctx.Value(taskStartOverFaultKey{}).(func(string) error); ok {
		return fn(step)
	}
	return nil
}

// StartOverDropReason maps a context-validation error onto a drop reason.
// Other errors are not droppable and must fail the command.
func StartOverDropReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var requirement *RequirementArchivedError
	var design *SystemDesignArchivedError
	if errors.As(err, &requirement) || errors.As(err, &design) {
		return "archived", true
	}
	var reference *TaskContextReferenceError
	if errors.As(err, &reference) {
		switch reference.Reason {
		case ContextReasonUnconfirmed:
			return "unconfirmed", true
		case ContextReasonNotFound:
			return "missing", true
		}
	}
	return "", false
}

// PartitionStartOverContext keeps present, confirmed, non-archived documents
// and reports each omitted inherited reference.
func PartitionStartOverContext(attached TaskContextInput, pinned map[string]int, validate func(TaskContextInput) error) (TaskContextInput, map[string]int, []core.TaskStartOverDroppedContext, error) {
	var kept TaskContextInput
	var drops []core.TaskStartOverDroppedContext
	requirementIDs := slices.Clone(attached.RequirementIDs)
	sort.Strings(requirementIDs)
	for _, id := range requirementIDs {
		err := validate(TaskContextInput{RequirementIDs: []string{id}})
		if reason, ok := StartOverDropReason(err); ok {
			drops = append(drops, core.TaskStartOverDroppedContext{Kind: "requirement", ID: id, Reason: reason})
			continue
		}
		if err != nil {
			return TaskContextInput{}, nil, nil, err
		}
		kept.RequirementIDs = append(kept.RequirementIDs, id)
	}
	designIDs := slices.Clone(attached.DesignIDs)
	sort.Strings(designIDs)
	keptPinned := map[string]int{}
	for _, id := range designIDs {
		err := validate(TaskContextInput{DesignIDs: []string{id}})
		if reason, ok := StartOverDropReason(err); ok {
			drops = append(drops, core.TaskStartOverDroppedContext{Kind: "system design", ID: id, Reason: reason})
			continue
		}
		if err != nil {
			return TaskContextInput{}, nil, nil, err
		}
		kept.DesignIDs = append(kept.DesignIDs, id)
		if version, ok := pinned[id]; ok {
			keptPinned[id] = version
		}
	}
	return kept, keptPinned, drops, nil
}

func StartOverDroppedContextFromEvents(events []core.Event) []core.TaskStartOverDroppedContext {
	for _, event := range events {
		if event.Kind != TaskStartOverContextDropped {
			continue
		}
		var payload struct {
			Documents []core.TaskStartOverDroppedContext `json:"documents"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil
		}
		return payload.Documents
	}
	return nil
}

func StartOverDroppedContextPayload(drops []core.TaskStartOverDroppedContext) []byte {
	return core.JSONPayload(map[string]any{"documents": drops})
}

// StartOverSuccessor copies intake policy, never execution or approval state.
// req-task-lifecycle-and-queue AC-7.3.
func StartOverSuccessor(old core.Task, r core.TaskStartOverRequest) core.Task {
	id := core.NewTaskID()
	direction := "Start-over reason: " + r.Reason
	if r.Note != "" {
		direction += "\nOperator note: " + r.Note
	}
	return core.Task{ID: id, Workspace: old.Workspace, Source: old.Source, Title: old.Title, Body: old.Body,
		Repo: old.Repo, BaseBranch: old.BaseBranch, Branch: gitx.BranchName(id),
		SpecApproval: old.SpecApproval, MergeApproval: old.MergeApproval, PolicyVersion: old.PolicyVersion,
		SetupContract: old.SetupContract, State: core.TaskQueued, NextStage: core.StageTriage,
		Supersedes: old.ID, IntakeOperatorDirection: direction, CreatedAt: time.Now().UTC()}
}

func StartOverPlanContent(taskID string, version int, content string) []byte {
	return []byte(fmt.Sprintf("# Previous approved execution plan (reference only)\n\nRetired task: %s\nPlan version: %d\n\nThis plan is reference context, not approval for the successor task.\n\n%s\n", taskID, version, content))
}

func StartOverDismissalDecided(err error) bool {
	var requirement *RequirementVersionDismissalConflict
	var design *SystemDesignVersionDismissalConflict
	return errors.As(err, &requirement) || errors.As(err, &design)
}
