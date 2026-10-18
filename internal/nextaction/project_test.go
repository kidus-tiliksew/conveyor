package nextaction

import (
	"encoding/json"
	"reflect"
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
		{name: "row8", in: waitingAndQuestion(now), actor: ActorOperator, kind: KindDocumentProposal, secondary: KindAgentQuestion},
		{name: "row9-10", in: questionAndVerify(now), actor: ActorOperator, kind: KindAgentQuestion, secondary: KindVerificationDisposition},
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
	for _, kind := range []string{KindParked, KindPlanApproval, KindPlanRevision, KindMergeApproval, KindReviewBounceLimit, KindMergeExecution, KindDocumentProposal, KindContextProposal, KindAgentQuestion, KindVerificationDisposition, KindDependencyUnsatisfiable} {
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
		{"job failure", "Job failure", "", "The failure stays at the human gate.", gate(core.GateKindJobFailure, nil), []string{"Approve", "Ask for changes…", "Reject"}},
		{"merge execution", "Merge branch", "Merge pull request #1312?", "Conveyor does not merge it.", Input{Task: core.Task{ID: "t", State: core.TaskApproved, MergeApproval: true}, PullRequest: 1312, Blocks: dependents, Now: now}, []string{"Merge", "Request changes…", "Close…"}},
		{"parked", "Decide on a parked task", "Put it back, or close it?", "It stays parked.", Input{Task: core.Task{ID: "t", State: core.TaskParked}, Markers: Markers{ParkCommand: "triage.park"}, Now: now}, []string{"Close", "Start over"}},
		{"verification disposition", "Confirm an outcome", "Did the change apply? The verify agent could not observe the result.", "The verify order stays at the checkpoint.", dispositionInput(now, &Disposition{ContextID: "ctx", RunID: "run", OperationIDs: []string{"op"}, InputDigest: "digest"}), []string{"Yes, it applied…", "No, it didn't…"}},
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
			typed := choice.ID == "redirect" || choice.ID == "decline" || choice.ID == "request_changes" || choice.ID == "answer" || choice.ID == "applied" || choice.ID == "not_applied" || (kind == KindMergeExecution && choice.ID == "close")
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

// TestDispositionBinding proves details.disposition appears only on row 10,
// copied from the selected verify order's binding, with applied and
// not_applied allowed by operate_gates alone. Without a binding both choices
// are omitted and refused with the assembler's reason. A higher row never
// carries the binding (feature-operator-next-action NA-3, Disposition
// binding; req-operator-next-action AC-1.4, AC-2.2).
func TestDispositionBinding(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	binding := &Disposition{ContextID: "ctx", RunID: "run", OperationIDs: []string{"op-a", "op-b"}, InputDigest: "digest"}
	in := dispositionInput(now, binding)
	got := Project(in)
	if got.Kind != KindVerificationDisposition || !reflect.DeepEqual(got.Details.Disposition, binding) || got.Details.Disposition == binding {
		t.Fatalf("row 10 = %s disposition %+v", got.Kind, got.Details.Disposition)
	}
	for _, id := range []string{"applied", "not_applied"} {
		choice, ok := choiceByID(got, id)
		if !ok || choice.Capability != string(core.CapabilityOperateGates) || choice.Allowed || choice.Path != "/v1/work-orders/v/recover" {
			t.Fatalf("choice %s = %+v ok=%t", id, choice, ok)
		}
	}
	in.Capabilities = map[core.Capability]bool{core.CapabilityRecoverWork: true}
	if choice, _ := choiceByID(Project(in), "applied"); choice.Allowed {
		t.Fatal("recover_work allowed a disposition")
	}
	in.Capabilities = map[core.Capability]bool{core.CapabilityOperateGates: true}
	if choice, _ := choiceByID(Project(in), "not_applied"); !choice.Allowed {
		t.Fatal("operate_gates did not allow a disposition")
	}

	refused := dispositionInput(now, nil)
	refused.Dispositions = map[string]DispositionInput{"v": {Refusal: "verification checkpoint ctx has no unresolved operations to dispose"}}
	missing := Project(refused)
	if missing.Details.Disposition != nil || hasChoice(missing, "applied") || hasChoice(missing, "not_applied") {
		t.Fatalf("unbound row 10 offered %+v disposition %+v", missing.Choices, missing.Details.Disposition)
	}
	for _, id := range []string{"applied", "not_applied"} {
		if !hasRefusalText(missing, id, "no unresolved operations") {
			t.Fatalf("refusals = %+v, want %s refused for missing operations", missing.Details.Refusals, id)
		}
	}
	if absent := Project(dispositionInput(now, nil)); hasChoice(absent, "applied") || !hasRefusal(absent, "not_applied") {
		t.Fatalf("row 10 without an assembled binding offered %+v", absent.Choices)
	}

	question := dispositionInput(now, binding)
	question.Markers.CheckpointDecision = true
	if got := Project(question); got.Kind != KindAgentQuestion || got.Details.Disposition != nil || !hasSecondary(got, KindVerificationDisposition) {
		t.Fatalf("row 9 = %s disposition %+v", got.Kind, got.Details.Disposition)
	}
	for name, other := range map[string]Input{"gate": gateInput(core.GateKindPlanApproval, now), "stalled": timedOut(now), "working": claimedAndQueued(now)} {
		other.Dispositions = map[string]DispositionInput{"v": {Binding: binding}, "o": {Binding: binding}, "c": {Binding: binding}}
		if got := Project(other); got.Details.Disposition != nil {
			t.Fatalf("%s row carried the binding", name)
		}
	}
}

func gateInput(kind core.GateKind, now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskAwaiting, GateKind: kind}, Now: now}
}

func conflictInput(exhausted bool, now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskApproved, MergeApproval: true}, MergeConflict: MergeConflict{Open: true, Exhausted: exhausted, NextRetryAt: now.Add(time.Minute)}, Now: now}
}

func waitingAndQuestion(now time.Time) Input {
	return Input{Task: core.Task{State: core.TaskRunning}, ClaimWaiting: []ProposalRef{{Tier: "requirement", ID: "req", Version: 1}}, Markers: Markers{CheckpointDecision: true}, Orders: []core.WorkOrder{{ID: "q", State: core.WorkOrderQueued, LastAttemptID: "a", LastAttemptOutcome: core.WorkOrderOutcomeReleased}}, Now: now}
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

func dispositionInput(now time.Time, binding *Disposition) Input {
	in := Input{Task: core.Task{ID: "t", State: core.TaskRunning}, Markers: Markers{VerificationDisposition: true}, Orders: []core.WorkOrder{{ID: "v", State: core.WorkOrderQueued, Stage: core.StageVerify, LastAttemptID: "a", LastAttemptOutcome: "released"}}, Now: now}
	if binding != nil {
		in.Dispositions = map[string]DispositionInput{"v": {Binding: binding}}
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

// TestCompactIgnoresDispositionBinding proves the row-10 binding and its
// refusal feed only details and choices, never a compact field, so a
// compact-only list read may skip the per-order verification read
// (store.NextActionOptions.CompactOnly; feature-operator-next-action NA-3).
func TestCompactIgnoresDispositionBinding(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	bound := dispositionInput(now, &Disposition{ContextID: "ctx", RunID: "run", OperationIDs: []string{"op"}, InputDigest: "digest"})
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
