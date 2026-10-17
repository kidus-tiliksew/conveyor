package nextaction

import (
	"fmt"
	"net/url"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// reviewChoices are the review-route choices of plan_approval,
// review_bounce_limit, and job_failure. Only the approve label differs by
// kind; a label ends with an ellipsis exactly when its route requires typed
// input (feature-operator-next-action NA-4).
func reviewChoices(approveLabel string) []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "approve", Label: approveLabel, Effect: "Accepts the gate.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
		{ID: "redirect", Label: "Ask for changes…", Effect: "Sends the task back with guidance.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
		{ID: "reject", Label: "Reject", Effect: "Closes the task.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
	}
}

func revisionChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "approve_revision", Label: "Approve revision", Effect: "Accepts the revised plan.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
		{ID: "decline", Label: "Decline…", Effect: "Sends the revision back with direction.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
		{ID: "reject", Label: "Reject", Effect: "Closes the task.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
	}
}

func mergeApprovalChoices() []ChoiceSpec {
	return []ChoiceSpec{
		// The one-act merge is offered only at readiness MERGEABLE (DEC-64(1,3);
		// feature-operator-next-action v6).
		{ID: "merge", Label: "Merge", Effect: "Approves and merges the reviewed head.", Method: "POST", Path: "/v1/tasks/{id}/merge", Capability: core.CapabilityOperateGates},
		{ID: "approve", Label: "Approve only", Effect: "Approves the merge.", Method: "POST", Path: "/v1/tasks/{id}/review", Capability: core.CapabilityOperateGates},
		{ID: "request_changes", Label: "Request changes…", Effect: "Sends the branch back.", Method: "POST", Path: "/v1/tasks/{id}/request-changes", Capability: core.CapabilityRequestChanges},
	}
}

func mergeExecutionChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "merge", Label: "Merge", Effect: "Merges the approved head.", Method: "POST", Path: "/v1/tasks/{id}/merge", Capability: core.CapabilityOperateGates},
		{ID: "request_changes", Label: "Request changes…", Effect: "Sends the branch back.", Method: "POST", Path: "/v1/tasks/{id}/request-changes", Capability: core.CapabilityRequestChanges},
		{ID: "close", Label: "Close…", Effect: "Closes the task.", Method: "POST", Path: "/v1/tasks/{id}/close", Capability: core.CapabilityOperateGates},
	}
}

func conflictChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "fix_merge_conflict", Label: "Fix merge conflict", Effect: "Dispatches a conflict-fix order.", Method: "POST", Path: "/v1/tasks/{id}/merge-conflict-fix", Capability: core.CapabilityRecoverWork},
	}
}

func answerChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "answer", Label: "Answer…", Effect: "Recovers the order with direction.", Method: "POST", Path: "/v1/work-orders/{order_id}/recover", Capability: core.CapabilityRecoverWork},
	}
}

// dispositionChoices require operate_gates, the capability
// AuthorizeVerificationRecovery checks; each asks for the reason the
// recovery route requires (feature-operator-next-action NA-3, NA-4).
func dispositionChoices() []ChoiceSpec {
	return []ChoiceSpec{
		{ID: "applied", Label: "Yes, it applied…", Effect: "Records the change as applied.", Method: "POST", Path: "/v1/work-orders/{order_id}/recover", Capability: core.CapabilityOperateGates},
		{ID: "not_applied", Label: "No, it didn't…", Effect: "Records the change as not applied.", Method: "POST", Path: "/v1/work-orders/{order_id}/recover", Capability: core.CapabilityOperateGates},
	}
}

// Shared effect copy of the proposal choices, one pair per kind
// (feature-operator-next-action NA-4). Deciding the last withholding version
// releases a document_proposal wait by itself (req-260810-70ce2f AC-1.2), so
// only the agent-question pair names recovery.
const (
	withheldConfirmEffect = "Confirms this pending revision."
	withheldDismissEffect = "Dismisses this pending revision."
	citedConfirmEffect    = "Confirms this pending revision. The order stays released until you recover it."
	citedDismissEffect    = "Dismisses this pending revision. The order stays released until you recover it."
)

// withheldProposalChoices offers confirm and dismiss for each version that
// withholds the task's verify and review claims (row 8).
func withheldProposalChoices(refs []ProposalRef) []ChoiceSpec {
	return versionProposalChoices(refs, withheldConfirmEffect, withheldDismissEffect)
}

// citedProposalChoices offers confirm and dismiss for each cited pending
// version. They precede the answer choice and never recover the order
// (req-260820-394cac AC-2.3, AC-2.5).
func citedProposalChoices(refs []ProposalRef) []ChoiceSpec {
	return versionProposalChoices(refs, citedConfirmEffect, citedDismissEffect)
}

// versionProposalChoices builds one confirm and one dismiss choice per
// version on its version-specific requirement or System Design route, the
// only document routes the server serves (req-operator-next-action AC-1.4).
func versionProposalChoices(refs []ProposalRef, confirmEffect, dismissEffect string) []ChoiceSpec {
	out := make([]ChoiceSpec, 0, 2*len(refs))
	for _, ref := range refs {
		collection := "/v1/system-designs/"
		if ref.Tier == "requirement" {
			collection = "/v1/requirements/"
		}
		route := fmt.Sprintf("%s%s/versions/%d", collection, url.PathEscape(ref.ID), ref.Version)
		key := fmt.Sprintf("%s:%s:%d", ref.Tier, ref.ID, ref.Version)
		name := fmt.Sprintf("%s v%d", ref.ID, ref.Version)
		out = append(out,
			ChoiceSpec{ID: "confirm:" + key, Label: "Confirm " + name, Effect: confirmEffect, Method: "POST", Path: route + "/confirm", Capability: core.CapabilityConfirmDocuments},
			ChoiceSpec{ID: "dismiss:" + key, Label: "Dismiss " + name, Effect: dismissEffect, Method: "POST", Path: route + "/dismiss", Capability: core.CapabilityConfirmDocuments},
		)
	}
	return out
}

