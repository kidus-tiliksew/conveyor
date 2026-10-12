package store

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

// Task flow stage statuses (feature-operator-next-action NA-7).
const (
	FlowDone          = "done"
	FlowInProgress    = "in_progress"
	FlowWaiting       = "waiting"
	FlowNeedsOperator = "needs_operator"
	FlowOff           = "off"
	FlowUpcoming      = "upcoming"
)

// flowStageNames, flowStageDescriptions and flowStatusCopy are the task flow's
// server-stated copy (NA-7): the client prints these sentences and carries none.
var flowStageNames = map[core.Stage]string{
	core.StageTriage:    "Triage",
	core.StageSpec:      "Plan",
	core.StageImplement: "Implement",
	core.StageVerify:    "Verify",
	core.StageReview:    "Code review",
	core.StageMerge:     "Merge",
}

var flowStageDescriptions = map[core.Stage]string{
	core.StageTriage:    "Triage reads the task and writes the brief the later stages work from.",
	core.StageSpec:      "An agent writes the execution plan, which the operator approves when the plan gate is on.",
	core.StageImplement: "An agent makes the change on the task branch and opens the pull request.",
	core.StageVerify:    "A verifier runs the task's verification kit against the change and records the evidence.",
	core.StageReview:    "Independent reviewers judge the pull request and approve it or send it back.",
	core.StageMerge:     "The pull request merges into the base branch, after the operator approves when the merge gate is on.",
}

type flowStatusText struct {
	label    string
	sentence func(name string) string
}

var flowStatusCopy = map[string]flowStatusText{
	FlowDone:          {"Done", func(name string) string { return name + " is done." }},
	FlowInProgress:    {"In progress", func(name string) string { return name + " is running now." }},
	FlowWaiting:       {"Waiting", func(name string) string { return name + " is queued, waiting for a worker to claim it." }},
	FlowNeedsOperator: {"Needs you", func(name string) string { return name + " is waiting on you; the next action says what to decide." }},
	FlowOff:           {"Off for this task", func(name string) string { return "This task's policy turned " + name + " off." }},
	FlowUpcoming:      {"Upcoming", func(name string) string { return name + " has not started yet." }},
}

// flowGateWaitText names the gate when the operator's decision is the gate
// the stage carries.
var flowGateWaitText = map[core.GateKind]string{
	core.GateKindPlanApproval:  "Plan is waiting for you to approve the plan.",
	core.GateKindMergeApproval: "Merge is waiting for you to approve the merge.",
}

// flowMissedCheckInLayout is the absolute UTC time in the missed check-in
// sentence, so the sentence does not go stale between reads.
const flowMissedCheckInLayout = "2006-01-02 15:04 UTC"

// TaskFlow is the task's position on its stage path, folded from its events.
// NextAction is the NA-3 statement embedded unchanged.
type TaskFlow struct {
	Stages     []FlowStage           `json:"stages"`
	Loops      []FlowLoop            `json:"loops"`
	Side       FlowSide              `json:"side"`
	NextAction nextaction.NextAction `json:"next_action"`
	Raw        FlowRaw               `json:"raw"`
}

// FlowStage is one main stage. Gate is null when the stage has no gate or the
// frozen policy turned that gate off for this task. Description, StatusLabel,
// StatusText and MissedCheckIn are server-stated copy; MissedCheckIn is empty
// unless the current attempt's lease is past due.
type FlowStage struct {
	ID             core.Stage              `json:"id"`
	Gate           *string                 `json:"gate"`
	Status         string                  `json:"status"`
	Description    string                  `json:"description"`
	StatusLabel    string                  `json:"status_label"`
	StatusText     string                  `json:"status_text"`
	MissedCheckIn  string                  `json:"missed_check_in"`
	Attempts       []FlowAttempt           `json:"attempts"`
	CurrentAttempt *nextaction.AttemptView `json:"current_attempt"`
}

