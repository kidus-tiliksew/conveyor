package storetest

import (
	"context"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"net/http"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Capabilities describe behavior, not merely satisfaction of Go interfaces.
// A production backend must exercise every capability (DEC-38).
type Capabilities struct {
	Identity, Membership, Tokens bool
}

type ArtifactListStatementContract uint8

const (
	ArtifactListStatementsUnspecified ArtifactListStatementContract = iota
	ArtifactListStatementsExactlyTwo
)

type Fixture struct {
	ReopenVerification func(*testing.T) store.Backend
	Backend            store.Backend
	Context            context.Context
	Workspace          string
	Config             *config.Config
	SeedLegacy         func(*testing.T, string) (int, func(*testing.T))

	RestoreSupersededDeliveries func(*testing.T, context.Context, string)
	// RestoreHistoricalDelivery sets one superseded delivery generation of a
	// task back to state, error class and retry time, as releases before the
	// supersede rule left older generations; no command writes such a row.
	RestoreHistoricalDelivery func(t *testing.T, ctx context.Context, taskID string, generation int, state, errorClass string, nextAttemptAt *time.Time)
	// DeliveryPassSkipsHeldKeys is set by a backend whose delivery commands
	// hold per-pull-request locks a reconcile pass can skip.
	DeliveryPassSkipsHeldKeys bool
	// SeedForeignVerificationOperation stores one more operation row for a
	// task whose payload does not decode, so a read that decoded another
	// task's operations fails.
	SeedForeignVerificationOperation func(t *testing.T, ctx context.Context, taskID string)
	DrainReadQueries                 func() map[string]uint64
	ArtifactListStatements           ArtifactListStatementContract

	// SeedHistoricalLink inserts one retained, non-projector-owned link row
	// exactly as historical migrations left it, such as migration 046's
	// historical_feature_assignment with legacy feature.migrated provenance.
	// No public writer accepts a link (component-lineage), so each backend
	// seeds it directly in its test binary.
	SeedHistoricalLink   func(*testing.T, context.Context, core.LineageLink)
	SeedArtifact         func(*testing.T, context.Context, core.Artifact, []byte)
	ArtifactRepairEvents func(context.Context) ([]core.Event, error)
	// WorkspaceEvents returns the bound workspace's events without a task
	// whose kind starts with kindPrefix, ascending by (at, id). Shared cases
	// use it where no public read exposes a workspace event kind, such as
	// decision supersession sweep transitions.
	WorkspaceEvents func(ctx context.Context, kindPrefix string) ([]core.Event, error)
	// SeedEvents inserts test-only task events with controlled immutable IDs
	// and recorded times, modelling SingleStore's per-aggregator
	// AUTO_INCREMENT ranges. Each event's ID is a positive relative rank added
	// to base; a zero base selects a fresh one far above every existing event
	// ID. An empty TaskID seeds a workspace-level event such as a proposal.
	// The stored events are returned. Committed rows are never rewritten.
	SeedEvents func(t *testing.T, ctx context.Context, base int64, events []core.Event) []core.Event
	// DeactivateUser marks one account inactive for backends that expose no
	// deactivation operation, such as SingleStore. Others use their
	// DeactivateIdentityUser method (component-identity-membership).
	DeactivateUser func(t *testing.T, ctx context.Context, userID string)
	// SeedSetupContract replaces one task's stored setup contract with raw
	// JSON, modelling rows an earlier encoding wrote. No public writer accepts
	// a raw contract; the volatile backend decodes it into the task.
	SeedSetupContract func(t *testing.T, ctx context.Context, taskID, raw string)
	// SeedTaskIntakeKey writes key as one task's intake key directly, without
	// the command's uniqueness check, as a SingleStore row an older writer
	// could leave. It reports whether the backend stored the key: a backend
	// whose schema refuses a duplicate key, as PostgreSQL's unique index
	// does, proves the refusal and reports false.
	SeedTaskIntakeKey func(t *testing.T, ctx context.Context, taskID, key string) bool
	// SeedStoredTaskHold sets one task's stored historical hold column to
	// true, modelling a row written before the hold retirement (DEC-125(3)).
	// No public writer can; each backend seeds it directly in its test
	// binary. The hold-retirement cases require it and never skip.
	SeedStoredTaskHold func(t *testing.T, ctx context.Context, taskID string)
	// SeedStoredStaleWorkOrder sets one stored order to the retired stale
	// value, as a row the retired queue clock left would read. With exhausted
	// it also stores the automatic-recovery exhaustion that clock could leave
	// (retry_suppressed with reason automatic recovery exhausted, a count of
	// two, a window started an hour ago, and no next automatic recovery
	// time), and a non-zero scheduledAt stores that next automatic recovery
	// time. No command writes stale any more (component-work-orders
	// invariant 5).
	SeedStoredStaleWorkOrder func(t *testing.T, ctx context.Context, orderID string, exhausted bool, scheduledAt time.Time)
	// SeedRiskRating inserts one numbered risk rating row and its risk.rated
	// event, modelling a rating with source reviewer that a review verdict
	// recorded before verdicts stopped stating a tier. No public writer
	// accepts that source (component-review invariant 13), so each test
	// binary seeds it directly. The recorded reviewer rating case requires it
	// and never skips (req-risk-based-review AC-5.6).
	SeedRiskRating func(t *testing.T, ctx context.Context, rating core.RiskRating)
	// SeedVerificationAttemptAuthorization sets one stored attempt's
	// ReplayAuthorizationID, modelling a row that an ungated start wrote
	// before ungated starts stopped recording the caller's ID. No public
	// writer can, so each test binary seeds it directly. The retry gate's
	// legacy-row case requires it and never skips
	// (component-verification-service invariant 19).
	SeedVerificationAttemptAuthorization func(t *testing.T, ctx context.Context, attemptID, authorizationID string)
	// SeedWorkspaceConfigYAML replaces the bound workspace's stored
	// configuration document with raw YAML without changing its version,
	// modelling a row an earlier release wrote, and WorkspaceConfigYAML
	// returns the stored document's bytes. No public writer or read exposes
	// either, so each test binary supplies both. The WorkspaceControl
	// legacy-row case requires them and never skips (component-persistence).
	SeedWorkspaceConfigYAML func(t *testing.T, ctx context.Context, raw string)
	WorkspaceConfigYAML     func(t *testing.T, ctx context.Context) string
	// LineageHTTP returns the real httpapi handler over Backend, bound to
	// Workspace and accepting LineageHTTPToken as its operator bearer, so the
	// Lineage suite reads GET /v1/lineage/{type}/{id}. storetest cannot
	// import internal/httpapi, because httpapi tests import storetest, so
	// each conformance entry point constructs it
	// (component-verification-strategy; component-lineage). The
	// dependency-removal case and the inbox attention count case require it
	// and never skip.
	LineageHTTP func(t *testing.T) http.Handler
	// RedispatchHTTP returns the LineageHTTP handler with onDispatch as its
	// OnCreate and a real dispatcher as its OnIntervention. The redispatch
	// gate case reads the route's dispatch request through onDispatch and
	// records a plan-revision decision through POST /v1/tasks/{id}/review,
	// which moves no task without a dispatcher. It requires this field and
	// never skips (component-work-orders invariant 39).
	RedispatchHTTP func(t *testing.T, onDispatch func(context.Context, string)) http.Handler
	// WorkOrderHTTP returns the LineageHTTP handler with a work-order service
	// over Backend, as conveyord configures it, so GET /v1/work-orders serves
	// the service's claim projections. The task observation claimability
	// case compares that list with GET /v1/tasks/{id}/observation. It
	// requires this field and never skips (component-next-action invariant
	// 57).
	WorkOrderHTTP func(t *testing.T) http.Handler
	// TaskIntakeHTTP returns the real httpapi handler over Backend, bound to
	// Workspace and accepting LineageHTTPToken, with generate as its title
	// generator. GitHubIssueIntake takes one ready issue in through the real
	// dispatcher with the label move stubbed. storetest cannot import httpapi
	// or dispatch, so each conformance entry point constructs them. The task
	// title cases require both and never skip (req-security-boundaries REQ-3).
	TaskIntakeHTTP    func(t *testing.T, generate func(context.Context, core.Task) (string, error)) http.Handler
	GitHubIssueIntake func(ctx context.Context, repo config.Repo, issue github.Issue) error
	// VerificationClaimReads observes the tasks whose verification rows
	// ReconcileVerificationClaims reads in Workspace and returns a reader of
	// the task IDs seen so far, in read order. The durable backends read
	// through internal SQL, so each test binary installs its backend's read
	// observer; durable backends must provide it. The volatile backend reads
	// in-process and leaves it nil (component-persistence).
	VerificationClaimReads func(t *testing.T) func() []string
	// SeedDashboardSessionTimes sets one dashboard session's stored last use
	// and expiry relative to the backend's own clock, and
	// DashboardSessionTimes returns that stored pair. No public writer or
	// read exposes either, so each test binary supplies both. The
	// InvitationSessions/SessionRefresh cases require them and never skip
	// (component-identity-membership).
	SeedDashboardSessionTimes func(t *testing.T, ctx context.Context, sessionID string, lastUsedAgo, expiresIn time.Duration)
	DashboardSessionTimes     func(t *testing.T, ctx context.Context, sessionID string) (lastUsedAt, expiresAt time.Time)
	// SeedCredentialLastUsed sets one bearer credential's stored last use
	// relative to the backend's own clock, and CredentialLastUsed returns
	// it, zero when unset. No public writer sets it and no public read
	// exposes a stored agent row's, so each test binary supplies both. The
	// Tokens/CredentialUseStamp cases require them and never skip
	// (component-identity-membership).
	SeedCredentialLastUsed func(t *testing.T, ctx context.Context, credentialID string, lastUsedAgo time.Duration)
	CredentialLastUsed     func(t *testing.T, ctx context.Context, credentialID string) time.Time
	// SeedStoredAgentCredential writes one user_tokens row of kind agent, as
	// the retired class stored it, and returns its ID and presented value. No
	// public writer can, so each test binary supplies it by raw SQL or its own
	// state. revoked sets revoked_at. The Tokens/RetiredAgentCredential cases
	// require it and never skip (DEC-116(3); component-identity-membership
	// invariant 35).
	SeedStoredAgentCredential func(t *testing.T, ctx context.Context, userID, label string, revoked bool) (id, value string)
	// DrainReadBytes drains the backend's read ledger and returns the bytes
	// received per read label. Production-capable backends must provide it;
	// the volatile backend transfers no bytes and leaves it nil
	// (component-persistence, Metrics and database read accounting).
	DrainReadBytes func() map[string]uint64
	// DrainReadRows drains the same read ledger and returns the rows received
	// per read label, so a suite can bound the rows one read transfers. A
	// backend that transfers no rows leaves it nil.
	DrainReadRows func() map[string]uint64
	// NestedTaskCommandsInterleave is true when a command started under a
	// WithTaskSideEffectLock context skips the task-operation lock that the
	// context already owns, so a second command of that context commits while
	// the first is inside its transaction: SingleStore. The risk order cases
	// then commit a cancellation, a raise, or a release inside a completion's
	// read-to-write window. That is the order a commit spanning partitions
	// produces when it releases the lock before the completion reads its
	// order row (component-persistence invariant 8). The volatile backend and
	// PostgreSQL leave it false: their completion holds the lock across that
	// window, and the cases prove that the other command waits.
	NestedTaskCommandsInterleave bool
	// AwaitTaskOperationWaiter returns once the backend reports a session
	// waiting on the task's task-operation lock, read from the database and
	// not inferred from elapsed time, and fails when done closes first.
	// PostgreSQL provides it. The volatile backend's lock is in-process and
	// has no such report, so it leaves it nil.
	AwaitTaskOperationWaiter func(t *testing.T, ctx context.Context, taskID string, done <-chan struct{})
}

// requireStoredTaskHoldSeed returns the fixture's historical hold seed or
// fails the case; a fixture without it cannot prove the retirement.
func requireStoredTaskHoldSeed(t *testing.T, seed func(*testing.T, context.Context, string)) func(*testing.T, context.Context, string) {
	t.Helper()
	if seed == nil {
		t.Fatal("conformance fixture must provide SeedStoredTaskHold for the hold-retirement cases (DEC-125(3))")
	}
	return seed
}

// Factory creates an isolated backend and workspace on every call. Cleanup
// belongs to the supplied testing.T. Repository names are fixture inputs.
type Factory struct {
	New               func(*testing.T, []config.Repo) Fixture
	Capabilities      Capabilities
	ProductionCapable bool
	// PlanRevisionGate is dispatch.PendingPlanRevisionGate. storetest cannot
	// import dispatch: dispatch integration tests import storetest.
	PlanRevisionGate PlanRevisionGateFunc
	// ReviewSweep is workorder.Service.SweepAutomaticReviewRecovery. storetest
	// cannot import workorder: workorder tests import storetest.
	ReviewSweep ReviewSweepFunc
	// ClaimService is workorder.Service.Claim. storetest cannot import
	// workorder: workorder tests import storetest. The claim-failure stall
	// case requires it and never skips.
	ClaimService ClaimServiceFunc
	// SubmitPlanService is workorder.Service.SubmitPlan with a real
	// dispatcher, for the same reason. The plan-submission serialization
	// case requires it and never skips.
	SubmitPlanService SubmitPlanServiceFunc
	// StatementMemo builds an HTTP server over the backend, so the workspace
	// notice suite proves each notice drops the response memo. A nil value
	// proves the notices alone.
	StatementMemo StatementMemoFunc
}

func (f Factory) fresh(t *testing.T, repos []config.Repo) Fixture {
	t.Helper()
	if repos == nil {
		repos = []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}, {Name: "app", URL: "https://example.test/app", Base: "main"}}
	}
	x := f.New(t, repos)
	if x.Backend == nil || x.Context == nil || x.Workspace == "" || x.Config == nil {
		t.Fatal("factory must return a backend, workspace context and configuration")
	}
	if workspace, ok := store.WorkspaceFromContext(x.Context); !ok || workspace != x.Workspace {
		t.Fatal("factory workspace does not match its context")
	}
	return x
}