// Shared effect copy of the context-proposal choices (feature-operator-next-action NA-4).
const (
	contextConfirmEffect = "Attaches the proposed context to this task."
	contextDismissEffect = "Dismisses the proposed context."
)

// contextProposalEntries is the one canonical sequence of a task's pending
// task-context proposals: deduplicated on target kind and target ID alone,
// never on the metadata, and ordered by target kind and target ID. Choices
// and details both read it, so each detail entry matches its choice pair
// (feature-operator-next-action NA-3).
func contextProposalEntries(refs []ContextProposalRef) []ContextProposalRef {
	sorted := append([]ContextProposalRef(nil), refs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Kind != sorted[j].Kind {
			return sorted[i].Kind < sorted[j].Kind
		}
		return sorted[i].TargetID < sorted[j].TargetID
	})
	type identity struct{ kind, target string }
	out := make([]ContextProposalRef, 0, len(sorted))
	seen := map[identity]bool{}
	for _, ref := range sorted {
		key := identity{ref.Kind, ref.TargetID}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ref)
	}
	return out
}

// contextProposalChoices offers confirm and dismiss for each pending
// task-context proposal on the task-context routes, which require
// operate_gates, in the canonical entry order (req-intake-and-triage AC-7.3;
// component-task-lifecycle). Labels name the target by title and tier word,
// never by ID (feature-operator-next-action NA-4).
func contextProposalChoices(taskID string, refs []ContextProposalRef) []ChoiceSpec {
	entries := contextProposalEntries(refs)
	out := make([]ChoiceSpec, 0, 2*len(entries))
	for _, ref := range entries {
		route := fmt.Sprintf("/v1/tasks/%s/context/proposals/%s/%s", url.PathEscape(taskID), url.PathEscape(ref.Kind), url.PathEscape(ref.TargetID))
		key := fmt.Sprintf("task_context:%s:%s", ref.Kind, ref.TargetID)
		name := ContextProposalName(ref.Kind, ref.TargetTitle)
		out = append(out,
			ChoiceSpec{ID: "confirm:" + key, Label: "Confirm " + name, Effect: contextConfirmEffect, Method: "POST", Path: route + "/confirm", Capability: core.CapabilityOperateGates},
			ChoiceSpec{ID: "dismiss:" + key, Label: "Dismiss " + name, Effect: contextDismissEffect, Method: "POST", Path: route + "/dismiss", Capability: core.CapabilityOperateGates},
		)
	}
	return out
}

// contextProposalDetails states the canonical entries for
// details.context_proposals (feature-operator-next-action NA-3,
// Context-proposal presentation).
func contextProposalDetails(refs []ContextProposalRef) []ContextProposalDetail {
	entries := contextProposalEntries(refs)
	out := make([]ContextProposalDetail, 0, len(entries))
	for _, ref := range entries {
		out = append(out, ContextProposalDetail{
			Kind: ref.Kind, TargetID: ref.TargetID, TargetTitle: ref.TargetTitle, TierLabel: ContextTierWord(ref.Kind),
			Justification: ref.Justification, Source: ref.Source, ProposedBy: ref.ProposedBy, CreatedAt: ref.CreatedAt.UTC(),
		})
	}
	return out
}

// CitedProposals returns the pending requirement and System Design versions
// whose document an authority-conflict citation names, whatever their
// origin, deduplicated and ordered by tier, document ID, and version.
// Decision and task-context proposals never match (component-work-orders,
// Next action; req-operator-next-action AC-1.6).
func CitedProposals(citations []core.WorkOrderAuthorityConflictCitation, pending []ProposalRef) []ProposalRef {
	cited := map[string]bool{}
	for _, citation := range citations {
		cited[citation.DocumentID] = true
	}
	seen := map[ProposalRef]bool{}
	var out []ProposalRef
	for _, ref := range pending {
		if ref.Tier != "requirement" && ref.Tier != "system_design" {
			continue
		}
		if !cited[ref.ID] || ref.Version <= 0 || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tier != out[j].Tier {
			return out[i].Tier < out[j].Tier
		}
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Version < out[j].Version
	})
	return out
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
	case "recover", "answer":
		if order == nil {
			return "no order to recover"
		}
		return core.NextActionRecoverRefusal(*order)
	case "applied", "not_applied":
		if order == nil {
			return "no order to recover"
		}
		if text := core.NextActionRecoverRefusal(*order); text != "" {
			return text
		}
		return dispositionRefusal(in, *order)
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

// dispositionRefusal withholds the applied and not-applied choices unless the
// selected verify order has a binding the recovery validator accepts
// (feature-operator-next-action NA-3, Disposition binding).
func dispositionRefusal(in Input, order core.WorkOrder) string {
	input, ok := in.Dispositions[order.ID]
	switch {
	case ok && input.Binding != nil:
		return ""
	case ok && input.Refusal != "":
		return input.Refusal
	default:
		return fmt.Sprintf("verify order %s has no sealed verification checkpoint to dispose", order.ID)
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
