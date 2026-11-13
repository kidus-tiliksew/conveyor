package nextaction

import (
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestProjectRowsAndLabel(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	cases := []struct {
		name        string
		in          Input
		actor, kind string
		stall       string
		secondary   string
	}{
		{name: "row1", in: Input{Task: core.Task{State: core.TaskMerged}, Now: now}, actor: ActorNone, kind: KindTerminal},
		{name: "row2", in: Input{Task: core.Task{State: core.TaskParked}, Markers: Markers{ParkCommand: "triage.park"}, Now: now}, actor: ActorOperator, kind: KindParked},
		{name: "row3", in: Input{Task: core.Task{State: core.TaskParked}, Markers: Markers{ParkCommand: "dispatch.fail_final"}, Now: now}, actor: ActorOperator, kind: KindDispatchExhausted},
		{name: "row4 plan", in: gateInput(core.GateKindPlanApproval, now), actor: ActorOperator, kind: KindPlanApproval},
		{name: "row4 revision", in: gateInput(core.GateKindPlanRevision, now), actor: ActorOperator, kind: KindPlanRevision},
		{name: "row4 merge", in: gateInput(core.GateKindMergeApproval, now), actor: ActorOperator, kind: KindMergeApproval},
		{name: "row4 bounce", in: gateInput(core.GateKindReviewBounceLimit, now), actor: ActorOperator, kind: KindReviewBounceLimit},
		{name: "row4 job", in: gateInput(core.GateKindJobFailure, now), actor: ActorOperator, kind: KindJobFailure},
		{name: "row5", in: conflictInput(true, now), actor: ActorOperator, kind: KindMergeConflict, secondary: KindMergeExecution},
		{name: "row5 pre", in: conflictInput(false, now), actor: ActorFactory, kind: KindRetrying},
		{name: "row6", in: Input{Task: core.Task{State: core.TaskApproved, MergeApproval: true}, Now: now}, actor: ActorOperator, kind: KindMergeExecution},
		{name: "row7", in: Input{Task: core.Task{State: core.TaskApproved}, Markers: Markers{ForgeFailure: true}, Now: now}, actor: ActorFactory, kind: KindMerging},
		{name: "row7 over question", in: mergingAndQuestion(now), actor: ActorFactory, kind: KindMerging, secondary: KindAgentQuestion},
		{name: "row9-10", in: questionAndVerify(now), actor: ActorOperator, kind: KindAgentQuestion, secondary: KindVerificationDisposition},
		{name: "row10-11 sealed set", in: sealedCheckpointInput(now, &DispositionInput{Set: sealedCheckpointSet()}), actor: ActorOperator, kind: KindVerificationDisposition, secondary: KindStalled},
		{name: "row10-11 sealed refused", in: sealedCheckpointInput(now, &DispositionInput{Refusal: "no set"}), actor: ActorOperator, kind: KindAgentQuestion, secondary: KindVerificationDisposition},
		{name: "row11-12", in: Input{Task: core.Task{State: core.TaskRunning}, Markers: Markers{ForgeFailure: true, ReviewRecovery: true}, Now: now}, actor: ActorOperator, kind: KindForgeFailure, secondary: KindReviewRecovery},
		{name: "row13-14", in: unsatisfiableAndSuppressed(now), actor: ActorOperator, kind: KindDependencyUnsatisfiable, secondary: KindStalled},
		{name: "row14 timed_out", in: timedOut(now), actor: ActorOperator, kind: KindStalled, stall: StallTimedOut},
		{name: "row14-15", in: suppressedAndBlocked(now), actor: ActorOperator, kind: KindStalled, secondary: KindBlocked},
		{name: "row15-17", in: blockedClaimable(now), actor: ActorDependency, kind: KindBlocked, secondary: KindQueued},
		{name: "row16-17", in: claimedAndQueued(now), actor: ActorAgent, kind: KindWorking, secondary: KindQueued},
		{name: "ar6", in: scheduled(now, later), actor: ActorFactory, kind: KindRetrying},
		{name: "ar6 exhausted", in: recoveryExhausted(now), actor: ActorOperator, kind: KindStalled, stall: StallAutomaticRecoveryExhausted},
		{name: "verify fallback", in: verifyRefused(now), actor: ActorOperator, kind: KindStalled, stall: StallUnclaimable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Project(tc.in)
			if got.Actor != tc.actor || got.Kind != tc.kind {
				t.Fatalf("actor/kind=%s/%s want %s/%s", got.Actor, got.Kind, tc.actor, tc.kind)
			}
			if got.Label == "" {
				t.Fatal("missing label")
			}
			entry, ok := Lookup(got.Kind, got.Details.StallReason, got.Details.ReleaseOutcome, got.Details.GateKind)
			if !ok || got.Label != entry.Label {
				t.Fatalf("label=%q entry=%q ok=%v", got.Label, entry.Label, ok)
			}
			if CompactFrom(got).Label != got.Label {
				t.Fatal("compact label drifted")
			}
			if tc.stall != "" && got.Details.StallReason != tc.stall {
				t.Fatalf("stall=%s", got.Details.StallReason)
			}
			if tc.secondary != "" && !hasSecondary(got, tc.secondary) {
				t.Fatalf("secondary=%v want %s", got.Details.Secondary, tc.secondary)
			}
		})
	}
}

