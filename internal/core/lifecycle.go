package core

import (
	"fmt"
	"sort"
	"strings"
)

// LifecycleSpace identifies one closed state space governed by this module
// (spec §21.37).
type LifecycleSpace string

const (
	TaskLifecycle              LifecycleSpace = "task"
	WorkOrderLifecycle         LifecycleSpace = "work_order"
	JobLifecycle               LifecycleSpace = "job"
	GitHubPublicationLifecycle LifecycleSpace = "github_publication"
	ReviewPublicationLifecycle LifecycleSpace = "review_publication"
)

// TransitionAlternative is one legal command and destination from a state.
type TransitionAlternative struct {
	Command string `json:"command"`
	To      string `json:"to"`
}

// ErrInvalidTransition is returned for every edge absent from a canonical
// lifecycle table (spec §21.37). Allowed is stable-sorted for useful logs and
// deterministic tests.
type ErrInvalidTransition struct {
	Space   LifecycleSpace          `json:"space"`
	From    string                  `json:"from"`
	Command string                  `json:"command"`
	Allowed []TransitionAlternative `json:"allowed"`
}

func (e *ErrInvalidTransition) Error() string {
	allowed := make([]string, 0, len(e.Allowed))
	for _, alternative := range e.Allowed {
		allowed = append(allowed, alternative.Command+"->"+alternative.To)
	}
	return fmt.Sprintf("invalid %s transition from %q with command %q (allowed: %s)", e.Space, e.From, e.Command, strings.Join(allowed, ", "))
}

type TaskCommand string

const (
	TaskIntakeFinalize            TaskCommand = "intake.finalize"
	TaskDispatchStart             TaskCommand = "dispatch.start"
	TaskOrderClaim                TaskCommand = "order.claim"
	TaskStageAdvance              TaskCommand = "stage.advance"
	TaskStageBounce               TaskCommand = "stage.bounce"
	TaskStageBounceLimit          TaskCommand = "stage.bounce_limit"
	TaskJobFail                   TaskCommand = "job.fail"
	TaskTriageRouteHuman          TaskCommand = "triage.route_human"
	TaskTriagePark                TaskCommand = "triage.park"
	TaskGateSpec                  TaskCommand = "gate.spec"
	TaskGateMerge                 TaskCommand = "gate.merge"
	TaskDispatchFailRetry         TaskCommand = "dispatch.fail_retry"
	TaskDispatchFailFinal         TaskCommand = "dispatch.fail_final"
	TaskInterventionReject        TaskCommand = "intervention.reject"
	TaskInterventionApproveSpec   TaskCommand = "intervention.approve_spec"
	TaskInterventionApproveReview TaskCommand = "intervention.approve_review"
	TaskInterventionRedirect      TaskCommand = "intervention.redirect"
	TaskMergeConfirm              TaskCommand = "merge.confirm"
	TaskRefreshReview             TaskCommand = "refresh.review"
	TaskConflictDispatch          TaskCommand = "conflict.dispatch"
	TaskRecover                   TaskCommand = "task.recover"
	TaskCancel                    TaskCommand = "task.cancel"
)

type WorkOrderCommand string

const (
	WorkOrderCmdCreate              WorkOrderCommand = "order.create"
	WorkOrderCmdClaim               WorkOrderCommand = "order.claim"
	WorkOrderCmdRenew               WorkOrderCommand = "claim.renew"
	WorkOrderCmdRelease             WorkOrderCommand = "claim.release"
	WorkOrderCmdExpire              WorkOrderCommand = "claim.expire"
	WorkOrderCmdSubmitForReview     WorkOrderCommand = "submit_for_review"
	WorkOrderCmdSubmitSpec          WorkOrderCommand = "submit_spec"
	WorkOrderCmdSubmitReviewVerdict WorkOrderCommand = "submit_review_verdict"
	WorkOrderCmdReviewTerminal      WorkOrderCommand = "review.terminal"
	WorkOrderCmdReviewRevise        WorkOrderCommand = "review.revise"
	WorkOrderCmdTimeout             WorkOrderCommand = "order.timeout"
	WorkOrderCmdMarkStale           WorkOrderCommand = "order.stale"
	WorkOrderCmdRecover             WorkOrderCommand = "order.recover"
	WorkOrderCmdRedispatch          WorkOrderCommand = "order.redispatch"
	WorkOrderCmdCancel              WorkOrderCommand = "order.cancel"
)

type lifecycleTable map[string]map[string]string