// FlowAttempt is one queue episode of one work order.
type FlowAttempt struct {
	OrderID    string     `json:"order_id"`
	AttemptID  string     `json:"attempt_id"`
	StartedAt  *time.Time `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	Outcome    string     `json:"outcome"`
	Superseded bool       `json:"superseded"`
	Auto       bool       `json:"auto"`
}

type FlowLoop struct {
	From  core.Stage `json:"from"`
	To    core.Stage `json:"to"`
	Count int        `json:"count"`
}

type FlowSide struct {
	BlockedBy     []nextaction.BlockerRef `json:"blocked_by"`
	Retrying      *FlowRetrying           `json:"retrying"`
	Parked        bool                    `json:"parked"`
	Closed        bool                    `json:"closed"`
	AgentQuestion bool                    `json:"agent_question"`
}

type FlowRetrying struct {
	Attempt int `json:"attempt"`
	Of      int `json:"of"`
}

type FlowRaw struct {
	State         string   `json:"state"`
	NextStage     string   `json:"next_stage"`
	RecoveryStage string   `json:"recovery_stage"`
	GateKind      string   `json:"gate_kind"`
	OrderIDs      []string `json:"order_ids"`
}

var taskFlowStages = []core.Stage{core.StageTriage, core.StageSpec, core.StageImplement, core.StageVerify, core.StageReview, core.StageMerge}

// taskFlowOpening events start a queue episode for their order.
var taskFlowOpening = map[string]bool{
	"work_order.created":      true,
	"work_order.recovered":    true,
	"work_order.redispatched": true,
	"work_order.requeued":     true,
}

// taskFlowClosing events end an order's queue episode; the outcome is the
// kind's suffix.
var taskFlowClosing = map[string]bool{
	"work_order.released":     true,
	"work_order.child_failed": true,
	"work_order.stalled":      true,
	"work_order.expired":      true,
	"work_order.timed_out":    true,
	"work_order.stale":        true,
	"work_order.cancelled":    true,
	"work_order.retired":      true,
	"work_order.preempted":    true,
}

type taskFlowOrder struct {
	id             string
	stage          core.Stage
	state          core.WorkOrderState
	entries        []FlowAttempt
	open           bool
	systemOpened   bool
	retired        bool
	claim          *nextaction.AttemptView
	recoveryNumber int
	recoveryWindow string
	retryCount     int
}

type taskFlowBounce struct {
	from, to core.Stage
	at       time.Time
}

// taskFlowOrderPayload covers both full core.WorkOrder payloads and the
// smaller maps lifecycle events write.
type taskFlowOrderPayload struct {
	ID                  string     `json:"id"`
	WorkOrderID         string     `json:"work_order_id"`
	Stage               core.Stage `json:"stage"`
	AttemptID           string     `json:"attempt_id"`
	ClaimedBy           string     `json:"claimed_by"`
	SessionID           string     `json:"session_id"`
	Agent               string     `json:"agent"`
	Model               string     `json:"model"`
	Progress            string     `json:"progress"`
	Message             string     `json:"message"`
	ExecutionStartedAt  *time.Time `json:"execution_started_at"`
	LeaseExpiresAt      *time.Time `json:"lease_expires_at"`
	LastAgentActivityAt *time.Time `json:"last_agent_activity_at"`
	AutomaticRetryCount *int       `json:"automatic_retry_count"`
	Automatic           bool       `json:"automatic"`
	RecoveryNumber      int        `json:"recovery_number"`
	WindowStartedAt     string     `json:"window_started_at"`
	// Successor names the order whose creation retired this one.
	Successor string `json:"authoritative_work_order_id"`
}

// FoldTaskFlow folds one task's events into its stage flow. It reads no store,
// task row, work-order row, job, intervention or clock, so replaying the same
// history with the same now reproduces it (req-operator-next-action AC-4.3).
// action is the task's NA-3 next action: needs_operator and the side states
// NA-3 owns come from it, so the flow never derives a second actor or kind
// (DEC-61). recovery supplies the automatic-recovery bound shown as
// retrying.of. now is the instant the next action was projected at; it alone
// decides whether a current attempt's lease is past due.
func FoldTaskFlow(events []core.Event, action nextaction.NextAction, recovery config.RecoveryPolicy, now time.Time) TaskFlow {
	ordered := append([]core.Event(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].At.Equal(ordered[j].At) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].At.Before(ordered[j].At)
	})

	specApproval, mergeApproval, verifyStage := true, true, true
	var (
		state, nextStage, recoveryStage, assignee string
		gateKind                                  core.GateKind
		lastClosedStage                           core.Stage
		windowStart                               time.Time
		bounces                                   []taskFlowBounce
		dependencies                              []string
		orderIDs                                  []string
	)
	visited := map[core.Stage]bool{}
	orders := map[string]*taskFlowOrder{}
	jobOrders := map[string]string{}
	jobStages := map[string]core.Stage{}
	jobStates := map[string]core.JobState{}
	// closedInPass counts episodes closed per stage since the latest
	// transition into that stage; a later system-opened episode there is a
	// replacement or retry (R2).
	closedInPass := map[core.Stage]int{}
	// replaced holds successors named by a retirement the ledger lists before
	// the successor's creation; backends order those same-transaction events
	// either way.
	replaced := map[string]bool{}

	closeEntry := func(order *taskFlowOrder, event core.Event, outcome string) {
		if !order.open {
			return
		}
		last := &order.entries[len(order.entries)-1]
		last.EndedAt, last.Outcome = flowTime(event.At), outcome
		order.open = false
		order.claim = nil
		closedInPass[order.stage]++
		lastClosedStage = order.stage
	}
	openEntry := func(order *taskFlowOrder, event core.Event, automatic bool) {
		closeEntry(order, event, strings.TrimPrefix(event.Kind, "work_order."))
		auto := event.ActorRole == core.ActorSystem && (automatic || closedInPass[order.stage] > 0 || replaced[order.id])
		order.entries = append(order.entries, FlowAttempt{OrderID: order.id, Auto: auto})
		order.open, order.systemOpened = true, event.ActorRole == core.ActorSystem
	}

	for _, event := range ordered {
		switch {
		case event.Kind == "task.created":
			var task core.Task
			if json.Unmarshal(event.Payload, &task) != nil {
				continue
			}
			specApproval, mergeApproval, verifyStage = task.SpecApproval, task.MergeApproval, task.SetupContract.VerifyStage
			if state == "" {
				state = string(task.State)
			}
			if nextStage == "" {
				nextStage = string(task.NextStage)
			}
			if task.Assignee != nil {
				assignee = task.Assignee.UserID
			}
		case event.Kind == "task.setup.changed":
			var payload struct {
				NewSetup *struct {
					VerifyStage bool `json:"verify_stage"`
				} `json:"new_setup"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.NewSetup != nil {
				verifyStage = payload.NewSetup.VerifyStage
			}
		case event.Kind == "task.assignee.set" || event.Kind == "task.assignee.cleared":
			var payload struct {
				UserID string `json:"assignee_user_id"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				assignee = payload.UserID
			}
		case event.Kind == "task.state_changed":
			var payload struct {
				To      core.TaskState   `json:"to"`
				Command core.TaskCommand `json:"command"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || payload.To == "" {
				continue
			}
			state = string(payload.To)
			gateKind = core.GateKindFor(payload.To, payload.Command)
		case event.Kind == "pipeline.transition_decided":
			var payload struct {
				FromStage     core.Stage `json:"from_stage"`
				NextStage     core.Stage `json:"next_stage"`
				RecoveryStage core.Stage `json:"recovery_stage"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			if payload.FromStage != "" {
				visited[payload.FromStage] = true
			}
			nextStage, recoveryStage = string(payload.NextStage), string(payload.RecoveryStage)
			delete(closedInPass, payload.NextStage)
		case event.Kind == "pipeline.bounced":
			var payload struct {
				From core.Stage `json:"from"`
				To   core.Stage `json:"to"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.From != "" && payload.To != "" {
				bounces = append(bounces, taskFlowBounce{from: payload.From, to: payload.To, at: event.At})
			}
		case strings.HasPrefix(event.Kind, "intervention."):
			if event.ActorRole == core.ActorHuman && event.At.After(windowStart) {
				windowStart = event.At
			}
		case event.Kind == "task.dependency_added" || event.Kind == "task.dependency_removed":
			var payload struct {
				DependsOn string `json:"depends_on_task_id"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || payload.DependsOn == "" {
				continue
			}
			dependencies = withoutString(dependencies, payload.DependsOn)
			if event.Kind == "task.dependency_added" {
				dependencies = append(dependencies, payload.DependsOn)
			}
		case event.Kind == "job.created" || event.Kind == "job.updated":
			var job core.Job
			if json.Unmarshal(event.Payload, &job) != nil {
				continue
			}
			id := job.ID
			if id == "" {
				id = event.JobID
			}
			if id == "" {
				continue
			}
			if job.Stage != "" {
				jobStages[id] = job.Stage
			}
			if job.State != "" {
				jobStates[id] = job.State
			}
			if event.Kind == "job.created" && job.Stage != "" {
				visited[job.Stage] = true
			}
		case strings.HasPrefix(event.Kind, "work_order."):
			var payload taskFlowOrderPayload
			if json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			id := payload.WorkOrderID
			if id == "" && payload.Stage != "" {
				id = payload.ID
			}
			if id == "" {
				id = jobOrders[event.JobID]
			}
			if id == "" {
				continue
			}
			order := orders[id]
			if order == nil {
				if event.Kind != "work_order.created" {
					continue
				}
				order = &taskFlowOrder{id: id, stage: payload.Stage}
				orders[id] = order
				orderIDs = append(orderIDs, id)
				if order.stage != "" {
					visited[order.stage] = true
				}
			}
			if event.JobID != "" && event.Kind == "work_order.created" {
				jobOrders[event.JobID] = id
			}
			if payload.AutomaticRetryCount != nil {
				order.retryCount = *payload.AutomaticRetryCount
			}
			if taskFlowOpening[event.Kind] {
				openEntry(order, event, payload.Automatic)
				switch {
				case payload.Automatic && payload.RecoveryNumber > 0 && event.Kind != "work_order.created":
					order.recoveryNumber, order.recoveryWindow = payload.RecoveryNumber, payload.WindowStartedAt
				case !payload.Automatic:
					// An operator recovery or redispatch resets the ledger window.
					order.recoveryNumber, order.recoveryWindow = 0, ""
				}
			}
			switch event.Kind {
			case "work_order.claimed":
				if !order.open || order.entries[len(order.entries)-1].AttemptID != "" {
					openEntry(order, event, false)
				}
				claimedAt := payload.ExecutionStartedAt
				if claimedAt == nil {
					claimedAt = flowTime(event.At)
				}
				entry := &order.entries[len(order.entries)-1]
				entry.AttemptID, entry.StartedAt = payload.AttemptID, claimedAt
				lastCheckIn := payload.LastAgentActivityAt
				if lastCheckIn == nil {
					lastCheckIn = flowTime(event.At)
				}
				order.claim = &nextaction.AttemptView{
					Harness: payload.Agent, Model: payload.Model, ClaimedBy: payload.ClaimedBy,
					SessionID: payload.SessionID, AttemptID: payload.AttemptID, ClaimedAt: claimedAt,
					LastCheckIn: lastCheckIn, LeaseExpiresAt: payload.LeaseExpiresAt, Progress: payload.Progress,
				}
			case "work_order.lease_renewed", "work_order.progress_reported", "work_order.attempt_checkpointed":
				if order.claim == nil || (payload.AttemptID != "" && payload.AttemptID != order.claim.AttemptID) {
					break
				}
				order.claim.LastCheckIn = flowTime(event.At)
				if payload.LastAgentActivityAt != nil {
					order.claim.LastCheckIn = payload.LastAgentActivityAt
				}
				if payload.LeaseExpiresAt != nil {
					order.claim.LeaseExpiresAt = payload.LeaseExpiresAt
				}
				if event.Kind == "work_order.progress_reported" && payload.Message != "" {
					order.claim.Progress = payload.Message
				} else if payload.Progress != "" {
					order.claim.Progress = payload.Progress
				}
			}
			if event.Kind == "work_order.retired" {
				order.retired = true
				// The successor that retired this order replaces it (R2).
				if successor := orders[payload.Successor]; successor == nil {
					replaced[payload.Successor] = payload.Successor != ""
				} else if successor.open && successor.systemOpened {
					successor.entries[len(successor.entries)-1].Auto = true
				}
			}
			if taskFlowClosing[event.Kind] {
				closeEntry(order, event, strings.TrimPrefix(event.Kind, "work_order."))
			}
			to := workOrderStateFromEvent(event)
			if command, relevant := workOrderEventCommand(event, order.state); relevant && command != "" {
				if expected, err := core.TransitionWorkOrder(order.state, command); err == nil {
					to = expected
				}
			}
			if to != "" {
				order.state = to
			}
			if order.state != "" && order.state != core.WorkOrderQueued && order.state != core.WorkOrderClaimed {
				closeEntry(order, event, string(order.state))
			}
		}
	}

	flow := TaskFlow{
		Stages:     make([]FlowStage, 0, len(taskFlowStages)),
		Loops:      []FlowLoop{},
		Side:       FlowSide{BlockedBy: []nextaction.BlockerRef{}},
		NextAction: action,
		Raw: FlowRaw{
			State: state, NextStage: nextStage, RecoveryStage: recoveryStage, GateKind: string(gateKind),
			OrderIDs: append([]string{}, orderIDs...),
		},
	}

	for _, bounce := range bounces {
		if !bounce.at.After(windowStart) {
			continue
		}
		counted := false
		for i := range flow.Loops {
			if flow.Loops[i].From == bounce.from && flow.Loops[i].To == bounce.to {
				flow.Loops[i].Count++
				counted = true
				break
			}
		}
		if !counted {
			flow.Loops = append(flow.Loops, FlowLoop{From: bounce.from, To: bounce.to, Count: 1})
		}
	}

	current := core.Stage(nextStage)
	switch gateKind {
	case core.GateKindPlanApproval, core.GateKindPlanRevision:
		current = core.StageSpec
	case core.GateKindMergeApproval:
		current = core.StageMerge
	case core.GateKindReviewBounceLimit:
		current = core.StageReview
	case core.GateKindJobFailure:
		current = lastClosedStage
	}

	for _, stage := range taskFlowStages {
		row := FlowStage{ID: stage, Attempts: []FlowAttempt{}}
		switch {
		case stage == core.StageSpec && specApproval:
			row.Gate = flowGate(core.GateKindPlanApproval)
		case stage == core.StageMerge && mergeApproval:
			row.Gate = flowGate(core.GateKindMergeApproval)
		}
		var latest *taskFlowOrder
		for _, id := range orderIDs {
			order := orders[id]
			if order.stage != stage {
				continue
			}
			latest = order
			for _, entry := range order.entries {
				entry.Superseded = order.retired
				row.Attempts = append(row.Attempts, entry)
			}
		}
		claimed := latest != nil && latest.state == core.WorkOrderClaimed
		if claimed && latest.claim != nil {
			view := *latest.claim
			view.User = assignee
			row.CurrentAttempt = &view
		}
		jobRunning := false
		for id, jobStage := range jobStages {
			if jobStage == stage && jobOrders[id] == "" && jobStates[id] == core.JobRunning {
				jobRunning = true
			}
		}
		switch {
		case stage == core.StageSpec && !specApproval, stage == core.StageVerify && !verifyStage:
			row.Status = FlowOff
		case stage == current && action.Actor == nextaction.ActorOperator:
			row.Status = FlowNeedsOperator
		case claimed || jobRunning || (stage == core.StageMerge && state == string(core.TaskApproved) && !mergeApproval):
			row.Status = FlowInProgress
		case latest != nil && latest.state == core.WorkOrderQueued:
			row.Status = FlowWaiting
		case (visited[stage] && stage != current) || (state == string(core.TaskMerged) && (visited[stage] || stage == core.StageMerge)):
			row.Status = FlowDone
		default:
			row.Status = FlowUpcoming
		}
		name := flowStageNames[stage]
		statusCopy := flowStatusCopy[row.Status]
		row.Description = flowStageDescriptions[stage]
		row.StatusLabel = statusCopy.label
		row.StatusText = statusCopy.sentence(name)
		if text, ok := flowGateWaitText[gateKind]; ok && row.Status == FlowNeedsOperator {
			row.StatusText = text
		}
		if attempt := row.CurrentAttempt; attempt != nil && attempt.LeaseExpiresAt != nil && now.After(*attempt.LeaseExpiresAt) {
			row.MissedCheckIn = "Missed check-in: the lease expired at " + attempt.LeaseExpiresAt.UTC().Format(flowMissedCheckInLayout) + " and the agent has not renewed it."
		}
		flow.Stages = append(flow.Stages, row)
	}

	flow.Side.Parked = state == string(core.TaskParked)
	flow.Side.Closed = state == string(core.TaskClosed)
	flow.Side.AgentQuestion = action.Kind == nextaction.KindAgentQuestion
	if action.Kind == nextaction.KindRetrying {
		retrying := &FlowRetrying{Of: recovery.AutomaticRecoveries}
		if action.CurrentOrder != nil {
			if order := orders[action.CurrentOrder.ID]; order != nil {
				retrying.Attempt = order.recoveryNumber
				if action.Details.ReasonCode == "automatic_recovery" {
					retrying.Attempt = scheduledRecoveryAttempt(order, action.Deadline, recovery.Window)
				} else if retrying.Attempt == 0 {
					retrying.Attempt = order.retryCount
				}
			}
		}
		flow.Side.Retrying = retrying
	}
	for _, id := range dependencies {
		for _, blocker := range action.BlockedBy {
			if blocker.TaskID == id {
				flow.Side.BlockedBy = append(flow.Side.BlockedBy, blocker)
				break
			}
		}
	}
	return flow
}

func flowGate(kind core.GateKind) *string {
	gate := string(kind)
	return &gate
}

func flowTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	at = at.UTC()
	return &at
}

// scheduledRecoveryAttempt is the window-local number the scheduled automatic
// recovery will stamp: one past the order's last automatic recovery, or the
// first of a new window when the window has rolled by the scheduled time, as
// store.StampAutomaticRecovery counts it.
func scheduledRecoveryAttempt(order *taskFlowOrder, at *time.Time, window time.Duration) int {
	if order.recoveryNumber == 0 {
		return 1
	}
	started, err := time.Parse(time.RFC3339Nano, order.recoveryWindow)
	if at != nil && window > 0 && err == nil && !at.Before(started.Add(window)) {
		return 1
	}
	return order.recoveryNumber + 1
}

func withoutString(values []string, value string) []string {
	out := values[:0]
	for _, candidate := range values {
		if candidate != value {
			out = append(out, candidate)
		}
	}
	return out
}