func TestRefusedChoices(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	claimed := Project(Input{Task: core.Task{ID: "t", State: core.TaskParked}, Markers: Markers{ParkCommand: "dispatch.fail_final"}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderClaimed}}, Now: now, Capabilities: allCaps()})
	if hasChoice(claimed, "recover") || !hasRefusal(claimed, "recover") {
		t.Fatalf("claimed recover choices=%v refusals=%v", claimed.Choices, claimed.Details.Refusals)
	}
	revision := Project(Input{Task: core.Task{ID: "t", State: core.TaskAwaiting, GateKind: core.GateKindPlanRevision}, GateSpecVersion: 1, LatestSpec: &SpecRef{Version: 1}, Now: now})
	if hasChoice(revision, "approve") || !hasRefusal(revision, "approve") {
		t.Fatalf("plan revision approve choices=%v refusals=%v", revision.Choices, revision.Details.Refusals)
	}
	superseded := Project(Input{Task: core.Task{ID: "t", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval}, GateSpecVersion: 1, LatestSpec: &SpecRef{Version: 2}, Now: now})
	if hasChoice(superseded, "approve") || !hasRefusal(superseded, "approve") {
		t.Fatalf("superseded choices=%v refusals=%v", superseded.Choices, superseded.Details.Refusals)
	}
	stalled := Project(timedOut(now))
	if hasChoice(stalled, "change_policy") || !hasRefusal(stalled, "change_policy") {
		t.Fatalf("policy choices=%v refusals=%v", stalled.Choices, stalled.Details.Refusals)
	}
	allowed := Project(Input{Task: core.Task{ID: "t", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval}, GateSpecVersion: 1, LatestSpec: &SpecRef{Version: 1}, Now: now, Capabilities: map[core.Capability]bool{core.CapabilityOperateGates: true}})
	choice, ok := choiceByID(allowed, "approve")
	if !ok || !choice.Allowed {
		t.Fatalf("allowed=%v ok=%v", choice, ok)
	}
	denied := Project(Input{Task: core.Task{ID: "t", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval}, GateSpecVersion: 1, LatestSpec: &SpecRef{Version: 1}, Now: now})
	choice, ok = choiceByID(denied, "approve")
	if !ok || choice.Allowed {
		t.Fatalf("denied allowed=%v", choice.Allowed)
	}
}

// TestCatalogueCoversEveryKind fails when a kind, stall reason, release
// outcome, gate kind, or agent-question variant lacks an entry, when a
// judgment kind lacks its question, or when a gate-specific copy or run-gate
// label drifts from its kind's decision wording (feature-operator-next-action
// NA-4).
func TestCatalogueCoversEveryKind(t *testing.T) {
	for _, kind := range Kinds() {
		entry, ok := Lookup(kind, "", "", "")
		if !ok || entry.Label == "" || entry.Summary == nil {
			t.Fatalf("kind %s missing catalogue", kind)
		}
	}
	for _, kind := range []string{KindParked, KindPlanApproval, KindPlanRevision, KindMergeApproval, KindReviewBounceLimit, KindMergeExecution, KindContextProposal, KindAgentQuestion, KindVerificationDisposition, KindDependencyUnsatisfiable} {
		entry, _ := Lookup(kind, "", "", "")
		if entry.Question == nil || entry.Question(Vars{}) == "" || entry.IfIgnored == nil {
			t.Fatalf("judgment kind %s missing question or if_ignored", kind)
		}
	}
	for _, kind := range MonitorOriginKinds() {
		if summary := MonitorOriginSummary(kind); summary == "Filed by the monitor" || summary == "" {
			t.Fatalf("monitor origin %s missing its sentence", kind)
		}
	}
	for _, reason := range StallReasons() {
		entry, ok := Lookup(KindStalled, reason, "", "")
		if !ok || entry.Label == "" || entry.Summary == nil {
			t.Fatalf("stall %s missing", reason)
		}
	}
	entry, ok := Lookup(KindPlanApproval, "", "", "")
	if !ok || entry.RunGate == nil || entry.RunGate.Kind != "spec" {
		t.Fatal("plan_approval run gate drifted")
	}
	for _, outcome := range ReleaseOutcomes() {
		entry, ok := Lookup("", "", outcome, "")
		if !ok || entry.Label == "" || entry.Summary == nil {
			t.Fatalf("release %s missing", outcome)
		}
	}
	for _, variant := range AgentQuestionVariants() {
		entry, ok := LookupVariant(KindAgentQuestion, "", ReleaseCheckpoint, "", variant)
		base, _ := Lookup(KindAgentQuestion, "", "", "")
		if !ok || entry.Label == "" || entry.Label == base.Label || entry.Summary == nil || entry.Question == nil {
			t.Fatalf("agent question variant %s missing", variant)
		}
	}
	for _, gate := range GateKinds() {
		entry, ok := Lookup(string(gate), "", "", string(gate))
		if !ok || entry.Label == "" || entry.RunGate == nil || entry.RunGate.Label == "" {
			t.Fatalf("gate %s missing run gate", gate)
		}
		base, _ := Lookup(string(gate), "", "", "")
		vars := Vars{Version: 2, PR: 7, Attempts: 3, Dependents: "Dependent"}
		if entry.Label != base.Label || render(entry.Question, vars) != render(base.Question, vars) || render(entry.IfIgnored, vars) != render(base.IfIgnored, vars) {
			t.Fatalf("gate %s copy drifted from kind entry", gate)
		}
		if gate != core.GateKindJobFailure && entry.RunGate.Label != entry.Label {
			t.Fatalf("gate %s run gate label %q, want the decision label %q", gate, entry.RunGate.Label, entry.Label)
		}
	}
}

