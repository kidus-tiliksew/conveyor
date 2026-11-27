package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// conformanceSchema is the one migrated schema the conformance run shares.
// Applying every migration costs about half a second per fixture locally and
// several seconds on a slow CI runner, and RunAll asks for 86 fixtures, so the
// schema is migrated once per test binary and every table but the migration
// ledger is truncated before each fixture instead. The conformance subtests
// run serially, and the other integration tests keep their own schemas.
var conformanceSchema struct {
	once   sync.Once
	err    error
	admin  *pgxpool.Pool
	pool   *pgxpool.Pool
	name   string
	tables string
	org    string
	store  *Store
}

func TestMain(m *testing.M) {
	code := m.Run()
	if conformanceSchema.admin != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if conformanceSchema.pool != nil {
			conformanceSchema.pool.Close()
		}
		if _, err := conformanceSchema.admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{conformanceSchema.name}.Sanitize()+" CASCADE"); err != nil {
			fmt.Fprintf(os.Stderr, "drop conformance schema %s: %v\n", conformanceSchema.name, err)
			if code == 0 {
				code = 1
			}
		}
		cancel()
		conformanceSchema.admin.Close()
	}
	os.Exit(code)
}

func openConformanceSchema(databaseURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return err
	}
	name := "conformance_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		return err
	}
	conformanceSchema.admin = admin
	conformanceSchema.name = name
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return err
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = name
	// The production tracer and counting dialer feed the read ledger the
	// conformance byte bounds drain (component-persistence, Metrics and database
	// read accounting).
	reads := dbmetrics.NewReadLedger()
	poolConfig.ConnConfig.Tracer = newRequestTracer(poolConfig.ConnConfig.Tracer, time.Now, nil)
	poolConfig.ConnConfig.DialFunc = countingDialFunc(poolConfig.ConnConfig.DialFunc, reads)
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return err
	}
	conformanceSchema.pool = pool
	if err = migrateControlPlaneToVersion(ctx, pool, 0); err != nil {
		return fmt.Errorf("migrate conformance schema: %w", err)
	}
	// orgs is excluded: migration 081 seeds the singleton deployment row and a
	// trigger forbids deleting or re-keying it, so the reset restores the seeded
	// row's other columns instead of truncating the table.
	rows, err := admin.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema=$1 AND table_type='BASE TABLE' AND table_name NOT IN ('conveyor_schema_migrations','orgs') ORDER BY table_name`, name)
	if err != nil {
		return err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			return err
		}
		tables = append(tables, pgx.Identifier{name, table}.Sanitize())
	}
	if err = rows.Err(); err != nil {
		return err
	}
	// Truncation must reproduce a freshly migrated schema, so refuse a
	// migration set that seeds rows into an empty database.
	var seeded []string
	for _, table := range tables {
		var count int
		if err = admin.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			seeded = append(seeded, fmt.Sprintf("%s=%d", table, count))
		}
	}
	if len(seeded) > 0 {
		return fmt.Errorf("fresh schema holds seeded rows: %s", strings.Join(seeded, ", "))
	}
	conformanceSchema.tables = strings.Join(tables, ", ")
	var orgCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM orgs`).Scan(&orgCount); err != nil {
		return err
	}
	if orgCount != 1 {
		return fmt.Errorf("fresh schema holds %d organization rows, want the one seeded by migration 081", orgCount)
	}
	if err = pool.QueryRow(ctx, `SELECT row_to_json(orgs)::text FROM orgs`).Scan(&conformanceSchema.org); err != nil {
		return err
	}
	conformanceSchema.store = newStore(pool)
	conformanceSchema.store.reads = reads
	return nil
}

// resetConformanceSchema empties every table but the ledger and the singleton
// organization, whose seeded columns it restores.
func resetConformanceSchema(ctx context.Context) error {
	if _, err := conformanceSchema.admin.Exec(ctx, "TRUNCATE "+conformanceSchema.tables+" RESTART IDENTITY CASCADE"); err != nil {
		return err
	}
	_, err := conformanceSchema.pool.Exec(ctx, `UPDATE orgs SET name = seeded.name, created_at = seeded.created_at FROM json_populate_record(NULL::orgs, $1::json) AS seeded`, conformanceSchema.org)
	return err
}

// conformanceStore returns the shared store with every table empty and no
// forge encryption key configured.
func conformanceStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := integrationDatabaseURL(t)
	conformanceSchema.once.Do(func() { conformanceSchema.err = openConformanceSchema(databaseURL) })
	if conformanceSchema.err != nil {
		t.Fatal(conformanceSchema.err)
	}
	started := time.Now()
	if err := resetConformanceSchema(t.Context()); err != nil {
		t.Fatalf("reset conformance schema: %v", err)
	}
	st := conformanceSchema.store
	st.ConfigureGitHubAppKeyEncryptionKey(nil)
	t.Logf("fixture reset in %s", time.Since(started).Round(time.Millisecond))
	return st
}

