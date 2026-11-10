package nextaction

import (
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Vars are the only values a catalogue template may read.
type Vars struct {
	// Checks names a hold's checks, each with its state.
	Checks string
	// ReadFailure is the recorded read_failure sentence of an unanswered read,
	// which says whether the check status or the pull request went unread.
	ReadFailure string
	Title       string
	PR          int
	Stage       string
	// AgentStage is the stage of the order an agent holds or waits for. It
	// is set only when an agent is the actor and the statement names an
	// order, so factory work and an absent order keep the generic sentence.
	AgentStage string
	OrderID    string
	Attempts   int
	// RetryScheduled reports a retry time still ahead. The time itself is the
	// statement's deadline, never part of a sentence (component-next-action
	// invariants 14, 18).
	RetryScheduled bool
	// Approvals is an enforcing task's approval shortfall, or nil
	// (req-risk-based-review AC-11.1).
	Approvals *RiskApprovals
	Blockers  string
	Branch    string
	Base      string
	Version   int
	State     string
	Next      string
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

// Gate catalogue variants keyed on the gate's recorded source
// (Input.GateSource) and recovery stage. TriageInvalidOutput is the
// job_failure of a triage whose output stayed invalid through the bounce limit
// (component-triage invariant 26), and TriageFailure is any other job_failure
// that triage's recovery stage names, such as a failed provider call
// (req-intake-and-triage AC-3.4). The InvalidOutput variants are
// review_bounce_limit gates that a stage other than review reached with
// invalid output, and UnreviewedBounceLimit is one whose source names no
// stage's invalid output and is not review's. A review-sourced limit keeps the
// base entry (req-operator-next-action AC-1.2, AC-1.8).
const (
	TriageInvalidOutput           = "triage_invalid_output"
	TriageFailure                 = "triage_failure"
	UnreviewedBounceLimit         = "unreviewed_bounce_limit"
	InvalidOutputTriage           = "invalid_output_triage"
	InvalidOutputPlan             = "invalid_output_spec"
	InvalidOutputImplementation   = "invalid_output_implement"
	InvalidOutputVerification     = "invalid_output_verify"
	InvalidOutputUnnamedStage     = "invalid_output"
	invalidOutputSourceSuffix     = ".output_invalid"
	triageInvalidOutputGateSource = "triage" + invalidOutputSourceSuffix
)

// invalidOutputStages names each stage an InvalidOutput variant states, in
// the plain words the statement prints.
var invalidOutputStages = []struct{ variant, stage, word string }{
	{InvalidOutputTriage, string(core.StageTriage), "Triage"},
	{InvalidOutputPlan, string(core.StageSpec), "Planning"},
	{InvalidOutputImplementation, string(core.StageImplement), "Implementation"},
	{InvalidOutputVerification, string(core.StageVerify), "Verification"},
	{InvalidOutputUnnamedStage, "", "A stage"},
}

// GateVariant is the catalogue variant a gate's recorded source and the
// task's recovery stage select, or empty for the gate's base entry. A
// review_bounce_limit gate keeps its base entry exactly when
// core.ReviewSourcedBounceLimit admits its source, the predicate that also
// decides whether Approve is offered there.
func GateVariant(gate core.GateKind, source string, recovery core.Stage) string {
	switch gate {
	case core.GateKindJobFailure:
		if source == triageInvalidOutputGateSource {
			return TriageInvalidOutput
		}
		if recovery == core.StageTriage {
			return TriageFailure
		}
	case core.GateKindReviewBounceLimit:
		if core.ReviewSourcedBounceLimit(source) {
			return ""
		}
		stage, ok := strings.CutSuffix(source, invalidOutputSourceSuffix)
		if !ok {
			return UnreviewedBounceLimit
		}
		for _, item := range invalidOutputStages {
			if item.stage == stage {
				return item.variant
			}
		}
		return InvalidOutputUnnamedStage
	}
	return ""
}

// GateVariants are the gate variants the catalogue must cover.
func GateVariants() []string {
	out := []string{TriageInvalidOutput, TriageFailure, UnreviewedBounceLimit}
	for _, item := range invalidOutputStages {
		out = append(out, item.variant)
	}
	return out
}

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
		{KindCheckHold, "Checks hold the merge", "A failed check holds the merge of the approved head.", "Rerun the failed check on GitHub, merge the approved head anyway, request changes, or close?", "The task stays approved and unmerged. A later read that finds nothing failed lets the merge continue.", checkHoldChoices(), &RunGate{Kind: "merge_execution", Label: "merge execution pending"}},
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
	// The working and queued sentences name the stated order's stage, so the
	// passive task-detail banner needs no separate stage line. An unknown
	// stage keeps the generic sentence (req-operator-next-action AC-1.8;
	// component-next-action invariants 17, 18).
	stageSummary(KindWorking, "An agent is working on this task.", map[string]string{
		string(core.StageSpec):      "An agent is planning this task.",
		string(core.StageImplement): "An agent is implementing this task.",
		string(core.StageReview):    "An agent is reviewing this task.",
		string(core.StageVerify):    "An agent is verifying this task.",
	})
	stageSummary(KindQueued, "An order is waiting to be claimed.", map[string]string{
		string(core.StageSpec):      "The plan order is waiting for an agent.",
		string(core.StageImplement): "The implementation order is waiting for an agent.",
		string(core.StageReview):    "The review order is waiting for an agent.",
		string(core.StageVerify):    "The verification order is waiting for an agent.",
	})
	// An enforcing task short of its required human approvals names how many
	// remain and whether a trusted author's approval is among them, at the
	// merge gate and after an approve-only act (req-risk-based-review
	// AC-11.1).
	for _, kind := range []string{KindMergeApproval, KindMergeExecution} {
		entry := catalogueTable[catalogueKey{kind, "", "", "", ""}]
		base := entry.Summary
		entry.Summary = func(v Vars) string {
			if v.Approvals == nil || v.Approvals.Satisfied {
				return base(v)
			}
			return RiskApprovalSummary(*v.Approvals)
		}
		catalogueTable[catalogueKey{kind, "", "", "", ""}] = entry
	}
	// The hold's sentence names each check; AC-1.5 requires it on every surface
	// that shows the summary, the compact list shape included.
	failedHold := catalogueTable[catalogueKey{KindCheckHold, "", "", "", ""}]
	failedHold.Summary = func(v Vars) string {
		if v.Checks == "" {
			return "A failed check holds the merge of the approved head."
		}
		return "A failed check holds the merge of the approved head: " + v.Checks + "."
	}
	// The question names the approved head and each held check
	// (component-next-action invariant 19).
	failedHold.Question = func(v Vars) string {
		if v.Checks == "" {
			return "Rerun the failed check on GitHub, merge the approved head anyway, request changes, or close?"
		}
		return "Rerun " + v.Checks + " on GitHub, merge the approved head anyway, request changes, or close?"
	}
	catalogueTable[catalogueKey{KindCheckHold, "", "", "", ""}] = failedHold
	refused := failedHold
	refused.Label = "Grant Checks read"
	refused.Summary = text("GitHub refused the check-status read, so Conveyor cannot tell whether the approved head may merge.")
	refused.Question = text("Grant the workspace GitHub App Checks read in workspace settings, or merge without a read?")
	refused.IfIgnored = text("The task stays approved and unmerged. A later read that GitHub answers decides the merge.")
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
	stuckRead.Label = "An unanswered read holds the merge"
	stuckRead.Summary = func(v Vars) string {
		summary := withReadFailure("Conveyor still could not read what the approved head's merge depends on at its deadline, so the merge is held.", v)
		if v.Checks == "" {
			return summary
		}
		return summary + " It last named: " + v.Checks + "."
	}
	stuckRead.Question = func(v Vars) string {
		if v.Checks == "" {
			return "Check the approved head on GitHub, merge it anyway, request changes, or close?"
		}
		return "Check " + v.Checks + " on GitHub, merge the approved head anyway, request changes, or close?"
	}
	stuckRead.IfIgnored = text("The task stays approved and unmerged until a later read lets it merge.")
	catalogueTable[catalogueKey{KindCheckHold, "", "", "", CheckHoldStuckRead}] = stuckRead
	readFailed := catalogueTable[catalogueKey{KindMerging, "", "", "", ""}]
	readFailed.Summary = func(v Vars) string {
		return withReadFailure("Conveyor could not read what the approved head's merge depends on and is reading it again.", v)
	}
	readFailed.IfIgnored = text("Conveyor reads again, and merges only when that read allows it.")
	catalogueTable[catalogueKey{KindMerging, "", "", "", CheckReadFailed}] = readFailed
	readExhausted := readFailed
	readExhausted.Summary = func(v Vars) string {
		return withReadFailure("Conveyor could not read what the approved head's merge depends on and its retries ended.", v) + " Nothing merges until a read succeeds."
	}
	readExhausted.IfIgnored = text("The next check delivery, timed check, daemon start, or check-status deadline reads it again. The merge act needs no read.")
	catalogueTable[catalogueKey{KindMerging, "", "", "", CheckReadRetriesExhausted}] = readExhausted
	gateSourceWording(text)
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

// stageSummary replaces kind's summary with one sentence per agent stage and
// keeps fallback for any other stage, including none.
func stageSummary(kind, fallback string, byStage map[string]string) {
	key := catalogueKey{kind, "", "", "", ""}
	entry := catalogueTable[key]
	entry.Summary = func(v Vars) string {
		if sentence, ok := byStage[v.AgentStage]; ok {
			return sentence
		}
		return fallback
	}
	catalogueTable[key] = entry
}

func render(fn func(Vars) string, vars Vars) string {
	if fn == nil {
		return ""
	}
	return fn(vars)
}

// summaryWithRetry adds the scheduled retry's attempt number to a summary. The
// retry time is the statement's deadline, so no sentence repeats it
// (component-next-action invariants 14, 17, 18).
func summaryWithRetry(base string, vars Vars) string {
	if !vars.RetryScheduled {
		return base
	}
	if vars.Attempts > 0 {
		return fmt.Sprintf("%s The next retry is attempt %d.", base, vars.Attempts)
	}
	return base + " A retry is scheduled."
}

// gateSourceWording states the gate variants keyed on the recorded source. A
// stage-sourced bounce limit names the stage and its invalid output in the
// label, summary, question, and run-gate label. Its Approve is withheld,
// because no review reached it, so its question offers only the acts that
// remain. A triage job failure offers Retry and reject: its question uses no
// review words, and the job_failure of a triage whose output stayed invalid
// names triage's invalid output (component-triage invariant 26;
// component-next-action invariants 7, 8, 17-20; req-intake-and-triage AC-3.4;
// req-operator-next-action AC-1.4, AC-1.8).
func gateSourceWording(text func(string) func(Vars) string) {
	for _, item := range invalidOutputStages {
		label := "Settle " + strings.ToLower(item.word) + " output"
		if item.stage == "" {
			label = "Settle invalid output"
		}
		word := item.word
		entry := catalogueTable[catalogueKey{KindReviewBounceLimit, "", "", "", ""}]
		entry.Label = label
		entry.Summary = text(word + " returned invalid output until the bounce limit.")
		entry.Question = func(v Vars) string {
			if v.Attempts > 0 {
				return fmt.Sprintf("%s returned invalid output %d times. Ask for changes, or reject?", word, v.Attempts)
			}
			return word + " returned invalid output until the bounce limit. Ask for changes, or reject?"
		}
		entry.RunGate = &RunGate{Kind: "human", Label: label}
		catalogueTable[catalogueKey{KindReviewBounceLimit, "", "", "", item.variant}] = entry
	}
	unreviewed := catalogueTable[catalogueKey{KindReviewBounceLimit, "", "", "", ""}]
	unreviewed.Label = "Settle bounce limit"
	unreviewed.Summary = text("A stage reached the bounce limit.")
	unreviewed.Question = text("A stage reached the bounce limit. Ask for changes, or reject?")
	unreviewed.RunGate = &RunGate{Kind: "human", Label: unreviewed.Label}
	catalogueTable[catalogueKey{KindReviewBounceLimit, "", "", "", UnreviewedBounceLimit}] = unreviewed
	for _, item := range []struct{ variant, summary string }{
		{TriageInvalidOutput, "Triage returned invalid output until the bounce limit, so it has no outcome."},
		{TriageFailure, "Triage failed, so it has no outcome."},
	} {
		triage := catalogueTable[catalogueKey{KindJobFailure, "", "", "", ""}]
		triage.Label = "Retry triage"
		triage.Summary = text(item.summary)
		triage.Question = text("Retry triage, or reject the task?")
		triage.Choices = triageRetryChoices()
		catalogueTable[catalogueKey{KindJobFailure, "", "", "", item.variant}] = triage
	}
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

// withReadFailure appends the recorded read_failure sentence to a summary, which
// is how a surface tells an unread pull request from an unread check status
// (component-next-action, The statement; req-review-gates-evidence AC-1.10).
func withReadFailure(summary string, v Vars) string {
	if v.ReadFailure == "" {
		return summary
	}
	return summary + " " + v.ReadFailure
}

// RiskApprovalSummary states an enforcing task's approval shortfall: the
// approvals recorded and required, how many remain, and whether a trusted
// author's approval or a non-author's approval is among them
// (req-risk-based-review AC-11.1).
func RiskApprovalSummary(a RiskApprovals) string {
	remaining := fmt.Sprintf("%d more approvals are needed", a.Remaining)
	if a.Remaining == 1 {
		remaining = "1 more approval is needed"
	}
	text := fmt.Sprintf("The reviewed head has %d of %d required human approvals. %s", a.Recorded, a.Required, remaining)
	switch {
	case a.TrustedAuthorNeeded && a.NonAuthorNeeded:
		return text + ", including a trusted author's approval and an approval from a person who is not an author."
	case a.TrustedAuthorNeeded:
		return text + ", including a trusted author's approval."
	case a.NonAuthorNeeded:
		return text + ", including an approval from a person who is not an author."
	}
	return text + ". A trusted author's approval is not among them."
}