// TestDecisionWording pins the NA-4 decision wording table and the per-kind
// choice labels: action-first labels, questions naming the plan version or
// pull request, if_ignored naming the dependents when blocks is non-empty,
// and an ellipsis exactly on the choices whose route takes typed input. Choice
// IDs, routes, and effects are unchanged (feature-operator-next-action NA-4).
func TestDecisionWording(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	dependents := []BlockerRef{{TaskID: "b-task", Title: "Billing export", State: "queued"}, {TaskID: "a-task", Title: "Audit log", State: "queued"}}
	gate := func(kind core.GateKind, blocks []BlockerRef) Input {
		return Input{Task: core.Task{ID: "t", State: core.TaskAwaiting, GateKind: kind}, LatestSpec: &SpecRef{Version: 3}, GateSpecVersion: 3, PullRequest: 1312, BounceCount: 4, Blocks: blocks, MergeReadiness: &MergeReadiness{State: "MERGEABLE"}, Now: now}
	}
	cases := []struct {
		name, label, question, ignored string
		in                             Input
		choices                        []string
	}{
		{"plan approval with dependents", "Approve plan", "Is plan v3 the right way to do this? Approving starts implementation.", "Billing export and Audit log stay blocked.", gate(core.GateKindPlanApproval, dependents), []string{"Approve plan", "Ask for changes…", "Reject"}},
		{"plan approval alone", "Approve plan", "Is plan v3 the right way to do this? Approving starts implementation.", "The task stays at the plan gate.", gate(core.GateKindPlanApproval, nil), []string{"Approve plan", "Ask for changes…", "Reject"}},
		{"plan revision with dependents", "Approve plan revision", "Accept plan v3, the revision implementation asked for?", "Billing export and Audit log stay blocked.", gate(core.GateKindPlanRevision, dependents), []string{"Approve revision", "Decline…", "Reject"}},
		{"plan revision alone", "Approve plan revision", "Accept plan v3, the revision implementation asked for?", "The revision stays pending.", gate(core.GateKindPlanRevision, nil), []string{"Approve revision", "Decline…", "Reject"}},
		{"merge approval with dependents", "Approve merge", "Review approved pull request #1312. Merge it?", "Billing export and Audit log stay blocked.", gate(core.GateKindMergeApproval, dependents), []string{"Merge", "Approve only", "Request changes…"}},
		{"merge approval alone", "Approve merge", "Review approved pull request #1312. Merge it?", "The merge gate stays open.", gate(core.GateKindMergeApproval, nil), []string{"Merge", "Approve only", "Request changes…"}},
		{"bounce limit", "Settle review", "Review bounced 4 times. Approve, ask for changes, or reject?", "The task stays at the human gate.", gate(core.GateKindReviewBounceLimit, dependents), []string{"Approve", "Ask for changes…", "Reject"}},
		// No review approved the state a job failure holds, so its Approve is
		// withheld (component-next-action invariant 7).
		{"job failure", "Job failure", "", "The failure stays at the human gate.", gate(core.GateKindJobFailure, nil), []string{"Ask for changes…", "Reject"}},
		{"merge execution", "Merge branch", "Merge pull request #1312?", "Conveyor does not merge it.", Input{Task: core.Task{ID: "t", State: core.TaskApproved, MergeApproval: true}, PullRequest: 1312, Blocks: dependents, Now: now}, []string{"Merge", "Request changes…", "Close…"}},
		{"parked", "Decide on a parked task", "Put it back, or close it?", "It stays parked.", Input{Task: core.Task{ID: "t", State: core.TaskParked, RecoveryStage: core.StageImplement}, Markers: Markers{ParkCommand: "triage.park"}, Now: now}, []string{"Recover", "Close", "Start over"}},
		{"verification disposition", "Confirm an outcome", "Did the change apply? The verify agent could not observe the result.", "The verify order stays at the checkpoint.", dispositionInput(now, []Disposition{{ContextID: "ctx", RunID: "run", OperationIDs: []string{"op"}, InputDigest: "digest"}}), []string{"Record dispositions…"}},
		{"agent question", "Agent question", "Answer the agent?", "The order stays released.", Input{Task: core.Task{ID: "t", State: core.TaskRunning}, Markers: Markers{CheckpointDecision: true}, Orders: []core.WorkOrder{{ID: "q", State: core.WorkOrderQueued, LastAttemptID: "a", LastAttemptOutcome: "released"}}, Now: now}, []string{"Answer…"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Project(tc.in)
			if got.Label != tc.label || got.Question != tc.question || got.IfIgnored != tc.ignored {
				t.Fatalf("label/question/if_ignored = %q / %q / %q, want %q / %q / %q", got.Label, got.Question, got.IfIgnored, tc.label, tc.question, tc.ignored)
			}
			labels := make([]string, 0, len(got.Choices))
			for _, choice := range got.Choices {
				labels = append(labels, choice.Label)
			}
			if !reflect.DeepEqual(labels, tc.choices) {
				t.Fatalf("choice labels = %q, want %q (refusals %+v)", labels, tc.choices, got.Details.Refusals)
			}
		})
	}
	unrecorded := []struct {
		name, want string
		in         Input
	}{
		{"plan without a version", "Is the latest plan the right way to do this? Approving starts implementation.", Input{Task: core.Task{State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval}, Now: now}},
		{"merge without a pull request", "Review approved the pull request. Merge it?", Input{Task: core.Task{State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval}, Now: now}},
		{"execution without a pull request", "Merge the pull request?", Input{Task: core.Task{State: core.TaskApproved, MergeApproval: true}, Now: now}},
		{"bounce without a recorded count", "Review reached the bounce limit. Approve, ask for changes, or reject?", Input{Task: core.Task{State: core.TaskAwaiting, GateKind: core.GateKindReviewBounceLimit}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderQueued, AutomaticRetryCount: 5}}, Now: now}},
	}
	for _, tc := range unrecorded {
		if got := Project(tc.in).Question; got != tc.want {
			t.Fatalf("%s question = %q, want %q", tc.name, got, tc.want)
		}
	}
	for _, kind := range Kinds() {
		entry, _ := Lookup(kind, "", "", "")
		for _, choice := range entry.Choices {
			typed := choice.ID == "redirect" || choice.ID == "decline" || choice.ID == "request_changes" || choice.ID == "answer" || choice.ID == "record_dispositions" || ((kind == KindMergeExecution || kind == KindCheckHold) && choice.ID == "close")
			if typed != strings.HasSuffix(choice.Label, "…") {
				t.Fatalf("%s choice %s label %q: ellipsis must mark typed input", kind, choice.ID, choice.Label)
			}
		}
	}
}

