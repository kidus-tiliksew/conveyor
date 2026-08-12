package store

import (
	"encoding/json"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func AtMergeGate(task core.Task, events []core.Event) bool {
	if task.State != core.TaskAwaiting || task.RecoveryStage != core.StageImplement {
		return false
	}
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Kind != "task.state_changed" {
			continue
		}
		var payload struct {
			Command core.TaskCommand `json:"command"`
		}
		return json.Unmarshal(events[index].Payload, &payload) == nil && payload.Command == core.TaskGateMerge
	}
	return false
}

func UserRequestChangesPending(events []core.Event) bool {
	pending := false
	for _, event := range events {
		switch event.Kind {
		case "pipeline.bounced":
			var payload struct {
				Source string `json:"source"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Source == UserRequestChangesSource {
				pending = true
			}
		case "work_order.claimed":
			var order core.WorkOrder
			if json.Unmarshal(event.Payload, &order) == nil && core.IsTaskRunClaimantID(order.ClaimantID) {
				pending = false
			}
		}
	}
	return pending
}

const (
	UserRequestChangesReason = "user-request-changes"
	UserRequestChangesSource = "user-request-changes"
)

// ChangesRequestedInput is the backend-neutral bounce contract shared by
// review-panel verdict aggregation and the merge-gate user command. Backends
// persist this plan inside their own serialized transaction.
type ChangesRequestedInput struct {
	TaskID       string
	JobID        string
	ActorID      string
	ActorRole    core.ActorRole
	ReasonCode   string
	Feedback     string
	Source       string
	Count        int
	Window       int
	MaxBounces   int
	ReviewRound  int
	Reviews      any
	Requeue      core.TaskCommand
	EnforceLimit bool
	At           time.Time
}

type ChangesRequestedPlan struct {
	Intervention core.Intervention
	Events       []core.Event
	Command      core.TaskCommand
	NextStage    core.Stage
	Recovery     core.Stage
}

func PlanChangesRequested(input ChangesRequestedInput) ChangesRequestedPlan {
	if input.At.IsZero() {
		input.At = time.Now().UTC()
	}
	payload := map[string]any{
		"from": "review", "to": "implement", "reason_code": input.ReasonCode,
		"feedback": input.Feedback, "count": input.Count, "source": input.Source,
	}
	redirectPayload := map[string]any{"reason_code": input.ReasonCode, "comment": input.Feedback}
	if input.ReviewRound > 0 {
		payload["review_round"] = input.ReviewRound
		redirectPayload["review_round"] = input.ReviewRound
	}
	if input.Reviews != nil {
		payload["reviews"] = input.Reviews
		redirectPayload["reviews"] = input.Reviews
	}
	plan := ChangesRequestedPlan{
		Intervention: core.Intervention{
			TaskID: input.TaskID, JobID: input.JobID, ActorID: input.ActorID,
			ActorRole: input.ActorRole, Action: core.InterventionRedirect,
			ReasonCode: input.ReasonCode, Comment: input.Feedback, At: input.At,
		},
		Events: []core.Event{
			{TaskID: input.TaskID, JobID: input.JobID, Kind: "intervention.redirect", ActorID: input.ActorID, ActorRole: input.ActorRole, Payload: core.JSONPayload(redirectPayload), At: input.At},
			{TaskID: input.TaskID, JobID: input.JobID, Kind: "pipeline.bounced", ActorID: input.ActorID, ActorRole: input.ActorRole, Payload: core.JSONPayload(payload), At: input.At},
		},
		Command: input.Requeue, NextStage: core.StageImplement,
	}
	if input.EnforceLimit && input.Window >= input.MaxBounces {
		plan.Command, plan.NextStage, plan.Recovery = core.TaskStageBounceLimit, "", core.StageImplement
		limitPayload := map[string]any{"count": input.Count, "window": input.Window, "max_bounces": input.MaxBounces}
		if input.ReviewRound > 0 {
			limitPayload["review_round"] = input.ReviewRound
		}
		plan.Events = append(plan.Events, core.Event{TaskID: input.TaskID, JobID: input.JobID, Kind: "pipeline.bounce_limit", ActorID: input.ActorID, ActorRole: input.ActorRole, Payload: core.JSONPayload(limitPayload), At: input.At})
	}
	return plan
}
