package nextaction

import (
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func reviewChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "approve", Label: "Approve", Effect: "Accepts the gate.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
		{ID: "redirect", Label: "Redirect", Effect: "Sends the task back with guidance.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
		{ID: "reject", Label: "Reject", Effect: "Closes the task.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
	}
}

func revisionChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "approve_revision", Label: "Approve revision", Effect: "Accepts the revised plan.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
		{ID: "decline", Label: "Decline", Effect: "Sends the revision back with direction.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
		{ID: "reject", Label: "Reject", Effect: "Closes the task.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
	}
}

func mergeApprovalChoices() []ChoiceSpec {
	return []ChoiceSpec{
		// The one-act merge is offered only at readiness MERGEABLE (DEC-64(1,3);
		// feature-operator-next-action v6).
		{ID: "merge", Label: "Merge", Effect: "Approves and merges the reviewed head.", Method: "POST", Path: "/v1/tasks/{id}/merge", Capability: core.CapabilityOperateGates},
		{ID: "approve", Label: "Approve only", Effect: "Approves the merge.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
		{ID: "request_changes", Label: "Request changes", Effect: "Sends the branch back.", Method: "POST", Path: "/v1/tasks/{id}/request-changes", Capability: core.CapabilityRequestChanges},
	}
}

func mergeExecutionChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "merge", Label: "Merge", Effect: "Merges the approved head.", Method: "POST", Path: "/v1/tasks/{id}/merge", Capability: core.CapabilityOperateGates},
		{ID: "request_changes", Label: "Request changes", Effect: "Sends the branch back.", Method: "POST", Path: "/v1/tasks/{id}/request-changes", Capability: core.CapabilityRequestChanges},
		{ID: "close", Label: "Close", Effect: "Closes the task.", Method: "POST", Path: "/v1/tasks/{id}/close", Capability: core.CapabilityOperateGates},
	}
}

func conflictChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "fix_merge_conflict", Label: "Fix merge conflict", Effect: "Dispatches a conflict-fix order.", Method: "POST", Path: "/v1/tasks/{id}/merge-conflict-fix", Capability: core.CapabilityRecoverWork},
	}
}

func answerChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "answer", Label: "Answer", Effect: "Recovers the order with direction.", Method: "POST", Path: "/v1/work-orders/{order_id}/recover", Capability: core.CapabilityRecoverWork},
	}
}

func dispositionChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "applied", Label: "Applied", Effect: "Records the change as applied.", Method: "POST", Path: "/v1/work-orders/{order_id}/recover", Capability: core.CapabilityRecoverWork},
		{ID: "not_applied", Label: "Not applied", Effect: "Records the change as not applied.", Method: "POST", Path: "/v1/work-orders/{order_id}/recover", Capability: core.CapabilityRecoverWork},
	}
}

func proposalChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "confirm", Label: "Confirm", Effect: "Confirms the proposal.", Method: "POST", Path: "/v1/documents/{id}/confirm", Capability: core.CapabilityConfirmDocuments},
		{ID: "dismiss", Label: "Dismiss", Effect: "Dismisses the proposal.", Method: "POST", Path: "/v1/documents/{id}/dismiss", Capability: core.CapabilityConfirmDocuments},
	}
}

func parkedChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "recover", Label: "Recover", Effect: "Puts the task back in the queue.", Method: "POST", Path: "/v1/tasks/{id}/redispatch", Capability: core.CapabilityRecoverWork},
		{ID: "close", Label: "Close", Effect: "Closes the task.", Method: "POST", Path: "/v1/tasks/{id}/close", Capability: core.CapabilityOperateGates},
		{ID: "start_over", Label: "Start over", Effect: "Closes the task so it can be refiled.", Method: "POST", Path: "/v1/tasks/{id}/restart", Capability: core.CapabilityOperateGates},
	}
}

func stalledChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "recover", Label: "Recover", Effect: "Recovers the order.", Method: "POST", Path: "/v1/work-orders/{order_id}/recover", Capability: core.CapabilityRecoverWork},
		{ID: "redispatch", Label: "Redispatch", Effect: "Puts the task back in the queue.", Method: "POST", Path: "/v1/tasks/{id}/redispatch", Capability: core.CapabilityRecoverWork},
		{ID: "start_over", Label: "Start over", Effect: "Closes the task so it can be refiled.", Method: "POST", Path: "/v1/tasks/{id}/restart", Capability: core.CapabilityOperateGates},
		{ID: "change_policy", Label: "Change policy", Effect: "Changes the frozen policy that blocks recovery.", Method: "POST", Path: "/v1/tasks/{id}/policy", Capability: core.CapabilityOperateGates},
	}
}

func roundChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "retry_round", Label: "Retry round", Effect: "Retries the review round.", Method: "POST", Path: "/v1/tasks/{id}/review-round/retry", Capability: core.CapabilityOperateGates},
		{ID: "recover_round", Label: "Recover round", Effect: "Recovers the interrupted review round.", Method: "POST", Path: "/v1/tasks/{id}/review-round/recover", Capability: core.CapabilityRecoverWork},
	}
}

func dependencyChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "remove_dependency", Label: "Remove dependency", Effect: "Removes the blocking dependency.", Method: "DELETE", Path: "/v1/tasks/{id}/dependencies/{dependency_id}", Capability: core.CapabilityOperateGates},
		{ID: "close", Label: "Close", Effect: "Closes the task.", Method: "POST", Path: "/v1/tasks/{id}/close", Capability: core.CapabilityOperateGates},
	}
}

func selectChoices(in Input, specs []ChoiceSpec, order *core.WorkOrder) ([]Choice, []Refusal) {
	var choices []Choice
	var refusals []Refusal
	for _, spec := range specs {
		path := spec.Path
		if order != nil {
			path = replaceOrder(path, order.ID)
		}
		path = replaceTask(path, in.Task.ID)
		refusal := refuseChoice(in, spec.ID, order)
		if refusal != "" {
			refusals = append(refusals, Refusal{Choice: spec.ID, Text: refusal})
			continue
		}
		choices = append(choices, Choice{
			ID: spec.ID, Label: spec.Label, Effect: spec.Effect, Method: spec.Method, Path: path,
			Capability: string(spec.Capability), Allowed: in.Capabilities[spec.Capability],
		})
	}
	if choices == nil {
		choices = []Choice{}
	}
	return choices, refusals
}

func refuseChoice(in Input, id string, order *core.WorkOrder) string {
	switch id {
	case "recover", "answer", "applied", "not_applied":
		if order == nil {
			return "no order to recover"
		}
		return core.NextActionRecoverRefusal(*order)
	case "approve":
		if in.Task.GateKind == core.GateKindPlanRevision {
			return core.NextActionPlainApproveRefusal(in.Task.GateKind)
		}
		return core.NextActionSupersessionRefusal(in.GateSpecVersion, latestVersion(in))
	case "merge":
		return mergeAtGateRefusal(in)
	case "approve_revision", "decline":
		return core.NextActionSupersessionRefusal(in.GateSpecVersion, latestVersion(in))
	case "redispatch":
		if order == nil {
			return ""
		}
		return core.NextActionRedispatchRefusal(*order)
	case "change_policy":
		if order == nil || order.RecoveryRefusal == "" {
			return core.NextActionChangePolicyRefusal("")
		}
		return ""
	case "remove_dependency":
		if len(in.Blockers) == 0 && (order == nil || len(order.UnsatisfiableTaskIDs) == 0) {
			return "no dependency to remove"
		}
		return ""
	case "fix_merge_conflict":
		if !in.MergeConflict.Open || !in.MergeConflict.Exhausted {
			return "conflict fix is refused before automatic fixes are exhausted"
		}
		return ""
	case "retry_round":
		if !in.Markers.ReviewRecovery && !in.Markers.ForgeFailure {
			return "review-round retry is refused without a recovery marker"
		}
		return ""
	case "recover_round":
		if !in.Markers.InterruptedReview && !in.Markers.ReviewRecovery {
			return "review-round recover is refused without an interrupted round"
		}
		return ""
	default:
		return ""
	}
}

func latestVersion(in Input) int {
	if in.LatestSpec == nil {
		return 0
	}
	return in.LatestSpec.Version
}

func replaceTask(path, id string) string {
	return replace(path, "{id}", id)
}

func replaceOrder(path, id string) string {
	return replace(path, "{order_id}", id)
}

func replace(path, token, value string) string {
	out := ""
	for {
		i := index(path, token)
		if i < 0 {
			return out + path
		}
		out += path[:i] + value
		path = path[i+len(token):]
	}
}

func index(s, token string) int {
	if len(token) == 0 || len(s) < len(token) {
		return -1
	}
	for i := 0; i+len(token) <= len(s); i++ {
		if s[i:i+len(token)] == token {
			return i
		}
	}
	return -1
}

func choiceSummary(id string) string {
	return fmt.Sprintf("choice %s", id)
}

// mergeAtGateRefusal refuses the merge_approval gate's one-act merge unless
// the last-known readiness peek is MERGEABLE; an unread peek is UNKNOWN
// (DEC-64(3); DEC-61). The approved-task merge choice is never refused here.
func mergeAtGateRefusal(in Input) string {
	if in.Task.State != core.TaskAwaiting || in.Task.GateKind != core.GateKindMergeApproval {
		return ""
	}
	state := "UNKNOWN"
	if in.MergeReadiness != nil && in.MergeReadiness.State != "" {
		state = in.MergeReadiness.State
	}
	if state == "MERGEABLE" {
		return ""
	}
	return "merge readiness " + state
}