// TestBlocksNeverChangeTheStatement proves that blocks is present on every
// kind, an empty array when nothing waits on the task, copied in task-ID
// order onto the compact shape without aliasing, and never changes actor,
// kind, or judgment (feature-operator-next-action NA-3, Dependents).
func TestBlocksNeverChangeTheStatement(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	blocks := []BlockerRef{{TaskID: "a-task", Title: "Audit log", State: "queued"}, {TaskID: "b-task", Title: "Billing export", State: "running"}}
	inputs := map[string]Input{
		"terminal": {Task: core.Task{State: core.TaskMerged}, Now: now},
		"gate":     gateInput(core.GateKindPlanApproval, now),
		"blocked":  blockedClaimable(now),
		"working":  claimedAndQueued(now),
		"stalled":  timedOut(now),
		"merging":  {Task: core.Task{State: core.TaskApproved}, Now: now},
	}
	for name, in := range inputs {
		plain := Project(in)
		if plain.Blocks == nil || len(plain.Blocks) != 0 {
			t.Fatalf("%s blocks = %#v, want an empty array", name, plain.Blocks)
		}
		raw, err := json.Marshal(CompactFrom(plain))
		if err != nil || !strings.Contains(string(raw), `"blocks":[]`) {
			t.Fatalf("%s compact = %s err=%v, want an empty blocks array", name, raw, err)
		}
		in.Blocks = blocks
		with := Project(in)
		if with.Actor != plain.Actor || with.Kind != plain.Kind || with.Judgment != plain.Judgment {
			t.Fatalf("%s blocks changed %s/%s/%t to %s/%s/%t", name, plain.Actor, plain.Kind, plain.Judgment, with.Actor, with.Kind, with.Judgment)
		}
		compact := CompactFrom(with)
		if !reflect.DeepEqual(with.Blocks, blocks) || !reflect.DeepEqual(compact.Blocks, blocks) {
			t.Fatalf("%s blocks = %+v compact %+v", name, with.Blocks, compact.Blocks)
		}
		compact.Blocks[0].Title = "changed"
		if with.Blocks[0].Title != "Audit log" || blocks[0].Title != "Audit log" {
			t.Fatalf("%s compact blocks alias the projection", name)
		}
	}
}

