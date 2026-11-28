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
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestMemoryConformance(t *testing.T) {
	storetest.RunAll(t, storetest.Factory{
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
			}, SeedTaskIntakeKey: func(t *testing.T, _ context.Context, taskID, key string) bool {
				return store.SeedTaskIntakeKeyForTest(t, st, taskID, key)
			}, RestoreSupersededDeliveries: func(t *testing.T, _ context.Context, taskID string) {
				store.RestoreSupersededDeliveriesForTest(t, st, taskID)
			}, RestoreHistoricalDelivery: func(t *testing.T, _ context.Context, taskID string, generation int, state, errorClass string, next *time.Time) {
				store.RestoreHistoricalDeliveryForTest(t, st, taskID, generation, state, errorClass, next)
			}, SeedForeignVerificationOperation: func(t *testing.T, _ context.Context, taskID string) {
				store.SeedForeignVerificationOperationForTest(t, st, taskID)
			}, SeedStoredTaskHold: func(t *testing.T, _ context.Context, taskID string) {
				store.SeedStoredTaskHoldForTest(t, st, taskID)
			}, SeedStoredStaleWorkOrder: func(t *testing.T, _ context.Context, orderID string, exhausted bool, scheduledAt time.Time) {
				store.SeedStoredStaleWorkOrderForTest(t, st, orderID, exhausted, scheduledAt)
			}, SeedRiskRating: func(t *testing.T, ctx context.Context, rating core.RiskRating) {
				store.SeedRiskRatingForTest(t, st, ctx, rating)
			}, SeedVerificationAttemptAuthorization: func(t *testing.T, ctx context.Context, attemptID, authorizationID string) {
				store.SeedVerificationAttemptAuthorizationForTest(t, st, ctx, attemptID, authorizationID)
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
			}, OAuthDeploymentEvents: func(t *testing.T, _ context.Context) []core.Event {
				return store.OAuthDeploymentEventsForTest(t, st)
			}, AgeOAuthRecord: func(t *testing.T, _ context.Context, table storetest.OAuthTable, id string, by time.Duration) {
				store.AgeOAuthRecordForTest(t, st, string(table), id, by)
			}, OAuthStoredRows: func(t *testing.T, ctx context.Context) []storetest.OAuthStoredRow {
				var rows []storetest.OAuthStoredRow
				for _, row := range store.OAuthStoredRowsForTest(ctx, t, st) {
					rows = append(rows, storetest.OAuthStoredRow{Table: storetest.OAuthTable(row.Table), ID: row.ID, Digest: row.Digest, Text: row.Text})
				}
				return rows
			}, AgeEnvironmentKey: func(t *testing.T, ctx context.Context, keyID string, by time.Duration) {
				store.AgeEnvironmentKeyForTest(t, st, ctx, keyID, by)
			}, StoredEnvironmentKey: func(t *testing.T, ctx context.Context, keyID string) storetest.EnvironmentStoredKey {
				digest, text := store.StoredEnvironmentKeyForTest(t, st, ctx, keyID)
				return storetest.EnvironmentStoredKey{Digest: digest, Text: text}
			}, CloseFeedbackReport: func(t *testing.T, ctx context.Context, reportID string, at time.Time) {
				store.CloseFeedbackReportForTest(t, st, ctx, reportID, at)
			}, SeedFeedbackReportLink: func(t *testing.T, ctx context.Context, link store.FeedbackReportLink) {
				store.SeedFeedbackReportLinkForTest(t, st, ctx, link)
			}, LineageHTTP: func(*testing.T) http.Handler {
				// The shared-token operator fixture (component-lineage).
				server := httpapi.NewServer(st)
				server.Credentials = nil
				server.Workspace, server.BearerToken = workspace, storetest.LineageHTTPToken
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
