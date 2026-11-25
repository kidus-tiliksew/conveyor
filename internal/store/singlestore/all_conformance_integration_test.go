package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
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

// resetWorkers bounds the tables one fixture reset empties at the same time.
const resetWorkers = 8

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
	// Every reset worker keeps its connection between fixtures.
	admin.SetMaxOpenConns(resetWorkers)
	admin.SetMaxIdleConns(resetWorkers)
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
	// Fresh installs create the operator-owned event table before normal startup.
	preparationConnector, err := mysql.NewConnector(cfg)
	if err != nil {
		return err
	}
	preparation := sql.OpenDB(preparationConnector)
	err = execEventsOperatorScript(ctx, preparation, "01-create.sql")
	preparation.Close()
	if err != nil {
		return err
	}
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
func openOwnedStore(t *testing.T, compress ...bool) *Store {
	t.Helper()
	resetSharedDatabase(t)
	st, err := Open(t.Context(), sharedDatabase.cfg.FormatDSN(), compress...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// resetSharedDatabase empties every table but the migration ledger. SingleStore
// enforces no foreign keys, so tables empty in any order.
//
// DELETE, not TRUNCATE: truncation is distributed DDL that costs hundreds of
// milliseconds per table on SingleStore, while a compiled DELETE on an empty
// rowstore table returns in a few milliseconds.
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
	err := resetTables(t.Context(), sharedDatabase.tables, resetWorkers, func(ctx context.Context, table string) error {
		_, err := sharedDatabase.admin.ExecContext(ctx, "DELETE FROM `"+sharedDatabase.name+"`.`"+table+"`")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture reset %d tables in %s", len(sharedDatabase.tables), time.Since(started).Round(time.Millisecond))
}

// resetTables calls reset for every table on at most workers goroutines. The
// first failure cancels every table not yet started, and resetTables returns
// that failure, naming its table. A reset already running gets the caller's
// context and finishes first, because a DELETE the client abandons can still
// run on the server and empty a table under the next fixture.
func resetTables(ctx context.Context, tables []string, workers int, reset func(context.Context, string) error) error {
	dispatch, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	queue := make(chan string)
	var running sync.WaitGroup
	for range min(workers, len(tables)) {
		running.Add(1)
		go func() {
			defer running.Done()
			for table := range queue {
				if dispatch.Err() != nil {
					return
				}
				if err := reset(ctx, table); err != nil {
					// The context keeps only its first cause.
					cancel(fmt.Errorf("reset %s: %w", table, err))
					return
				}
			}
		}()
	}
feed:
	for _, table := range tables {
		select {
		case queue <- table:
		case <-dispatch.Done():
			break feed
		}
	}
	close(queue)
	running.Wait()
	return context.Cause(dispatch)
}

func TestResetTablesEmptiesEveryTableWithBoundedWorkers(t *testing.T) {
	tables := make([]string, 50)
	for i := range tables {
		tables[i] = fmt.Sprintf("table_%d", i)
	}
	var mu sync.Mutex
	seen := map[string]int{}
	active, peak := 0, 0
	release := make(chan struct{})
	err := resetTables(t.Context(), tables, 4, func(_ context.Context, table string) error {
		mu.Lock()
		seen[table]++
		active++
		peak = max(peak, active)
		if active == 4 {
			select {
			case <-release:
			default:
				close(release)
			}
		}
		mu.Unlock()
		<-release // holds the first workers until all four run at once
		mu.Lock()
		active--
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if peak != 4 {
		t.Errorf("peak concurrent resets = %d, want 4", peak)
	}
	for _, table := range tables {
		if seen[table] != 1 {
			t.Errorf("table %s reset %d times, want 1", table, seen[table])
		}
	}
}

func TestResetTablesStopsAtFirstFailure(t *testing.T) {
	failure := errors.New("delete refused")
	var started []string
	err := resetTables(t.Context(), []string{"emptied", "broken", "later_1", "later_2"}, 1, func(_ context.Context, table string) error {
		started = append(started, table)
		if table == "broken" {
			return failure
		}
		return nil
	})
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "reset broken") {
		t.Errorf("error = %v, want the broken table's failure", err)
	}
	if got := strings.Join(started, " "); got != "emptied broken" {
		t.Errorf("started %q, want no table after the failure", got)
	}
}

func TestResetTablesFailureWaitsForRunningResets(t *testing.T) {
	failure := errors.New("delete refused")
	running, failed := make(chan struct{}), make(chan struct{})
	var finished atomic.Bool
	err := resetTables(t.Context(), []string{"slow", "broken"}, 2, func(ctx context.Context, table string) error {
		if table == "broken" {
			<-running
			defer close(failed)
			return failure
		}
		close(running)
		<-failed
		time.Sleep(20 * time.Millisecond) // resetTables must outwait this
		finished.Store(true)
		return ctx.Err() // nil: the failure leaves a running reset its context
	})
	if !errors.Is(err, failure) {
		t.Errorf("error = %v, want %v", err, failure)
	}
	if !finished.Load() {
		t.Error("resetTables returned before a running reset finished")
	}
}

func TestResetTablesReportsCancelledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := resetTables(ctx, []string{"a", "b"}, 2, func(ctx context.Context, _ string) error { return ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestSingleStoreConformanceIntegration(t *testing.T) {
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
		ProductionCapable: true,
		Capabilities:      storetest.Capabilities{Identity: true, Membership: true, Tokens: true},
		New: func(t *testing.T, repos []config.Repo) storetest.Fixture {
			st := integrationStore(t)
			// These controls compare socket bytes against decoded body sizes.
			// Keep compression off so their payload-elision bounds remain exact.
			if strings.HasSuffix(t.Name(), "/InboxAttentionCountBodyFree") || strings.HasSuffix(t.Name(), "/ActivityMarkerCompactReads") {
				st = openOwnedStore(t, false)
			}
			ws := "conformance-" + core.NewTaskID()
			ctx := store.WithActor(store.WithWorkspace(t.Context(), ws), store.SystemActor())
			cfg := &config.Config{Workspace: ws, Repos: repos, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
			if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			reads := newConformanceReadLedger(st)
			return storetest.Fixture{ArtifactListStatements: storetest.ArtifactListStatementsExactlyTwo, DrainReadBytes: reads.drainBytes, DrainReadQueries: reads.drainQueries, DrainReadRows: reads.drainRows, VerificationClaimReads: verificationClaimReads(ws), DeactivateUser: func(t *testing.T, ctx context.Context, userID string) {
				t.Helper()
				if _, err := st.db.ExecContext(ctx, `UPDATE users SET status='deactivated' WHERE id=?`, userID); err != nil {
					t.Fatal(err)
				}
			}, DeliveryPassSkipsHeldKeys: true, RestoreHistoricalDelivery: func(t *testing.T, ctx context.Context, taskID string, generation int, state, errorClass string, next *time.Time) {
				t.Helper()
				var raw []byte
				if err := st.db.QueryRowContext(ctx, `SELECT body FROM verification_publication_deliveries WHERE workspace_id=? AND task_id=? AND generation=? AND state='superseded'`, ws, taskID, generation).Scan(&raw); err != nil {
					t.Fatalf("historical delivery generation %d: %v", generation, err)
				}
				var d core.VerificationDelivery
				if err := json.Unmarshal(raw, &d); err != nil {
					t.Fatal(err)
				}
				d.State, d.ErrorClass, d.NextAttemptAt = state, errorClass, next
				body, err := json.Marshal(d)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.db.ExecContext(ctx, `UPDATE verification_publication_deliveries SET state=?, body=?, next_attempt_at=? WHERE workspace_id=? AND task_id=? AND generation=?`, state, body, next, ws, taskID, generation); err != nil {
					t.Fatal(err)
				}
			}, SeedForeignVerificationOperation: func(t *testing.T, ctx context.Context, taskID string) {
				t.Helper()
				result, err := st.db.ExecContext(ctx, `INSERT INTO verification_operations(workspace_id,id,task_id,context_id,run_id,logical_key,key_hash,state,body) SELECT workspace_id,CONCAT(id,'-malformed'),task_id,context_id,run_id,CONCAT(logical_key,'-malformed'),CONCAT(MD5(id),MD5(id)),'completed','{"ID":7,"StepID":7}' FROM verification_operations WHERE workspace_id=? AND task_id=? LIMIT 1`, ws, taskID)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := result.RowsAffected(); err != nil || n != 1 {
					t.Fatalf("seed malformed operation: %d rows, %v", n, err)
				}
			}, RestoreSupersededDeliveries: func(t *testing.T, ctx context.Context, taskID string) {
				t.Helper()
				result, err := st.db.ExecContext(ctx, `UPDATE verification_publication_deliveries SET state='pending', body=JSON_SET_STRING(body, 'State', 'pending') WHERE workspace_id=? AND task_id=? AND state='superseded'`, ws, taskID)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := result.RowsAffected(); err != nil || n == 0 {
					t.Fatalf("restore superseded delivery: %d rows, %v", n, err)
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
				rows, err := st.db.QueryContext(ctx, `SELECT kind,actor_id,payload_json FROM events_columnstore WHERE workspace_id=? AND kind='artifact.metadata_repaired' ORDER BY id`, ws)
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
				rows, err := st.db.QueryContext(ctx, `SELECT id,kind,actor_id,actor_role,payload_json,at FROM events_columnstore WHERE workspace_id=? AND task_id IS NULL AND LEFT(kind,CHAR_LENGTH(?))=? ORDER BY at,id`, ws, kindPrefix, kindPrefix)
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
			}, SeedWorkspaceConfigYAML: func(t *testing.T, ctx context.Context, raw string) {
				// A row an earlier release wrote; the version is unchanged.
				if _, err := st.db.ExecContext(ctx, `UPDATE workspaces SET config_yaml=? WHERE id=?`, raw, ws); err != nil {
					t.Fatal(err)
				}
			}, WorkspaceConfigYAML: func(t *testing.T, ctx context.Context) string {
				var raw string
				if err := st.db.QueryRowContext(ctx, `SELECT config_yaml FROM workspaces WHERE id=?`, ws).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				return raw
			}, SeedTaskIntakeKey: func(t *testing.T, ctx context.Context, taskID, key string) bool {
				// SingleStore has no unique key on intake_key, so a row can
				// carry a duplicate that no command writes (DEC-70).
				if _, err := st.db.ExecContext(ctx, `UPDATE tasks SET intake_key=? WHERE workspace_id=? AND id=?`, key, ws, taskID); err != nil {
					t.Fatal(err)
				}
				return true
			}, SeedRiskRating: func(t *testing.T, ctx context.Context, rating core.RiskRating) {
				// A reviewer rating a verdict recorded before verdicts stopped
				// stating a tier; no public writer accepts the source
				// (component-risk-review invariant 17).
				if err := st.taskTx(ctx, rating.TaskID, func(tx *sql.Tx) error {
					return insertRiskRatingTx(ctx, tx, rating)
				}); err != nil {
					t.Fatal(err)
				}
			}, SeedStoredStaleWorkOrder: func(t *testing.T, ctx context.Context, orderID string, exhausted bool, scheduledAt time.Time) {
				// A row the retired queue clock left; no command writes stale
				// any more (component-work-orders invariant 5).
				query := `UPDATE work_orders SET state='stale'`
				args := []any{}
				if exhausted {
					query += `,retry_suppressed=TRUE,retry_suppression_reason='automatic recovery exhausted',automatic_recovery_count=2,automatic_recovery_window_started_at=?`
					args = append(args, time.Now().UTC().Add(-time.Hour))
				}
				if !scheduledAt.IsZero() {
					query += `,next_automatic_recovery_at=?`
					args = append(args, scheduledAt.UTC())
				} else if exhausted {
					query += `,next_automatic_recovery_at=NULL`
				}
				query += ` WHERE workspace_id=? AND id=?`
				args = append(args, ws, orderID)
				result, err := st.db.ExecContext(ctx, query, args...)
				if err != nil {
					t.Fatal(err)
				}
				if rows, err := result.RowsAffected(); err != nil || rows != 1 {
					t.Fatalf("seed stored stale rows=%d err=%v", rows, err)
				}
			}, SeedStoredTaskHold: func(t *testing.T, ctx context.Context, taskID string) {
				// A row written before the hold retirement; no public writer
				// sets the column (DEC-125(3); component-persistence).
				result, err := st.db.ExecContext(ctx, `UPDATE tasks SET hold=true WHERE workspace_id=? AND id=?`, ws, taskID)
				if err != nil {
					t.Fatal(err)
				}
				if rows, err := result.RowsAffected(); err != nil || rows != 1 {
					t.Fatalf("seed stored hold rows=%d err=%v", rows, err)
				}
			}, SeedVerificationAttemptAuthorization: func(t *testing.T, ctx context.Context, attemptID, authorizationID string) {
				// A row an ungated start wrote before this rule; no public
				// writer sets it (component-verification-service invariant 19).
				result, err := st.db.ExecContext(ctx, `UPDATE verification_attempts SET body=JSON_SET_STRING(body,'ReplayAuthorizationID',?) WHERE workspace_id=? AND id=?`, authorizationID, ws, attemptID)
				if err != nil {
					t.Fatal(err)
				}
				if rows, err := result.RowsAffected(); err != nil || rows != 1 {
					t.Fatalf("seed attempt authorization rows=%d err=%v", rows, err)
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
			}, SeedStoredAgentCredential: func(t *testing.T, ctx context.Context, userID, label string, revoked bool) (string, string) {
				return seedStoredAgentCredential(t, ctx, st, userID, label, revoked)
			}, CredentialLastUsed: func(t *testing.T, ctx context.Context, credentialID string) time.Time {
				var lastUsed sql.NullTime
				if err := st.db.QueryRowContext(ctx, `SELECT last_used_at FROM user_tokens WHERE id=?`, credentialID).Scan(&lastUsed); err != nil {
					t.Fatal(err)
				}
				return lastUsed.Time.UTC()
			}, OAuthDeploymentEvents: func(t *testing.T, ctx context.Context) []core.Event {
				return oauthDeploymentEvents(t, ctx, st)
			}, AgeOAuthRecord: func(t *testing.T, ctx context.Context, table storetest.OAuthTable, id string, by time.Duration) {
				ageOAuthRecord(t, ctx, st, table, id, by)
			}, OAuthStoredRows: func(t *testing.T, ctx context.Context) []storetest.OAuthStoredRow {
				return oauthStoredRows(t, ctx, st)
			}, AgeEnvironmentKey: func(t *testing.T, ctx context.Context, keyID string, by time.Duration) {
				ageEnvironmentKey(t, ctx, st, keyID, by)
			}, StoredEnvironmentKey: func(t *testing.T, ctx context.Context, keyID string) storetest.EnvironmentStoredKey {
				return storedEnvironmentKey(t, ctx, st, keyID)
			}, LineageHTTP: func(*testing.T) http.Handler {
				// The shared-token operator fixture (component-lineage).
				server := httpapi.NewServer(st)
				server.Credentials = nil
				server.Workspace, server.BearerToken = ws, storetest.LineageHTTPToken
				return server.Handler()
			}}
		}})
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

// conformanceReadLedger keeps the store's read ledger in three pending maps
// (bytes, queries, rows per read label). Every ReadStats call drains the whole
// ledger, so each drain collects a fresh interval into all three maps, then
// returns and clears only the metric it was asked for. Reads collected while
// another metric was drained stay pending for that metric's next drain.
type conformanceReadLedger struct {
	st      *Store
	mu      sync.Mutex
	bytes   map[string]uint64
	queries map[string]uint64
	rows    map[string]uint64
}

func newConformanceReadLedger(st *Store) *conformanceReadLedger {
	return &conformanceReadLedger{
		st:      st,
		bytes:   map[string]uint64{},
		queries: map[string]uint64{},
		rows:    map[string]uint64{},
	}
}

// drainBytes returns the bytes per read label since the previous bytes drain.
func (l *conformanceReadLedger) drainBytes() map[string]uint64 {
	return l.drain(&l.bytes)
}

// drainQueries returns the queries per read label since the previous queries drain.
func (l *conformanceReadLedger) drainQueries() map[string]uint64 {
	return l.drain(&l.queries)
}

// drainRows returns the rows per read label since the previous rows drain.
func (l *conformanceReadLedger) drainRows() map[string]uint64 {
	return l.drain(&l.rows)
}

func (l *conformanceReadLedger) drain(pending *map[string]uint64) map[string]uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, total := range l.st.ReadStats(1 << 16).Top {
		l.bytes[total.Label] += total.Bytes
		l.queries[total.Label] += total.Queries
		l.rows[total.Label] += total.Rows
	}
	drained := *pending
	*pending = map[string]uint64{}
	return drained
}

// oauthDeploymentEvents reads the appended OAuth audit records from the
// deployment ledger itself; no workspace feed carries them
// (component-identity-membership invariant 36).
func oauthDeploymentEvents(t *testing.T, ctx context.Context, st *Store) []core.Event {
	t.Helper()
	rows, err := st.db.QueryContext(ctx, "SELECT id,kind,actor_id,actor_role,payload_json,at FROM deployment_events WHERE LEFT(kind, CHAR_LENGTH('identity.oauth_'))='identity.oauth_' ORDER BY at,id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []core.Event
	for rows.Next() {
		var event core.Event
		var role string
		var payload []byte
		if err := rows.Scan(&event.ID, &event.Kind, &event.ActorID, &role, &payload, &event.At); err != nil {
			t.Fatal(err)
		}
		event.ActorRole, event.Payload, event.At = core.ActorRole(role), json.RawMessage(payload), event.At.UTC()
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

// oauthTimeColumns lists every stored time of each OAuth table.
var oauthTimeColumns = map[storetest.OAuthTable][]string{
	storetest.OAuthClientsTable:            {"created_at", "unused_expires_at", "first_used_at"},
	storetest.OAuthAuthorizationCodesTable: {"created_at", "expires_at", "consumed_at"},
	storetest.OAuthGrantsTable:             {"created_at", "last_used_at", "revoked_at"},
	storetest.OAuthTokensTable:             {"created_at", "expires_at", "rotated_at"},
}

// ageOAuthRecord moves every stored time of one OAuth row back by by.
func ageOAuthRecord(t *testing.T, ctx context.Context, st *Store, table storetest.OAuthTable, id string, by time.Duration) {
	t.Helper()
	columns, ok := oauthTimeColumns[table]
	if !ok {
		t.Fatalf("unknown OAuth table %q", table)
	}
	sets := make([]string, 0, len(columns))
	args := make([]any, 0, len(columns)+1)
	for _, column := range columns {
		sets = append(sets, column+"=DATE_SUB("+column+",INTERVAL ? MICROSECOND)")
		args = append(args, by.Microseconds())
	}
	result, err := st.db.ExecContext(ctx, "UPDATE "+string(table)+" SET "+strings.Join(sets, ",")+" WHERE id=?", append(args, id)...)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		t.Fatalf("age %s row %s rows=%d err=%v", table, id, rows, err)
	}
}

// oauthStoredRows renders every OAuth row: its digest column apart and every
// other column as text.
func oauthStoredRows(t *testing.T, ctx context.Context, st *Store) []storetest.OAuthStoredRow {
	t.Helper()
	var out []storetest.OAuthStoredRow
	for _, table := range []struct {
		name   storetest.OAuthTable
		digest string
	}{
		{storetest.OAuthClientsTable, ""},
		{storetest.OAuthAuthorizationCodesTable, "code_hash"},
		{storetest.OAuthGrantsTable, ""},
		{storetest.OAuthTokensTable, "token_hash"},
	} {
		rows, err := st.db.QueryContext(ctx, "SELECT * FROM "+string(table.name)+" ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]sql.RawBytes, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			row := storetest.OAuthStoredRow{Table: table.name}
			var text []string
			for i, column := range columns {
				switch column {
				case "id":
					row.ID = string(values[i])
				case table.digest:
					row.Digest = append([]byte(nil), values[i]...)
					continue
				}
				text = append(text, column+"="+string(values[i]))
			}
			row.Text = strings.Join(text, " ")
			out = append(out, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}
