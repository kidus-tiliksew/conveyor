package storetest

import (
	"context"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Capabilities describe behavior, not merely satisfaction of Go interfaces.
// A production backend must exercise every capability (DEC-38).
type Capabilities struct {
	Identity, Membership, Tokens bool
}

type Fixture struct {
	ReopenVerification func(*testing.T) store.Backend
	Backend            store.Backend
	Context            context.Context
	Workspace          string
	Config             *config.Config
	SeedLegacy         func(*testing.T, string) (int, func(*testing.T))
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
	// DeactivateIdentityUser method (component-work-orders, Workers).
	DeactivateUser func(t *testing.T, ctx context.Context, userID string)
	// SeedSetupContract replaces one task's stored setup contract with raw
	// JSON, modelling rows an earlier encoding wrote. No public writer accepts
	// a raw contract; the volatile backend decodes it into the task.
	SeedSetupContract func(t *testing.T, ctx context.Context, taskID, raw string)
	// SeedStoredTaskHold sets one task's stored historical hold column to
	// true, modelling a row written before the hold retirement (DEC-74(3)).
	// No public writer can; each backend seeds it directly in its test
	// binary. The hold-retirement cases require it and never skip.
	SeedStoredTaskHold func(t *testing.T, ctx context.Context, taskID string)
	// WorkerService exercises the real worker.Service over this fixture's
	// backend and configuration. storetest cannot import internal/worker,
	// because worker tests import storetest, so each conformance entry point
	// constructs it (component-verification-strategy). The hold-retirement
	// cases require it and never skip.
	WorkerService func(t *testing.T) WorkerExercise
}

// WorkerExercise is the worker-service surface the Workers hold-retirement
// cases call: listing, visible orders, and claim, each through the real
// service rather than the store (component-work-orders, Workers).
type WorkerExercise struct {
	ListClaimable     func(ctx context.Context, worker core.Worker) ([]core.WorkOrder, error)
	ListVisibleOrders func(ctx context.Context, worker core.Worker) ([]core.WorkOrder, error)
	ClaimForWorker    func(ctx context.Context, worker core.Worker, id string, claim core.WorkOrderClaim) (core.WorkOrder, error)
}

// requireStoredTaskHoldSeed returns the fixture's historical hold seed or
// fails the case; a fixture without it cannot prove the retirement.
func requireStoredTaskHoldSeed(t *testing.T, seed func(*testing.T, context.Context, string)) func(*testing.T, context.Context, string) {
	t.Helper()
	if seed == nil {
		t.Fatal("conformance fixture must provide SeedStoredTaskHold for the hold-retirement cases (DEC-74(3))")
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
	run("TaskStartOver", func(t *testing.T) { runTaskStartOver(t, factory) })
	run("TaskMap", func(t *testing.T) { RunTaskMapConformance(t, factory.fresh(t, nil)) })
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
			return LineageFixture{Store: x.Backend, Context: x.Context, ForeignContext: ctx, Workspace: x.Workspace, SeedLegacy: x.SeedLegacy, SeedHistoricalLink: x.SeedHistoricalLink}
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
	run("ZeroSetupReviewSeats", func(t *testing.T) {
		RunZeroSetupReviewSeatsConformance(t, factory.fresh(t, nil), factory.ReviewSweep)
	})
	for _, suite := range []struct {
		name    string
		enabled bool
		run     func(*testing.T, Fixture)
	}{
		{"TaskOperationsPagination", true, runTaskPagination},
		{"TaskLifecycle", true, runTaskLifecycle},
		{"TaskBranchUniqueness", true, runTaskBranchUniqueness},
		{"TaskBranchConcurrentClaims", true, runTaskBranchConcurrentClaims},
		{"TaskBranchAttach", true, runTaskBranchAttach},
		{"TaskEventAtomicity", true, runTaskEventAtomicity},
		{"TaskCreationProvenance", true, runTaskCreationProvenance},
		{"ArtifactRepair", true, runArtifactRepair},
		{"Verification", true, runVerification},
		{"ArtifactIntake", true, runArtifactIntake},
		{"LegacyVerificationEvidence", true, runLegacyVerificationEvidence},
		{"Workers", true, runWorkers},
		{"WorkOrders", true, runWorkOrders},
		{"AttemptObservability", true, runAttemptObservability},
		{"MergeTaskAtGate", true, runMergeTaskAtGate},
		{"ClaimBlockingProposals", true, runClaimBlockingProposals},
		{"AutomaticRecoveryAuthority", true, runAutomaticRecoveryAuthority},
		{"WorktreeHandoff", true, runWorktreeHandoff},
		{"ContextFreshness", true, runContextFreshness},
		{"WorkOrderClocks", true, runWorkOrderClocks},
		{"QueueReentryPins", true, runQueueReentryPins},
		{"ReviewRounds", true, runReviewRounds},
		{"ReviewAcceptance", true, runReviewAcceptance},
		{"ReviewAccept", true, runReviewAccept},
		{"ReviewRoundSeats", true, RunReviewRoundSeatsConformance},
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
		{"Identity", factory.Capabilities.Identity, runIdentity},
		{"Membership", factory.Capabilities.Membership, runMembership},
		{"InvitationSessions", factory.Capabilities.Identity && factory.Capabilities.Membership, runInvitationSessions},
		{"Tokens", factory.Capabilities.Tokens, runTokens},
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
