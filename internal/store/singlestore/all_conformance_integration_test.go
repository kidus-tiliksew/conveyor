package singlestore

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// sharedDatabase is the one isolated SingleStore database a test binary
// creates and migrates. Creating a distributed database costs seconds on
// SingleStore, so fixtures share it and reset its tables instead of creating
// one database each; every table but the migration ledger is emptied before
// a fixture is handed out. Tests in this package run serially.
var sharedDatabase struct {
	once   sync.Once
	err    error
	admin  *sql.DB
	cfg    *mysql.Config
	name   string
	tables []string
	store  *Store
}

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedDatabase.admin != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, err := sharedDatabase.admin.ExecContext(ctx, "DROP DATABASE `"+sharedDatabase.name+"`"); err != nil {
			fmt.Fprintf(os.Stderr, "drop shared test database %s: %v\n", sharedDatabase.name, err)
			if code == 0 {
				code = 1
			}
		}
		cancel()
		if sharedDatabase.store != nil {
			sharedDatabase.store.Close()
		}
		sharedDatabase.admin.Close()
	}
	os.Exit(code)
}

func openSharedDatabase() error {
	raw := os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL")
	cfg, err := connectionConfig(raw)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(cfg.DBName, "_test") {
		return fmt.Errorf("SingleStore integration database must end in _test")
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return err
	}
	admin := sql.OpenDB(connector)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name := fmt.Sprintf("conveyor_%d_test", time.Now().UnixNano())
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` PARTITIONS 2"); err != nil {
		admin.Close()
		return err
	}
	sharedDatabase.admin = admin
	sharedDatabase.name = name
	cfg.DBName = name
	sharedDatabase.cfg = cfg
	// One Store for the whole binary: Open migrates once, and its pool stays
	// warm so compiled query plans are reused across fixtures.
	st, err := Open(ctx, cfg.FormatDSN())
	if err != nil {
		return err
	}
	sharedDatabase.store = st
	rows, err := admin.QueryContext(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema=? AND table_name<>'conveyor_singlestore_migrations' ORDER BY table_name", name)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			return err
		}
		sharedDatabase.tables = append(sharedDatabase.tables, table)
	}
	return rows.Err()
}

// integrationStore returns the shared Store with every table empty and no
// forge encryption key configured. Tests that need to close a pool call
// openOwnedStore instead.
func integrationStore(t *testing.T) *Store {
	t.Helper()
	resetSharedDatabase(t)
	st := sharedDatabase.store
	st.ConfigureGitHubAppKeyEncryptionKey(nil)
	return st
}

// openOwnedStore opens a private pool on the shared, emptied database.
func openOwnedStore(t *testing.T) *Store {
	t.Helper()
	resetSharedDatabase(t)
	st, err := Open(t.Context(), sharedDatabase.cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func resetSharedDatabase(t *testing.T) {
	t.Helper()
	if os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL") == "" {
		t.Skip("CONVEYOR_TEST_SINGLESTORE_URL is unset")
	}
	sharedDatabase.once.Do(func() { sharedDatabase.err = openSharedDatabase() })
	if sharedDatabase.err != nil {
		t.Fatal(sharedDatabase.err)
	}
	started := time.Now()
	// DELETE, not TRUNCATE: truncation is distributed DDL that costs hundreds
	// of milliseconds per table on SingleStore, while a compiled DELETE on an
	// empty rowstore table returns in a few milliseconds.
	for _, table := range sharedDatabase.tables {
		if _, err := sharedDatabase.admin.ExecContext(t.Context(), "DELETE FROM `"+sharedDatabase.name+"`.`"+table+"`"); err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}
	t.Logf("fixture reset %d tables in %s", len(sharedDatabase.tables), time.Since(started).Round(time.Millisecond))
}
func TestSingleStoreConformanceIntegration(t *testing.T) {
	storetest.RunAll(t, storetest.Factory{
		PlanRevisionGate: func(ctx context.Context, st store.Store, taskID string) (bool, error) {
			_, pending, err := dispatch.PendingPlanRevisionGate(ctx, st, taskID)
			return pending, err
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
		ProductionCapable: true,
		Capabilities:      storetest.Capabilities{Identity: true, Membership: true, Tokens: true},
		New: func(t *testing.T, repos []config.Repo) storetest.Fixture {
			st := integrationStore(t)
			ws := "conformance-" + core.NewTaskID()
			ctx := store.WithActor(store.WithWorkspace(t.Context(), ws), store.SystemActor())
			cfg := &config.Config{Workspace: ws, Repos: repos, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
			if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			return storetest.Fixture{VerificationClaimReads: verificationClaimReads(ws), DeactivateUser: func(t *testing.T, ctx context.Context, userID string) {
				t.Helper()
				if _, err := st.db.ExecContext(ctx, `UPDATE users SET status='deactivated' WHERE id=?`, userID); err != nil {
					t.Fatal(err)
				}
			}, ReopenVerification: func(t *testing.T) store.Backend {
				reopened, err := Open(t.Context(), sharedDatabase.cfg.FormatDSN())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(reopened.Close)
				return reopened
			}, Backend: st, Context: ctx, Workspace: ws, Config: cfg, ArtifactRepairEvents: func(ctx context.Context) ([]core.Event, error) {
				ws, _ := store.WorkspaceFromContext(ctx)
				rows, err := st.db.QueryContext(ctx, `SELECT kind,actor_id,payload_json FROM events WHERE workspace_id=? AND kind='artifact.metadata_repaired' ORDER BY id`, ws)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				var result []core.Event
				for rows.Next() {
					var e core.Event
					if err := rows.Scan(&e.Kind, &e.ActorID, &e.Payload); err != nil {
						return nil, err
					}
					result = append(result, e)
				}
				return result, rows.Err()
			}, WorkspaceEvents: func(ctx context.Context, kindPrefix string) ([]core.Event, error) {
				ws, _ := store.WorkspaceFromContext(ctx)
				rows, err := st.db.QueryContext(ctx, `SELECT id,kind,actor_id,actor_role,payload_json,at FROM events WHERE workspace_id=? AND task_id IS NULL AND LEFT(kind,CHAR_LENGTH(?))=? ORDER BY at,id`, ws, kindPrefix, kindPrefix)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				var result []core.Event
				for rows.Next() {
					var e core.Event
					var payload []byte
					var role string
					if err := rows.Scan(&e.ID, &e.Kind, &e.ActorID, &role, &payload, &e.At); err != nil {
						return nil, err
					}
					e.ActorRole = core.ActorRole(role)
					e.Payload = payload
					result = append(result, e)
				}
				return result, rows.Err()
			}, SeedHistoricalLink: func(t *testing.T, ctx context.Context, link core.LineageLink) {
				if _, err := st.db.ExecContext(ctx, `INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event,created_at) VALUES (?,?,?,?,?,?,?,?)`,
					link.Workspace, string(link.SrcType), link.SrcID, string(link.DstType), link.DstID, link.Kind, link.LegacyCreatedByEvent, link.CreatedAt); err != nil {
					t.Fatal(err)
				}
			}, SeedArtifact: func(t *testing.T, ctx context.Context, a core.Artifact, b []byte) {
				_, err := st.db.ExecContext(ctx, `UPDATE artifacts SET content_type=?,size_bytes=?,content=? WHERE workspace_id=? AND id=?`, a.ContentType, a.SizeBytes, b, a.Workspace, a.ID)
				if err != nil {
					t.Fatal(err)
				}
			}, SeedEvents: func(t *testing.T, ctx context.Context, base int64, events []core.Event) []core.Event {
				return singlestoreSeedEvents(t, st, ctx, base, events)
			}, SeedSetupContract: func(t *testing.T, ctx context.Context, taskID, raw string) {
				if _, err := st.db.ExecContext(ctx, `UPDATE tasks SET setup_contract=? WHERE workspace_id=? AND id=?`, raw, ws, taskID); err != nil {
					t.Fatal(err)
				}
			}, SeedStoredTaskHold: func(t *testing.T, ctx context.Context, taskID string) {
				// A row written before the hold retirement; no public writer
				// sets the column (DEC-74(3); component-persistence).
				result, err := st.db.ExecContext(ctx, `UPDATE tasks SET hold=true WHERE workspace_id=? AND id=?`, ws, taskID)
				if err != nil {
					t.Fatal(err)
				}
				if rows, err := result.RowsAffected(); err != nil || rows != 1 {
					t.Fatalf("seed stored hold rows=%d err=%v", rows, err)
				}
			}, SeedDashboardSessionTimes: func(t *testing.T, ctx context.Context, sessionID string, lastUsedAgo, expiresIn time.Duration) {
				// No public writer sets a session's stored times
				// (component-identity-membership).
				result, err := st.db.ExecContext(ctx, `UPDATE dashboard_sessions SET last_used_at=DATE_SUB(CURRENT_TIMESTAMP(6),INTERVAL ? MICROSECOND),expires_at=DATE_ADD(CURRENT_TIMESTAMP(6),INTERVAL ? MICROSECOND) WHERE id=?`, lastUsedAgo.Microseconds(), expiresIn.Microseconds(), sessionID)
				if err != nil {
					t.Fatal(err)
				}
				if rows, err := result.RowsAffected(); err != nil || rows != 1 {
					t.Fatalf("seed dashboard session times rows=%d err=%v", rows, err)
				}
			}, DashboardSessionTimes: func(t *testing.T, ctx context.Context, sessionID string) (time.Time, time.Time) {
				var lastUsed sql.NullTime
				var expires time.Time
				if err := st.db.QueryRowContext(ctx, `SELECT last_used_at,expires_at FROM dashboard_sessions WHERE id=?`, sessionID).Scan(&lastUsed, &expires); err != nil {
					t.Fatal(err)
				}
				return lastUsed.Time.UTC(), expires.UTC()
			}, SeedCredentialLastUsed: func(t *testing.T, ctx context.Context, credentialID string, lastUsedAgo time.Duration) {
				// No public writer sets a credential's last use
				// (component-identity-membership).
				result, err := st.db.ExecContext(ctx, `UPDATE user_tokens SET last_used_at=DATE_SUB(CURRENT_TIMESTAMP(6),INTERVAL ? MICROSECOND) WHERE id=?`, lastUsedAgo.Microseconds(), credentialID)
				if err != nil {
					t.Fatal(err)
				}
				if rows, err := result.RowsAffected(); err != nil || rows != 1 {
					t.Fatalf("seed credential last use rows=%d err=%v", rows, err)
				}
			}, CredentialLastUsed: func(t *testing.T, ctx context.Context, credentialID string) time.Time {
				var lastUsed sql.NullTime
				if err := st.db.QueryRowContext(ctx, `SELECT last_used_at FROM user_tokens WHERE id=?`, credentialID).Scan(&lastUsed); err != nil {
					t.Fatal(err)
				}
				return lastUsed.Time.UTC()
			}, WorkerService: func(*testing.T) storetest.WorkerExercise {
				return conformanceWorkerExercise(st, cfg)
			}, LineageHTTP: func(*testing.T) http.Handler {
				// The shared-token operator fixture (component-lineage).
				server := httpapi.NewServer(st)
				server.Credentials = nil
				server.Workspace, server.BearerToken = ws, storetest.LineageHTTPToken
				return server.Handler()
			}}
		}})
}

// conformanceWorkerExercise builds the real worker service over one
// conformance fixture; storetest cannot import internal/worker
// (component-verification-strategy).
func conformanceWorkerExercise(st store.Store, cfg *config.Config) storetest.WorkerExercise {
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	service := &worker.Service{Store: st, WorkOrders: &workorder.Service{Store: st, ConfigProvider: provider}, ConfigProvider: provider}
	return storetest.WorkerExercise{
		ListClaimable: func(ctx context.Context, enrolled core.Worker) ([]core.WorkOrder, error) {
			items, err := service.ListClaimable(ctx, enrolled)
			orders := make([]core.WorkOrder, 0, len(items))
			for _, item := range items {
				orders = append(orders, item.Order)
			}
			return orders, err
		},
		ListVisibleOrders: service.ListVisibleOrders,
		ClaimForWorker:    service.ClaimForWorker,
	}
}

// verificationClaimReadWorkspaces routes the backend's verification claim
// read observer to the conformance fixture of each workspace, so parallel
// fixtures never see each other's reads.
var (
	verificationClaimReadWorkspaces sync.Map
	verificationClaimReadInstall    sync.Once
)

// verificationClaimReads is the fixture's VerificationClaimReads: it records
// the tasks whose verification rows ReconcileVerificationClaims reads in the
// workspace, through the backend's internal read boundary.
func verificationClaimReads(workspaceID string) func(*testing.T) func() []string {
	return func(t *testing.T) func() []string {
		verificationClaimReadInstall.Do(func() {
			observe := func(workspace, taskID string) {
				if recorder, ok := verificationClaimReadWorkspaces.Load(workspace); ok {
					recorder.(*verificationClaimReadRecorder).add(taskID)
				}
			}
			verificationClaimReadObserver.Store(&observe)
		})
		recorder := &verificationClaimReadRecorder{}
		verificationClaimReadWorkspaces.Store(workspaceID, recorder)
		t.Cleanup(func() { verificationClaimReadWorkspaces.Delete(workspaceID) })
		return recorder.ids
	}
}

type verificationClaimReadRecorder struct {
	mu   sync.Mutex
	read []string
}

func (r *verificationClaimReadRecorder) add(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.read = append(r.read, taskID)
}

func (r *verificationClaimReadRecorder) ids() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.read...)
}
