package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Cross-implementation conformance for requirement documents and planning
// sessions (design-document-corpus). Requirements are versioned
// and confirmed, never gated: this suite is what proves the in-memory store and
// Postgres agree on that, so a behaviour an operator relies on cannot hold in
// one deployment and quietly fail in the other.
//
// Assertions are on behaviour, not on error text. The two implementations reach
// the same rejections by different routes — the in-memory store pre-checks
// existence and uniqueness while Postgres relies on primary keys, unique
// indexes, foreign keys, and CHECK constraints — so the contract is "this is
// refused", not "refused with this sentence".

// RequirementFixture is one isolated workspace on a live Store.
type RequirementFixture struct {
	Store     store.Store
	Context   context.Context
	Workspace string
}

// RequirementFactory provisions a fresh workspace per subtest, mirroring
// BlueprintFactory so both suites share one harness shape.
type RequirementFactory func(*testing.T, []config.Repo) RequirementFixture

type planningSessionEventStore interface {
	ListPlanningSessionEvents(context.Context, string) ([]core.Event, error)
}

// requirementConformanceActor is the confirming operator. Confirmation records
// identity, so the suite owns the actor rather than depending on
// whatever each harness happens to install in its context.
const requirementConformanceActor = "operator-conformance"

// requirementCanonicalParts is written in the exact byte form a Postgres jsonb
// column renders — object keys sorted, one space after each colon — so both
// implementations hand back identical bytes and the suite can assert byte
// identity of a restored AI SDK part, not merely that it decodes the same.
const requirementCanonicalParts = `[{"text": "Stream restored verbatim.", "type": "text"}]`

