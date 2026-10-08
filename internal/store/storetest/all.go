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
	ReopenVerification   func(*testing.T) store.Backend
	Backend              store.Backend
	Context              context.Context
	Workspace            string
	Config               *config.Config
	SeedLegacy           func(*testing.T, string) (int, func(*testing.T))
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
}

// Factory creates an isolated backend and workspace on every call. Cleanup
// belongs to the supplied testing.T. Repository names are fixture inputs.
type Factory struct {
	New               func(*testing.T, []config.Repo) Fixture
	Capabilities      Capabilities
	ProductionCapable bool
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

// RunAll is the common memory and PostgreSQL conformance entry point.
// component-verification-strategy owns these cases; logtest stays separate.
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
	run("Blueprint", func(t *testing.T) {
		RunBlueprintConformance(t, func(t *testing.T, repos []config.Repo) BlueprintFixture {
			x := factory.fresh(t, repos)
			return BlueprintFixture{x.Backend, x.Context, x.Workspace}
		})
	})
	run("TaskStartOver", func(t *testing.T) { runTaskStartOver(t, factory) })
	run("Requirements", func(t *testing.T) { RunRequirementConformance(t, requirements) })
	run("VersionDismissal", func(t *testing.T) { RunVersionDismissalConformance(t, requirements) })
	run("Lineage", func(t *testing.T) {
		RunLineageConformance(t, func(t *testing.T, repos []config.Repo) LineageFixture {
			x := factory.fresh(t, repos)
			foreign := x.Workspace + "-other"
			ctx := store.WithWorkspace(t.Context(), foreign)
			if _, err := x.Backend.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: foreign, Repos: repos}); err != nil {
				t.Fatal(err)
			}
			return LineageFixture{Store: x.Backend, Context: x.Context, ForeignContext: ctx, Workspace: x.Workspace, SeedLegacy: x.SeedLegacy}
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
	for _, suite := range []struct {
		name    string
		enabled bool
		run     func(*testing.T, Fixture)
	}{
		{"TaskOperationsPagination", true, runTaskPagination},
		{"TaskLifecycle", true, runTaskLifecycle},
		{"TaskBranchUniqueness", true, runTaskBranchUniqueness},
		{"TaskBranchAttach", true, runTaskBranchAttach},
		{"TaskEventAtomicity", true, runTaskEventAtomicity},
		{"ArtifactRepair", true, runArtifactRepair},
		{"Verification", true, runVerification},
		{"ArtifactIntake", true, runArtifactIntake},
		{"LegacyVerificationEvidence", true, runLegacyVerificationEvidence},
		{"Workers", true, runWorkers},
		{"WorkOrders", true, runWorkOrders},
		{"ClaimBlockingProposals", true, runClaimBlockingProposals},
		{"WorktreeHandoff", true, runWorktreeHandoff},
		{"ContextFreshness", true, runContextFreshness},
		{"WorkOrderClocks", true, runWorkOrderClocks},
		{"QueueReentryPins", true, runQueueReentryPins},
		{"ReviewRounds", true, runReviewRounds},
		{"ReviewAcceptance", true, runReviewAcceptance},
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