var taskLifecycleTable = lifecycleTable{
	string(TaskClaiming): {string(TaskIntakeFinalize): string(TaskQueued), string(TaskCancel): string(TaskClosed)},
	string(TaskQueued):   {string(TaskDispatchStart): string(TaskRunning), string(TaskOrderClaim): string(TaskRunning), string(TaskDispatchFailRetry): string(TaskQueued), string(TaskDispatchFailFinal): string(TaskParked), string(TaskCancel): string(TaskClosed)},
	string(TaskRunning):  {string(TaskStageAdvance): string(TaskQueued), string(TaskStageBounce): string(TaskQueued), string(TaskStageBounceLimit): string(TaskAwaiting), string(TaskJobFail): string(TaskAwaiting), string(TaskTriageRouteHuman): string(TaskAwaiting), string(TaskTriagePark): string(TaskParked), string(TaskGateSpec): string(TaskAwaiting), string(TaskGateMerge): string(TaskAwaiting), string(TaskCancel): string(TaskClosed)},
	string(TaskAwaiting): {string(TaskInterventionReject): string(TaskClosed), string(TaskInterventionApproveSpec): string(TaskQueued), string(TaskInterventionApproveReview): string(TaskApproved), string(TaskInterventionRedirect): string(TaskQueued), string(TaskCancel): string(TaskClosed)},
	string(TaskApproved): {string(TaskMergeConfirm): string(TaskMerged), string(TaskRefreshReview): string(TaskQueued), string(TaskConflictDispatch): string(TaskQueued), string(TaskCancel): string(TaskClosed)},
	string(TaskParked):   {string(TaskRecover): string(TaskQueued), string(TaskCancel): string(TaskClosed)},
}

var workOrderLifecycleTable = lifecycleTable{
	"":                         {string(WorkOrderCmdCreate): string(WorkOrderQueued)},
	string(WorkOrderQueued):    {string(WorkOrderCmdClaim): string(WorkOrderClaimed), string(WorkOrderCmdTimeout): string(WorkOrderTimedOut), string(WorkOrderCmdMarkStale): string(WorkOrderStale), string(WorkOrderCmdCancel): string(WorkOrderCancelled)},
	string(WorkOrderClaimed):   {string(WorkOrderCmdRenew): string(WorkOrderClaimed), string(WorkOrderCmdRelease): string(WorkOrderQueued), string(WorkOrderCmdExpire): string(WorkOrderQueued), string(WorkOrderCmdSubmitForReview): string(WorkOrderSubmitted), string(WorkOrderCmdSubmitSpec): string(WorkOrderCompleted), string(WorkOrderCmdSubmitReviewVerdict): string(WorkOrderCompleted), string(WorkOrderCmdTimeout): string(WorkOrderTimedOut), string(WorkOrderCmdMarkStale): string(WorkOrderStale), string(WorkOrderCmdCancel): string(WorkOrderCancelled)},
	string(WorkOrderSubmitted): {string(WorkOrderCmdReviewTerminal): string(WorkOrderCompleted), string(WorkOrderCmdReviewRevise): string(WorkOrderClaimed), string(WorkOrderCmdMarkStale): string(WorkOrderStale), string(WorkOrderCmdCancel): string(WorkOrderCancelled)},
	string(WorkOrderTimedOut):  {string(WorkOrderCmdRecover): string(WorkOrderQueued), string(WorkOrderCmdCancel): string(WorkOrderCancelled)},
	// W14 is intentionally narrower than W13: its handler additionally guards
	// this stale edge to never-claimed orders (spec §3.3, §21.41).
	string(WorkOrderStale): {string(WorkOrderCmdRecover): string(WorkOrderQueued), string(WorkOrderCmdRedispatch): string(WorkOrderQueued), string(WorkOrderCmdCancel): string(WorkOrderCancelled)},
}

var jobLifecycleTable = lifecycleTable{
	"":                 {"job.create": string(JobPending)},
	string(JobPending): {"job.start": string(JobRunning)},
	string(JobRunning): {"job.complete": string(JobDone), "job.fail": string(JobFailed)},
	string(JobFailed):  {"job.retry": string(JobPending)},
}

func publicationLifecycleTable(queued, retrying, published, failed string) lifecycleTable {
	return lifecycleTable{
		"":       {"publication.create": queued},
		queued:   {"publication.retry": retrying, "publication.publish": published, "publication.fail": failed},
		retrying: {"publication.retry": retrying, "publication.publish": published, "publication.fail": failed},
		failed:   {"publication.retry": retrying},
	}
}

