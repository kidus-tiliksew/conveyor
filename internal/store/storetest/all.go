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
	// dependency-removal case requires it and never skips.
	LineageHTTP func(t *testing.T) http.Handler
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
	// OAuthDeploymentEvents returns the deployment audit events whose kind
	// starts with identity.oauth_, ascending by (at, id). AgeOAuthRecord moves
	// every stored time of one OAuth row back by the given duration.
	// OAuthStoredRows returns every row of the four OAuth tables. No public
	// read exposes the deployment ledger, a stored digest, or a stored time,
	// so each test binary supplies all three. The OAuth cases require them
	// and never skip (req-mcp-oauth; component-identity-membership
	// invariants 36 and 51 to 54).
	OAuthDeploymentEvents func(t *testing.T, ctx context.Context) []core.Event
	AgeOAuthRecord        func(t *testing.T, ctx context.Context, table OAuthTable, id string, by time.Duration)
	OAuthStoredRows       func(t *testing.T, ctx context.Context) []OAuthStoredRow
	// AgeEnvironmentKey moves one environment key's stored last use back by
	// the given duration, and StoredEnvironmentKey returns the key's stored
	// row. No public writer or read exposes either, so each test binary
	// supplies both. The Environments cases require them and never skip
	// (component-environments invariants 11 and 14).
	AgeEnvironmentKey    func(t *testing.T, ctx context.Context, keyID string, by time.Duration)
	StoredEnvironmentKey func(t *testing.T, ctx context.Context, keyID string) EnvironmentStoredKey
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
	// ReviewSweep is workorder.Service.SweepAutomaticReviewRecovery. storetest
	// cannot import workorder: workorder tests import storetest.
	ReviewSweep ReviewSweepFunc
	// ClaimService is workorder.Service.Claim. storetest cannot import
	// workorder: workorder tests import storetest. The claim-failure stall
	// case requires it and never skips.
	ClaimService ClaimServiceFunc
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
	run("Crates", func(t *testing.T) { runCrates(t, factory) })
	run("Environments", func(t *testing.T) { runEnvironments(t, factory) })
	run("OAuth", func(t *testing.T) { runOAuth(t, factory) })
	run("ClaimFailureStall", func(t *testing.T) { runClaimFailureStall(t, factory.fresh(t, nil), factory.ClaimService) })
	run("TaskStartOver", func(t *testing.T) { runTaskStartOver(t, factory) })
	run("TaskMap", func(t *testing.T) { RunTaskMapConformance(t, factory.fresh(t, nil)) })
	run("ActivityValidation", func(t *testing.T) { RunActivityValidationConformance(t, factory.fresh(t, nil)) })
	run("DocumentationGate", func(t *testing.T) { runDocumentationGate(t, factory.fresh(t, nil)) })
	run("FeatureRetirement", func(t *testing.T) { RunFeatureRetirementConformance(t, factory.fresh(t, nil)) })
	run("Requirements", func(t *testing.T) { RunRequirementConformance(t, requirements) })
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
		{"PlanRevision", RunPlanRevisionConformance},
		{"ReferenceDocuments", RunReferenceDocumentConformance},
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
	run("AutomaticPublicationRecovery", func(t *testing.T) {
		x := factory.fresh(t, nil)
		RunAutomaticPublicationRecoveryConformance(t, x.Backend, x.Context, x.Workspace, factory.ReviewSweep)
	})
	run("RiskRatings", func(t *testing.T) {
		RunRiskRatingConformance(t, factory.fresh(t, nil))
	})
	run("HeadRiskOrders", func(t *testing.T) {
		RunHeadRiskOrderConformance(t, factory.fresh(t, nil))
	})
	run("PlanRiskOrders", func(t *testing.T) {
		RunPlanRiskOrderConformance(t, factory.fresh(t, nil))
	})
	run("TaskApprovals", func(t *testing.T) {
		RunTaskApprovalConformance(t, factory.fresh(t, nil))
	})
	run("ReviewPolicyStatuses", func(t *testing.T) {
		RunReviewPolicyStatusConformance(t, factory.fresh(t, nil))
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
		{"ActivityMarkerReviewRounds", true, RunActivityMarkerReviewRoundsConformance},
		{"RequirementDetailReads", true, runRequirementDetailReads},
		{"ArtifactRepair", true, runArtifactRepair},
		{"Verification", true, runVerification},
		{"WorkOrders", true, runWorkOrders},
		{"CheckStatusDeadline", true, runCheckStatusDeadline},
		{"ContextFreshness", true, runContextFreshness},
		{"ReviewSummary", true, runReviewSummary},
		{"ReviewFailFast", true, runReviewFailFast},
		{"RequirementInsight", true, RunRequirementInsightConformance},
		{"Decisions", true, runDecisions},
		{"Monitor", true, runMonitor},
		{"EmptyProjections", true, runEmptyProjections},
		{"ProjectionReads", true, runProjectionReads},
		{"MonitorPullRequestEvents", true, runMonitorPullRequestEvents},
		{"EventRecency", true, runEventRecency},
		{"WorkspaceControl", true, runWorkspaceControl},
		{"RepositoryInstall", true, runRepositoryInstall},
		{"CommandRefusals", true, runCommandRefusals},
		{"ApprovalRefresh", true, runApprovalRefresh},
		{"PlanningReads", true, runPlanningReads},
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