var requirementConformanceRepos = []config.Repo{
	{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"},
}

// RunRequirementConformance exercises the externally visible requirement and
// planning-session persistence contract against any Store implementation
func RunRequirementConformance(t *testing.T, factory RequirementFactory) {
	t.Helper()

	t.Run("task context proposals unify requirement and design confirmation", func(t *testing.T) {
		fixture := factory(t, requirementConformanceRepos)
		st, ctx := fixture.Store, store.WithActor(fixture.Context, store.Actor{ID: requirementConformanceActor, Role: core.ActorUser})
		requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Context proposal"},
			core.RequirementVersion{Content: "Context proposal", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Confirm context."}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
			t.Fatal(err)
		}
		design, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-" + core.NewTaskID(), Title: "Context design", Category: "Architecture"},
			core.SystemDesignVersion{Content: "# Context\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version); err != nil {
			t.Fatal(err)
		}
		taskID := core.NewTaskID()
		if err = st.CreateTask(ctx, core.Task{ID: taskID, Workspace: fixture.Workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		for _, input := range []core.TaskContextProposalInput{
			{TaskID: taskID, TargetKind: core.TaskContextProposalRequirement, TargetID: requirement.ID, Source: core.TaskContextProposalTriage, Justification: "REQ-1 serves this task."},
			{TaskID: taskID, TargetKind: core.TaskContextProposalSystemDesign, TargetID: design.ID, Source: core.TaskContextProposalPlanning, Justification: "The design governs this path."},
		} {
			proposal, suppressed, proposeErr := st.ProposeTaskContext(ctx, input)
			if proposeErr != nil || suppressed || proposal.Justification != input.Justification {
				t.Fatalf("proposal=%+v suppressed=%t err=%v", proposal, suppressed, proposeErr)
			}
		}
		pending, err := st.ListPendingProposals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		contextPending := 0
		for _, proposal := range pending {
			if proposal.Tier == "task_context" && proposal.OriginType == "task" && proposal.OriginID == taskID && proposal.Justification != "" {
				contextPending++
			}
		}
		if contextPending != 2 {
			t.Fatalf("pending proposals=%+v", pending)
		}
		projection, err := st.PendingProposalsProjection(ctx)
		if err != nil || len(projection.Items) != len(pending) {
			t.Fatalf("projection=%+v err=%v", projection, err)
		}
		if _, err = st.ConfirmTaskContextProposal(ctx, taskID, core.TaskContextProposalRequirement, requirement.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = st.ConfirmTaskContextProposal(ctx, taskID, core.TaskContextProposalSystemDesign, design.ID); err != nil {
			t.Fatal(err)
		}
		attached, err := store.TaskContextForTask(ctx, st, taskID)
		if err != nil || len(attached.Requirements) != 1 || len(attached.Designs) != 1 || attached.Designs[0].Version != designVersion.Version || len(attached.Proposals) != 0 {
			t.Fatalf("attached=%+v err=%v", attached, err)
		}
		pending, err = st.ListPendingProposals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, proposal := range pending {
			if proposal.Tier == "task_context" && proposal.OriginID == taskID {
				t.Fatalf("resolved proposal remained pending: %+v", proposal)
			}
		}
	})

	t.Run("requirement staleness acknowledgments are durable audited events", func(t *testing.T) {
		st, ctx, _ := newRequirementFixture(t, factory)
		requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-staleness-ack", Title: "Staleness acknowledgment"}, core.RequirementVersion{
			Content: "Audited judgment.", Statements: []core.RequirementStatement{requirementStatement("REQ-1", "Operators can acknowledge a delivery signal.")}, Origin: core.RequirementOriginOperator,
		})
		if err != nil {
			t.Fatal(err)
		}
		through := time.Now().UTC().Add(-time.Minute)
		ack, err := st.AcknowledgeRequirementStaleness(ctx, core.RequirementStalenessAcknowledgment{
			RequirementID: requirement.ID, SignalID: "signal-1", DeliveryTaskID: "delivery-1",
			DeliveryEventID: 42, AcknowledgedThrough: through,
		})
		if err != nil {
			t.Fatal(err)
		}
		if ack.AcknowledgedBy != requirementConformanceActor || ack.AcknowledgedAt.IsZero() || !sameInstant(ack.AcknowledgedThrough, through) {
			t.Fatalf("acknowledgment=%+v", ack)
		}
		events, err := st.ListRequirementEvents(ctx, requirement.ID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, event := range events {
			if event.Kind != "requirement.staleness_acknowledged" {
				continue
			}
			var payload core.RequirementStalenessAcknowledgment
			if err = json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			found = event.ActorID == requirementConformanceActor && event.ActorRole == core.ActorHuman &&
				payload.SignalID == "signal-1" && payload.DeliveryEventID == 42 && sameInstant(payload.AcknowledgedThrough, through) &&
				sameInstant(payload.AcknowledgedAt, ack.AcknowledgedAt)
		}
		if !found {
			t.Fatalf("audited acknowledgment missing from events: %+v", events)
		}
	})

	t.Run("drift resolution can attach a confirmed requirement", func(t *testing.T) {
		st, ctx, workspace := newRequirementFixture(t, factory)
		monitorStore, ok := st.(monitor.Store)
		if !ok {
			t.Fatal("store does not implement monitor.Store")
		}
		confirmed, _, err := st.CreateRequirement(ctx,
			core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Confirmed runtime intent"},
			chatVersion("Runtime intent.", requirementStatement("REQ-1", "External changes remain traceable.")))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, confirmed.ID, 1); err != nil {
			t.Fatal(err)
		}
		pending, _ := createRequirement(t, ctx, st, "Pending runtime intent",
			chatVersion("Pending intent.", requirementStatement("REQ-1", "Pending intent stays unconfirmed.")))
		taskID := core.NewTaskID()
		if err = st.CreateTask(ctx, core.Task{
			ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main",
			Branch: "conveyor/task-" + taskID, Title: "Reconcile drift", State: core.TaskQueued,
		}); err != nil {
			t.Fatal(err)
		}
		drift := monitor.Drift{
			ID: "drift-" + core.NewTaskID(), WorkspaceID: workspace, Repository: "conveyor",
			Kind: monitor.ExternalPRMerge, SourceURL: "https://example.test/pull/355", CommitSHA: "abc355",
			TaskID: taskID, DetectedAt: time.Now().UTC(),
		}
		if _, fresh, recordErr := monitorStore.RecordDrift(ctx, drift); recordErr != nil || !fresh {
			t.Fatalf("record drift fresh=%t err=%v", fresh, recordErr)
		}
		for _, testCase := range []struct {
			name, outcome, requirementID string
			target                       error
		}{
			{"missing", "requirements_amended", "", monitor.ErrRequirementIDMissing},
			{"unknown", "requirements_amended", "req-missing", monitor.ErrUnknownRequirementID},
			{"pending", "requirements_amended", pending.ID, monitor.ErrRequirementIDInvalid},
			{"irrelevant outcome", "conflict_resolved", confirmed.ID, monitor.ErrRequirementIDNotAllowed},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				if _, resolveErr := monitorStore.ResolveDrift(ctx, drift.ID, testCase.outcome, testCase.requirementID); !errors.Is(resolveErr, testCase.target) {
					t.Fatalf("resolve error=%v, want %v", resolveErr, testCase.target)
				}
				persisted, fresh, recordErr := monitorStore.RecordDrift(ctx, drift)
				if recordErr != nil || fresh || persisted.RequirementID != "" || !persisted.ResolvedAt.IsZero() || persisted.Outcome != "" {
					t.Fatalf("failed resolution mutated drift: drift=%+v fresh=%t err=%v", persisted, fresh, recordErr)
				}
				versions, versionsErr := st.ListRequirementVersions(ctx, confirmed.ID)
				if versionsErr != nil || len(versions) != 1 {
					t.Fatalf("failed resolution mutated versions=%+v err=%v", versions, versionsErr)
				}
				pendingVersions, pendingErr := st.ListRequirementVersions(ctx, pending.ID)
				if pendingErr != nil || len(pendingVersions) != 1 {
					t.Fatalf("failed resolution mutated pending versions=%+v err=%v", pendingVersions, pendingErr)
				}
				events, eventsErr := st.ListEvents(ctx, taskID)
				if eventsErr != nil {
					t.Fatal(eventsErr)
				}
				for _, event := range events {
					if event.Kind == "monitor.drift_reconciled" {
						t.Fatalf("failed resolution emitted reconciliation event: %+v", event)
					}
				}
			})
		}
		resolved, err := monitorStore.ResolveDrift(ctx, drift.ID, "requirements_amended", confirmed.ID)
		if err != nil || resolved.RequirementID != confirmed.ID || resolved.Outcome != "requirements_amended" || resolved.ResolvedAt.IsZero() {
			t.Fatalf("resolved drift=%+v err=%v", resolved, err)
		}
		repeated, err := monitorStore.ResolveDrift(ctx, drift.ID, "requirements_amended", confirmed.ID)
		if err != nil || repeated.RequirementID != confirmed.ID || !sameInstant(repeated.ResolvedAt, resolved.ResolvedAt) {
			t.Fatalf("repeated resolution=%+v err=%v", repeated, err)
		}
		versions, err := st.ListRequirementVersions(ctx, confirmed.ID)
		if err != nil || len(versions) != 2 || versions[1].OriginDriftID != drift.ID || versions[1].Confirmed {
			t.Fatalf("amendment versions=%+v err=%v", versions, err)
		}
		events, err := st.ListEvents(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		reconciled := 0
		for _, event := range events {
			if event.Kind != "monitor.drift_reconciled" {
				continue
			}
			reconciled++
			var payload map[string]any
			if err = json.Unmarshal(event.Payload, &payload); err != nil || payload["requirement_id"] != confirmed.ID {
				t.Fatalf("reconciliation payload=%s err=%v", event.Payload, err)
			}
		}
		if reconciled != 1 {
			t.Fatalf("reconciliation event count=%d, want 1", reconciled)
		}
	})

	t.Run("planning uploads follow the produced entity on finalize", func(t *testing.T) {
		for _, target := range []string{"requirement", "task"} {
			t.Run(target, func(t *testing.T) {
				st, ctx, workspace := newRequirementFixture(t, factory)
				session := createPlanningSession(t, ctx, st)
				upload, err := st.CreateArtifact(ctx, core.Artifact{
					Name: "planning-context.txt", ContentType: "text/plain",
					Role: core.ArtifactRoleTaskContext, PlanningSessionID: session.ID,
				}, []byte("planning context"))
				if err != nil {
					t.Fatal(err)
				}
				request := store.PlanningFinalizeRequest{SessionID: session.ID}
				if target == "requirement" {
					requirement, _ := createRequirement(t, ctx, st, "Produced requirement",
						chatVersion("Produced prose.", requirementStatement("REQ-1", "Produced intent is explicit.")))
					request.RequirementID = requirement.ID
				} else {
					taskID := core.NewTaskID()
					if err = st.CreateTask(ctx, core.Task{
						ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main",
						Branch: "conveyor/task-" + taskID, Title: "Produced blueprint", State: core.TaskAwaiting,
					}); err != nil {
						t.Fatal(err)
					}
					request.TaskID = taskID
				}
				if _, err = st.FinalizePlanningSession(ctx, request); err != nil {
					t.Fatal(err)
				}
				artifacts, err := st.ListArtifacts(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var rehomed core.Artifact
				for _, artifact := range artifacts {
					if artifact.ID == upload.ID {
						rehomed = artifact
					}
				}
				if rehomed.ID == "" || rehomed.PlanningSessionID != "" || rehomed.RequirementID != request.RequirementID || rehomed.TaskID != request.TaskID {
					t.Fatalf("rehomed artifact=%+v request=%+v", rehomed, request)
				}
			})
		}
	})

	t.Run("operator proposals use the common pending confirmation and high-water contract", func(t *testing.T) {
		st, ctx, _ := newRequirementFixture(t, factory)
		requirement, first, err := st.CreateRequirement(ctx,
			core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Operator REST parity"},
			core.RequirementVersion{Content: "First operator proposal.", Origin: core.RequirementOriginOperator,
				Statements: []core.RequirementStatement{requirementStatement("REQ-2", "Initial operator intent.")}})
		if err != nil {
			t.Fatal(err)
		}
		if requirement.CurrentVersion != 0 || first.Confirmed {
			t.Fatalf("new operator proposal requirement=%+v version=%+v", requirement, first)
		}
		confirmed, confirmedVersion, err := st.ConfirmRequirementVersion(ctx, requirement.ID, first.Version)
		if err != nil || confirmed.CurrentVersion != first.Version || !confirmedVersion.Confirmed {
			t.Fatalf("confirm operator proposal requirement=%+v version=%+v err=%v", confirmed, confirmedVersion, err)
		}
		second, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: requirement.ID, Content: "Second operator proposal.", Origin: core.RequirementOriginOperator,
			Statements: []core.RequirementStatement{requirementStatement("REQ-3", "Later operator intent.")},
		})
		if err != nil || second.Version != 2 || second.Confirmed {
			t.Fatalf("second operator proposal=%+v err=%v", second, err)
		}
		if _, err = st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: requirement.ID, Content: "Recycled operator proposal.", Origin: core.RequirementOriginOperator,
			Statements: []core.RequirementStatement{requirementStatement("REQ-1", "Recycled operator intent.")},
		}); err == nil || !strings.Contains(err.Error(), "reuses a retired identifier") {
			t.Fatalf("recycled operator proposal error=%v", err)
		}
	})

	t.Run("monitor requirement references are validated before persistence", func(t *testing.T) {
		st, ctx, workspace := newRequirementFixture(t, factory)
		monitorStore, ok := st.(monitor.Store)
		if !ok {
			t.Fatal("store does not implement monitor.Store")
		}
		intakeCalls := 0
		service := &monitor.Service{
			Store: monitorStore, WorkspaceID: workspace, Enabled: true,
			Repositories: map[string]struct{}{"conveyor": {}},
			Intake: func(context.Context, monitor.TaskRequest) (monitor.IntakeResult, error) {
				intakeCalls++
				taskID := core.NewTaskID()
				task := core.Task{
					ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main",
					Branch: "conveyor/task-" + taskID, Title: "Monitor task", State: core.TaskQueued,
				}
				return monitor.IntakeResult{Task: task, Created: true}, st.CreateTask(ctx, task)
			},
		}
		observation := monitor.Observation{
			Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: "unknown-requirement",
			SourceURL: "https://example.test/commit/unknown", CommitSHA: "unknown", RequirementID: "req-missing",
		}
		if _, err := service.Process(ctx, observation); !errors.Is(err, monitor.ErrUnknownRequirementID) || !strings.Contains(err.Error(), observation.RequirementID) {
			t.Fatalf("unknown requirement error=%v", err)
		}
		if intakeCalls != 0 {
			t.Fatalf("unknown requirement reached intake %d times", intakeCalls)
		}
		requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-known", Title: "Known intent"},
			chatVersion("Known intent remains valid.", requirementStatement("REQ-1", "Monitor references resolve.")))
		if err != nil {
			t.Fatal(err)
		}
		observation.RequirementID, observation.OccurrenceID = requirement.ID, "known-requirement"
		if _, err = service.Process(ctx, observation); err != nil {
			t.Fatalf("known requirement rejected: %v", err)
		}
		if intakeCalls != 1 {
			t.Fatalf("known requirement intake calls=%d, want 1", intakeCalls)
		}
	})

	t.Run("content and statement fence divergence is rejected", func(t *testing.T) {
		st, ctx, _ := newRequirementFixture(t, factory)
		_, _, err := st.CreateRequirement(ctx, core.Requirement{
			ID: "req-" + core.NewTaskID(), Title: "Divergent requirement",
		}, core.RequirementVersion{
			Content:    "Operator prose.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Fence statement.\n```",
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Different supplied statement."}},
			Origin:     core.RequirementOriginChat, OriginSessionID: "session-divergent",
		})
		if err == nil || !strings.Contains(err.Error(), "diverge") {
			t.Fatalf("divergent content/statements error=%v", err)
		}
	})

	t.Run("serves links remain proposals until an operator decision", func(t *testing.T) {
		st, ctx, workspace := newRequirementFixture(t, factory)
		requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Served intent"},
			chatVersion("Blueprints serve confirmed intent.", requirementStatement("REQ-1", "Serves links are operator-confirmed.")))
		if err != nil {
			t.Fatal(err)
		}
		taskID := core.NewTaskID()
		task := core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, Title: "Blueprint", State: core.TaskAwaiting, CreatedAt: time.Now().UTC()}
		if err = st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		proposed, err := st.ProposeRequirementServes(ctx, task.ID, requirement.ID, core.RequirementServesPlanning, false)
		if err != nil || proposed.State != core.RequirementServesProposed || proposed.CreatedByEventID == 0 {
			t.Fatalf("proposed link=%+v err=%v", proposed, err)
		}
		if repeated, repeatErr := st.ProposeRequirementServes(ctx, task.ID, requirement.ID, core.RequirementServesPlanning, false); repeatErr != nil || repeated.CreatedByEventID != proposed.CreatedByEventID {
			t.Fatalf("repeated proposal=%+v err=%v", repeated, repeatErr)
		}
		confirmed, err := st.ConfirmRequirementServes(ctx, task.ID, requirement.ID)
		if err != nil || confirmed.State != core.RequirementServesConfirmed || confirmed.DecisionEventID == 0 || confirmed.DecidedBy != requirementConformanceActor {
			t.Fatalf("confirmed link=%+v err=%v", confirmed, err)
		}
		if _, err = st.DismissRequirementServes(ctx, task.ID, requirement.ID); !errors.Is(err, store.ErrRequirementServesTransition) {
			t.Fatalf("dismiss confirmed error=%v", err)
		}
		links, err := st.ListRequirementServes(ctx)
		if err != nil || len(links) != 1 || links[0].State != core.RequirementServesConfirmed {
			t.Fatalf("serves links=%+v err=%v", links, err)
		}
		events, err := st.ListEvents(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		kinds := map[string]int{}
		for _, event := range events {
			kinds[event.Kind]++
		}
		if kinds["task.requirement_suggested"] != 1 || kinds["requirement.serves_confirmed"] != 1 {
			t.Fatalf("serves events=%+v", events)
		}
	})

	t.Run("dismissing a proposal removes any stale serves projection", func(t *testing.T) {
		st, ctx, workspace := newRequirementFixture(t, factory)
		requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Dismissed intent"},
			chatVersion("Dismissed service is not delivery authority.", requirementStatement("REQ-1", "Dismissal removes projection.")))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
			t.Fatal(err)
		}
		taskID := core.NewTaskID()
		if err = st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if _, err = st.ProposeRequirementServes(ctx, taskID, requirement.ID, core.RequirementServesPlanning, false); err != nil {
			t.Fatal(err)
		}
		// Simulate the stale projection this repair must clean up while the
		// durable proposal is still dismissible.
		if err = st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "requirement.serves_confirmed", Payload: core.JSONPayload(map[string]any{"requirement_id": requirement.ID})}); err != nil {
			t.Fatal(err)
		}
		node := core.LineageNode{Type: core.LineageRequirement, ID: requirement.ID}
		if exists, existsErr := st.LineageNodeExists(ctx, node); existsErr != nil || !exists {
			t.Fatalf("stale projection exists=%t err=%v", exists, existsErr)
		}
		if served, servedErr := store.ServedRequirementsForTask(ctx, st, taskID); servedErr != nil || len(served.Requirements) != 1 {
			t.Fatalf("stale citation authority=%+v err=%v", served, servedErr)
		}
		if _, err = st.DismissRequirementServes(ctx, taskID, requirement.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "dismissal conformance", RequestID: core.NewTaskID()}); err != nil {
			t.Fatal(err)
		}
		links, listErr := st.ListLineageLinks(ctx)
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, link := range links {
			if link.Kind == "serves" && link.SrcID == requirement.ID && link.DstID == taskID {
				t.Fatalf("dismissal retained serves projection: %+v", link)
			}
		}
		if served, servedErr := store.ServedRequirementsForTask(ctx, st, taskID); servedErr != nil || len(served.Requirements) != 0 {
			t.Fatalf("dismissed citation authority=%+v err=%v", served, servedErr)
		}
	})

	t.Run("creation commits a pending document and its first version", func(t *testing.T) {
		st, ctx, workspace := newRequirementFixture(t, factory)
		id := "req-" + core.NewTaskID()
		requirement, first, err := st.CreateRequirement(ctx,
			core.Requirement{ID: id, Title: "Planning Intent Corpus"},
			chatVersion("Operators must see pending intent.",
				requirementStatement("REQ-1", "Intent is versioned."),
				requirementStatement("REQ-2", "Confirmation is explicit.")))
		if err != nil {
			t.Fatal(err)
		}
		// A new document is never silently authoritative: current_version stays
		// unset and the high-water mark starts at the first block's largest
		// REQ-n.
		if requirement.ID != id || requirement.Workspace != workspace ||
			requirement.Slug != "planning-intent-corpus" || requirement.Title != "Planning Intent Corpus" ||
			requirement.CurrentVersion != 0 || requirement.StatementHighWaterMark != 2 ||
			requirement.CreatedAt.IsZero() || requirement.UpdatedAt.IsZero() {
			t.Fatalf("created requirement=%+v", requirement)
		}
		if first.RequirementID != id || first.Workspace != workspace || first.Version != 1 ||
			first.Confirmed || first.ConfirmedBy != "" || !first.ConfirmedAt.IsZero() ||
			first.CreatedAt.IsZero() {
			t.Fatalf("first version=%+v, want pending v1", first)
		}
		assertRequirementRoundTrip(t, ctx, st, requirement)
		assertRequirementVersionRoundTrip(t, ctx, st, first)

		// A caller cannot smuggle authority in through the create call either.
		forgedID := "req-" + core.NewTaskID()
		forged := chatVersion("Forged authority.", requirementStatement("REQ-1", "Should stay pending."))
		forged.Confirmed = true
		forged.ConfirmedBy = "impostor"
		forged.ConfirmedAt = time.Now().UTC()
		forgedDoc, forgedVersion, err := st.CreateRequirement(ctx,
			core.Requirement{ID: forgedID, Title: "Forged Authority"}, forged)
		if err != nil {
			t.Fatal(err)
		}
		if forgedDoc.CurrentVersion != 0 || forgedVersion.Confirmed ||
			forgedVersion.ConfirmedBy != "" || !forgedVersion.ConfirmedAt.IsZero() {
			t.Fatalf("forged create doc=%+v version=%+v, want pending", forgedDoc, forgedVersion)
		}
		assertRequirementVersionRoundTrip(t, ctx, st, forgedVersion)

		// An explicit slug is honoured; only omission derives one.
		explicit, _, err := st.CreateRequirement(ctx,
			core.Requirement{ID: "req-" + core.NewTaskID(), Slug: "custom-handle", Title: "Ignored For Slug"},
			chatVersion("Explicit handle.", requirementStatement("REQ-1", "Handle is stable.")))
		if err != nil {
			t.Fatal(err)
		}
		if explicit.Slug != "custom-handle" {
			t.Fatalf("explicit slug=%q, want custom-handle", explicit.Slug)
		}

		// Identity and handle are both workspace-unique: two documents claiming
		// the same REQ corpus entry would make citations ambiguous.
		rejected := map[string]core.Requirement{
			"duplicate id":            {ID: id, Title: "A Different Title Entirely"},
			"duplicate derived slug":  {ID: "req-" + core.NewTaskID(), Title: "Planning Intent Corpus"},
			"duplicate explicit slug": {ID: "req-" + core.NewTaskID(), Slug: "custom-handle", Title: "Another Title"},
			"missing id":              {Title: "No Identity"},
			"missing title":           {ID: "req-" + core.NewTaskID()},
		}
		for name, candidate := range rejected {
			if _, _, err = st.CreateRequirement(ctx, candidate,
				chatVersion("Must not commit.", requirementStatement("REQ-1", "Rejected."))); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		}
		if _, _, err = st.CreateRequirement(ctx,
			core.Requirement{ID: "req-" + core.NewTaskID(), Slug: "custom-handle", Title: "Typed Slug Conflict"},
			chatVersion("Must not commit.", requirementStatement("REQ-1", "Rejected."))); !errors.Is(err, store.ErrRequirementSlugConflict) {
			t.Fatalf("slug conflict error=%v, want ErrRequirementSlugConflict", err)
		}
		// A malformed statement block is refused with the document, so a
		// requirement never exists without a first version.
		if _, _, err = st.CreateRequirement(ctx,
			core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Bad Statement Block"},
			chatVersion("Bad block.", requirementStatement("REQ-0", "Zero is not a REQ-n."))); err == nil {
			t.Fatal("invalid statement id was accepted")
		}
		// Every rejection rolled back: only the three good documents exist.
		assertRequirementCount(t, ctx, st, 3)
	})

	t.Run("origin provenance names the act that produced a version", func(t *testing.T) {
		st, ctx, _ := newRequirementFixture(t, factory)
		statements := []core.RequirementStatement{requirementStatement("REQ-1", "Intent is traceable.")}
		// A migration seed is produced by neither a session nor a drift record,
		// so it carries neither identifier.
		seed, seedVersion, err := st.CreateRequirement(ctx,
			core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Migrated Feature Node"},
			core.RequirementVersion{
				Content:    "Verbatim text carried over from the retired feature tree.",
				Statements: statements, Origin: core.RequirementOriginFeatureMigration,
			})
		if err != nil {
			t.Fatal(err)
		}
		if seedVersion.Origin != core.RequirementOriginFeatureMigration ||
			seedVersion.OriginSessionID != "" || seedVersion.OriginDriftID != "" || seedVersion.Confirmed {
			t.Fatalf("migration seed version=%+v", seedVersion)
		}
		operator, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: seed.ID, Content: "Operator proposal.", Statements: statements,
			Origin: core.RequirementOriginOperator,
		})
		if err != nil {
			t.Fatal(err)
		}
		if operator.Origin != core.RequirementOriginOperator || operator.OriginSessionID != "" || operator.OriginDriftID != "" || operator.Confirmed {
			t.Fatalf("operator version=%+v", operator)
		}
		assertRequirementVersionRoundTrip(t, ctx, st, operator)

		// The monitor's requirements_amended outcome proposes a *pending*
		// version carrying its drift record.
		driftID := "drift-" + core.NewTaskID()
		amended, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: seed.ID, Content: "Amended after observed drift.",
			Statements: statements, Origin: core.RequirementOriginDriftAmendment, OriginDriftID: driftID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if amended.Version != 3 || amended.Confirmed || amended.OriginDriftID != driftID ||
			amended.OriginSessionID != "" {
			t.Fatalf("drift amendment version=%+v", amended)
		}
		assertRequirementVersionRoundTrip(t, ctx, st, amended)
		if current, getErr := st.GetRequirement(ctx, seed.ID); getErr != nil || current.CurrentVersion != 0 {
			t.Fatalf("document after drift amendment=%+v err=%v, want still pending", current, getErr)
		}

		// The identifiers are exclusive: exactly one act produced the version.
		for name, candidate := range map[string]core.RequirementVersion{
			"chat without a session":               {Origin: core.RequirementOriginChat},
			"chat carrying a drift id":             {Origin: core.RequirementOriginChat, OriginSessionID: "session-x", OriginDriftID: "drift-x"},
			"drift amendment without a drift":      {Origin: core.RequirementOriginDriftAmendment},
			"drift amendment carrying a session":   {Origin: core.RequirementOriginDriftAmendment, OriginDriftID: "drift-x", OriginSessionID: "session-x"},
			"feature migration carrying a session": {Origin: core.RequirementOriginFeatureMigration, OriginSessionID: "session-x"},
			"feature migration carrying a drift":   {Origin: core.RequirementOriginFeatureMigration, OriginDriftID: "drift-x"},
			"operator carrying a session":          {Origin: core.RequirementOriginOperator, OriginSessionID: "session-x"},
			"operator carrying a drift":            {Origin: core.RequirementOriginOperator, OriginDriftID: "drift-x"},
			"operator carrying a task":             {Origin: core.RequirementOriginOperator, OriginTaskID: "task-x"},
			"implementation without a task":        {Origin: core.RequirementOriginImplementation},
			"implementation carrying a session":    {Origin: core.RequirementOriginImplementation, OriginTaskID: "task-x", OriginSessionID: "session-x"},
			"implementation carrying a drift":      {Origin: core.RequirementOriginImplementation, OriginTaskID: "task-x", OriginDriftID: "drift-x"},
			"unrecognised origin":                  {Origin: "operator_hunch", OriginSessionID: "session-x"},
		} {
			candidate.Content = "Must not commit."
			candidate.Statements = statements
			candidate.RequirementID = seed.ID
			if _, err = st.ProposeRequirementVersion(ctx, candidate); err == nil {
				t.Fatalf("%s was accepted as a proposal", name)
			}
			if _, _, err = st.CreateRequirement(ctx,
				core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Rejected " + name}, candidate); err == nil {
				t.Fatalf("%s was accepted at document creation", name)
			}
		}
		// None of those rejections consumed a version number or a document.
		next, err := st.ProposeRequirementVersion(ctx, chatVersionFor(seed.ID, "Third revision.", statements...))
		if err != nil {
			t.Fatal(err)
		}
		if next.Version != 4 {
			t.Fatalf("version after rejected proposals=%d, want 4", next.Version)
		}
		assertRequirementCount(t, ctx, st, 1)
	})

	t.Run("proposals append monotonically and are never born confirmed", func(t *testing.T) {
		st, ctx, workspace := newRequirementFixture(t, factory)
		requirement, first := createRequirement(t, ctx, st, "Monotonic Revisions",
			chatVersion("First intent.", requirementStatement("REQ-1", "One.")))

		// Confirmation is a separate audited operator act, so a proposal that
		// asks to be confirmed is still stored pending.
		forged := chatVersionFor(requirement.ID, "Second intent.",
			requirementStatement("REQ-1", "One, reworded."), requirementStatement("REQ-2", "Two."))
		forged.Confirmed = true
		forged.ConfirmedBy = "impostor"
		forged.ConfirmedAt = time.Now().UTC()
		second, err := st.ProposeRequirementVersion(ctx, forged)
		if err != nil {
			t.Fatal(err)
		}
		if second.Version != 2 || second.Confirmed || second.ConfirmedBy != "" ||
			!second.ConfirmedAt.IsZero() || second.Workspace != workspace {
			t.Fatalf("second version=%+v, want pending v2", second)
		}
		assertRequirementVersionRoundTrip(t, ctx, st, second)
		// The high-water mark advances with the block so REQ-2 can never be
		// reissued to a different statement later.
		advanced, err := st.GetRequirement(ctx, requirement.ID)
		if err != nil {
			t.Fatal(err)
		}
		if advanced.StatementHighWaterMark != 2 || advanced.CurrentVersion != 0 || advanced.UpdatedAt.IsZero() {
			t.Fatalf("document after v2=%+v, want high-water 2 and no current version", advanced)
		}

		third, err := st.ProposeRequirementVersion(ctx, driftVersionFor(requirement.ID, "Third intent.",
			requirementStatement("REQ-1", "One."), requirementStatement("REQ-2", "Two."),
			requirementStatement("REQ-3", "Three.")))
		if err != nil {
			t.Fatal(err)
		}
		if third.Version != 3 {
			t.Fatalf("third version=%d, want 3", third.Version)
		}
		if advanced, err = st.GetRequirement(ctx, requirement.ID); err != nil || advanced.StatementHighWaterMark != 3 {
			t.Fatalf("document after v3=%+v err=%v, want high-water 3", advanced, err)
		}

		versions, err := st.ListRequirementVersions(ctx, requirement.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(versions) != 3 {
			t.Fatalf("versions=%d, want 3", len(versions))
		}
		for index, version := range versions {
			if version.Version != index+1 || version.Confirmed || version.RequirementID != requirement.ID {
				t.Fatalf("version %d=%+v, want ascending and unconfirmed", index, version)
			}
		}
		if !sameRequirementStatements(versions[0].Statements, first.Statements) {
			t.Fatalf("listed v1 statements=%+v, want %+v", versions[0].Statements, first.Statements)
		}

		// A proposal for a document that does not exist is refused, never
		// silently creating one.
		if _, err = st.ProposeRequirementVersion(ctx,
			chatVersionFor("req-does-not-exist", "Orphan.", requirementStatement("REQ-1", "One."))); err == nil {
			t.Fatal("proposal against an unknown requirement was accepted")
		}
	})

	t.Run("REQ-n identifiers are never reissued", func(t *testing.T) {
		st, ctx, _ := newRequirementFixture(t, factory)
		// v1 issues REQ-1 and REQ-5, so the mark is 5 while REQ-2..REQ-4 were
		// never issued by this document.
		requirement, _ := createRequirement(t, ctx, st, "Stable Statement Identity",
			chatVersion("Sparse identifiers.",
				requirementStatement("REQ-1", "One."), requirementStatement("REQ-5", "Five.")))
		if requirement.StatementHighWaterMark != 5 {
			t.Fatalf("initial high-water=%d, want 5", requirement.StatementHighWaterMark)
		}

		// Dropping a statement in a revision is allowed and does not lower the
		// mark: the identifier stays retired, not reusable.
		dropped, err := st.ProposeRequirementVersion(ctx, chatVersionFor(requirement.ID, "Five retired.",
			requirementStatement("REQ-1", "One.")))
		if err != nil {
			t.Fatal(err)
		}
		if dropped.Version != 2 {
			t.Fatalf("drop revision version=%d, want 2", dropped.Version)
		}
		if current, getErr := st.GetRequirement(ctx, requirement.ID); getErr != nil || current.StatementHighWaterMark != 5 {
			t.Fatalf("high-water after drop=%+v err=%v, want 5", current, getErr)
		}

		// A never-issued identifier at or below the mark would hand a retired
		// statement's identity to different intent, breaking every REQ-n
		// citation that outlived it.
		for _, reused := range []string{"REQ-2", "REQ-4"} {
			if _, err = st.ProposeRequirementVersion(ctx, chatVersionFor(requirement.ID, "Reuse attempt.",
				requirementStatement("REQ-1", "One."), requirementStatement(reused, "Different intent."))); err == nil {
				t.Fatalf("never-issued identifier %s at or below the high-water mark was accepted", reused)
			}
		}

		// Reinstating an identifier an earlier version issued is allowed —
		// otherwise a document that dropped a statement in an unconfirmed
		// proposal could never restore its own confirmed text.
		reinstated, err := st.ProposeRequirementVersion(ctx, chatVersionFor(requirement.ID, "Five reinstated.",
			requirementStatement("REQ-1", "One."), requirementStatement("REQ-5", "Five, reworded.")))
		if err != nil {
			t.Fatalf("reinstating a previously issued identifier: %v", err)
		}
		// The rejected proposals consumed no version numbers.
		if reinstated.Version != 3 {
			t.Fatalf("reinstating version=%d, want 3", reinstated.Version)
		}

		// New statements must exceed the mark.
		grown, err := st.ProposeRequirementVersion(ctx, chatVersionFor(requirement.ID, "Six added.",
			requirementStatement("REQ-1", "One."), requirementStatement("REQ-5", "Five."),
			requirementStatement("REQ-6", "Six.")))
		if err != nil {
			t.Fatal(err)
		}
		if grown.Version != 4 {
			t.Fatalf("growth version=%d, want 4", grown.Version)
		}
		if current, getErr := st.GetRequirement(ctx, requirement.ID); getErr != nil || current.StatementHighWaterMark != 6 {
			t.Fatalf("high-water after growth=%+v err=%v, want 6", current, getErr)
		}
	})

	t.Run("acceptance-criterion identifiers are never reissued", func(t *testing.T) {
		st, ctx, _ := newRequirementFixture(t, factory)
		firstStatement := requirementStatement("REQ-1", "Stable requirement.")
		firstStatement.AcceptanceCriteria = []core.AcceptanceCriterion{
			{ID: "AC-1.1", Statement: "First criterion."},
			{ID: "AC-1.3", Statement: "Third criterion."},
		}
		requirement, _ := createRequirement(t, ctx, st, "Stable AC Identity", chatVersion("Initial criteria.", firstStatement))

		dropped := requirementStatement("REQ-1", "Stable requirement.")
		dropped.AcceptanceCriteria = []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "First criterion."}}
		if _, err := st.ProposeRequirementVersion(ctx, chatVersionFor(requirement.ID, "Third retired.", dropped)); err != nil {
			t.Fatal(err)
		}

		reused := dropped
		reused.AcceptanceCriteria = append(append([]core.AcceptanceCriterion(nil), dropped.AcceptanceCriteria...), core.AcceptanceCriterion{ID: "AC-1.2", Statement: "Reassigned criterion."})
		if _, err := st.ProposeRequirementVersion(ctx, chatVersionFor(requirement.ID, "Reuse attempt.", reused)); err == nil {
			t.Fatal("retired AC-1.2 at or below the historical AC-1.3 high-water mark was accepted")
		}

		reinstated := dropped
		reinstated.AcceptanceCriteria = append(append([]core.AcceptanceCriterion(nil), dropped.AcceptanceCriteria...), core.AcceptanceCriterion{ID: "AC-1.3", Statement: "Third criterion, revised."})
		if _, err := st.ProposeRequirementVersion(ctx, chatVersionFor(requirement.ID, "Third restored.", reinstated)); err != nil {
			t.Fatalf("reinstating historically issued AC-1.3: %v", err)
		}

		grown := reinstated
		grown.AcceptanceCriteria = append(append([]core.AcceptanceCriterion(nil), reinstated.AcceptanceCriteria...), core.AcceptanceCriterion{ID: "AC-1.4", Statement: "Fourth criterion."})
		if _, err := st.ProposeRequirementVersion(ctx, chatVersionFor(requirement.ID, "Fourth added.", grown)); err != nil {
			t.Fatalf("adding AC-1.4 above the historical high-water mark: %v", err)
		}
	})

	t.Run("confirmation is forward-only and records the operator", func(t *testing.T) {
		st, ctx, _ := newRequirementFixture(t, factory)
		requirement, _ := createRequirement(t, ctx, st, "Confirmed Intent",
			chatVersion("First intent.", requirementStatement("REQ-1", "One.")))
		if _, err := st.ProposeRequirementVersion(ctx, chatVersionFor(requirement.ID, "Second intent.",
			requirementStatement("REQ-1", "One."), requirementStatement("REQ-2", "Two."))); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ProposeRequirementVersion(ctx, chatVersionFor(requirement.ID, "Third intent.",
			requirementStatement("REQ-1", "One."), requirementStatement("REQ-2", "Two."),
			requirementStatement("REQ-3", "Three."))); err != nil {
			t.Fatal(err)
		}

		confirmedDoc, confirmedVersion, err := st.ConfirmRequirementVersion(ctx, requirement.ID, 2)
		if err != nil {
			t.Fatal(err)
		}
		if confirmedDoc.CurrentVersion != 2 || confirmedDoc.UpdatedAt.IsZero() {
			t.Fatalf("document after confirming v2=%+v", confirmedDoc)
		}
		if !confirmedVersion.Confirmed || confirmedVersion.Version != 2 ||
			confirmedVersion.ConfirmedBy != requirementConformanceActor || confirmedVersion.ConfirmedAt.IsZero() {
			t.Fatalf("confirmed version=%+v, want v2 confirmed by %s", confirmedVersion, requirementConformanceActor)
		}
		assertRequirementRoundTrip(t, ctx, st, confirmedDoc)
		assertRequirementVersionRoundTrip(t, ctx, st, confirmedVersion)
		// The earlier unconfirmed proposal is retired and audited; the later
		// proposal remains independently actionable.
		retired, getErr := st.GetRequirementVersion(ctx, requirement.ID, 1)
		if getErr != nil || !retired.Retired || retired.Confirmed || retired.RetiredBy != requirementConformanceActor ||
			retired.RetiredAt.IsZero() || retired.RetiredByVersion != 2 {
			t.Fatalf("retired version=%+v err=%v", retired, getErr)
		}
		pending, getErr := st.GetRequirementVersion(ctx, requirement.ID, 3)
		if getErr != nil || pending.Confirmed || pending.Retired {
			t.Fatalf("later pending version=%+v err=%v", pending, getErr)
		}
		events, eventErr := st.ListRequirementEvents(ctx, requirement.ID)
		if eventErr != nil {
			t.Fatal(eventErr)
		}
		retirementEvents := 0
		for _, event := range events {
			if event.Kind == "requirement.version_retired" {
				retirementEvents++
			}
		}
		if retirementEvents != 1 {
			t.Fatalf("retirement events=%d, want 1", retirementEvents)
		}

		// Re-confirming the current version is a no-op retry, not an error.
		repeatDoc, repeatVersion, err := st.ConfirmRequirementVersion(ctx, requirement.ID, 2)
		if err != nil {
			t.Fatal(err)
		}
		if repeatDoc.CurrentVersion != 2 || !repeatVersion.Confirmed ||
			repeatVersion.ConfirmedBy != requirementConformanceActor ||
			!sameInstant(repeatVersion.ConfirmedAt, confirmedVersion.ConfirmedAt) {
			t.Fatalf("repeat confirm doc=%+v version=%+v, want the original confirmation", repeatDoc, repeatVersion)
		}

		// Confirming backwards would silently revert intent the operator
		// already advanced past.
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, 1); err == nil {
			t.Fatal("confirming a superseded version was accepted")
		} else {
			var superseded *store.RequirementVersionSuperseded
			if !errors.As(err, &superseded) || superseded.SupersededBy != 2 {
				t.Fatalf("backward confirmation error=%v, want typed superseded condition", err)
			}
		}
		if reread, getErr := st.GetRequirement(ctx, requirement.ID); getErr != nil || reread.CurrentVersion != 2 {
			t.Fatalf("document after rejected backward confirm=%+v err=%v, want current version 2", reread, getErr)
		}

		// Forward is fine.
		if forwardDoc, _, forwardErr := st.ConfirmRequirementVersion(ctx, requirement.ID, 3); forwardErr != nil ||
			forwardDoc.CurrentVersion != 3 {
			t.Fatalf("confirming v3 doc=%+v err=%v", forwardDoc, forwardErr)
		}

		// Versions that do not exist cannot be confirmed.
		for _, missing := range []int{0, -1, 99} {
			if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, missing); err == nil {
				t.Fatalf("confirming nonexistent version %d was accepted", missing)
			}
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, "req-does-not-exist", 1); err == nil {
			t.Fatal("confirming a version of an unknown requirement was accepted")
		}

		// A statement-less version cannot become current intent: a migration
		// seed carries the retired node's prose verbatim and must be edited
		// before it is authoritative.
		seed, _, err := st.CreateRequirement(ctx,
			core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Seed Without Statements"},
			core.RequirementVersion{
				Content: "Prose only, no machine block.", Origin: core.RequirementOriginFeatureMigration,
			})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, seed.ID, 1); err == nil {
			t.Fatal("confirming a version with no REQ-n statements was accepted")
		}
		if reread, getErr := st.GetRequirement(ctx, seed.ID); getErr != nil || reread.CurrentVersion != 0 {
			t.Fatalf("seed after rejected confirm=%+v err=%v, want still pending", reread, getErr)
		}
	})

	t.Run("reads are deterministic and absence is an error", func(t *testing.T) {
		st, ctx, _ := newRequirementFixture(t, factory)
		// Inserted in reverse title order with ids that also run backwards, so
		// the listing order can only come from the title.
		for _, seeded := range []struct{ id, title string }{
			{"req-a-inserted-first", "Gamma Intent"},
			{"req-b-inserted-second", "Beta Intent"},
			{"req-c-inserted-third", "Alpha Intent"},
		} {
			if _, _, err := st.CreateRequirement(ctx, core.Requirement{ID: seeded.id, Title: seeded.title},
				chatVersion(seeded.title+" prose.", requirementStatement("REQ-1", "One."))); err != nil {
				t.Fatal(err)
			}
		}
		listed, err := st.ListRequirements(ctx)
		if err != nil {
			t.Fatal(err)
		}
		gotTitles := make([]string, len(listed))
		for index, requirement := range listed {
			gotTitles[index] = requirement.Title
		}
		if !reflect.DeepEqual(gotTitles, []string{"Alpha Intent", "Beta Intent", "Gamma Intent"}) {
			t.Fatalf("listed titles=%v, want title order", gotTitles)
		}
		for _, requirement := range listed {
			if requirement.ID == "" || requirement.Slug == "" || requirement.CreatedAt.IsZero() {
				t.Fatalf("listing returned a partially populated document: %+v", requirement)
			}
		}

		if _, err = st.GetRequirement(ctx, "req-does-not-exist"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown requirement error=%v, want ErrNotFound", err)
		}
		if _, err = st.GetRequirementVersion(ctx, "req-a-inserted-first", 2); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unwritten version error=%v, want ErrNotFound", err)
		}
		if _, err = st.GetRequirementVersion(ctx, "req-does-not-exist", 1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown requirement version error=%v, want ErrNotFound", err)
		}
		if _, err = st.GetTask(ctx, "task-does-not-exist"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown task error=%v, want ErrNotFound", err)
		}
		if _, _, err = st.GetArtifact(ctx, "artifact-does-not-exist"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown artifact error=%v, want ErrNotFound", err)
		}
		// Listing versions of an unknown document is empty rather than an
		// error: it is a projection, not an identity lookup.
		if versions, listErr := st.ListRequirementVersions(ctx, "req-does-not-exist"); listErr != nil || len(versions) != 0 {
			t.Fatalf("versions of an unknown requirement=%+v err=%v, want empty", versions, listErr)
		}
	})

	t.Run("planning sessions restore their transcript in order", func(t *testing.T) {
		st, ctx, workspace := newRequirementFixture(t, factory)
		opened, _ := createRequirement(t, ctx, st, "Session Context",
			chatVersion("Context prose.", requirementStatement("REQ-1", "One.")))

		sessionID := "session-" + core.NewTaskID()
		session, err := st.CreatePlanningSession(ctx, core.PlanningSession{
			ID: sessionID, Title: "Plan the queue rewrite", RequirementContextID: opened.ID,
			Goal:  core.PlanningGoalBlueprint,
			Model: "planner", Effort: "high", ExplorationOutputTokens: 10_000,
			PrimaryRepo: "api", PinnedRevisions: map[string]string{"api": strings.Repeat("a", 40)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if session.ID != sessionID || session.Workspace != workspace ||
			session.Status != core.PlanningSessionActive || session.Title != "Plan the queue rewrite" ||
			session.Goal != core.PlanningGoalBlueprint ||
			session.RequirementContextID != opened.ID || session.ProducedRequirementID != "" ||
			session.ProducedTaskID != "" || session.TranscriptArtifactID != "" ||
			session.Model != "planner" || session.Effort != "high" ||
			session.ExplorationOutputTokens != 10_000 ||
			session.PinnedRevisions["api"] != strings.Repeat("a", 40) ||
			!session.FinalizedAt.IsZero() || session.CreatedAt.IsZero() || session.UpdatedAt.IsZero() {
			t.Fatalf("created session=%+v", session)
		}
		assertPlanningSessionRoundTrip(t, ctx, st, session)
		// The goal is declared once. Omitting it is
		// compatible and reads back as `open` — historical rows migrate the
		// same way — while an unknown goal is refused at the boundary.
		defaulted, err := st.CreatePlanningSession(ctx, core.PlanningSession{
			ID: "session-" + core.NewTaskID(), Title: "Exploring…",
		})
		if err != nil || defaulted.Goal != core.PlanningGoalOpen {
			t.Fatalf("goal-less session=%+v err=%v, want open", defaulted, err)
		}
		assertPlanningSessionRoundTrip(t, ctx, st, defaulted)
		promotion, err := st.CreatePlanningSession(ctx, core.PlanningSession{
			ID: "session-" + core.NewTaskID(), Title: "Promoting requirement…", Goal: core.PlanningGoalRequirement,
			Promotion: &core.RequirementDerivation{DocumentID: "ref-overview", Version: 2, SectionAnchor: "#billing", TargetID: "AC-1.1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertPlanningSessionRoundTrip(t, ctx, st, promotion)
		if _, err = st.CreatePlanningSession(ctx, core.PlanningSession{
			ID: "session-" + core.NewTaskID(), Goal: core.PlanningSessionGoal("epic"),
		}); err == nil {
			t.Fatal("planning session with an unknown goal was accepted")
		}
		pinned, err := st.PinPlanningSessionRepo(ctx, sessionID, "web", strings.Repeat("b", 40))
		if err != nil || pinned.PinnedRevisions["web"] != strings.Repeat("b", 40) {
			t.Fatalf("pinned session=%+v err=%v", pinned, err)
		}
		conflicted, conflictErr := st.PinPlanningSessionRepo(ctx, sessionID, "web", strings.Repeat("c", 40))
		if conflictErr == nil {
			t.Fatal("conflicting immutable pin was silently accepted")
		}
		if conflicted.ID != sessionID || conflicted.PinnedRevisions["web"] != strings.Repeat("b", 40) {
			t.Fatalf("pin conflict return=%+v err=%v, want populated winning session", conflicted, conflictErr)
		}
		pinnedAgain, err := st.GetPlanningSession(ctx, sessionID)
		if err != nil || pinnedAgain.PinnedRevisions["web"] != strings.Repeat("b", 40) {
			t.Fatalf("immutable pin changed after conflict: %+v err=%v", pinnedAgain, err)
		}
		accounted, err := st.RecordPlanningExplorationTokens(ctx, sessionID, 42)
		if err != nil || accounted.ExplorationTokensUsed != 42 {
			t.Fatalf("exploration accounting=%+v err=%v", accounted, err)
		}

		if _, err = st.CreatePlanningSession(ctx, core.PlanningSession{ID: sessionID}); err == nil {
			t.Fatal("duplicate planning session id was accepted")
		}
		if _, err = st.CreatePlanningSession(ctx, core.PlanningSession{ID: ""}); err == nil {
			t.Fatal("planning session without an id was accepted")
		}
		// A session opened "from" a requirement must name a real one — that
		// link is what auto-proposes serves later.
		if _, err = st.CreatePlanningSession(ctx, core.PlanningSession{
			ID: "session-" + core.NewTaskID(), RequirementContextID: "req-does-not-exist",
		}); err == nil {
			t.Fatal("planning session referencing an unknown requirement was accepted")
		}

		// Sequence numbers start at 1 and increment, because the transport
		// restores a session by replaying them in order.
		appended := []core.PlanningMessage{}
		for index, message := range []core.PlanningMessage{
			{SessionID: sessionID, Role: core.PlanningMessageUser, Content: "Rewrite the queue.",
				Parts: json.RawMessage(requirementCanonicalParts)},
			{SessionID: sessionID, Role: core.PlanningMessageAssistant, Content: "Reading the spec.",
				Parts: json.RawMessage(`[{"type":"tool-call","toolName":"read","args":{"path":"spec.md"}}]`)},
			{SessionID: sessionID, Role: core.PlanningMessageTool, Content: "spec.md contents",
				Parts: json.RawMessage(`[{"type":"tool-result","result":{"ok":true,"lines":42}}]`)},
			{SessionID: sessionID, Role: core.PlanningMessageSystem, Content: "Session policy."},
		} {
			stored, appendErr := st.AppendPlanningMessage(ctx, message)
			if appendErr != nil {
				t.Fatal(appendErr)
			}
			if stored.Seq != index+1 || stored.Workspace != workspace ||
				stored.SessionID != sessionID || stored.CreatedAt.IsZero() {
				t.Fatalf("appended message=%+v, want seq %d", stored, index+1)
			}
			appended = append(appended, message)
		}

		restored, err := st.ListPlanningMessages(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(restored) != len(appended) {
			t.Fatalf("restored messages=%d, want %d", len(restored), len(appended))
		}
		for index, message := range restored {
			want := appended[index]
			if message.Seq != index+1 || message.Role != want.Role || message.Content != want.Content ||
				message.SessionID != sessionID || message.CreatedAt.IsZero() {
				t.Fatalf("restored message %d=%+v, want %+v", index, message, want)
			}
			assertSamePlanningParts(t, message.Parts, want.Parts)
		}
		// The first message's parts were supplied in canonical form, so the
		// exact bytes survive the round trip in both implementations.
		if string(restored[0].Parts) != requirementCanonicalParts {
			t.Fatalf("canonical parts round trip=%s, want %s", restored[0].Parts, requirementCanonicalParts)
		}

		if _, err = st.AppendPlanningMessage(ctx, core.PlanningMessage{
			SessionID: sessionID, Role: "narrator", Content: "Invalid role.",
		}); err == nil {
			t.Fatal("invalid planning message role was accepted")
		}
		if _, err = st.AppendPlanningMessage(ctx, core.PlanningMessage{
			SessionID: "session-does-not-exist", Role: core.PlanningMessageUser, Content: "Orphan.",
		}); err == nil {
			t.Fatal("message against an unknown session was accepted")
		}
		// The rejections did not consume a sequence number.
		if next, appendErr := st.AppendPlanningMessage(ctx, core.PlanningMessage{
			SessionID: sessionID, Role: core.PlanningMessageUser, Content: "Still going.",
		}); appendErr != nil || next.Seq != len(appended)+1 {
			t.Fatalf("next seq=%+v err=%v, want %d", next, appendErr, len(appended)+1)
		}

		// Listing is most-recently-touched first: appending to the older
		// session brings it back to the top.
		quiet, err := st.CreatePlanningSession(ctx, core.PlanningSession{
			ID: "session-" + core.NewTaskID(), Title: "Untouched",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = st.AppendPlanningMessage(ctx, core.PlanningMessage{
			SessionID: sessionID, Role: core.PlanningMessageUser, Content: "One more turn.",
		}); err != nil {
			t.Fatal(err)
		}
		sessions, err := st.ListPlanningSessions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) != 4 || sessions[0].ID != sessionID || sessions[1].ID != quiet.ID ||
			sessions[2].ID != promotion.ID || sessions[3].ID != defaulted.ID {
			t.Fatalf("listed sessions=%+v, want %s before %s before %s before %s",
				sessions, sessionID, quiet.ID, promotion.ID, defaulted.ID)
		}
		// Listing exposes the declared goal, so a session list can label it
		// without a second read.
		if sessions[0].Goal != core.PlanningGoalBlueprint || sessions[3].Goal != core.PlanningGoalOpen {
			t.Fatalf("listed goals=%q/%q, want blueprint/open", sessions[0].Goal, sessions[3].Goal)
		}
		for _, listed := range sessions {
			if listed.Status != core.PlanningSessionActive || listed.Workspace != workspace ||
				listed.CreatedAt.IsZero() || listed.UpdatedAt.IsZero() {
				t.Fatalf("listing returned a partially populated session: %+v", listed)
			}
		}
		// Messages of an unknown session are an empty transcript, not an error.
		if messages, listErr := st.ListPlanningMessages(ctx, "session-does-not-exist"); listErr != nil || len(messages) != 0 {
			t.Fatalf("messages of an unknown session=%+v err=%v, want empty", messages, listErr)
		}
	})

	t.Run("planning run claims fail fast and release on every terminal path", func(t *testing.T) {
		st, ctx, _ := newRequirementFixture(t, factory)
		session := createPlanningSession(t, ctx, st)
		entered := make(chan struct{})
		release := make(chan struct{})
		firstDone := make(chan error, 1)
		go func() {
			firstDone <- st.WithPlanningSessionRun(ctx, session.ID, func(context.Context) error {
				close(entered)
				<-release
				return nil
			})
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("first run did not acquire its claim")
		}
		competingRan := false
		if err := st.WithPlanningSessionRun(ctx, session.ID, func(context.Context) error {
			competingRan = true
			return nil
		}); !errors.Is(err, store.ErrPlanningSessionRunConflict) {
			t.Fatalf("competing run error=%v", err)
		}
		if competingRan {
			t.Fatal("competing run entered the claimed session")
		}
		close(release)
		if err := <-firstDone; err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("run callback panic was swallowed")
				}
			}()
			_ = st.WithPlanningSessionRun(ctx, session.ID, func(context.Context) error {
				panic("release claim")
			})
		}()
		if err := st.WithPlanningSessionRun(ctx, session.ID, func(context.Context) error { return nil }); err != nil {
			t.Fatalf("run claim remained held after panic: %v", err)
		}
	})

	t.Run("planning run claims do not block abandonment", func(t *testing.T) {
		st, ctx, _ := newRequirementFixture(t, factory)
		session := createPlanningSession(t, ctx, st)
		entered := make(chan struct{})
		release := make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(release)
			}
		}()
		runDone := make(chan error, 1)
		go func() {
			runDone <- st.WithPlanningSessionRun(ctx, session.ID, func(context.Context) error {
				close(entered)
				<-release
				return nil
			})
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("run did not acquire its claim")
		}
		abandonDone := make(chan error, 1)
		go func() {
			_, err := st.AbandonPlanningSession(ctx, session.ID)
			abandonDone <- err
		}()
		select {
		case err := <-abandonDone:
			if err != nil {
				t.Fatalf("abandon active run: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("abandonment blocked behind the model run claim")
		}
		close(release)
		released = true
		if err := <-runDone; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("closed sessions accept no further messages", func(t *testing.T) {
		st, ctx, _ := newRequirementFixture(t, factory)
		requirement, _ := createRequirement(t, ctx, st, "Closed Session Product",
			chatVersion("Product prose.", requirementStatement("REQ-1", "One.")))

		finalized := createPlanningSession(t, ctx, st)
		if _, err := st.AppendPlanningMessage(ctx, core.PlanningMessage{
			SessionID: finalized.ID, Role: core.PlanningMessageUser, Content: "Before finalize.",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.FinalizePlanningSession(ctx, store.PlanningFinalizeRequest{
			SessionID: finalized.ID, RequirementID: requirement.ID,
		}); err != nil {
			t.Fatal(err)
		}
		// A finalized transcript is the archived record of what produced the
		// artifact; appending to it would rewrite history.
		if _, err := st.AppendPlanningMessage(ctx, core.PlanningMessage{
			SessionID: finalized.ID, Role: core.PlanningMessageUser, Content: "After finalize.",
		}); err == nil {
			t.Fatal("message appended to a finalized session")
		}

		abandoned := createPlanningSession(t, ctx, st)
		if _, err := st.AbandonPlanningSession(ctx, abandoned.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AppendPlanningMessage(ctx, core.PlanningMessage{
			SessionID: abandoned.ID, Role: core.PlanningMessageUser, Content: "After abandon.",
		}); err == nil {
			t.Fatal("message appended to an abandoned session")
		}
		// Model the losing side of an abandon/finalize race deterministically:
		// once abandon commits under the session lock, a previously in-flight
		// run may reach finalization but cannot resurrect the session or attach
		// produced lineage.
		if _, err := st.FinalizePlanningSession(ctx, store.PlanningFinalizeRequest{
			SessionID: abandoned.ID, RequirementID: requirement.ID,
		}); err == nil {
			t.Fatal("abandoned session was resurrected by finalize")
		}
		stillAbandoned, err := st.GetPlanningSession(ctx, abandoned.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stillAbandoned.Status != core.PlanningSessionAbandoned ||
			stillAbandoned.ProducedRequirementID != "" ||
			stillAbandoned.ProducedTaskID != "" ||
			stillAbandoned.TranscriptArtifactID != "" ||
			!stillAbandoned.FinalizedAt.IsZero() {
			t.Fatalf("session after losing finalize=%+v, want terminal abandoned state", stillAbandoned)
		}
		for _, sessionID := range []string{finalized.ID, abandoned.ID} {
			messages, err := st.ListPlanningMessages(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if sessionID == finalized.ID && len(messages) != 1 {
				t.Fatalf("finalized transcript=%d messages, want the one appended while active", len(messages))
			}
			if sessionID == abandoned.ID && len(messages) != 0 {
				t.Fatalf("abandoned transcript=%d messages, want none", len(messages))
			}
		}
	})

	t.Run("abandonment wins before produced writes", func(t *testing.T) {
		st, ctx, workspace := newRequirementFixture(t, factory)
		session := createPlanningSession(t, ctx, st)
		if _, err := st.AbandonPlanningSession(ctx, session.ID); err != nil {
			t.Fatal(err)
		}

		callbackInvoked := false
		err := st.WithPlanningSessionFinalization(ctx, session.ID, func(lockedCtx context.Context) error {
			callbackInvoked = true
			requirement, _, createErr := st.CreateRequirement(lockedCtx,
				core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Losing output"},
				chatVersion("Must remain invisible.", requirementStatement("REQ-1", "One.")),
			)
			if createErr != nil {
				return createErr
			}
			task := planningTask(workspace)
			if createErr = st.CreateTask(lockedCtx, task); createErr != nil {
				return createErr
			}
			_, createErr = st.CreateArtifact(lockedCtx, core.Artifact{
				Name: "losing-transcript.json", ContentType: "application/json",
				Role: core.ArtifactRoleGeneratedAudit, RequirementID: requirement.ID,
			}, []byte(`{"must":"not survive"}`))
			return createErr
		})
		if err == nil {
			t.Fatal("finalization callback was accepted after abandonment")
		}
		if callbackInvoked {
			t.Fatal("produced-write callback ran after abandonment won")
		}
		requirements, err := st.ListRequirements(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tasks, err := st.ListTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		artifacts, err := st.ListArtifacts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(requirements) != 0 || len(tasks) != 0 || len(artifacts) != 0 {
			t.Fatalf("visible output after abandonment: requirements=%+v tasks=%+v artifacts=%+v",
				requirements, tasks, artifacts)
		}
	})

	t.Run("finalization boundary serializes abandonment across produced writes", func(t *testing.T) {
		st, ctx, _ := newRequirementFixture(t, factory)
		session := createPlanningSession(t, ctx, st)
		produced := make(chan struct{})
		continueFinalization := make(chan struct{})
		finalizeDone := make(chan error, 1)
		go func() {
			finalizeDone <- st.WithPlanningSessionFinalization(ctx, session.ID, func(lockedCtx context.Context) error {
				requirement, _, err := st.CreateRequirement(
					lockedCtx,
					core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Serialized Finalization"},
					chatVersion("Serialized prose.", requirementStatement("REQ-1", "One.")),
				)
				if err != nil {
					return err
				}
				transcript, err := st.CreateArtifact(lockedCtx, core.Artifact{
					Name: "serialized-planning-transcript.json", ContentType: "application/json",
					Role: core.ArtifactRoleGeneratedAudit, RequirementID: requirement.ID,
				}, []byte(`{"status":"produced-before-session-transition"}`))
				if err != nil {
					return err
				}
				close(produced)
				select {
				case <-continueFinalization:
				case <-lockedCtx.Done():
					return lockedCtx.Err()
				}
				_, err = st.FinalizePlanningSession(lockedCtx, store.PlanningFinalizeRequest{
					SessionID: session.ID, RequirementID: requirement.ID,
					TranscriptArtifactID: transcript.ID,
				})
				return err
			})
		}()
		select {
		case <-produced:
		case <-time.After(2 * time.Second):
			t.Fatal("finalization did not reach the interleaving point")
		}
		abandonDone := make(chan error, 1)
		go func() {
			_, err := st.AbandonPlanningSession(ctx, session.ID)
			abandonDone <- err
		}()
		select {
		case err := <-abandonDone:
			t.Fatalf("abandonment split produced writes from the session transition: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		close(continueFinalization)
		select {
		case err := <-finalizeDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("finalization remained blocked")
		}
		select {
		case err := <-abandonDone:
			if err == nil {
				t.Fatal("late abandonment replaced finalized lineage")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("late abandonment remained blocked")
		}
		finalized, err := st.GetPlanningSession(ctx, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if finalized.Status != core.PlanningSessionFinalized ||
			finalized.ProducedRequirementID == "" || finalized.TranscriptArtifactID == "" {
			t.Fatalf("finalized session=%+v", finalized)
		}
		if _, err = st.GetRequirement(ctx, finalized.ProducedRequirementID); err != nil {
			t.Fatal(err)
		}
		if artifact, _, artifactErr := st.GetArtifact(ctx, finalized.TranscriptArtifactID); artifactErr != nil ||
			artifact.RequirementID != finalized.ProducedRequirementID {
			t.Fatalf("transcript artifact=%+v err=%v", artifact, artifactErr)
		}

		// If abandon owns the boundary first, the production callback never
		// runs, so there is nothing to compensate and no orphan can become
		// visible after the terminal state is recorded.
		abandoned := createPlanningSession(t, ctx, st)
		if _, err = st.AbandonPlanningSession(ctx, abandoned.ID); err != nil {
			t.Fatal(err)
		}
		callbackRan := false
		if err = st.WithPlanningSessionFinalization(ctx, abandoned.ID, func(context.Context) error {
			callbackRan = true
			return nil
		}); err == nil {
			t.Fatal("abandoned session entered the produced-write callback")
		}
		if callbackRan {
			t.Fatal("produced-write callback ran after abandonment won")
		}
	})

	t.Run("finalize produces exactly one artifact and repeats idempotently", func(t *testing.T) {
		st, ctx, workspace := newRequirementFixture(t, factory)
		produced, _ := createRequirement(t, ctx, st, "Produced Requirement",
			chatVersion("Produced prose.", requirementStatement("REQ-1", "One.")))
		other, _ := createRequirement(t, ctx, st, "Other Requirement",
			chatVersion("Other prose.", requirementStatement("REQ-1", "One.")))
		blueprint := planningTask(workspace)
		if err := st.CreateTask(ctx, blueprint); err != nil {
			t.Fatal(err)
		}
		transcript, err := st.CreateArtifact(ctx, core.Artifact{
			Name: "planning-transcript.json", ContentType: "application/json",
			Role: core.ArtifactRoleGeneratedAudit, RequirementID: produced.ID,
		}, []byte(`{"session":"`+core.NewTaskID()+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		// A second, equally valid transcript for the same requirement: the
		// finalize contract has to distinguish it from the archived one.
		otherTranscript, err := st.CreateArtifact(ctx, core.Artifact{
			Name: "planning-transcript-retry.json", ContentType: "application/json",
			Role: core.ArtifactRoleGeneratedAudit, RequirementID: produced.ID,
		}, []byte(`{"session":"`+core.NewTaskID()+`"}`))
		if err != nil {
			t.Fatal(err)
		}

		// A session produces a requirement or a blueprint task, never both and
		// never neither — a session that produced nothing is abandoned.
		session := createPlanningSession(t, ctx, st)
		for name, request := range map[string]store.PlanningFinalizeRequest{
			"both artifacts": {SessionID: session.ID, RequirementID: produced.ID, TaskID: blueprint.ID},
			"no artifact":    {SessionID: session.ID},
		} {
			if _, err = st.FinalizePlanningSession(ctx, request); err == nil {
				t.Fatalf("finalize with %s was accepted", name)
			}
		}
		if unchanged, getErr := st.GetPlanningSession(ctx, session.ID); getErr != nil ||
			unchanged.Status != core.PlanningSessionActive || !unchanged.FinalizedAt.IsZero() {
			t.Fatalf("session after rejected finalize=%+v err=%v, want still active", unchanged, getErr)
		}
		if _, err = st.FinalizePlanningSession(ctx, store.PlanningFinalizeRequest{
			SessionID: "session-does-not-exist", RequirementID: produced.ID,
		}); err == nil {
			t.Fatal("finalize of an unknown session was accepted")
		}

		var requirementRun core.PlanningSession
		err = st.WithPlanningSessionFinalization(ctx, session.ID, func(lockedCtx context.Context) error {
			var finalizeErr error
			requirementRun, finalizeErr = st.FinalizePlanningSession(lockedCtx, store.PlanningFinalizeRequest{
				SessionID: session.ID, RequirementID: produced.ID, TranscriptArtifactID: transcript.ID,
				Title: produced.Title,
			})
			return finalizeErr
		})
		if err != nil {
			t.Fatal(err)
		}
		if requirementRun.Status != core.PlanningSessionFinalized ||
			requirementRun.ProducedRequirementID != produced.ID || requirementRun.ProducedTaskID != "" ||
			requirementRun.TranscriptArtifactID != transcript.ID || requirementRun.FinalizedAt.IsZero() {
			t.Fatalf("finalized session=%+v", requirementRun)
		}
		// The produced artifact names the session; the provisional title is
		// gone.
		if requirementRun.Title != produced.Title {
			t.Fatalf("finalized session title=%q, want produced requirement title %q",
				requirementRun.Title, produced.Title)
		}
		assertPlanningSessionRoundTrip(t, ctx, st, requirementRun)

		// An identical repeat is a retry — a stream that reconnected — so it
		// must not error or move the finalized timestamp.
		repeat, err := st.FinalizePlanningSession(ctx, store.PlanningFinalizeRequest{
			SessionID: session.ID, RequirementID: produced.ID, TranscriptArtifactID: transcript.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if repeat.ProducedRequirementID != produced.ID || repeat.ProducedTaskID != "" ||
			!repeat.FinalizedAt.Equal(requirementRun.FinalizedAt) ||
			repeat.Title != produced.Title {
			t.Fatalf("idempotent finalize=%+v, want %+v", repeat, requirementRun)
		}
		// A different artifact is a contradiction, not a retry: overwriting
		// would strand the lineage the first finalize recorded. The archived
		// transcript is part of that lineage, so naming a different one — or
		// dropping it — contradicts just as much as a different requirement
		// would; accepting it silently would acknowledge a transcript that was
		// never persisted.
		for name, request := range map[string]store.PlanningFinalizeRequest{
			"a different requirement": {SessionID: session.ID, RequirementID: other.ID},
			"a blueprint task":        {SessionID: session.ID, TaskID: blueprint.ID},
			"a different transcript": {SessionID: session.ID, RequirementID: produced.ID,
				TranscriptArtifactID: otherTranscript.ID},
			"no transcript at all": {SessionID: session.ID, RequirementID: produced.ID},
		} {
			if _, err = st.FinalizePlanningSession(ctx, request); err == nil {
				t.Fatalf("contradicting finalize naming %s was accepted", name)
			}
		}
		if intact, getErr := st.GetPlanningSession(ctx, session.ID); getErr != nil ||
			intact.ProducedRequirementID != produced.ID || intact.ProducedTaskID != "" ||
			intact.TranscriptArtifactID != transcript.ID ||
			!intact.FinalizedAt.Equal(requirementRun.FinalizedAt) {
			t.Fatalf("session after contradicting finalize=%+v err=%v", intact, getErr)
		}
		// Abandoning would strand what the session produced.
		if _, err = st.AbandonPlanningSession(ctx, session.ID); err == nil {
			t.Fatal("abandoning a finalized session was accepted")
		}

		// The blueprint-task branch of the exclusive contract.
		taskSession := createPlanningSession(t, ctx, st)
		var blueprintRun core.PlanningSession
		err = st.WithPlanningSessionFinalization(ctx, taskSession.ID, func(lockedCtx context.Context) error {
			var finalizeErr error
			blueprintRun, finalizeErr = st.FinalizePlanningSession(lockedCtx, store.PlanningFinalizeRequest{
				SessionID: taskSession.ID, TaskID: blueprint.ID,
			})
			return finalizeErr
		})
		if err != nil {
			t.Fatal(err)
		}
		if blueprintRun.ProducedTaskID != blueprint.ID || blueprintRun.ProducedRequirementID != "" ||
			blueprintRun.Status != core.PlanningSessionFinalized || blueprintRun.FinalizedAt.IsZero() {
			t.Fatalf("blueprint-producing session=%+v", blueprintRun)
		}
		assertPlanningSessionRoundTrip(t, ctx, st, blueprintRun)

		// Abandonment is idempotent, so a closed browser tab retried twice
		// leaves one record.
		spare := createPlanningSession(t, ctx, st)
		firstAbandon, err := st.AbandonPlanningSession(ctx, spare.ID, "Requirements were superseded")
		if err != nil {
			t.Fatal(err)
		}
		if firstAbandon.Status != core.PlanningSessionAbandoned || !firstAbandon.FinalizedAt.IsZero() {
			t.Fatalf("abandoned session=%+v", firstAbandon)
		}
		eventStore, ok := st.(planningSessionEventStore)
		if !ok {
			t.Fatal("store cannot list planning-session events")
		}
		events, err := eventStore.ListPlanningSessionEvents(ctx, spare.ID)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		for _, event := range events {
			if event.Kind == "planning_session.abandoned" {
				_ = json.Unmarshal(event.Payload, &payload)
			}
		}
		if payload["reason"] != "Requirements were superseded" {
			t.Fatalf("abandon payload=%v", payload)
		}
		secondAbandon, err := st.AbandonPlanningSession(ctx, spare.ID)
		if err != nil {
			t.Fatal(err)
		}
		if secondAbandon.Status != core.PlanningSessionAbandoned ||
			!secondAbandon.UpdatedAt.Equal(firstAbandon.UpdatedAt) {
			t.Fatalf("repeat abandon=%+v, want %+v", secondAbandon, firstAbandon)
		}
		if _, err = st.AbandonPlanningSession(ctx, "session-does-not-exist"); err == nil {
			t.Fatal("abandoning an unknown session was accepted")
		}
		assertPlanningSessionRoundTrip(t, ctx, st, secondAbandon)
	})

	t.Run("every read is workspace scoped", func(t *testing.T) {
		st, ctx, workspace := newRequirementFixture(t, factory)
		requirement, first := createRequirement(t, ctx, st, "Scoped Intent",
			chatVersion("Scoped prose.", requirementStatement("REQ-1", "One.")))
		session := createPlanningSession(t, ctx, st)
		if _, err := st.AppendPlanningMessage(ctx, core.PlanningMessage{
			SessionID: session.ID, Role: core.PlanningMessageUser, Content: "Scoped turn.",
		}); err != nil {
			t.Fatal(err)
		}

		// A neighbouring workspace sees none of it. The sibling scope needs no
		// provisioning because only reads are exercised through it — the point
		// is that identity alone never crosses the boundary.
		sibling := store.WithWorkspace(ctx, workspace+"-sibling")
		if _, err := st.GetRequirement(sibling, requirement.ID); err == nil {
			t.Fatal("requirement was readable from another workspace")
		}
		if _, err := st.GetRequirementVersion(sibling, requirement.ID, first.Version); err == nil {
			t.Fatal("requirement version was readable from another workspace")
		}
		if _, err := st.GetPlanningSession(sibling, session.ID); err == nil {
			t.Fatal("planning session was readable from another workspace")
		}
		if listed, err := st.ListRequirements(sibling); err != nil || len(listed) != 0 {
			t.Fatalf("cross-workspace ListRequirements=%+v err=%v, want empty", listed, err)
		}
		if versions, err := st.ListRequirementVersions(sibling, requirement.ID); err != nil || len(versions) != 0 {
			t.Fatalf("cross-workspace ListRequirementVersions=%+v err=%v, want empty", versions, err)
		}
		if sessions, err := st.ListPlanningSessions(sibling); err != nil || len(sessions) != 0 {
			t.Fatalf("cross-workspace ListPlanningSessions=%+v err=%v, want empty", sessions, err)
		}
		if messages, err := st.ListPlanningMessages(sibling, session.ID); err != nil || len(messages) != 0 {
			t.Fatalf("cross-workspace ListPlanningMessages=%+v err=%v, want empty", messages, err)
		}
		// Confirmation cannot reach across either: an operator in one workspace
		// must not be able to make another workspace's intent authoritative.
		if _, _, err := st.ConfirmRequirementVersion(sibling, requirement.ID, first.Version); err == nil {
			t.Fatal("requirement version was confirmable from another workspace")
		}
		// The owning workspace still sees everything.
		if _, err := st.GetRequirement(ctx, requirement.ID); err != nil {
			t.Fatal(err)
		}
		if messages, err := st.ListPlanningMessages(ctx, session.ID); err != nil || len(messages) != 1 {
			t.Fatalf("owning workspace messages=%+v err=%v, want 1", messages, err)
		}
	})

	t.Run("task authored requirement proposals gate review until decided", func(t *testing.T) {
		st, ctx, workspace := newRequirementFixture(t, factory)
		taskID := "task-requirement-gate-" + core.NewTaskID()
		if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		requirement, initial, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-gate-" + core.NewTaskID(), Title: "Review gate"}, core.RequirementVersion{
			Content:    "# Review gate\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Review waits for proposal decisions.\n```",
			Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Review waits for proposal decisions."}}, Origin: core.RequirementOriginOperator,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, initial.Version); err != nil {
			t.Fatal(err)
		}
		proposal, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: requirement.ID,
			Content:       "# Review gate\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Review waits for every task-authored proposal decision.\n```",
			Statements:    []core.RequirementStatement{{ID: "REQ-1", Statement: "Review waits for every task-authored proposal decision."}},
			Origin:        core.RequirementOriginImplementation,
			OriginTaskID:  taskID,
		})
		if err != nil {
			t.Fatal(err)
		}
		pending, err := st.ListPendingProposals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range pending {
			found = found || item.Tier == "requirement" && item.ID == requirement.ID && item.Version == proposal.Version && item.OriginType == "task" && item.OriginID == taskID
		}
		if !found {
			t.Fatalf("task-authored requirement missing from pending proposals: %+v", pending)
		}
		job := core.Job{ID: taskID + "-review-1", TaskID: taskID, Stage: core.StageReview, State: core.JobPending}
		if err = st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err = For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued}); err != nil {
			t.Fatal(err)
		}
		projection, err := st.PendingProposalsProjection(ctx)
		if err != nil || projection.TaskCount != 1 {
			t.Fatalf("task-authored requirement attention=%+v err=%v", projection, err)
		}
		if _, err = For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "blocked", ClientToken: "blocked-token", Lease: time.Minute}); err == nil || !strings.Contains(err.Error(), requirement.ID) {
			t.Fatalf("pending requirement did not name itself in review gate: %v", err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, proposal.Version, initial.Version); err != nil {
			t.Fatal(err)
		}
		if _, err = For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "released", ClientToken: "released-token", Lease: time.Minute}); err != nil {
			t.Fatalf("review stayed blocked after requirement decision: %v", err)
		}

		dismissedTaskID := "task-requirement-dismiss-" + core.NewTaskID()
		if err = st.CreateTask(ctx, core.Task{ID: dismissedTaskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + dismissedTaskID, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		dismissed, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: requirement.ID,
			Content:       "# Review gate\n\n```conveyor:requirements\n- id: REQ-1\n  statement: This task-authored alternative may be dismissed.\n```",
			Statements:    []core.RequirementStatement{{ID: "REQ-1", Statement: "This task-authored alternative may be dismissed."}},
			Origin:        core.RequirementOriginImplementation,
			OriginTaskID:  dismissedTaskID,
		})
		if err != nil {
			t.Fatal(err)
		}
		operatorChoice, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
			RequirementID: requirement.ID,
			Content:       "# Review gate\n\n```conveyor:requirements\n- id: REQ-1\n  statement: The operator selected a different revision.\n```",
			Statements:    []core.RequirementStatement{{ID: "REQ-1", Statement: "The operator selected a different revision."}},
			Origin:        core.RequirementOriginOperator,
		})
		if err != nil {
			t.Fatal(err)
		}
		dismissedJob := core.Job{ID: dismissedTaskID + "-review-1", TaskID: dismissedTaskID, Stage: core.StageReview, State: core.JobPending}
		if err = st.CreateJob(ctx, dismissedJob); err != nil {
			t.Fatal(err)
		}
		if err = For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: dismissedJob.ID, TaskID: dismissedTaskID, JobID: dismissedJob.ID, Stage: core.StageReview, State: core.WorkOrderQueued}); err != nil {
			t.Fatal(err)
		}
		if _, err = For(st).ClaimWorkOrder(ctx, dismissedJob.ID, core.WorkOrderClaim{SessionID: "dismiss-blocked", ClientToken: "dismiss-blocked-token", Lease: time.Minute}); err == nil || !strings.Contains(err.Error(), requirement.ID) {
			t.Fatalf("review was not blocked before dismissal: %v", err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, operatorChoice.Version, proposal.Version); err != nil {
			t.Fatal(err)
		}
		dismissed, err = st.GetRequirementVersion(ctx, requirement.ID, dismissed.Version)
		if err != nil || !dismissed.Retired || dismissed.RetiredByVersion != operatorChoice.Version {
			t.Fatalf("task-authored proposal was not dismissed by the selected version: %+v err=%v", dismissed, err)
		}
		if _, err = For(st).ClaimWorkOrder(ctx, dismissedJob.ID, core.WorkOrderClaim{SessionID: "dismiss-released", ClientToken: "dismiss-released-token", Lease: time.Minute}); err != nil {
			t.Fatalf("review stayed blocked after requirement dismissal: %v", err)
		}
	})
}

func newRequirementFixture(t *testing.T, factory RequirementFactory) (store.Store, context.Context, string) {
	t.Helper()
	fixture := factory(t, requirementConformanceRepos)
	return fixture.Store,
		store.WithActor(fixture.Context, store.Actor{ID: requirementConformanceActor, Role: core.ActorHuman}),
		fixture.Workspace
}

func requirementStatement(id, statement string) core.RequirementStatement {
	return core.RequirementStatement{ID: id, Statement: statement}
}

// chatVersion builds a planning-session revision. Chat origin carries the
// session that revised the document.
func chatVersion(content string, statements ...core.RequirementStatement) core.RequirementVersion {
	return core.RequirementVersion{
		Content: content, Statements: statements,
		Origin: core.RequirementOriginChat, OriginSessionID: "session-" + core.NewTaskID(),
	}
}

func chatVersionFor(requirementID, content string, statements ...core.RequirementStatement) core.RequirementVersion {
	version := chatVersion(content, statements...)
	version.RequirementID = requirementID
	return version
}

// driftVersionFor builds the monitor's requirements_amended revision, which
// carries the drift record instead of a session.
func driftVersionFor(requirementID, content string, statements ...core.RequirementStatement) core.RequirementVersion {
	return core.RequirementVersion{
		RequirementID: requirementID, Content: content, Statements: statements,
		Origin: core.RequirementOriginDriftAmendment, OriginDriftID: "drift-" + core.NewTaskID(),
	}
}

func createRequirement(t *testing.T, ctx context.Context, st store.Store, title string, first core.RequirementVersion) (core.Requirement, core.RequirementVersion) {
	t.Helper()
	requirement, version, err := st.CreateRequirement(ctx,
		core.Requirement{ID: "req-" + core.NewTaskID(), Title: title}, first)
	if err != nil {
		t.Fatal(err)
	}
	return requirement, version
}

func createPlanningSession(t *testing.T, ctx context.Context, st store.Store) core.PlanningSession {
	t.Helper()
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{
		ID: "session-" + core.NewTaskID(), Title: "Planning",
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func planningTask(workspace string) core.Task {
	id := core.NewTaskID()
	return core.Task{
		ID: id, Workspace: workspace, Source: "test:planning", Title: "Blueprint from planning",
		Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id,
		State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(),
	}
}

func assertRequirementRoundTrip(t *testing.T, ctx context.Context, st store.Store, want core.Requirement) {
	t.Helper()
	got, err := st.GetRequirement(ctx, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Slug != want.Slug || got.Title != want.Title ||
		got.Workspace != want.Workspace || got.CurrentVersion != want.CurrentVersion ||
		got.StatementHighWaterMark != want.StatementHighWaterMark ||
		!sameInstant(got.CreatedAt, want.CreatedAt) || got.UpdatedAt.IsZero() {
		t.Fatalf("read back requirement=%+v, want %+v", got, want)
	}
}

func assertRequirementVersionRoundTrip(t *testing.T, ctx context.Context, st store.Store, want core.RequirementVersion) {
	t.Helper()
	got, err := st.GetRequirementVersion(ctx, want.RequirementID, want.Version)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequirementID != want.RequirementID || got.Version != want.Version ||
		got.Content != want.Content || got.Workspace != want.Workspace ||
		got.Origin != want.Origin || got.OriginSessionID != want.OriginSessionID ||
		got.OriginDriftID != want.OriginDriftID || got.Confirmed != want.Confirmed ||
		got.ConfirmedBy != want.ConfirmedBy || !sameInstant(got.ConfirmedAt, want.ConfirmedAt) ||
		got.Retired != want.Retired || got.RetiredBy != want.RetiredBy ||
		!sameInstant(got.RetiredAt, want.RetiredAt) || got.RetiredByVersion != want.RetiredByVersion ||
		!sameInstant(got.CreatedAt, want.CreatedAt) {
		t.Fatalf("read back version=%+v, want %+v", got, want)
	}
	if !sameRequirementStatements(got.Statements, want.Statements) {
		t.Fatalf("read back statements=%+v, want %+v", got.Statements, want.Statements)
	}
}

func assertPlanningSessionRoundTrip(t *testing.T, ctx context.Context, st store.Store, want core.PlanningSession) {
	t.Helper()
	got, err := st.GetPlanningSession(ctx, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Title != want.Title || got.Status != want.Status ||
		got.Goal != want.Goal ||
		got.Workspace != want.Workspace || got.RequirementContextID != want.RequirementContextID ||
		got.ProducedRequirementID != want.ProducedRequirementID || got.ProducedTaskID != want.ProducedTaskID ||
		got.TranscriptArtifactID != want.TranscriptArtifactID ||
		!sameInstant(got.CreatedAt, want.CreatedAt) || !sameInstant(got.FinalizedAt, want.FinalizedAt) {
		t.Fatalf("read back session=%+v, want %+v", got, want)
	}
	if (got.Promotion == nil) != (want.Promotion == nil) ||
		got.Promotion != nil && *got.Promotion != *want.Promotion {
		t.Fatalf("read back promotion=%+v, want %+v", got.Promotion, want.Promotion)
	}
}

func assertRequirementCount(t *testing.T, ctx context.Context, st store.Store, want int) {
	t.Helper()
	listed, err := st.ListRequirements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != want {
		t.Fatalf("requirements=%d, want %d", len(listed), want)
	}
}

// sameRequirementStatements compares statement blocks by content and order. It
// treats a nil block and an empty block as equal: the in-memory store returns
// the caller's nil slice for a statement-less migration seed while Postgres
// round-trips it through an empty jsonb array. Nothing an operator can observe
// distinguishes them.
func sameRequirementStatements(got, want []core.RequirementStatement) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if !reflect.DeepEqual(got[index], want[index]) {
			return false
		}
	}
	return true
}

// assertSamePlanningParts compares AI SDK message parts by decoded value.
// Postgres stores parts in a jsonb column, which re-serializes canonically
// (sorted keys, its own spacing), while the in-memory store hands back exactly
// the bytes it was given. The contract both keep is that nothing in the payload
// is lost, so the suite asserts decoded equality here and asserts byte identity
// separately against a payload already written in canonical form.
func assertSamePlanningParts(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	if len(want) == 0 {
		// An omitted parts payload is stored as nil by one implementation and
		// as an empty JSON array by the other; both mean "no parts".
		if len(got) != 0 && string(got) != "[]" {
			t.Fatalf("parts for a message with none=%s, want empty", got)
		}
		return
	}
	var decodedGot, decodedWant any
	if err := json.Unmarshal(got, &decodedGot); err != nil {
		t.Fatalf("restored parts %s are not JSON: %v", got, err)
	}
	if err := json.Unmarshal(want, &decodedWant); err != nil {
		t.Fatalf("submitted parts %s are not JSON: %v", want, err)
	}
	if !reflect.DeepEqual(decodedGot, decodedWant) {
		t.Fatalf("restored parts=%s, want %s", got, want)
	}
}

// sameInstant compares timestamps at the coarsest precision both stores keep.
// Postgres timestamptz truncates to microseconds while the in-memory store
// keeps the nanosecond clock reading, so an exact comparison would fail on a
// difference that carries no meaning.
func sameInstant(got, want time.Time) bool {
	if got.IsZero() != want.IsZero() {
		return false
	}
	delta := got.Sub(want)
	if delta < 0 {
		delta = -delta
	}
	return delta < time.Microsecond
}
