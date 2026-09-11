package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
)

var ErrStartOverConfirmDocuments = errors.New("task has pending proposals: confirm_documents capability is required")
var ErrStartOverRequestConflict = errors.New("request_id was already used with a different reason or note")

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
		Hold: old.Hold, SpecApproval: old.SpecApproval, MergeApproval: old.MergeApproval, PolicyVersion: old.PolicyVersion,
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