func TestPostgresConformanceIntegration(t *testing.T) {
	integrationDatabaseURL(t)
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
			t.Helper()
			st := conformanceStore(t)
			workspace := "conformance-" + core.NewTaskID()
			ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.SystemActor())
			cfg := &config.Config{Workspace: workspace, Repos: repos, Routing: config.Routing{Stages: map[string]config.StageRoute{
				"implement": {Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour},
			}}}
			if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			reads := newConformanceReadLedger(st)
			return storetest.Fixture{ArtifactListStatements: storetest.ArtifactListStatementsExactlyTwo, DrainReadBytes: reads.drainBytes, DrainReadQueries: reads.drainQueries, DrainReadRows: reads.drainRows, VerificationClaimReads: verificationClaimReads(workspace), RestoreSupersededDeliveries: func(t *testing.T, ctx context.Context, taskID string) {
				t.Helper()
				tag, err := st.pool.Exec(ctx, `UPDATE verification_publication_deliveries SET state='pending', body=jsonb_set(body, '{State}', '"pending"') WHERE workspace_id=$1 AND task_id=$2 AND state='superseded'`, workspace, taskID)
				if err != nil || tag.RowsAffected() == 0 {
					t.Fatalf("restore superseded delivery: %v, rows %d", err, tag.RowsAffected())
				}
			}, DeliveryPassSkipsHeldKeys: true, RestoreHistoricalDelivery: func(t *testing.T, ctx context.Context, taskID string, generation int, state, errorClass string, next *time.Time) {
				t.Helper()
				var raw []byte
				if err := st.pool.QueryRow(ctx, `SELECT body FROM verification_publication_deliveries WHERE workspace_id=$1 AND task_id=$2 AND generation=$3 AND state='superseded'`, workspace, taskID, generation).Scan(&raw); err != nil {
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
				if _, err := st.pool.Exec(ctx, `UPDATE verification_publication_deliveries SET state=$4, body=$5, next_attempt_at=$6 WHERE workspace_id=$1 AND task_id=$2 AND generation=$3`, workspace, taskID, generation, state, body, next); err != nil {
					t.Fatal(err)
				}
			}, SeedForeignVerificationOperation: func(t *testing.T, ctx context.Context, taskID string) {
				t.Helper()
				tag, err := st.pool.Exec(ctx, `INSERT INTO verification_operations(workspace_id,id,task_id,context_id,run_id,logical_key,key_hash,state,body) SELECT workspace_id,id||'-malformed',task_id,context_id,run_id,logical_key||'-malformed',md5(id)||md5(id),'completed','{"ID":7,"StepID":7}'::jsonb FROM verification_operations WHERE workspace_id=$1 AND task_id=$2 LIMIT 1`, workspace, taskID)
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("seed malformed operation: %v, rows %d", err, tag.RowsAffected())
				}
			}, ReopenVerification: func(t *testing.T) store.Backend {
				pool, err := pgxpool.NewWithConfig(t.Context(), st.pool.Config())
				if err != nil {
					t.Fatal(err)
				}
				reopened := newStore(pool)
				t.Cleanup(reopened.Close)
				return reopened
			}, Backend: st, Context: ctx, Workspace: workspace, Config: cfg, ArtifactRepairEvents: func(ctx context.Context) ([]core.Event, error) {
				rows, err := st.pool.Query(ctx, `SELECT kind,actor_id,payload_json FROM events WHERE workspace_id=$1 AND kind='artifact.metadata_repaired' ORDER BY id`, workspaceFromArtifactContext(ctx))
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
				rows, err := st.pool.Query(ctx, `SELECT id,kind,actor_id,actor_role,payload_json,at FROM events WHERE workspace_id=$1 AND task_id IS NULL AND starts_with(kind,$2) ORDER BY at,id`, ws, kindPrefix)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				var result []core.Event
				for rows.Next() {
					var e core.Event
					var role string
					if err := rows.Scan(&e.ID, &e.Kind, &e.ActorID, &role, &e.Payload, &e.At); err != nil {
						return nil, err
					}
					e.ActorRole = core.ActorRole(role)
					result = append(result, e)
				}
				return result, rows.Err()
			}, SeedLegacy: postgresConformanceLegacySeed(st, ctx, workspace), SeedHistoricalLink: func(t *testing.T, ctx context.Context, link core.LineageLink) {
				if _, err := st.pool.Exec(ctx, `INSERT INTO links (workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
					link.Workspace, string(link.SrcType), link.SrcID, string(link.DstType), link.DstID, link.Kind, link.LegacyCreatedByEvent, link.CreatedAt); err != nil {
					t.Fatal(err)
				}
			}, SeedArtifact: func(t *testing.T, ctx context.Context, a core.Artifact, b []byte) {
				_, err := st.pool.Exec(ctx, `UPDATE artifacts SET content_type=$3,size_bytes=$4,content=$5 WHERE workspace_id=$1 AND id=$2`, a.Workspace, a.ID, a.ContentType, a.SizeBytes, b)
				if err != nil {
					t.Fatal(err)
				}
			}, SeedEvents: func(t *testing.T, ctx context.Context, base int64, events []core.Event) []core.Event {
				return postgresSeedEvents(t, st, ctx, base, events)
			}, SeedWorkspaceConfigYAML: func(t *testing.T, ctx context.Context, raw string) {
				// A row an earlier release wrote; the version is unchanged.
				tag, err := st.pool.Exec(ctx, `UPDATE workspaces SET config_yaml=$1 WHERE id=$2`, raw, workspace)
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("seed workspace config: rows=%d err=%v", tag.RowsAffected(), err)
				}
			}, WorkspaceConfigYAML: func(t *testing.T, ctx context.Context) string {
				var raw string
				if err := st.pool.QueryRow(ctx, `SELECT config_yaml FROM workspaces WHERE id=$1`, workspace).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				return raw
			}, SeedTaskIntakeKey: func(t *testing.T, ctx context.Context, taskID, key string) bool {
				// tasks_workspace_intake_key_idx refuses a duplicate key, so
				// PostgreSQL never holds one; the write proves the refusal.
				_, err := st.pool.Exec(ctx, `UPDATE tasks SET intake_key=$1 WHERE workspace_id=$2 AND id=$3`, key, workspace, taskID)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
					t.Fatalf("duplicate intake key write = %v, want a unique violation", err)
				}
				return false
			}, SeedRiskRating: func(t *testing.T, ctx context.Context, rating core.RiskRating) {
				// A reviewer rating a verdict recorded before verdicts stopped
				// stating a tier; no public writer accepts the source
				// (component-risk-review invariant 17).
				if err := st.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
					if err := lockWorkOrderTaskTx(ctx, tx, workspace, rating.TaskID); err != nil {
						return err
					}
					return insertRiskRatingTx(ctx, tx, q, rating)
				}); err != nil {
					t.Fatal(err)
				}
			}, SeedStoredStaleWorkOrder: func(t *testing.T, ctx context.Context, orderID string, exhausted bool, scheduledAt time.Time) {
				// A row the retired queue clock left; no command writes stale
				// any more (component-work-orders invariant 5).
				var scheduled any
				if !scheduledAt.IsZero() {
					scheduled = scheduledAt
				}
				tag, err := st.pool.Exec(ctx, `UPDATE work_orders SET state='stale',
					retry_suppressed=CASE WHEN $3 THEN true ELSE retry_suppressed END,
					retry_suppression_reason=CASE WHEN $3 THEN 'automatic recovery exhausted' ELSE retry_suppression_reason END,
					automatic_recovery_count=CASE WHEN $3 THEN 2 ELSE automatic_recovery_count END,
					automatic_recovery_window_started_at=CASE WHEN $3 THEN now() - interval '1 hour' ELSE automatic_recovery_window_started_at END,
					next_automatic_recovery_at=CASE WHEN $4::timestamptz IS NOT NULL THEN $4::timestamptz WHEN $3 THEN NULL ELSE next_automatic_recovery_at END
					WHERE workspace_id=$1 AND id=$2`, workspace, orderID, exhausted, scheduled)
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("seed stored stale rows=%d err=%v", tag.RowsAffected(), err)
				}
			}, SeedStoredTaskHold: func(t *testing.T, ctx context.Context, taskID string) {
				// A row written before the hold retirement; no public writer
				// sets the column (DEC-125(3); component-persistence).
				tag, err := st.pool.Exec(ctx, `UPDATE tasks SET hold=true WHERE workspace_id=$1 AND id=$2`, workspace, taskID)
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("seed stored hold rows=%d err=%v", tag.RowsAffected(), err)
				}
			}, SeedVerificationAttemptAuthorization: func(t *testing.T, ctx context.Context, attemptID, authorizationID string) {
				// A row an ungated start wrote before this rule. The retention
				// guard refuses ordinary attempt updates, so only this isolated
				// transaction changes its trigger mode
				// (component-verification-service invariant 19).
				tx, err := st.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				if _, err = tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
					t.Fatal(err)
				}
				tag, err := tx.Exec(ctx, `UPDATE verification_attempts SET body=jsonb_set(body,'{ReplayAuthorizationID}',to_jsonb($3::text)) WHERE workspace_id=$1 AND id=$2`, workspace, attemptID, authorizationID)
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("seed attempt authorization rows=%d err=%v", tag.RowsAffected(), err)
				}
				if err = tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}, SeedDashboardSessionTimes: func(t *testing.T, ctx context.Context, sessionID string, lastUsedAgo, expiresIn time.Duration) {
				// No public writer sets a session's stored times
				// (component-identity-membership).
				tag, err := st.pool.Exec(ctx, `UPDATE dashboard_sessions SET last_used_at=now()-$2::bigint*interval '1 microsecond',expires_at=now()+$3::bigint*interval '1 microsecond' WHERE id=$1`, sessionID, lastUsedAgo.Microseconds(), expiresIn.Microseconds())
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("seed dashboard session times rows=%d err=%v", tag.RowsAffected(), err)
				}
			}, DashboardSessionTimes: func(t *testing.T, ctx context.Context, sessionID string) (time.Time, time.Time) {
				var lastUsed *time.Time
				var expires time.Time
				if err := st.pool.QueryRow(ctx, `SELECT last_used_at,expires_at FROM dashboard_sessions WHERE id=$1`, sessionID).Scan(&lastUsed, &expires); err != nil {
					t.Fatal(err)
				}
				if lastUsed == nil {
					return time.Time{}, expires.UTC()
				}
				return lastUsed.UTC(), expires.UTC()
			}, SeedCredentialLastUsed: func(t *testing.T, ctx context.Context, credentialID string, lastUsedAgo time.Duration) {
				// No public writer sets a credential's last use
				// (component-identity-membership).
				tag, err := st.pool.Exec(ctx, `UPDATE user_tokens SET last_used_at=now()-$2::bigint*interval '1 microsecond' WHERE id=$1`, credentialID, lastUsedAgo.Microseconds())
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("seed credential last use rows=%d err=%v", tag.RowsAffected(), err)
				}
			}, SeedStoredAgentCredential: func(t *testing.T, ctx context.Context, userID, label string, revoked bool) (string, string) {
				return seedStoredAgentCredential(t, ctx, st, userID, label, revoked)
			}, CredentialLastUsed: func(t *testing.T, ctx context.Context, credentialID string) time.Time {
				var lastUsed *time.Time
				if err := st.pool.QueryRow(ctx, `SELECT last_used_at FROM user_tokens WHERE id=$1`, credentialID).Scan(&lastUsed); err != nil {
					t.Fatal(err)
				}
				if lastUsed == nil {
					return time.Time{}
				}
				return lastUsed.UTC()
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
			}, CloseFeedbackReport: func(t *testing.T, ctx context.Context, reportID string, at time.Time) {
				closeFeedbackReport(t, ctx, st, reportID, at)
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

func workspaceFromArtifactContext(ctx context.Context) string {
	ws, _ := store.WorkspaceFromContext(ctx)
	return ws
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
	rows, err := st.pool.Query(ctx, `SELECT id,kind,actor_id,actor_role,payload_json,at FROM deployment_events WHERE starts_with(kind, 'identity.oauth_') ORDER BY at,id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []core.Event
	for rows.Next() {
		var event core.Event
		var role string
		if err := rows.Scan(&event.ID, &event.Kind, &event.ActorID, &role, &event.Payload, &event.At); err != nil {
			t.Fatal(err)
		}
		event.ActorRole = core.ActorRole(role)
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
	for _, column := range columns {
		sets = append(sets, column+"="+column+"-$2::bigint*interval '1 microsecond'")
	}
	tag, err := st.pool.Exec(ctx, "UPDATE "+string(table)+" SET "+strings.Join(sets, ",")+" WHERE id=$1", id, by.Microseconds())
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("age %s row %s rows=%d err=%v", table, id, tag.RowsAffected(), err)
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
		digest := "NULL::bytea"
		text := "to_jsonb(r)::text"
		if table.digest != "" {
			digest = "r." + table.digest
			text = "(to_jsonb(r)-'" + table.digest + "')::text"
		}
		rows, err := st.pool.Query(ctx, "SELECT r.id,"+digest+","+text+" FROM "+string(table.name)+" r ORDER BY r.id")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			row := storetest.OAuthStoredRow{Table: table.name}
			if err := rows.Scan(&row.ID, &row.Digest, &row.Text); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			out = append(out, row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}