// TestDispositionSet proves details.dispositions appears only on row 11,
// copied in order from the selected verify order's disposition set without
// aliasing it, with the one record_dispositions choice allowed by
// operate_gates alone. Without a set the choice is omitted and refused with
// the assembler's reason. A higher row never carries the set
// (component-next-action invariants 7, 8, 9, and 15; DEC-133(1)).
func TestDispositionSet(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	set := []Disposition{
		{ContextID: "ctx-a", RunID: "run-1", OperationIDs: []string{}, InputDigest: "digest-1", Subject: "kit:provider/login", State: "blocked"},
		{ContextID: "ctx-b", RunID: "run-2", OperationIDs: []string{"op-a", "op-b"}, InputDigest: "digest-2", Subject: "ordinary:checkout", State: "running"},
	}
	in := dispositionInput(now, set)
	got := Project(in)
	if got.Kind != KindVerificationDisposition || !reflect.DeepEqual(got.Details.Dispositions, set) || &got.Details.Dispositions[1].OperationIDs[0] == &set[1].OperationIDs[0] {
		t.Fatalf("row 11 = %s dispositions %+v", got.Kind, got.Details.Dispositions)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("row 11 choices = %+v, want record_dispositions only", got.Choices)
	}
	choice, ok := choiceByID(got, "record_dispositions")
	if !ok || choice.Capability != string(core.CapabilityOperateGates) || choice.Allowed || choice.Path != "/v1/work-orders/v/recover" || choice.Method != "POST" {
		t.Fatalf("record_dispositions = %+v ok=%t", choice, ok)
	}
	for _, retired := range []string{"applied", "not_applied"} {
		if hasChoice(got, retired) || hasRefusal(got, retired) {
			t.Fatalf("row 11 still states the retired choice %s", retired)
		}
	}
	in.Capabilities = map[core.Capability]bool{core.CapabilityRecoverWork: true}
	if choice, _ := choiceByID(Project(in), "record_dispositions"); choice.Allowed {
		t.Fatal("recover_work allowed a disposition")
	}
	in.Capabilities = map[core.Capability]bool{core.CapabilityOperateGates: true}
	if choice, _ := choiceByID(Project(in), "record_dispositions"); !choice.Allowed {
		t.Fatal("operate_gates did not allow a disposition")
	}

	refused := dispositionInput(now, nil)
	refused.Dispositions = map[string]DispositionInput{"v": {Refusal: "verify order v has no unresolved operations and no retry-gated attempts to dispose"}}
	missing := Project(refused)
	if missing.Details.Dispositions != nil || hasChoice(missing, "record_dispositions") {
		t.Fatalf("row 11 without a set offered %+v dispositions %+v", missing.Choices, missing.Details.Dispositions)
	}
	if !hasRefusalText(missing, "record_dispositions", "no unresolved operations") {
		t.Fatalf("refusals = %+v, want record_dispositions refused for an empty set", missing.Details.Refusals)
	}
	if absent := Project(dispositionInput(now, nil)); hasChoice(absent, "record_dispositions") || !hasRefusal(absent, "record_dispositions") {
		t.Fatalf("row 11 without an assembled set offered %+v", absent.Choices)
	}

	question := dispositionInput(now, set)
	question.Markers.CheckpointDecision = true
	if got := Project(question); got.Kind != KindAgentQuestion || got.Details.Dispositions != nil || !hasSecondary(got, KindVerificationDisposition) {
		t.Fatalf("row 10 = %s dispositions %+v", got.Kind, got.Details.Dispositions)
	}
	for name, other := range map[string]Input{"gate": gateInput(core.GateKindPlanApproval, now), "stalled": timedOut(now), "working": claimedAndQueued(now)} {
		other.Dispositions = map[string]DispositionInput{"v": {Set: set}, "o": {Set: set}, "c": {Set: set}}
		if got := Project(other); got.Details.Dispositions != nil {
			t.Fatalf("%s row carried the set", name)
		}
	}
}

func gateInput(kind core.GateKind, now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskAwaiting, GateKind: kind}, Now: now}
}

func conflictInput(exhausted bool, now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskApproved, MergeApproval: true}, MergeConflict: MergeConflict{Open: true, Exhausted: exhausted, NextRetryAt: now.Add(time.Minute)}, Now: now}
}

func mergingAndQuestion(now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskApproved}, PendingProposals: []ProposalRef{{Tier: "requirement", ID: "req", Version: 1}}, Markers: Markers{CheckpointDecision: true}, Orders: []core.WorkOrder{{ID: "q", State: core.WorkOrderQueued, LastAttemptID: "a", LastAttemptOutcome: core.WorkOrderOutcomeReleased}}, Now: now}
}

func questionAndVerify(now time.Time) Input {
	hold := now.Add(time.Hour)
	return Input{Task: core.Task{State: core.TaskRunning}, Markers: Markers{CheckpointDecision: true, VerificationDisposition: true, VerificationHoldDeadline: &hold}, Orders: []core.WorkOrder{{ID: "q", State: core.WorkOrderQueued, Stage: core.StageVerify, LastAttemptID: "a", LastAttemptOutcome: core.WorkOrderOutcomeReleased}}, Now: now}
}

func unsatisfiableAndSuppressed(now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderQueued, UnsatisfiableTaskIDs: []string{"dep"}, RetrySuppressed: true}}, Now: now}
}

func timedOut(now time.Time) Input {
	return Input{Task: core.Task{ID: "t", State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderTimedOut}}, Now: now}
}

func suppressedAndBlocked(now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderQueued, RetrySuppressed: true, BlockingTaskIDs: []string{"b"}}}, Blockers: []BlockerRef{{TaskID: "b", Title: "B"}}, Now: now}
}

func blockedClaimable(now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "blocked", State: core.WorkOrderQueued, BlockingTaskIDs: []string{"b"}, Claimable: true}, {ID: "free", State: core.WorkOrderQueued, Claimable: true}}, Now: now}
}

func claimedAndQueued(now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "c", State: core.WorkOrderClaimed}, {ID: "q", State: core.WorkOrderQueued, Claimable: true}}, Now: now}
}

func scheduled(now, later time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderQueued, Claimable: true}}, Recovery: map[string]AutomaticRecovery{"o": {NextAt: &later}}, Now: now}
}

func recoveryExhausted(now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, Orders: []core.WorkOrder{{ID: "o", State: core.WorkOrderTimedOut, RetrySuppressed: true, RetrySuppressionReason: core.AutomaticRecoveryExhaustedReason}}, Recovery: map[string]AutomaticRecovery{"o": {Exhausted: true}}, Now: now}
}

func verifyRefused(now time.Time) Input {
	return Input{Task: core.Task{ID: "t", State: core.TaskRunning, NextStage: core.StageImplement, SetupContract: core.Task{}.SetupContract}, Orders: []core.WorkOrder{{ID: "v", State: core.WorkOrderQueued, Stage: core.StageVerify, HeadSHA: "abc"}}, Now: now}
}

func dispositionInput(now time.Time, set []Disposition) Input {
	in := Input{Task: core.Task{ID: "t", State: core.TaskRunning}, Markers: Markers{VerificationDisposition: true}, Orders: []core.WorkOrder{{ID: "v", State: core.WorkOrderQueued, Stage: core.StageVerify, LastAttemptID: "a", LastAttemptOutcome: "released"}}, Now: now}
	if set != nil {
		in.Dispositions = map[string]DispositionInput{"v": {Set: set}}
	}
	return in
}

