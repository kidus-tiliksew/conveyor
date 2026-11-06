package store_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestMemoryConformance(t *testing.T) {
	storetest.RunAll(t, storetest.Factory{
		PlanRevisionGate: func(ctx context.Context, st store.Store, taskID string) (bool, error) {
			_, pending, err := dispatch.PendingPlanRevisionGate(ctx, st, taskID)
			return pending, err
		},
		ClaimService: func(st store.Store, cfg *config.Config) func(context.Context, string, core.WorkOrderClaim) (core.WorkOrder, error) {
			service := &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
			return service.Claim
		},
		ReviewSweep: func(ctx context.Context, st store.Store, cfg *config.Config, head string, now time.Time) (int, error) {
			service := &workorder.Service{
				Store:          st,
				ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
				ReviewTarget: func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
					return githubtrigger.ReviewTarget{Number: 7, HeadSHA: head}, nil
				},
			}
			return service.SweepAutomaticReviewRecovery(ctx, now)
		},
		Capabilities: storetest.Capabilities{Identity: true, Membership: true, Tokens: true},
		New: func(t *testing.T, repos []config.Repo) storetest.Fixture {
			t.Helper()
			st := store.NewVolatileBackend()
			t.Cleanup(st.Close)
			workspace := "conformance-" + core.NewTaskID()
			ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.SystemActor())
			cfg := &config.Config{Workspace: workspace, Repos: repos, Routing: config.Routing{Stages: map[string]config.StageRoute{
				"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour},
			}}}
			if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			return storetest.Fixture{Backend: st, Context: ctx, Workspace: workspace, Config: cfg, ArtifactRepairEvents: func(ctx context.Context) ([]core.Event, error) { return st.ListEvents(ctx, "") }, WorkspaceEvents: func(ctx context.Context, kindPrefix string) ([]core.Event, error) {
				// The volatile ledger keeps taskless events of every workspace
				// in one list. An event whose payload names another
				// workspace_id belongs to that workspace's ledger.
				ws, _ := store.WorkspaceFromContext(ctx)
				events, err := st.ListEvents(ctx, "")
				var out []core.Event
				for _, event := range events {
					var scope struct {
						WorkspaceID string `json:"workspace_id"`
					}
					if json.Unmarshal(event.Payload, &scope) == nil && scope.WorkspaceID != "" && scope.WorkspaceID != ws {
						continue
					}
					if strings.HasPrefix(event.Kind, kindPrefix) {
						out = append(out, event)
					}
				}
				return out, err
			}, SeedHistoricalLink: func(t *testing.T, _ context.Context, link core.LineageLink) {
				store.SeedLineageLinkForTest(t, st, link)
			}, SeedArtifact: func(t *testing.T, ctx context.Context, a core.Artifact, b []byte) {
				store.SeedArtifactMetadataForTest(t, st, ctx, a, b)
			}, SeedEvents: func(t *testing.T, ctx context.Context, base int64, events []core.Event) []core.Event {
				return store.SeedEventsForTest(t, st, ctx, base, events)
			}, SeedWorkspaceConfigYAML: func(t *testing.T, _ context.Context, raw string) {
				store.SeedWorkspaceConfigYAMLForTest(t, st, workspace, raw)
			}, WorkspaceConfigYAML: func(t *testing.T, _ context.Context) string {
				return store.WorkspaceConfigYAMLForTest(t, st, workspace)
			}, SeedSetupContract: func(t *testing.T, _ context.Context, taskID, raw string) {
				store.SeedSetupContractForTest(t, st, taskID, raw)
			}, SeedStoredTaskHold: func(t *testing.T, _ context.Context, taskID string) {
				store.SeedStoredTaskHoldForTest(t, st, taskID)
			}, SeedDashboardSessionTimes: func(t *testing.T, _ context.Context, sessionID string, lastUsedAgo, expiresIn time.Duration) {
				store.SeedDashboardSessionTimesForTest(t, st, sessionID, lastUsedAgo, expiresIn)
			}, DashboardSessionTimes: func(t *testing.T, _ context.Context, sessionID string) (time.Time, time.Time) {
				return store.DashboardSessionTimesForTest(t, st, sessionID)
			}, SeedCredentialLastUsed: func(t *testing.T, _ context.Context, credentialID string, lastUsedAgo time.Duration) {
				store.SeedCredentialLastUsedForTest(t, st, credentialID, lastUsedAgo)
			}, SeedStoredAgentCredential: func(t *testing.T, _ context.Context, userID, label string, revoked bool) (string, string) {
				return store.SeedStoredAgentCredentialForTest(t, st, userID, label, revoked)
			}, CredentialLastUsed: func(t *testing.T, _ context.Context, credentialID string) time.Time {
				return store.CredentialLastUsedForTest(t, st, credentialID)
			}, LineageHTTP: func(*testing.T) http.Handler {
				// The shared-token operator fixture (component-lineage).
				server := httpapi.NewServer(st)
				server.Credentials = nil
				server.Workspace, server.BearerToken = workspace, storetest.LineageHTTPToken
				return server.Handler()
			}, RedispatchHTTP: func(_ *testing.T, onDispatch func(context.Context, string)) http.Handler {
				server := httpapi.NewServer(st)
				server.Credentials = nil
				server.Workspace, server.BearerToken = workspace, storetest.LineageHTTPToken
				server.OnCreate = onDispatch
				dispatcher := dispatch.New(st, cfg, nil)
				dispatcher.DisableMemoryQueueForTest()
				server.OnIntervention = dispatcher.HandleIntervention
				return server.Handler()
			}}
		},
	})
}

// TestMemoryFreshDeploymentBootstrap runs the empty-deployment DEC-75 cases
// against a new volatile backend per case (component-identity-membership).
func TestMemoryFreshDeploymentBootstrap(t *testing.T) {
	storetest.RunFreshDeploymentBootstrap(t, storetest.FreshDeployment{
		Open: func(t *testing.T) store.Backend {
			st := store.NewVolatileBackend()
			t.Cleanup(st.Close)
			return st
		},
		Deactivate: func(t *testing.T, st store.Backend, userID string) {
			deactivator, ok := st.(interface {
				DeactivateIdentityUser(context.Context, string) (core.IdentityUser, error)
			})
			if !ok {
				t.Fatal("volatile backend lacks account deactivation")
			}
			ctx := store.WithActor(t.Context(), store.Actor{ID: "system", Role: core.ActorSystem})
			if _, err := deactivator.DeactivateIdentityUser(ctx, userID); err != nil {
				t.Fatal(err)
			}
		},
	})
}
