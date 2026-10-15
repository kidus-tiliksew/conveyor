package nextaction

import (
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Vars are the only values a catalogue template may read.
type Vars struct {
	Title    string
	PR       int
	Stage    string
	OrderID  string
	Attempts int
	RetryAt  string
	Blockers string
	Branch   string
	Base     string
	Version  int
	State    string
	Next     string
}

type Entry struct {
	Label     string
	Summary   func(Vars) string
	Question  func(Vars) string
	IfIgnored func(Vars) string
	Choices   []ChoiceSpec
	RunGate   *RunGate
}

type ChoiceSpec struct {
	ID         string
	Label      string
	Effect     string
	Method     string
	Path       string
	Capability core.Capability
}

type RunGate struct {
	Kind  string
	Label string
}

type catalogueKey struct {
	kind, stall, release, gate, variant string
}

// Agent-question catalogue variants for an authority_conflict checkpoint: the
// operator is asked to decide cited pending revisions, or to author a
// revision when no cited document has one (req-260820-394cac AC-3.2;
// feature-operator-next-action NA-4).
const (
	AgentQuestionCitedRevisions = "cited_revisions"
	AgentQuestionRevisionNeeded = "revision_needed"
)

var catalogueTable = map[catalogueKey]Entry{}

func init() {
	text := func(s string) func(Vars) string {
		return func(Vars) string { return s }
	}
	put := func(kind, stall, release, gate string, entry Entry) {
		catalogueTable[catalogueKey{kind, stall, release, gate, ""}] = entry
	}
	kindEntries := []struct {
		kind, label, summary, question, ignored string
		choices                                 []ChoiceSpec
		run                                     *RunGate
	}{
		{KindTerminal, "Finished", "This task is finished.", "", "", nil, nil},
		{KindParked, "Parked", "This task is parked.", "Put it back, or close it?", "It stays parked.", parkedChoices(), nil},
		{KindDispatchExhausted, "Dispatch exhausted", "Dispatch stopped after repeated failures.", "", "It stays parked until someone recovers it.", stalledChoices(), nil},
		{KindPlanApproval, "Plan approval", "The execution plan is waiting for approval.", "Approve this plan?", "The task stays at the plan gate.", reviewChoices(), &RunGate{Kind: "spec", Label: "spec approval gate"}},
		{KindPlanRevision, "Plan revision", "Implementation asked to revise the plan.", "Accept the revised plan?", "The revision stays pending.", revisionChoices(), &RunGate{Kind: "plan_revision", Label: "plan revision gate"}},
		{KindMergeApproval, "Merge approval", "The reviewed branch is waiting for merge approval.", "Approve the merge?", "The merge gate stays open.", mergeApprovalChoices(), &RunGate{Kind: "merge", Label: "merge approval gate"}},
		{KindReviewBounceLimit, "Review bounce limit", "Review bounced until the limit.", "Recover this review?", "The task stays at the human gate.", reviewChoices(), &RunGate{Kind: "human", Label: "human recovery gate"}},
		{KindJobFailure, "Job failure", "A job failed and needs a person.", "", "The failure stays at the human gate.", reviewChoices(), &RunGate{Kind: "human", Label: "human recovery gate"}},
		{KindMergeConflict, "Merge conflict", "Automatic conflict fixes are exhausted.", "", "The conflict stays until someone fixes it.", conflictChoices(), &RunGate{Kind: "merge_execution", Label: "merge execution pending"}},
		{KindMergeExecution, "Merge", "The merge gate is on and the branch is ready to merge.", "Merge this branch?", "Conveyor does not merge it.", mergeExecutionChoices(), &RunGate{Kind: "merge_execution", Label: "merge execution pending"}},
		{KindMerging, "Merging", "Conveyor is merging this branch.", "", "The merge continues.", nil, nil},
		{KindDocumentProposal, "Document proposal", "A document proposal is withholding the next claim.", "Confirm or dismiss the proposal?", "The claim stays withheld.", proposalChoices(), nil},
		{KindAgentQuestion, "Agent question", "The agent stopped for a decision.", "Answer the agent?", "The order stays released.", answerChoices(), nil},
		{KindVerificationDisposition, "Verification disposition", "Verification needs a disposition.", "Was the change applied?", "The verify order stays at the checkpoint.", dispositionChoices(), nil},
		{KindForgeFailure, "Forge failure", "A forge write failed.", "", "The forge failure stays until it is retried.", roundChoices(), nil},
		{KindReviewRecovery, "Review recovery", "A review round needs recovery.", "", "The review stays interrupted.", roundChoices(), nil},
		{KindDependencyUnsatisfiable, "Dependency unsatisfiable", "A dependency ended without merging.", "Remove the dependency, or close the task?", "The order stays unclaimable.", dependencyChoices(), nil},
		{KindStalled, "Stalled", "This order cannot be claimed.", "", "It stays unclaimed.", stalledChoices(), nil},
		{KindBlocked, "Blocked", "This task is waiting on another task.", "", "It stays blocked.", nil, nil},
		{KindWorking, "Working", "An agent is working on this task.", "", "The attempt continues.", nil, nil},
		{KindQueued, "Queued", "An order is waiting to be claimed.", "", "It stays in the queue.", nil, nil},
		{KindRetrying, "Retrying", "Conveyor will retry this automatically.", "", "The next retry still runs.", nil, nil},
	}
	for _, item := range kindEntries {
		entry := Entry{
			Label: item.label, Summary: text(item.summary), Choices: item.choices, RunGate: item.run,
		}
		if item.question != "" {
			entry.Question = text(item.question)
		}
		if item.ignored != "" {
			entry.IfIgnored = text(item.ignored)
		}
		put(item.kind, "", "", "", entry)
	}
	for _, item := range []struct{ variant, label, summary, question string }{
		{AgentQuestionCitedRevisions, "Cited revisions need a decision",
			"The agent asks you to confirm or dismiss proposed revisions to cited documents, then recover the order with direction.",
			"Will you confirm or dismiss the cited revisions, then recover the order with direction?"},
		{AgentQuestionRevisionNeeded, "Cited documents need a revision",
			"The agent asks you to author a revision of the cited documents, then recover the order with direction.",
			"Will you author a revision of the cited documents, then recover the order with direction?"},
	} {
		entry := catalogueTable[catalogueKey{KindAgentQuestion, "", "", "", ""}]
		entry.Label, entry.Summary, entry.Question = item.label, text(item.summary), text(item.question)
		catalogueTable[catalogueKey{KindAgentQuestion, "", "", "", item.variant}] = entry
	}
	for _, reason := range []struct{ code, label, summary string }{
		{StallRetrySuppressed, "Retry suppressed", "Automatic retry is suppressed."},
		{StallStale, "Stale", "The queue deadline expired before anyone claimed it."},
		{StallTimedOut, "Timed out", "The execution deadline passed."},
		{StallUnclaimable, "Unclaimable", "No eligible claimant can take this order."},
		{StallDispatchFailing, "Dispatch failing", "Dispatch is failing repeatedly."},
		{StallAutomaticRecoveryExhausted, "Recovery exhausted", "Automatic recovery is exhausted."},
	} {
		base := catalogueTable[catalogueKey{KindStalled, "", "", "", ""}]
		base.Label = reason.label
		base.Summary = text(reason.summary)
		put(KindStalled, reason.code, "", "", base)
	}
	for _, outcome := range []struct{ code, label string }{
		{ReleaseCheckpoint, "Checkpoint"},
		{ReleaseExpired, "Expired"},
		{ReleaseReleased, "Released"},
		{core.WorkOrderOutcomeChildFailure, "Child failure"},
		{core.WorkOrderOutcomeStalled, "Stalled release"},
		{core.WorkOrderOutcomeCancelled, "Cancelled"},
		{core.WorkOrderOutcomePreempted, "Preempted"},
	} {
		put("", "", outcome.code, "", Entry{Label: outcome.label, Summary: text(outcome.label + " release."), Choices: answerChoices()})
	}
	for _, gate := range []core.GateKind{core.GateKindPlanApproval, core.GateKindPlanRevision, core.GateKindMergeApproval, core.GateKindReviewBounceLimit, core.GateKindJobFailure} {
		if _, ok := catalogueTable[catalogueKey{string(gate), "", "", string(gate), ""}]; ok {
			continue
		}
		base := catalogueTable[catalogueKey{string(gate), "", "", "", ""}]
		put(string(gate), "", "", string(gate), base)
	}
}

func Lookup(kind, stall, release, gate string) (Entry, bool) {
	return LookupVariant(kind, stall, release, gate, "")
}

// LookupVariant prefers the kind's variant entry when one exists, then
// falls back as Lookup does.
func LookupVariant(kind, stall, release, gate, variant string) (Entry, bool) {
	if variant != "" {
		if entry, ok := catalogueTable[catalogueKey{kind, "", "", "", variant}]; ok {
			return entry, true
		}
	}
	if entry, ok := catalogueTable[catalogueKey{kind, stall, release, gate, ""}]; ok {
		return entry, true
	}
	if entry, ok := catalogueTable[catalogueKey{kind, stall, "", "", ""}]; ok {
		return entry, true
	}
	if entry, ok := catalogueTable[catalogueKey{kind, "", release, "", ""}]; ok {
		return entry, true
	}
	if entry, ok := catalogueTable[catalogueKey{kind, "", "", gate, ""}]; ok {
		return entry, true
	}
	entry, ok := catalogueTable[catalogueKey{kind, "", "", "", ""}]
	return entry, ok
}

// AgentQuestionVariants are the agent-question variants the catalogue must cover.
func AgentQuestionVariants() []string {
	return []string{AgentQuestionCitedRevisions, AgentQuestionRevisionNeeded}
}

// Kinds, StallReasons, ReleaseOutcomes, and GateKinds are the constants the
// catalogue must cover.
func Kinds() []string {
	return []string{
		KindTerminal, KindParked, KindDispatchExhausted, KindPlanApproval, KindPlanRevision,
		KindMergeApproval, KindReviewBounceLimit, KindJobFailure, KindMergeConflict, KindMergeExecution,
		KindMerging, KindDocumentProposal, KindAgentQuestion, KindVerificationDisposition, KindForgeFailure,
		KindReviewRecovery, KindDependencyUnsatisfiable, KindStalled, KindBlocked, KindWorking, KindQueued, KindRetrying,
	}
}

func StallReasons() []string {
	return []string{StallRetrySuppressed, StallStale, StallTimedOut, StallUnclaimable, StallDispatchFailing, StallAutomaticRecoveryExhausted}
}

func ReleaseOutcomes() []string {
	return []string{ReleaseCheckpoint, ReleaseExpired, ReleaseReleased, core.WorkOrderOutcomeChildFailure, core.WorkOrderOutcomeStalled, core.WorkOrderOutcomeCancelled, core.WorkOrderOutcomePreempted}
}

func GateKinds() []core.GateKind {
	return []core.GateKind{core.GateKindPlanApproval, core.GateKindPlanRevision, core.GateKindMergeApproval, core.GateKindReviewBounceLimit, core.GateKindJobFailure}
}

func render(fn func(Vars) string, vars Vars) string {
	if fn == nil {
		return ""
	}
	return fn(vars)
}

func summaryWithRetry(base string, vars Vars) string {
	if vars.RetryAt == "" {
		return base
	}
	return fmt.Sprintf("%s Next retry at %s (attempt %d).", base, vars.RetryAt, vars.Attempts)
}