func hasRefusalText(na NextAction, id, text string) bool {
	for _, item := range na.Details.Refusals {
		if item.Choice == id && strings.Contains(item.Text, text) {
			return true
		}
	}
	return false
}

func allCaps() map[core.Capability]bool {
	return map[core.Capability]bool{core.CapabilityOperateGates: true, core.CapabilityRecoverWork: true, core.CapabilityRequestChanges: true, core.CapabilityConfirmDocuments: true}
}

func hasSecondary(na NextAction, kind string) bool {
	for _, item := range na.Details.Secondary {
		if item.Kind == kind {
			return true
		}
	}
	return false
}

func hasChoice(na NextAction, id string) bool {
	_, ok := choiceByID(na, id)
	return ok
}

func hasRefusal(na NextAction, id string) bool {
	for _, item := range na.Details.Refusals {
		if item.Choice == id && item.Text != "" {
			return true
		}
	}
	return false
}

func choiceByID(na NextAction, id string) (Choice, bool) {
	for _, choice := range na.Choices {
		if choice.ID == id {
			return choice, true
		}
	}
	return Choice{}, false
}

// TestCompactIgnoresDispositionSet proves the row-11 disposition set and its
// refusal feed only details and choices, never a compact field, so a
// compact-only list read may skip the per-order verification read
// (store.NextActionOptions.CompactOnly; component-next-action invariant 15).
func TestCompactIgnoresDispositionSet(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	bound := dispositionInput(now, []Disposition{{ContextID: "ctx", RunID: "run", OperationIDs: []string{"op"}, InputDigest: "digest"}, {ContextID: "ctx", RunID: "run-2", OperationIDs: []string{}, InputDigest: "digest"}})
	unread := dispositionInput(now, nil)
	unread.Dispositions = nil
	refused := dispositionInput(now, nil)
	refused.Dispositions = map[string]DispositionInput{"v": {Refusal: "verify order v has no sealed verification checkpoint to dispose"}}
	for name, in := range map[string]Input{"unread": unread, "refused": refused} {
		if got, want := CompactFrom(Project(in)), CompactFrom(Project(bound)); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s compact=%+v want %+v", name, got, want)
		}
	}
	if Project(bound).Kind != KindVerificationDisposition {
		t.Fatalf("fixture kind=%s", Project(bound).Kind)
	}
}

