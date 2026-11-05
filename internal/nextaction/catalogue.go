package nextaction

import (
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Vars are the only values a catalogue template may read.
type Vars struct {
	// Checks names a hold's checks, each with its state.
	Checks   string
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
	// Dependents is the titles of blocks joined as a list, empty when nothing
	// waits on the task (feature-operator-next-action NA-4).
	Dependents string
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
	Direction  string
	// Refusal withholds the choice with this text even when its route would
	// accept it; the route's own refusal is stated first.
	Refusal string
}

type RunGate struct {
	Kind  string
	Label string
}

type catalogueKey struct {
	kind, stall, release, gate, variant string
}

// Agent-question catalogue variants for an authority_conflict checkpoint: the
// operator is asked to decide cited pending revisions, to recover once every
// cited document has a newer confirmed version, or to author a revision
// otherwise (req-260820-394cac AC-3.2; feature-operator-next-action NA-4).
const (
	AgentQuestionCitedRevisions = "cited_revisions"
	AgentQuestionCitedConfirmed = "cited_revisions_confirmed"
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
		{KindParked, "Decide on a parked task", "This task is parked.", "Put it back, or close it?", "It stays parked.", parkedChoices(), nil},
		{KindDispatchExhausted, "Dispatch exhausted", "Dispatch stopped after repeated failures.", "", "It stays parked until someone recovers it.", stalledChoices(), nil},
		{KindPlanApproval, "Approve plan", "The execution plan is waiting for approval.", "", "", reviewChoices("Approve plan"), &RunGate{Kind: "spec", Label: "Approve plan"}},
		{KindPlanRevision, "Approve plan revision", "Implementation asked to revise the plan.", "", "", revisionChoices(), &RunGate{Kind: "plan_revision", Label: "Approve plan revision"}},
		{KindMergeApproval, "Approve merge", "The reviewed branch is waiting for merge approval.", "", "", mergeApprovalChoices(), &RunGate{Kind: "merge", Label: "Approve merge"}},
		{KindReviewBounceLimit, "Settle review", "Review bounced until the limit.", "", "The task stays at the human gate.", reviewChoices("Approve"), &RunGate{Kind: "human", Label: "Settle review"}},
		{KindJobFailure, "Job failure", "A job failed and needs a person.", "", "The failure stays at the human gate.", reviewChoices("Approve"), &RunGate{Kind: "human", Label: "human recovery gate"}},
		{KindMergeConflict, "Merge conflict", "Automatic conflict fixes are exhausted.", "", "The conflict stays until someone fixes it.", conflictChoices(), &RunGate{Kind: "merge_execution", Label: "merge execution pending"}},
		{KindMergeExecution, "Merge branch", "The merge gate is on and the branch is ready to merge.", "", "Conveyor does not merge it.", mergeExecutionChoices(), &RunGate{Kind: "merge_execution", Label: "merge execution pending"}},
		{KindCheckHold, "Checks hold the merge", "A failed check holds the merge of the approved head.", "Rerun the check on GitHub, merge anyway, request changes, or close?", "The merge stays held. A later read that finds nothing failed lets it continue.", checkHoldChoices(), &RunGate{Kind: "merge_execution", Label: "merge execution pending"}},
		{KindMerging, "Merging", "Conveyor is merging this branch.", "", "The merge continues.", nil, nil},
		{KindContextProposal, "Context proposal", "A context proposal is waiting for a decision.", "Confirm or dismiss the proposed context?", "The proposal stays pending.", nil, nil},
		{KindAgentQuestion, "Agent question", "The agent stopped for a decision.", "Answer the agent?", "The order stays released.", answerChoices(), nil},
		{KindVerificationDisposition, "Confirm an outcome", "Verification needs a disposition.", "Did the change apply? The verify agent could not observe the result.", "The verify order stays at the checkpoint.", dispositionChoices(), nil},
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
	decisionWording(text)
	// The hold's sentence names each check; AC-1.5 requires it on every surface
	// that shows the summary, the compact list shape included.
	failedHold := catalogueTable[catalogueKey{KindCheckHold, "", "", "", ""}]
	failedHold.Summary = func(v Vars) string {
		if v.Checks == "" {
			return "A failed check holds the merge of the approved head."
		}
		return "A failed check holds the merge of the approved head: " + v.Checks + "."
	}
	catalogueTable[catalogueKey{KindCheckHold, "", "", "", ""}] = failedHold
	refused := failedHold
	refused.Label = "Grant Checks read"
	refused.Summary = text("GitHub refused the check-status read, so Conveyor cannot tell whether the approved head may merge.")
	refused.Question = text("Grant the workspace GitHub App Checks read in workspace settings, or merge without a read?")
	refused.IfIgnored = text("The merge stays held. A later read that GitHub answers decides it.")
	catalogueTable[catalogueKey{KindCheckHold, "", "", "", CheckHoldPermission}] = refused
	stuck := failedHold
	stuck.Label = "Stuck check holds the merge"
	stuck.Summary = func(v Vars) string {
		if v.Checks == "" {
			return "A check is stuck, so the merge of the approved head is held."
		}
		return "A check is stuck, so the merge of the approved head is held: " + v.Checks + "."
	}
	stuck.Question = func(v Vars) string {
		if v.Checks == "" {
			return "Rerun the stuck check on GitHub, merge the approved head anyway, request changes, or close?"
		}
		return "Rerun " + v.Checks + " on GitHub, merge the approved head anyway, request changes, or close?"
	}
	stuck.IfIgnored = text("The task stays approved and unmerged until a later read finds no check pending.")
	catalogueTable[catalogueKey{KindCheckHold, "", "", "", CheckHoldStuck}] = stuck
	stuckRead := stuck
	stuckRead.Label = "Unread checks hold the merge"
	stuckRead.Summary = func(v Vars) string {
		if v.Checks == "" {
			return "Conveyor still could not read the approved head's check status at its deadline, so the merge is held."
		}
		return "Conveyor still could not read the approved head's check status at its deadline, so the merge is held. It last named: " + v.Checks + "."
	}
	stuckRead.Question = func(v Vars) string {
		if v.Checks == "" {
			return "Check the approved head on GitHub, merge it anyway, request changes, or close?"
		}
		return "Check " + v.Checks + " on GitHub, merge the approved head anyway, request changes, or close?"
	}
	catalogueTable[catalogueKey{KindCheckHold, "", "", "", CheckHoldStuckRead}] = stuckRead
	readFailed := catalogueTable[catalogueKey{KindMerging, "", "", "", ""}]
	readFailed.Summary = text("Conveyor could not read the approved head's check status and is reading it again.")
	readFailed.IfIgnored = text("Conveyor reads the check status again, and merges only when that read allows it.")
	catalogueTable[catalogueKey{KindMerging, "", "", "", CheckReadFailed}] = readFailed
	readExhausted := readFailed
	readExhausted.Summary = text("Conveyor could not read the approved head's check status and its retries ended. Nothing merges until a read succeeds.")
	readExhausted.IfIgnored = text("The next check delivery, timed check, daemon start, or check-status deadline reads it again. The merge act needs no read.")
	catalogueTable[catalogueKey{KindMerging, "", "", "", CheckReadRetriesExhausted}] = readExhausted
	for _, item := range []struct{ variant, label, summary, question string }{
		{AgentQuestionCitedRevisions, "Cited revisions need a decision",
			"The agent asks you to confirm or dismiss proposed revisions to cited documents, then recover the order with direction.",
			"Will you confirm or dismiss the cited revisions, then recover the order with direction?"},
		{AgentQuestionCitedConfirmed, "Cited revisions are confirmed",
			"The cited documents have newer confirmed versions. Recover the order with direction to continue.",
			"Recover the order with direction to continue under the confirmed versions?"},
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
	// The observed claim-failure stall names the server fault and its
	// effect; the count and times stay in details (feature-operator-next-action NA-4).
	claimFailing := catalogueTable[catalogueKey{KindStalled, "", "", "", ""}]
	claimFailing.Label = "Claims failing"
	claimFailing.Summary = text("Claims of this order keep failing with a server database error.")
	claimFailing.IfIgnored = text("Claims keep failing until the server's database error is fixed.")
	claimFailing.Choices = claimFailingChoices()
	put(KindStalled, StallClaimFailing, "", "", claimFailing)
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
	return []string{AgentQuestionCitedRevisions, AgentQuestionCitedConfirmed, AgentQuestionRevisionNeeded}
}

// Kinds, StallReasons, ReleaseOutcomes, and GateKinds are the constants the
// catalogue must cover.
func Kinds() []string {
	return []string{
		KindTerminal, KindParked, KindDispatchExhausted, KindPlanApproval, KindPlanRevision,
		KindMergeApproval, KindReviewBounceLimit, KindJobFailure, KindMergeConflict, KindMergeExecution,
		KindCheckHold, KindMerging, KindContextProposal, KindAgentQuestion, KindVerificationDisposition, KindForgeFailure,
		KindReviewRecovery, KindDependencyUnsatisfiable, KindStalled, KindBlocked, KindWorking, KindQueued, KindRetrying,
	}
}

func StallReasons() []string {
	return []string{StallRetrySuppressed, StallStale, StallTimedOut, StallUnclaimable, StallDispatchFailing, StallAutomaticRecoveryExhausted, StallClaimFailing}
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

// decisionWording states the judgment kinds' questions and if-ignored
// sentences that read the plan version, pull-request number, bounce count,
// and dependents (feature-operator-next-action NA-4, Decision wording). A
// value the factory never recorded is described rather than invented.
func decisionWording(text func(string) func(Vars) string) {
	blockedOr := func(otherwise string) func(Vars) string {
		return func(v Vars) string {
			if v.Dependents != "" {
				return v.Dependents + " stay blocked."
			}
			return otherwise
		}
	}
	plan := func(v Vars) string {
		if v.Version > 0 {
			return fmt.Sprintf("plan v%d", v.Version)
		}
		return "the latest plan"
	}
	set := func(kind string, question, ignored func(Vars) string) {
		key := catalogueKey{kind, "", "", "", ""}
		entry := catalogueTable[key]
		entry.Question = question
		if ignored != nil {
			entry.IfIgnored = ignored
		}
		catalogueTable[key] = entry
	}
	set(KindPlanApproval, func(v Vars) string {
		return fmt.Sprintf("Is %s the right way to do this? Approving starts implementation.", plan(v))
	}, blockedOr("The task stays at the plan gate."))
	set(KindPlanRevision, func(v Vars) string {
		return fmt.Sprintf("Accept %s, the revision implementation asked for?", plan(v))
	}, blockedOr("The revision stays pending."))
	set(KindMergeApproval, func(v Vars) string {
		if v.PR > 0 {
			return fmt.Sprintf("Review approved pull request #%d. Merge it?", v.PR)
		}
		return "Review approved the pull request. Merge it?"
	}, blockedOr("The merge gate stays open."))
	set(KindMergeExecution, func(v Vars) string {
		if v.PR > 0 {
			return fmt.Sprintf("Merge pull request #%d?", v.PR)
		}
		return "Merge the pull request?"
	}, nil)
	set(KindReviewBounceLimit, func(v Vars) string {
		if v.Attempts > 0 {
			return fmt.Sprintf("Review bounced %d times. Approve, ask for changes, or reject?", v.Attempts)
		}
		return "Review reached the bounce limit. Approve, ask for changes, or reject?"
	}, text("The task stays at the human gate."))
}

// ContextTierWord is the tier word a context_proposal label and detail entry
// carry for a target kind (feature-operator-next-action NA-4).
func ContextTierWord(kind string) string {
	switch kind {
	case string(core.TaskContextProposalSystemDesign):
		return "System Design"
	case string(core.TaskContextProposalRequirement):
		return "requirement"
	default:
		return kind
	}
}

// ContextProposalName is the target as a context_proposal choice label names
// it: the title and the tier word, or "proposed <tier word>" when the stored
// record has no title. It never carries the target ID
// (feature-operator-next-action NA-4).
func ContextProposalName(kind, title string) string {
	tier := ContextTierWord(kind)
	if title = strings.TrimSpace(title); title == "" {
		return "proposed " + tier
	}
	return fmt.Sprintf("%s (%s)", title, tier)
}

// monitorOriginWords names each monitor signal kind that files a task in the
// origin sentence (feature-operator-next-action NA-4).
var monitorOriginWords = map[string]string{
	"post_merge_failure": "post-merge failure",
	"direct_push":        "direct push",
	"external_pr_merge":  "external pull request merge",
	"revert":             "revert",
}

// MonitorOriginKinds lists the signal kinds with their own origin sentence.
func MonitorOriginKinds() []string {
	return []string{"post_merge_failure", "direct_push", "external_pr_merge", "revert"}
}

// MonitorOriginSummary is the origin sentence for a monitor signal kind.
func MonitorOriginSummary(kind string) string {
	if words, ok := monitorOriginWords[kind]; ok {
		return "Filed by the monitor: " + words
	}
	return "Filed by the monitor"
}

// DocumentProposalCopy is the copy of one inbox document-proposal item, keyed
// by tier (feature-operator-next-action NA-4, NA-5; req-operator-next-action
// AC-2.6). A pending proposal holds no task's work (req-260810-23b69f REQ-3;
// DEC-102(6)), so ignoring one leaves only the proposal pending.
func DocumentProposalCopy(tier string) (label, question, ifIgnored string) {
	switch tier {
	case "requirement":
		label, question = "Requirement proposal", "Confirm or dismiss this proposed version?"
	case "system_design":
		label, question = "System Design proposal", "Confirm or dismiss this proposed version?"
	case "decision":
		label, question = "Decision proposal", "Confirm or dismiss this proposed decision?"
	default:
		label, question = "Document proposal", "Confirm or dismiss the proposal?"
	}
	return label, question, "The proposal stays pending."
}