func transition(space LifecycleSpace, table lifecycleTable, from, command string) (string, error) {
	if to, ok := table[from][command]; ok {
		return to, nil
	}
	allowed := make([]TransitionAlternative, 0, len(table[from]))
	for legalCommand, to := range table[from] {
		allowed = append(allowed, TransitionAlternative{Command: legalCommand, To: to})
	}
	sort.Slice(allowed, func(i, j int) bool { return allowed[i].Command < allowed[j].Command })
	return "", &ErrInvalidTransition{Space: space, From: from, Command: command, Allowed: allowed}
}

func alternatives(table lifecycleTable, from string) []TransitionAlternative {
	result := make([]TransitionAlternative, 0, len(table[from]))
	for command, to := range table[from] {
		result = append(result, TransitionAlternative{Command: command, To: to})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Command < result[j].Command })
	return result
}

func TransitionTask(from TaskState, command TaskCommand) (TaskState, error) {
	to, err := transition(TaskLifecycle, taskLifecycleTable, string(from), string(command))
	return TaskState(to), err
}

func TransitionWorkOrder(from WorkOrderState, command WorkOrderCommand) (WorkOrderState, error) {
	to, err := transition(WorkOrderLifecycle, workOrderLifecycleTable, string(from), string(command))
	return WorkOrderState(to), err
}

func TaskTransitionAlternatives(from TaskState) []TransitionAlternative {
	return alternatives(taskLifecycleTable, string(from))
}

func WorkOrderTransitionAlternatives(from WorkOrderState) []TransitionAlternative {
	return alternatives(workOrderLifecycleTable, string(from))
}

func TransitionJob(from JobState, command string) (JobState, error) {
	to, err := transition(JobLifecycle, jobLifecycleTable, string(from), command)
	return JobState(to), err
}

func TransitionGitHubPublication(from GitHubPublicationState, command string) (GitHubPublicationState, error) {
	table := publicationLifecycleTable(string(GitHubPublicationQueued), string(GitHubPublicationRetrying), string(GitHubPublicationPublished), string(GitHubPublicationFailed))
	to, err := transition(GitHubPublicationLifecycle, table, string(from), command)
	return GitHubPublicationState(to), err
}

func TransitionReviewPublication(from ReviewPublicationState, command string) (ReviewPublicationState, error) {
	table := publicationLifecycleTable(string(ReviewPublicationQueued), string(ReviewPublicationRetrying), string(ReviewPublicationPublished), string(ReviewPublicationFailed))
	to, err := transition(ReviewPublicationLifecycle, table, string(from), command)
	return ReviewPublicationState(to), err
}

func TaskStates() []TaskState {
	return []TaskState{TaskClaiming, TaskQueued, TaskRunning, TaskAwaiting, TaskApproved, TaskMerged, TaskClosed, TaskParked}
}

func WorkOrderStates() []WorkOrderState {
	return []WorkOrderState{WorkOrderQueued, WorkOrderClaimed, WorkOrderSubmitted, WorkOrderCompleted, WorkOrderCancelled, WorkOrderStale, WorkOrderTimedOut}
}

// LifecycleStateDiagram generates the requirements UI diagram directly from
// the canonical transition tables, so documentation cannot drift from the
// machine that admits lifecycle commands (spec §§3.3-3.4, §21.38).
func LifecycleStateDiagram() string {
	var out strings.Builder
	out.WriteString("stateDiagram-v2\n")
	writeLifecycleDiagram(&out, "Task", "task", taskLifecycleTable)
	writeLifecycleDiagram(&out, "Work order", "order", workOrderLifecycleTable)
	return out.String()
}

func writeLifecycleDiagram(out *strings.Builder, label, prefix string, table lifecycleTable) {
	fmt.Fprintf(out, "  state %q as %s_lifecycle {\n", label, prefix)
	states := make([]string, 0, len(table))
	for from := range table {
		states = append(states, from)
	}
	sort.Strings(states)
	for _, from := range states {
		commands := make([]string, 0, len(table[from]))
		for command := range table[from] {
			commands = append(commands, command)
		}
		sort.Strings(commands)
		for _, command := range commands {
			to := table[from][command]
			source := prefix + "_" + strings.ReplaceAll(from, "-", "_")
			if from == "" {
				source = "[*]"
			}
			target := prefix + "_" + strings.ReplaceAll(to, "-", "_")
			fmt.Fprintf(out, "    %s --> %s: %s\n", source, target, command)
		}
	}
	out.WriteString("  }\n")
}