// TestGateSourceWording proves the recorded gate source and recovery stage
// select the gate's wording and choices. A triage job failure names triage,
// offers Retry and reject, and its question uses no review words. A bounce
// limit another stage reached names that stage, and its Approve is withheld
// with the route's refusal code because no review reached it. A review-sourced
// limit, from review's invalid output or recorded without a source, keeps the
// review wording and Approve. A source cut to its first characters is never
// read as review's, and no sentence interpolates the source
// (component-triage invariant 26; component-next-action invariants 7, 8,
// 17-20; req-intake-and-triage AC-3.4; req-operator-next-action AC-1.2,
// AC-1.4, AC-1.8).
func TestGateSourceWording(t *testing.T) {
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	gate := func(kind core.GateKind, source string, recovery core.Stage, count int) Input {
		return Input{Task: core.Task{ID: "t", State: core.TaskAwaiting, GateKind: kind, RecoveryStage: recovery}, BounceCount: count, GateSource: source, Now: now}
	}
	reviewChoices := []string{"Approve", "Ask for changes…", "Reject"}
	withheld := []string{"Ask for changes…", "Reject"}
	retry := []string{"Retry", "Reject"}
	cut := core.ReviewOutputInvalidSource + strings.Repeat("x", 64-len(core.ReviewOutputInvalidSource))
	for _, tc := range []struct {
		name, source, label, summary, question, runGate string
		kind                                            core.GateKind
		recovery                                        core.Stage
		count                                           int
		choices                                         []string
	}{
		{"triage invalid output job failure", "triage.output_invalid", "Retry triage", "Triage returned invalid output until the bounce limit, so it has no outcome.", "Retry triage, or reject the task?", "human recovery gate", core.GateKindJobFailure, core.StageTriage, 3, retry},
		{"triage provider job failure", "", "Retry triage", "Triage failed, so it has no outcome.", "Retry triage, or reject the task?", "human recovery gate", core.GateKindJobFailure, core.StageTriage, 0, retry},
		{"sourceless job failure", "", "Job failure", "A job failed and needs a person.", "", "human recovery gate", core.GateKindJobFailure, core.StageImplement, 0, withheld},
		{"other job failure source", "spec.output_invalid", "Job failure", "A job failed and needs a person.", "", "human recovery gate", core.GateKindJobFailure, core.StageSpec, 0, withheld},
		{"planning bounce limit", "spec.output_invalid", "Settle planning output", "Planning returned invalid output until the bounce limit.", "Planning returned invalid output 3 times. Ask for changes, or reject?", "Settle planning output", core.GateKindReviewBounceLimit, core.StageSpec, 3, withheld},
		{"planning bounce limit without a count", "spec.output_invalid", "Settle planning output", "Planning returned invalid output until the bounce limit.", "Planning returned invalid output until the bounce limit. Ask for changes, or reject?", "Settle planning output", core.GateKindReviewBounceLimit, core.StageSpec, 0, withheld},
		{"implementation bounce limit", "implement.output_invalid", "Settle implementation output", "Implementation returned invalid output until the bounce limit.", "Implementation returned invalid output 2 times. Ask for changes, or reject?", "Settle implementation output", core.GateKindReviewBounceLimit, core.StageImplement, 2, withheld},
		{"triage bounce limit", "triage.output_invalid", "Settle triage output", "Triage returned invalid output until the bounce limit.", "Triage returned invalid output 3 times. Ask for changes, or reject?", "Settle triage output", core.GateKindReviewBounceLimit, core.StageTriage, 3, withheld},
		{"unnamed stage bounce limit", "custom.output_invalid", "Settle invalid output", "A stage returned invalid output until the bounce limit.", "A stage returned invalid output 3 times. Ask for changes, or reject?", "Settle invalid output", core.GateKindReviewBounceLimit, core.StageImplement, 3, withheld},
		{"review bounce limit", "", "Settle review", "Review bounced until the limit.", "Review bounced 3 times. Approve, ask for changes, or reject?", "Settle review", core.GateKindReviewBounceLimit, core.StageImplement, 3, reviewChoices},
		{"review invalid output bounce limit", core.ReviewOutputInvalidSource, "Settle review", "Review bounced until the limit.", "Review bounced 3 times. Approve, ask for changes, or reject?", "Settle review", core.GateKindReviewBounceLimit, core.StageReview, 3, reviewChoices},
		{"other source bounce limit", "mcp-review", "Settle bounce limit", "A stage reached the bounce limit.", "A stage reached the bounce limit. Ask for changes, or reject?", "Settle bounce limit", core.GateKindReviewBounceLimit, core.StageImplement, 3, withheld},
		{"cut source bounce limit", cut, "Settle bounce limit", "A stage reached the bounce limit.", "A stage reached the bounce limit. Ask for changes, or reject?", "Settle bounce limit", core.GateKindReviewBounceLimit, core.StageImplement, 3, withheld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := gate(tc.kind, tc.source, tc.recovery, tc.count)
			got := Project(in)
			if got.Kind != string(tc.kind) || got.Details.GateKind != string(tc.kind) || got.Actor != ActorOperator || got.Judgment != (tc.kind != core.GateKindJobFailure) {
				t.Fatalf("statement kind=%s gate=%s actor=%s judgment=%t", got.Kind, got.Details.GateKind, got.Actor, got.Judgment)
			}
			if got.Label != tc.label || got.Summary != tc.summary || got.Question != tc.question {
				t.Fatalf("label/summary/question = %q / %q / %q, want %q / %q / %q", got.Label, got.Summary, got.Question, tc.label, tc.summary, tc.question)
			}
			entry, ok := LookupVariant(string(tc.kind), "", "", string(tc.kind), GateVariant(tc.kind, tc.source, tc.recovery))
			if !ok || entry.RunGate == nil || entry.RunGate.Label != tc.runGate {
				t.Fatalf("run gate = %+v, want label %q", entry.RunGate, tc.runGate)
			}
			labels := make([]string, 0, len(got.Choices))
			for _, choice := range got.Choices {
				labels = append(labels, choice.Label)
				if choice.ID == "redirect" && tc.label == "Retry triage" && choice.Effect != "Runs triage again with the guidance you give." {
					t.Fatalf("retry effect = %q", choice.Effect)
				}
			}
			if !reflect.DeepEqual(labels, tc.choices) {
				t.Fatalf("choice labels = %q, want %q", labels, tc.choices)
			}
			approveOffered := slices.Contains(labels, "Approve")
			wantRefusals := []Refusal{{Choice: "approve", Text: core.ApproveWithoutReviewRefusal, Code: core.ApproveWithoutReviewCode}}
			if approveOffered {
				wantRefusals = nil
			}
			if !reflect.DeepEqual(got.Details.Refusals, wantRefusals) {
				t.Fatalf("refusals = %+v, want %+v", got.Details.Refusals, wantRefusals)
			}
			// The offer and the route share one predicate.
			if routeRefuses := core.NextActionUnreviewedApproveRefusal(in.Task, tc.source) != ""; routeRefuses == approveOffered {
				t.Fatalf("route refuses=%t while the statement offers approve=%t", routeRefuses, approveOffered)
			}
			for _, text := range []string{got.Label, got.Summary, got.Question, got.IfIgnored} {
				if tc.source != "" && strings.Contains(text, tc.source) {
					t.Fatalf("sentence %q interpolates the source %q", text, tc.source)
				}
				if !approveOffered && strings.Contains(text, "Approve") {
					t.Fatalf("sentence %q offers the withheld approve", text)
				}
				if !strings.HasPrefix(tc.source, "review.") && tc.label != "Settle review" && tc.label != "Job failure" && strings.Contains(strings.ToLower(text), "review") {
					t.Fatalf("a %s gate names review: %q", tc.source, text)
				}
			}
			for _, refusal := range got.Details.Refusals {
				if strings.Contains(refusal.Text, refusal.Code) || strings.Contains(refusal.Text, "_") {
					t.Fatalf("refusal %q interpolates an identifier", refusal.Text)
				}
			}
		})
	}
	for _, variant := range GateVariants() {
		kind := KindReviewBounceLimit
		if variant == TriageInvalidOutput || variant == TriageFailure {
			kind = KindJobFailure
		}
		entry, ok := LookupVariant(kind, "", "", kind, variant)
		base, _ := Lookup(kind, "", "", "")
		if !ok || entry.Label == "" || entry.Label == base.Label || entry.Summary == nil || entry.Question == nil || entry.IfIgnored == nil || entry.RunGate == nil {
			t.Fatalf("gate variant %s missing", variant)
		}
	}
}