// RunAll is the common conformance entry point for the memory, PostgreSQL,
// and SingleStore backends. component-verification-strategy owns its
// orchestration and method coverage check; each domain component owns its own
// cases, and logtest stays separate.
func RunAll(t *testing.T, factory Factory) {
	t.Helper()
	started := time.Now()
	t.Cleanup(func() { t.Logf("RunAll elapsed=%s", time.Since(started)) })
	if err := factory.validate(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	run := func(name string, fn func(*testing.T)) {
		if _, ok := suiteMethods[name]; !ok {
			t.Fatalf("suite %s has no method declaration", name)
		}
		if seen[name] {
			t.Fatalf("suite %s was registered twice", name)
		}
		seen[name] = true
		t.Run(name, fn)
	}
	t.Cleanup(func() {
		for name := range suiteMethods {
			if !seen[name] {
				t.Errorf("declared suite %s was not registered", name)
			}
		}
	})
	requirements := func(t *testing.T, repos []config.Repo) RequirementFixture {
		x := factory.fresh(t, repos)
		return RequirementFixture{x.Backend, x.Context, x.Workspace}
	}
	run("ListPages", func(t *testing.T) { runListPages(t, factory.fresh(t, nil)) })
	run("NextAction", func(t *testing.T) {
		x := factory.fresh(t, nil)
		RunNextActionConformance(t, x.Backend, x.Context)
	})
	run("Blueprint", func(t *testing.T) {
		RunBlueprintConformance(t, func(t *testing.T, repos []config.Repo) BlueprintFixture {
			x := factory.fresh(t, repos)
			return BlueprintFixture{x.Backend, x.Context, x.Workspace, x.SeedStoredTaskHold}
		})
	})
	run("ChangeNotices", func(t *testing.T) { RunChangeNoticeConformance(t, factory.fresh(t, nil)) })
	run("WorkspaceNotices", func(t *testing.T) {
		RunWorkspaceNoticeConformance(t, factory.fresh(t, nil), factory.StatementMemo, factory.Capabilities.Identity && factory.Capabilities.Membership)
	})
	run("Crates", func(t *testing.T) { runCrates(t, factory) })
	run("ClaimFailureStall", func(t *testing.T) { runClaimFailureStall(t, factory.fresh(t, nil), factory.ClaimService) })
	run("RedispatchOfferParity", func(t *testing.T) { runRedispatchOfferParity(t, factory.fresh(t, nil)) })
	run("RedispatchPlanRevisionGate", func(t *testing.T) { runRedispatchPlanRevisionGate(t, factory.fresh(t, nil), factory.PlanRevisionGate) })
	run("InboxAttentionCount", func(t *testing.T) { runInboxAttentionCount(t, factory.fresh(t, nil)) })
	run("TaskObservationClaimability", func(t *testing.T) { runTaskObservationClaimability(t, factory.fresh(t, nil)) })
	run("InboxAttentionCountBodyFree", func(t *testing.T) {
		x := factory.fresh(t, nil)
		if factory.ProductionCapable && x.DrainReadBytes == nil {
			t.Fatal("production-capable conformance fixture must provide DrainReadBytes")
		}
		runInboxAttentionCountBodyFree(t, x)
	})
	run("HoldRetirement", func(t *testing.T) { runHoldRetirement(t, factory.fresh(t, nil), factory.ClaimService) })
	run("PlanSubmitSerialization", func(t *testing.T) {
		runPlanSubmitSerialization(t, factory.fresh(t, nil), factory.SubmitPlanService)
	})
	run("ReviewDistinctModelsNoClaimStall", func(t *testing.T) {
		runDistinctModelsNoClaimFailure(t, factory.fresh(t, nil), factory.ClaimService)
	})
	run("TaskStartOver", func(t *testing.T) { runTaskStartOver(t, factory) })
	run("TaskMap", func(t *testing.T) { RunTaskMapConformance(t, factory.fresh(t, nil)) })
	run("ActivityValidation", func(t *testing.T) { RunActivityValidationConformance(t, factory.fresh(t, nil)) })
	run("ActivityAnchorPaging", func(t *testing.T) { RunActivityAnchorPagingConformance(t, factory.fresh(t, nil)) })
	run("ActivityMarkerCompactReads", func(t *testing.T) {
		x := factory.fresh(t, nil)
		if factory.ProductionCapable && x.DrainReadBytes == nil {
			t.Fatal("production-capable conformance fixture must provide DrainReadBytes")
		}
		runActivityMarkerCompactReads(t, x)
	})
	run("DocumentationGate", func(t *testing.T) { runDocumentationGate(t, factory.fresh(t, nil)) })
	run("TaskFlow", func(t *testing.T) { runTaskFlow(t, factory) })
	run("GateKind", func(t *testing.T) {
		x := factory.fresh(t, nil)
		RunGateKindConformance(t, x.Backend, x.Context, x.Workspace)
	})
	run("FeatureRetirement", func(t *testing.T) { RunFeatureRetirementConformance(t, factory.fresh(t, nil)) })
	run("Requirements", func(t *testing.T) { RunRequirementConformance(t, requirements) })
	run("VersionDismissal", func(t *testing.T) { RunVersionDismissalConformance(t, requirements) })
	run("Lineage", func(t *testing.T) {
		RunLineageConformance(t, func(t *testing.T, repos []config.Repo) LineageFixture {
			x := factory.fresh(t, repos)
			foreign := x.Workspace + "-other"
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), foreign)
			if _, err := x.Backend.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: foreign, Repos: repos}); err != nil {
				t.Fatal(err)
			}
			return LineageFixture{Store: x.Backend, Context: x.Context, ForeignContext: ctx, Workspace: x.Workspace, SeedLegacy: x.SeedLegacy, SeedHistoricalLink: x.SeedHistoricalLink, HTTP: x.LineageHTTP, SeedEvents: x.SeedEvents,
				DrainReadQueries: x.DrainReadQueries, DrainReadRows: x.DrainReadRows}
		})
	})
	for _, suite := range []struct {
		name string
		run  func(*testing.T, store.Store, context.Context)
	}{
		{"CheckpointDecisionRequests", RunCheckpointDecisionRequestConformance},
		{"CheckpointRenewal", RunCheckpointRenewalConformance},
		{"ForgeAuthorIdentity", RunForgeAuthorIdentityConformance},
		{"PlanRevision", RunPlanRevisionConformance},
		{"ReferenceDocuments", RunReferenceDocumentConformance},
		{"SystemDesignProposals", RunSystemDesignProposalConformance},
	} {
		run(suite.name, func(t *testing.T) { x := factory.fresh(t, nil); suite.run(t, x.Backend, x.Context) })
	}
	run("PlanRevisionGatePayload", func(t *testing.T) {
		x := factory.fresh(t, nil)
		RunPlanRevisionGatePayloadConformance(t, x.Backend, x.Context, factory.PlanRevisionGate)
	})
	run("PlanningBundles", func(t *testing.T) {
		RunPlanningBundleConformance(t, func(t *testing.T) (store.Store, context.Context, string) {
			x := factory.fresh(t, nil)
			return x.Backend, x.Context, x.Workspace
		})
	})
	run("SystemDesignConsultation", func(t *testing.T) { RunSystemDesignConsultationConformance(t, factory.fresh(t, nil)) })
	run("SystemDesignDrift", func(t *testing.T) {
		RunSystemDesignDriftConformance(t, func(t *testing.T) (store.Store, context.Context, string) {
			x := factory.fresh(t, nil)
			return x.Backend, x.Context, x.Workspace
		})
	})
	run("TaskContextRepin", func(t *testing.T) {
		x := factory.fresh(t, nil)
		RunTaskContextQueueRepinConformance(t, x.Backend, x.Context, x.Config)
	})
	run("DependencyAddition", func(t *testing.T) {
		x := factory.fresh(t, nil)
		RunDependencyAdditionConformance(t, x.Backend, x.Context, x.Workspace)
	})
	run("AutomaticRecovery", func(t *testing.T) {
		x := factory.fresh(t, nil)
		RunAutomaticRecoveryConformance(t, x.Backend, x.Context)
	})
	run("ReconcileWaitingOrder", func(t *testing.T) {
		x := factory.fresh(t, nil)
		RunReconcileWaitingOrderConformance(t, x.Backend, x.Context, x.Workspace, factory.ProductionCapable)
	})
	run("TimedOutSiblingRetirement", func(t *testing.T) {
		RunTimedOutSiblingRetirementConformance(t, factory.fresh(t, nil))
	})
	run("TerminalOrderRetirement", func(t *testing.T) {
		RunTerminalOrderRetirementConformance(t, factory.fresh(t, nil))
	})
	run("TerminalOrderClock", func(t *testing.T) {
		RunTerminalOrderClockConformance(t, factory.fresh(t, nil))
	})
	run("AutomaticPublicationRecovery", func(t *testing.T) {
		x := factory.fresh(t, nil)
		RunAutomaticPublicationRecoveryConformance(t, x.Backend, x.Context, x.Workspace, factory.ReviewSweep)
	})
	run("AutomaticReviewRecovery", func(t *testing.T) {
		x := factory.fresh(t, nil)
		RunAutomaticReviewRecoveryConformance(t, x.Backend, x.Context, x.Workspace, factory.ReviewSweep)
	})
	run("RiskPolicyFreeze", func(t *testing.T) {
		RunRiskPolicyFreezeConformance(t, factory.fresh(t, nil))
	})
	run("RiskRatings", func(t *testing.T) {
		RunRiskRatingConformance(t, factory.fresh(t, nil))
	})
	run("RiskOrders", func(t *testing.T) {
		RunRiskOrderConformance(t, factory.fresh(t, nil))
	})
	run("HeadRiskOrders", func(t *testing.T) {
		RunHeadRiskOrderConformance(t, factory.fresh(t, nil))
	})
	run("PlanRiskOrders", func(t *testing.T) {
		RunPlanRiskOrderConformance(t, factory.fresh(t, nil))
	})
	run("RiskWaitNextAction", func(t *testing.T) {
		RunRiskWaitNextActionConformance(t, factory.fresh(t, nil))
	})
	run("PlanGateRiskRecovery", func(t *testing.T) {
		RunPlanGateRiskRecoveryConformance(t, factory.fresh(t, nil))
	})
	run("RiskEnforcement", func(t *testing.T) {
		RunRiskEnforcementConformance(t, factory.fresh(t, nil))
	})
	run("RiskAcceptanceAuthors", func(t *testing.T) {
		RunRiskAcceptanceAuthorsConformance(t, factory.fresh(t, nil))
	})
	run("TaskApprovals", func(t *testing.T) {
		RunTaskApprovalConformance(t, factory.fresh(t, nil))
	})
	run("ReviewPolicyStatuses", func(t *testing.T) {
		RunReviewPolicyStatusConformance(t, factory.fresh(t, nil))
	})
	run("ZeroSetupReviewSeats", func(t *testing.T) {
		RunZeroSetupReviewSeatsConformance(t, factory.fresh(t, nil), factory.ReviewSweep)
	})
	for _, suite := range []struct {
		name    string
		enabled bool
		run     func(*testing.T, Fixture)
	}{
		{"BoardConformance", true, runBoardConformance},
		{"TaskOperationsPagination", true, runTaskPagination},
		{"ReconcileReads", true, runReconcileReads},
		{"TaskDetailReads", true, runTaskDetailReads},
		{"ActivityMarkerBounds", true, RunActivityMarkerBoundsConformance},
		{"ActivityMarkerHorizon", true, RunActivityMarkerHorizonConformance},
		{"RequirementDetailReads", true, runRequirementDetailReads},
		{"DocumentVersionSummaries", true, runDocumentVersionSummaries},
		{"RequirementEventReads", true, RunRequirementEventReads},
		{"RequirementDeliveryEvents", true, runRequirementDeliveryEvents},
		{"ScopedDocumentEventReads", true, runScopedDocumentEventReads},
		{"TaskLifecycle", true, runTaskLifecycle},
		{"TaskBranchUniqueness", true, runTaskBranchUniqueness},
		{"TaskBranchConcurrentClaims", true, runTaskBranchConcurrentClaims},
		{"TaskBranchAttach", true, runTaskBranchAttach},
		{"TaskEventAtomicity", true, runTaskEventAtomicity},
		{"TaskCreationProvenance", true, runTaskCreationProvenance},
		{"PlanStateIntake", true, runPlanStateIntake},
		{"PlanStateActs", true, runPlanStateActs},
		{"ArtifactRepair", true, runArtifactRepair},
		{"Verification", true, runVerification},
		{"ArtifactIntake", true, runArtifactIntake},
		{"LegacyVerificationEvidence", true, runLegacyVerificationEvidence},
		{"WorkOrders", true, runWorkOrders},
		{"MergeTaskAtGate", true, runMergeTaskAtGate},
		{"CheckStatusDeadline", true, runCheckStatusDeadline},
		{"ClaimsProceedWithPendingProposals", true, runClaimsProceedWithPendingProposals},
		{"LargeArgumentWrites", true, runLargeArgumentWrites},
		{"AutomaticRecoveryAuthority", true, runAutomaticRecoveryAuthority},
		{"ContextFreshness", true, runContextFreshness},
		{"TaskContextReads", true, runTaskContextReads},
		{"TaskTitleRedaction", true, runTaskTitleRedaction},
		{"RedactionCounts", true, runRedactionCounts},
		{"WorkOrderClocks", true, runWorkOrderClocks},
		{"QueueClockRetired", true, runQueueClockRetired},
		{"QueueReentryPins", true, runQueueReentryPins},
		{"ReviewRounds", true, runReviewRounds},
		{"ReviewAcceptance", true, runReviewAcceptance},
		{"ReviewAccept", true, runReviewAccept},
		{"ReviewRoundSeats", true, RunReviewRoundSeatsConformance},
		{"ReviewSummary", true, runReviewSummary},
		{"ReviewFailFast", true, runReviewFailFast},
		{"ReviewSeatRetirement", true, runReviewSeatRetirement},
		{"ReviewDistinctModels", true, runReviewDistinctModels},
		{"LaterRoundReview", true, runLaterRoundReview},
		{"DeltaReviewRounds", true, runDeltaReviewRounds},
		{"RequirementInsight", true, RunRequirementInsightConformance},
		{"Decisions", true, runDecisions},
		{"ArchiveRestore", true, runArchiveRestore},
		{"Monitor", true, runMonitor},
		{"EmptyProjections", true, runEmptyProjections},
		{"ProjectionReads", true, runProjectionReads},
		{"MonitorPullRequestEvents", true, runMonitorPullRequestEvents},
		{"TaskEventOrdering", true, runTaskEventOrdering},
		{"TaskEventWindows", true, runTaskEventWindows},
		{"EventRecency", true, runEventRecency},
		{"PopulatedProjections", factory.Capabilities.Membership, runPopulatedProjections},
		{"WorkspaceControl", true, runWorkspaceControl},
		{"WorkspaceNameUniqueness", true, runWorkspaceNameUniqueness},
		{"RepositoryInstall", true, runRepositoryInstall},
		{"CommandRefusals", true, runCommandRefusals},
		{"ApprovalRefresh", true, runApprovalRefresh},
		{"PlanningReads", true, runPlanningReads},
		{"TaskFilter", factory.Capabilities.Membership, runTaskFilter},
		{"TaskAssigneeMembership", factory.Capabilities.Membership, runTaskAssigneeMembership},
		{"TaskListHydration", factory.Capabilities.Membership, runTaskListHydration},
		{"Identity", factory.Capabilities.Identity, runIdentity},
		{"Membership", factory.Capabilities.Membership, runMembership},
		{"InvitationSessions", factory.Capabilities.Identity && factory.Capabilities.Membership, runInvitationSessions},
		{"Tokens", factory.Capabilities.Tokens, runTokens},
		{"StagePreferences", factory.Capabilities.Identity && factory.Capabilities.Membership, runStagePreferences},
		{"GitHubApps", factory.Capabilities.Tokens, runGitHubApps},
	} {
		run(suite.name, func(t *testing.T) {
			if !suite.enabled {
				t.Skip("factory does not report this capability")
			}
			suite.run(t, factory.fresh(t, nil))
		})
	}
}

func (f Factory) validate() error {
	if f.New == nil {
		return fmt.Errorf("conformance factory is required")
	}
	if f.ProductionCapable && f.Capabilities != (Capabilities{Identity: true, Membership: true, Tokens: true}) {
		return fmt.Errorf("production backend must support identity, membership and tokens")
	}
	return nil
}