// TestUnreviewedApproveRefusal pins the shared predicate: an approve is
// refused at every job_failure and at a review_bounce_limit whose recorded
// source is not review's, and admitted at a review-sourced limit and at plan
// approval, plan revision, and merge approval (component-next-action
// invariant 7; req-operator-next-action AC-1.4).
func TestUnreviewedApproveRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		task   core.Task
		source string
		refuse bool
	}{
		{"review round limit without a source", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindReviewBounceLimit}, "", false},
		{"review invalid output limit", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindReviewBounceLimit}, core.ReviewOutputInvalidSource, false},
		{"planning invalid output limit", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindReviewBounceLimit}, "spec.output_invalid", true},
		{"verification invalid output limit", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindReviewBounceLimit}, "verify.output_invalid", true},
		{"cut source limit", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindReviewBounceLimit}, core.ReviewOutputInvalidSource + strings.Repeat("x", 43), true},
		{"job failure with a reviewed head", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindJobFailure, ReviewedHeadSHA: "head"}, "", true},
		{"triage job failure", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindJobFailure, RecoveryStage: core.StageTriage}, "triage.output_invalid", true},
		{"awaiting without a gate kind", core.Task{State: core.TaskAwaiting}, "", true},
		{"plan approval", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval}, "spec.output_invalid", false},
		{"plan revision", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindPlanRevision}, "", false},
		{"merge approval", core.Task{State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval}, "", false},
		{"not awaiting", core.Task{State: core.TaskRunning, GateKind: core.GateKindJobFailure}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := core.NextActionUnreviewedApproveRefusal(tc.task, tc.source)
			if (got != "") != tc.refuse || (tc.refuse && got != core.ApproveWithoutReviewRefusal) {
				t.Fatalf("refusal=%q want refuse=%t", got, tc.refuse)
			}
		})
	}
}

// TestRetrySummaryCarriesNoTime proves a scheduled retry's summary keeps the
// attempt number and carries no RFC 3339 time, while the time stays the
// statement's deadline (component-next-action invariants 14, 17, 18).
func TestRetrySummaryCarriesNoTime(t *testing.T) {
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	later := now.Add(10 * time.Minute)
	stamp := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`)
	recovery := scheduled(now, later)
	recovery.Recovery = map[string]AutomaticRecovery{"o": {NextAt: &later, Attempt: 2}}
	for _, tc := range []struct {
		name, summary string
		in            Input
	}{
		{"automatic recovery", "Conveyor will retry this automatically. The next retry is attempt 2.", recovery},
		{"merge conflict", "Conveyor will retry this automatically. A retry is scheduled.", conflictInput(false, now)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Project(tc.in)
			if got.Kind != KindRetrying || got.Summary != tc.summary || got.IfIgnored != "The next retry still runs." {
				t.Fatalf("summary=%q if_ignored=%q, want %q", got.Summary, got.IfIgnored, tc.summary)
			}
			if got.Deadline == nil || !got.Deadline.After(now) {
				t.Fatalf("deadline=%v, want the retry time", got.Deadline)
			}
			for _, text := range []string{got.Label, got.Summary, got.Question, got.IfIgnored} {
				if stamp.MatchString(text) {
					t.Fatalf("sentence %q carries an RFC 3339 time", text)
				}
			}
		})
	}
	past := conflictInput(false, now)
	past.MergeConflict.NextRetryAt = now.Add(-time.Minute)
	if got := Project(past); got.Summary != "Conveyor will retry this automatically." {
		t.Fatalf("a passed retry time summary=%q", got.Summary)
	}
}

// TestAttemptUserIsTheRunClaimant pins that the current attempt names the
// user of a run:<user id> claimant, never the task assignee, and no user for
// any other claimant (req-operator-next-action AC-7.6).
func TestAttemptUserIsTheRunClaimant(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, claimant string
		assignee       *core.TaskAssignee
		want           string
	}{
		{name: "unassigned run claimant", claimant: "run:usr_selam", want: "usr_selam"},
		{name: "claimant differs from assignee", claimant: "run:usr_selam", assignee: &core.TaskAssignee{UserID: "usr_kidus"}, want: "usr_selam"},
		{name: "non-run claimant", claimant: "worker-1", assignee: &core.TaskAssignee{UserID: "usr_kidus"}, want: ""},
		{name: "empty run claimant", claimant: "run:", assignee: &core.TaskAssignee{UserID: "usr_kidus"}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{Task: core.Task{State: core.TaskRunning}, Assignee: tc.assignee, Orders: []core.WorkOrder{{ID: "c", State: core.WorkOrderClaimed, ClaimantID: tc.claimant}}, Now: now}
			got := Project(in)
			if got.Kind != KindWorking || got.Attempt == nil {
				t.Fatalf("kind=%s attempt=%+v", got.Kind, got.Attempt)
			}
			if got.Attempt.User != tc.want || got.Attempt.ClaimedBy != tc.claimant {
				t.Fatalf("attempt user=%q claimed_by=%q, want user %q", got.Attempt.User, got.Attempt.ClaimedBy, tc.want)
			}
		})
	}
}
