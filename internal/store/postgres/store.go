// Package postgres implements the Phase 2 event-sourced store with pgx and
// sqlc. Every projection mutation and its audit event commit in one
// transaction; events and interventions are append-only at the database layer
// (component-persistence).
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/pglog"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
	"gopkg.in/yaml.v3"
)

type Store struct {
	pool *pgxpool.Pool
	// boundary is the pool behind error translation. Every direct query
	// goes through it; the raw pool only begins transactions and acquires
	// lock connections (component-persistence).
	boundary               boundaryDB
	queries                *db.Queries
	queue                  *logDispatchQueue
	log                    *pglog.Store
	knownTables            sync.Map // table name -> true once seen to exist
	gitHubAppEncryptionKey []byte
}

type sideEffectConnKey struct{}

const minimumPostgresVersionNum = 150000

func requireSupportedPostgres(ctx context.Context, pool *pgxpool.Pool) error {
	var raw string
	if err := pool.QueryRow(ctx, `SHOW server_version_num`).Scan(&raw); err != nil {
		return fmt.Errorf("read Postgres server version: %w", err)
	}
	version, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("parse Postgres server version %q: %w", raw, err)
	}
	return validatePostgresVersion(version)
}

func validatePostgresVersion(version int) error {
	if version < minimumPostgresVersionNum {
		return fmt.Errorf("Postgres 15 or newer is required; detected server_version_num %d", version)
	}
	return nil
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("configure Postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect Postgres: %w", err)
	}
	if err := requireSupportedPostgres(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return newStore(pool), nil
}

// newStore assembles a store over a migrated pool.
func newStore(pool *pgxpool.Pool) *Store {
	log := pglog.New(pool)
	boundary := boundaryDB{pool}
	return &Store{pool: pool, boundary: boundary, queries: db.New(boundary), queue: newLogDispatchQueue(boundary, log), log: log}
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Log is the event log the store's queue runs on. Jobs are appended in the
// same transactions as the rows that demand them.
func (s *Store) Log() eventlog.Store { return s.log }
func (s *Store) IsDurable() bool     { return true }

// WithTaskSideEffectLock holds a workspace-scoped Postgres advisory lock across one
// external side effect. This keeps duplicate dashboard requests and multiple
// daemon instances from issuing concurrent forge merge calls.
func (s *Store) WithTaskSideEffectLock(ctx context.Context, taskID string, fn func(context.Context) error) error {
	key := "conveyor:task-operation:" + workspace(ctx) + ":" + taskID
	return s.withAdvisoryLock(ctx, key, fn)
}

func (s *Store) withAdvisoryLock(ctx context.Context, key string, fn func(context.Context) error) error {
	// Keep nested branch and task advisories on one session. inTx must keep
	// using the connection that owns the outer task lock (AC-3.5).
	if conn, ok := ctx.Value(sideEffectConnKey{}).(*pgxpool.Conn); ok {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", key); err != nil {
			return translateDriverError(err)
		}
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock(hashtext($1))", key); err != nil {
				// The outer owner releases the pool slot; close the underlying
				// session so a held lock can never return to the pool.
				_ = conn.Conn().Close(unlockCtx)
			}
		}()
		return fn(ctx)
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return translateDriverError(err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", key); err != nil {
		conn.Release()
		return translateDriverError(err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock(hashtext($1))", key); err != nil {
			// A session lock survives until its connection closes. Never return a
			// connection with a possibly-held advisory lock to the pool.
			_ = conn.Hijack().Close(unlockCtx)
			return
		}
		conn.Release()
	}()
	return fn(context.WithValue(ctx, sideEffectConnKey{}, conn))
}

// withDetachedAdvisoryLock removes the lock connection from the pool while the
// callback runs. Planning finalization performs ordinary Store reads as well
// as nested transactions; reserving one counted pool slot for the session lock
// could otherwise make a saturated or single-connection pool self-deadlock.
// The detached connection is always closed, which also releases the session
// lock if the explicit unlock fails.
func (s *Store) withDetachedAdvisoryLock(ctx context.Context, key string, fn func(context.Context) error) error {
	pooled, err := s.pool.Acquire(ctx)
	if err != nil {
		return translateDriverError(err)
	}
	conn := pooled.Hijack()
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", key); err != nil {
		_ = conn.Close(ctx)
		return translateDriverError(err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock(hashtext($1))", key)
		_ = conn.Close(unlockCtx)
	}()
	return fn(ctx)
}

func (s *Store) BootstrapConfig(ctx context.Context, cfg *config.Config) error {
	_, err := s.BootstrapWorkspaceConfig(ctx, cfg)
	return err
}

// BootstrapWorkspaceConfig imports workspace scope only when the row is
// empty. Subsequent starts reconcile only the file-owned capacity metadata
// and report seeded=false so callers can emit the required startup notice
func (s *Store) BootstrapWorkspaceConfig(ctx context.Context, cfg *config.Config) (bool, error) {
	configYAML, err := config.MarshalPolicyDocument(cfg)
	if err != nil {
		return false, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	q := s.queries.WithTx(tx)
	seeded := true
	if _, err := q.InsertWorkspace(ctx, db.InsertWorkspaceParams{
		ID: cfg.Workspace, Name: cfg.Workspace, ConfigYaml: string(configYAML),
	}); errors.Is(err, pgx.ErrNoRows) {
		seeded = false
		row, getErr := q.GetWorkspaceConfig(ctx, cfg.Workspace)
		if getErr != nil {
			return false, getErr
		}
		stored, legacy, parseErr := config.ParseStoredWorkspaceDocument([]byte(row.ConfigYaml), cfg, "database workspace config")
		if parseErr != nil {
			return false, parseErr
		}
		if legacy {
			canonical, marshalErr := config.MarshalPolicyDocument(stored)
			if marshalErr != nil {
				return false, marshalErr
			}
			if _, updateErr := tx.Exec(ctx, "UPDATE workspaces SET config_yaml = $1 WHERE id = $2", string(canonical), cfg.Workspace); updateErr != nil {
				return false, updateErr
			}
		}
	} else if err != nil {
		return false, err
	}
	if seeded {
		// Historical migration fixtures stop before the new column exists.
		// Normal startup finishes migrations before bootstrapping configuration.
		var installSchema bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='repos' AND column_name='install_conveyor')`).Scan(&installSchema); err != nil {
			return false, err
		}
		for _, repo := range cfg.Repos {
			if !installSchema {
				if _, err := tx.Exec(ctx, `INSERT INTO repos(workspace_id,name,url,github_slug,default_base) VALUES($1,$2,$3,$4,$5) ON CONFLICT(workspace_id,name) DO UPDATE SET url=EXCLUDED.url,github_slug=EXCLUDED.github_slug,default_base=EXCLUDED.default_base`, cfg.Workspace, repo.Name, repo.URL, repo.GitHub, repo.Base); err != nil {
					return false, err
				}
				continue
			}
			if err := upsertRepo(ctx, q, cfg.Workspace, repo); err != nil {
				return false, err
			}
		}
		// Startup bootstraps identity before the configured singleton
		// workspace. Bind that seeded operator without requiring request
		// credential context so the legacy shared token remains zero-config.
		// Historical migration tests intentionally stop before migration 084.
		var membershipSchema bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('workspace_role_bindings') IS NOT NULL`).Scan(&membershipSchema); err != nil {
			return false, err
		}
		if membershipSchema {
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role)
				SELECT $1,id,'operator' FROM users ORDER BY created_at,id LIMIT 1
				ON CONFLICT(workspace_id,user_id) DO NOTHING`, cfg.Workspace); err != nil {
				return false, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return seeded, nil
}

func workspace(ctx context.Context) string {
	value, _ := store.WorkspaceFromContext(ctx)
	return value
}

func (s *Store) ListWorkspaces(ctx context.Context) ([]core.Workspace, error) {
	rows, err := s.boundary.Query(ctx, `SELECT id,name,config_version,created_at FROM workspaces ORDER BY lower(name),id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.Workspace
	for rows.Next() {
		var item core.Workspace
		if err := rows.Scan(&item.ID, &item.Name, &item.ConfigVersion, &item.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) GetWorkspace(ctx context.Context, id string) (core.Workspace, error) {
	var item core.Workspace
	err := s.boundary.QueryRow(ctx, `SELECT id,name,config_version,created_at FROM workspaces WHERE id=$1`, id).
		Scan(&item.ID, &item.Name, &item.ConfigVersion, &item.CreatedAt)
	if err != nil {
		return core.Workspace{}, notFound(err, "workspace %s", id)
	}
	return item, nil
}

// CreateWorkspace commits identity, configuration, repositories, and the
// workspace.created audit event atomically.
func (s *Store) CreateWorkspace(ctx context.Context, id, name string, cfg *config.Config) (core.Workspace, error) {
	data, err := config.MarshalPolicyDocument(cfg)
	if err != nil {
		return core.Workspace{}, err
	}
	var created core.Workspace
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		row, err := q.InsertWorkspace(ctx, db.InsertWorkspaceParams{ID: id, Name: name, ConfigYaml: string(data)})
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrWorkspaceConflict
		}
		if err != nil {
			if strings.Contains(err.Error(), "workspaces_name") || strings.Contains(err.Error(), "workspaces_name_lower") {
				return store.ErrWorkspaceConflict
			}
			return err
		}
		for _, repo := range cfg.Repos {
			if err := upsertRepo(ctx, q, id, repo); err != nil {
				return err
			}
		}
		// Workspace creation is instance administration. Its authenticated
		// creator receives the initial operator binding atomically.
		if credential, ok := store.CredentialFromContext(ctx); ok {
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role) VALUES($1,$2,'operator')`, id, credential.OwnerUserID); err != nil {
				return err
			}
		}
		actor := store.ActorFromContext(ctx)
		_, err = q.InsertWorkspaceEvent(ctx, db.InsertWorkspaceEventParams{
			WorkspaceID: id, Kind: "workspace.created", ActorID: actor.ID, ActorRole: string(actor.Role),
			PayloadJson: core.JSONPayload(map[string]any{"id": id, "name": name, "config_version": row.ConfigVersion}),
			At:          timestamp(time.Now().UTC()),
		})
		if err != nil {
			return err
		}
		created = core.Workspace{ID: row.ID, Name: row.Name, ConfigVersion: row.ConfigVersion, CreatedAt: row.CreatedAt.Time}
		return nil
	})
	return created, err
}

func upsertRepo(ctx context.Context, q *db.Queries, workspace string, repo config.Repo) error {
	return q.UpsertRepo(ctx, db.UpsertRepoParams{
		WorkspaceID: workspace, Name: repo.Name, Url: repo.URL,
		GithubSlug: repo.GitHub, DefaultBase: repo.Base, InstallConveyor: repo.InstallEnabled(),
	})
}

func (s *Store) WorkspaceConfig(ctx context.Context) (config.VersionedDocument, error) {
	row, err := s.queries.GetWorkspaceConfig(ctx, workspace(ctx))
	if err != nil {
		return config.VersionedDocument{}, notFound(err, "workspace %s", workspace(ctx))
	}
	var document config.WorkspaceDocument
	decoder := yaml.NewDecoder(strings.NewReader(row.ConfigYaml))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return config.VersionedDocument{}, fmt.Errorf("decode stored workspace config: %w", err)
	}
	config.StoredRepositoryDefaults(document.Repos)
	if document.Harnesses == nil {
		document.Harnesses = []config.Harness{}
	}
	if document.Repos == nil {
		document.Repos = []config.Repo{}
	}
	return config.VersionedDocument{Document: document, Version: row.ConfigVersion}, nil
}

// RuntimeConfig composes the latest database policy document with the
// deployment's control-plane settings through the shared runtime helper
// (component-runtime; component-persistence). Callers take one value per
// dispatch so running jobs do not observe mid-flight policy changes.
func (s *Store) RuntimeConfig(ctx context.Context, deployment *config.Config) (*config.Config, error) {
	if deployment == nil {
		return nil, fmt.Errorf("deployment configuration is required")
	}
	id := workspace(ctx)
	row, err := s.queries.GetWorkspaceConfig(ctx, id)
	if err != nil {
		return nil, notFound(err, "workspace %s", id)
	}
	base := *deployment
	base.Workspace = id
	return config.ParseRuntimeWorkspaceDocument([]byte(row.ConfigYaml), &base, "database workspace config")
}

func (s *Store) UpdateWorkspaceConfig(ctx context.Context, expectedVersion int64, next *config.Config) (config.UpdateReceipt, error) {
	data, err := config.MarshalPolicyDocument(next)
	if err != nil {
		return config.UpdateReceipt{}, err
	}
	var result config.UpdateReceipt
	err = s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		before, err := q.GetWorkspaceConfig(ctx, workspace(ctx))
		if err != nil {
			return notFound(err, "workspace %s", workspace(ctx))
		}
		updated, err := q.UpdateWorkspaceConfig(ctx, db.UpdateWorkspaceConfigParams{
			ID: workspace(ctx), ExpectedVersion: expectedVersion, ConfigYaml: string(data),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return config.ErrVersionConflict
		}
		if err != nil {
			return err
		}
		for _, repo := range next.Repos {
			if err := upsertRepo(ctx, q, workspace(ctx), repo); err != nil {
				return err
			}
		}
		var previous config.WorkspaceDocument
		if err := yaml.Unmarshal([]byte(before.ConfigYaml), &previous); err != nil {
			return fmt.Errorf("decode previous workspace config: %w", err)
		}
		sections := configDiff(previous, next.PolicyDocument())
		actor := store.ActorFromContext(ctx)
		event, err := q.InsertWorkspaceEvent(ctx, db.InsertWorkspaceEventParams{
			WorkspaceID: workspace(ctx), Kind: "config.updated", ActorID: actor.ID,
			ActorRole: string(actor.Role), PayloadJson: core.JSONPayload(map[string]any{
				"from_version": before.ConfigVersion,
				"to_version":   updated.ConfigVersion,
				"sections":     sections,
			}), At: timestamp(time.Now().UTC()),
		})
		if err != nil {
			return err
		}
		result = config.UpdateReceipt{
			VersionedDocument: config.VersionedDocument{Document: next.PolicyDocument(), Version: updated.ConfigVersion},
			EventID:           event.ID, ActorID: actor.ID, Sections: sections,
		}
		return nil
	})
	return result, err
}

func configDiff(before, after config.WorkspaceDocument) []string {
	sections := make([]string, 0, 9)
	if before.Workspace != after.Workspace || before.MaxBounces != after.MaxBounces ||
		before.WorkOrderQueueTimeoutText != after.WorkOrderQueueTimeoutText {
		sections = append(sections, "workspace")
	}
	if !reflect.DeepEqual(before.Routing, after.Routing) {
		sections = append(sections, "routing")
	}
	if !reflect.DeepEqual(before.ExecutionSettings, after.ExecutionSettings) {
		sections = append(sections, "execution_settings")
	}
	if !reflect.DeepEqual(before.Repos, after.Repos) {
		sections = append(sections, "repos")
	}
	if !reflect.DeepEqual(before.Monitor, after.Monitor) {
		sections = append(sections, "monitor")
	}
	if !reflect.DeepEqual(before.Harnesses, after.Harnesses) {
		sections = append(sections, "harnesses")
	}
	if !reflect.DeepEqual(before.Review, after.Review) {
		sections = append(sections, "review")
	}
	if !reflect.DeepEqual(before.Setups, after.Setups) || before.DefaultSetup != after.DefaultSetup {
		sections = append(sections, "setups")
	}
	if !reflect.DeepEqual(before.Execution, after.Execution) {
		sections = append(sections, "execution")
	}
	return sections
}

func emptyIfNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func (s *Store) AppendEvent(ctx context.Context, event core.Event) error {
	var taskID string
	err := s.boundary.QueryRow(ctx, `SELECT id FROM tasks WHERE workspace_id=$1 AND id=$2`, workspace(ctx), event.TaskID).Scan(&taskID)
	if err != nil {
		return notFound(err, "task %s", event.TaskID)
	}
	if event.JobID != "" {
		job, err := s.queries.GetJob(ctx, db.GetJobParams{ID: event.JobID, WorkspaceID: workspace(ctx)})
		if err != nil || job.TaskID != taskID {
			return fmt.Errorf("job %s does not belong to task %s in workspace %s", event.JobID, event.TaskID, workspace(ctx))
		}
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		return insertEvent(ctx, q, event)
	})
}

func (s *Store) ListEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	rows, err := s.queries.ListEvents(ctx, db.ListEventsParams{TaskID: nullableText(taskID), WorkspaceID: workspace(ctx)})
	if err != nil {
		return nil, err
	}
	result := make([]core.Event, len(rows))
	for i := range rows {
		result[i] = eventFromDB(rows[i])
	}
	return result, nil
}

// ListEventsAfter resolves a nonzero afterID as an anchor event owned by the
// task in this workspace and returns the events strictly after its (at, id)
// tuple in ascending (at, id) order (component-persistence; DEC-39).
func (s *Store) ListEventsAfter(ctx context.Context, taskID string, afterID int64) ([]core.Event, error) {
	if afterID != 0 {
		var exists int
		err := s.boundary.QueryRow(ctx, `SELECT 1 FROM events e JOIN tasks t ON t.id = e.task_id WHERE e.task_id = $1 AND t.workspace_id = $2 AND e.id = $3`, taskID, workspace(ctx), afterID).Scan(&exists)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrEventAnchorNotFound
		}
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.queries.ListEventsAfter(ctx, db.ListEventsAfterParams{
		TaskID: nullableText(taskID), WorkspaceID: workspace(ctx), ID: afterID,
	})
	if err != nil {
		return nil, err
	}
	result := make([]core.Event, len(rows))
	for i := range rows {
		result[i] = eventFromDB(rows[i])
	}
	return result, nil
}

// ReadTaskEventStream selects one bounded (at, id) page on the task timeline
// index with one lookahead row.
func (s *Store) ReadTaskEventStream(ctx context.Context, q store.TaskEventStreamQuery) (store.TaskEventStreamPage, error) {
	if err := store.ValidateTaskEventStreamQuery(q); err != nil {
		return store.TaskEventStreamPage{}, err
	}
	var exists int
	if err := s.boundary.QueryRow(ctx, `SELECT 1 FROM tasks WHERE workspace_id=$1 AND id=$2`, workspace(ctx), q.TaskID).Scan(&exists); err != nil {
		return store.TaskEventStreamPage{}, notFound(err, "task %s", q.TaskID)
	}
	since := pgtype.Timestamptz{}
	if !q.Since.IsZero() {
		since = timestamp(q.Since)
	}
	seek, afterAt, afterID := false, pgtype.Timestamptz{}, int64(0)
	if q.After != nil {
		seek, afterAt, afterID = true, timestamp(q.After.At), q.After.ID
	}
	rows, err := s.boundary.Query(ctx, `SELECT e.id, e.task_id, e.job_id, e.kind, e.actor_id, e.actor_role, e.payload_json, e.at, e.workspace_id FROM events e
WHERE e.task_id = $1 AND ($2::timestamptz IS NULL OR e.at >= $2::timestamptz)
	AND (NOT $3::boolean OR (e.at, e.id) > ($4::timestamptz, $5::bigint))
ORDER BY e.at, e.id
LIMIT $6`, q.TaskID, since, seek, afterAt, afterID, q.Limit+1)
	if err != nil {
		return store.TaskEventStreamPage{}, err
	}
	defer rows.Close()
	page := store.TaskEventStreamPage{Events: []core.Event{}}
	for rows.Next() {
		var row db.Event
		if err := rows.Scan(&row.ID, &row.TaskID, &row.JobID, &row.Kind, &row.ActorID, &row.ActorRole, &row.PayloadJson, &row.At, &row.WorkspaceID); err != nil {
			return store.TaskEventStreamPage{}, err
		}
		if len(page.Events) == q.Limit {
			page.More = true
			break
		}
		page.Events = append(page.Events, eventFromDB(row))
	}
	return page, rows.Err()
}

func (s *Store) CountEventsSinceHumanIntervention(ctx context.Context, taskID, kind string) (int, error) {
	count, err := s.queries.CountEventsSinceHumanIntervention(ctx, db.CountEventsSinceHumanInterventionParams{TaskID: nullableText(taskID), Kind: kind, WorkspaceID: workspace(ctx)})
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

func (s *Store) CountEvents(ctx context.Context, taskID, kind string) (int, error) {
	count, err := s.queries.CountEvents(ctx, db.CountEventsParams{TaskID: nullableText(taskID), Kind: kind, WorkspaceID: workspace(ctx)})
	return int(count), err
}

func (s *Store) ListActivityMarkers(ctx context.Context) ([]store.ActivityMarker, error) {
	return s.listActivityMarkers(ctx, nil)
}

func (s *Store) ListActivityMarkersForTasks(ctx context.Context, taskIDs []string) ([]store.ActivityMarker, error) {
	if taskIDs != nil && len(taskIDs) == 0 {
		return []store.ActivityMarker{}, nil
	}
	return s.listActivityMarkers(ctx, taskIDs)
}

func (s *Store) listActivityMarkers(ctx context.Context, taskIDs []string) ([]store.ActivityMarker, error) {
	type markerRow struct {
		taskID      string
		latestStage string
		lastEventAt time.Time
		lastEventID int64
	}
	var rows []markerRow
	var selected pgx.Rows
	var err error
	if len(taskIDs) == 0 {
		selected, err = s.boundary.Query(ctx, `SELECT t.id,
			COALESCE(
				(SELECT w.stage FROM work_orders w WHERE w.workspace_id=t.workspace_id AND w.task_id=t.id
					AND w.state='claimed'
					ORDER BY w.execution_started_at DESC NULLS LAST,w.created_at DESC,w.id DESC LIMIT 1),
				(SELECT j.stage FROM jobs j WHERE j.task_id=t.id ORDER BY j.started_at DESC,j.id DESC LIMIT 1),
				''
			)::text,
			COALESCE((SELECT e.at FROM events e WHERE e.task_id=t.id ORDER BY e.at DESC, e.id DESC LIMIT 1),t.created_at)::timestamptz,
			COALESCE((SELECT e.id FROM events e WHERE e.task_id=t.id ORDER BY e.at DESC, e.id DESC LIMIT 1),0)::bigint
			FROM tasks t
			WHERE t.workspace_id=$1
			ORDER BY t.created_at,t.id`, workspace(ctx))
	} else {
		selected, err = s.boundary.Query(ctx, `SELECT t.id,
			COALESCE(
				(SELECT w.stage FROM work_orders w WHERE w.workspace_id=t.workspace_id AND w.task_id=t.id
					AND w.task_id=ANY($2::text[]) AND w.state='claimed'
					ORDER BY w.execution_started_at DESC NULLS LAST,w.created_at DESC,w.id DESC LIMIT 1),
				(SELECT j.stage FROM jobs j WHERE j.task_id=t.id ORDER BY j.started_at DESC,j.id DESC LIMIT 1),
				''
			)::text,
			COALESCE((SELECT e.at FROM events e WHERE e.task_id=t.id ORDER BY e.at DESC, e.id DESC LIMIT 1),t.created_at)::timestamptz,
			COALESCE((SELECT e.id FROM events e WHERE e.task_id=t.id ORDER BY e.at DESC, e.id DESC LIMIT 1),0)::bigint
			FROM tasks t
			WHERE t.workspace_id=$1 AND t.id=ANY($2::text[])
			ORDER BY t.created_at,t.id`, workspace(ctx), taskIDs)
	}
	if err != nil {
		return nil, err
	}
	for selected.Next() {
		var row markerRow
		if err = selected.Scan(&row.taskID, &row.latestStage, &row.lastEventAt, &row.lastEventID); err != nil {
			selected.Close()
			return nil, err
		}
		rows = append(rows, row)
	}
	if err = selected.Err(); err != nil {
		selected.Close()
		return nil, err
	}
	selected.Close()
	// Scope the order read to the requested tasks. The activity feed still
	// wants the whole workspace, but a Tasks page must not pull every
	// workspace order in only to discard most of it.
	orders, err := s.listWorkOrdersForTasks(ctx, taskIDs)
	if err != nil {
		return nil, err
	}
	var implementTaskIDs []string
	seenImplementTask := map[string]bool{}
	for _, order := range orders {
		if (order.Stage == core.StageImplement || order.Stage == core.StageVerify) && !seenImplementTask[order.TaskID] {
			implementTaskIDs = append(implementTaskIDs, order.TaskID)
			seenImplementTask[order.TaskID] = true
		}
	}
	blockersByTask, err := s.ListDependencyBlockers(ctx, implementTaskIDs)
	if err != nil {
		return nil, err
	}
	ordersByTask := make(map[string][]core.WorkOrder)
	hasReviewOrders := false
	reviewTaskIDs := make([]string, 0)
	seenReviewTask := map[string]bool{}
	for _, order := range orders {
		if order.Stage == core.StageImplement || order.Stage == core.StageVerify {
			blockers := blockersByTask[order.TaskID]
			order.BlockingTaskIDs = append([]string(nil), blockers.BlockingTaskIDs...)
			order.UnsatisfiableTaskIDs = append([]string(nil), blockers.UnsatisfiableTaskIDs...)
			if len(order.BlockingTaskIDs) > 0 {
				order.Claimable = false
			}
		}
		ordersByTask[order.TaskID] = append(ordersByTask[order.TaskID], order)
		hasReviewOrders = hasReviewOrders || order.Stage == core.StageReview
		if order.Stage == core.StageReview && (order.State == core.WorkOrderClaimed || order.State == core.WorkOrderQueued) &&
			!order.ExecutionStartedAt.IsZero() && !seenReviewTask[order.TaskID] {
			reviewTaskIDs = append(reviewTaskIDs, order.TaskID)
			seenReviewTask[order.TaskID] = true
		}
	}
	eventsByTask := make(map[string][]core.Event)
	requestEventsByTask := make(map[string][]core.Event)
	forgeEventsByTask := make(map[string][]core.Event)
	markerQuery := `SELECT e.id,e.task_id,COALESCE(e.job_id,''),e.kind,e.payload_json,e.at
		FROM events e JOIN tasks t ON t.id=e.task_id
		WHERE t.workspace_id=$1
		AND (cardinality($2::text[])=0 OR t.id=ANY($2::text[]))
		AND (
			(e.kind='pipeline.bounced' AND e.payload_json->>'source'='user-request-changes') OR
			(e.kind='work_order.claimed' AND e.payload_json->>'stage'='implement') OR
			(e.kind IN ('work_order.claimed','work_order.lease_renewed','work_order.released','review.completed','review.accepted','task.setup.changed')
				AND e.task_id=ANY($3::text[])) OR
			e.kind IN (
				'github_issue.publication_failed','github_issue.publication_published',
				'review.publication_failed','review.publication_published',
				'merge.failed','merge.confirmed','merge.reconciled'
			)
		) ORDER BY e.at,e.id`
	markerArgs := []any{workspace(ctx), emptyIfNil(taskIDs), emptyIfNil(reviewTaskIDs)}
	markerRows, err := s.boundary.Query(ctx, markerQuery, markerArgs...)
	if err != nil {
		return nil, err
	}
	for markerRows.Next() {
		var event core.Event
		if scanErr := markerRows.Scan(&event.ID, &event.TaskID, &event.JobID, &event.Kind, &event.Payload, &event.At); scanErr != nil {
			markerRows.Close()
			return nil, scanErr
		}
		switch event.Kind {
		case "work_order.claimed", "work_order.lease_renewed", "work_order.released", "review.completed", "review.accepted", "task.setup.changed":
			if hasReviewOrders {
				eventsByTask[event.TaskID] = append(eventsByTask[event.TaskID], event)
			}
		}
		switch event.Kind {
		case "pipeline.bounced", "work_order.claimed":
			requestEventsByTask[event.TaskID] = append(requestEventsByTask[event.TaskID], event)
		case "github_issue.publication_failed", "github_issue.publication_published", "review.publication_failed", "review.publication_published", "merge.failed", "merge.confirmed", "merge.reconciled":
			forgeEventsByTask[event.TaskID] = append(forgeEventsByTask[event.TaskID], event)
		}
	}
	if err = markerRows.Err(); err != nil {
		markerRows.Close()
		return nil, err
	}
	markerRows.Close()
	taskIDsForState := make([]string, 0, len(rows))
	for _, row := range rows {
		taskIDsForState = append(taskIDsForState, row.taskID)
	}
	taskStates := make(map[string]core.TaskState, len(taskIDsForState))
	if len(taskIDsForState) > 0 {
		stateRows, stateErr := s.boundary.Query(ctx, `SELECT id,state FROM tasks WHERE workspace_id=$1 AND id=ANY($2::text[])`, workspace(ctx), taskIDsForState)
		if stateErr != nil {
			return nil, stateErr
		}
		for stateRows.Next() {
			var taskID string
			var state core.TaskState
			if stateErr = stateRows.Scan(&taskID, &state); stateErr != nil {
				stateRows.Close()
				return nil, stateErr
			}
			taskStates[taskID] = state
		}
		if stateErr = stateRows.Err(); stateErr != nil {
			stateRows.Close()
			return nil, stateErr
		}
		stateRows.Close()
	}
	result := make([]store.ActivityMarker, len(rows))
	for i, row := range rows {
		task := core.Task{ID: row.taskID, State: taskStates[row.taskID]}
		result[i] = store.ActivityMarker{
			TaskID: row.taskID, LatestStage: core.Stage(row.latestStage), LastEventAt: row.lastEventAt, LastEventID: row.lastEventID,
			ForgeFailure:              store.LatestForgeFailure(forgeEventsByTask[row.taskID]),
			ReviewDiagnostics:         store.ReviewVerdictDiagnostics(ordersByTask[row.taskID], eventsByTask[row.taskID], time.Now().UTC()),
			ReviewRecovery:            store.ReviewRecoveryNeeded(ordersByTask[row.taskID], eventsByTask[row.taskID]),
			InterruptedReviewRecovery: store.InterruptedReviewRecoveryNeeded(task, store.CurrentReviewOrders(ordersByTask[row.taskID], eventsByTask[row.taskID]), eventsByTask[row.taskID]),
			Stalled:                   store.StalledTask(ordersByTask[row.taskID]),
			UserChangesRequested:      store.UserRequestChangesPending(requestEventsByTask[row.taskID]),
		}
	}
	return result, nil
}

func (s *Store) UpsertTranscript(ctx context.Context, transcript core.Transcript) error {
	if transcript.CreatedAt.IsZero() {
		transcript.CreatedAt = time.Now().UTC()
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		job, err := q.GetJob(ctx, db.GetJobParams{ID: transcript.JobID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "job %s", transcript.JobID)
		}
		if _, err := q.UpsertTranscript(ctx, db.UpsertTranscriptParams{
			JobID: transcript.JobID, Uri: transcript.URI,
			RedactionStats: core.JSONPayload(transcript.RedactionStats),
			CreatedAt:      timestamp(transcript.CreatedAt),
		}); err != nil {
			return err
		}
		if artifactID := strings.TrimPrefix(transcript.URI, "artifact://"); artifactID != transcript.URI {
			if _, err := tx.Exec(ctx, `UPDATE artifact_links legacy
				SET role=$1
				WHERE legacy.workspace_id=$2 AND legacy.artifact_id=$3 AND legacy.task_id=$4 AND legacy.role=$5
				  AND NOT EXISTS (
					SELECT 1 FROM artifact_links explicit
					WHERE explicit.workspace_id=legacy.workspace_id AND explicit.artifact_id=legacy.artifact_id
					  AND explicit.task_id=legacy.task_id AND explicit.role=$1
				  )`, core.ArtifactRoleGeneratedAudit, workspace(ctx), artifactID, job.TaskID, core.ArtifactRoleTaskContext); err != nil {
				return err
			}
		}
		return insertEvent(ctx, q, core.Event{
			TaskID: job.TaskID, JobID: job.ID, Kind: "transcript.persisted",
			Payload: core.JSONPayload(map[string]any{"uri": transcript.URI, "redaction_stats": transcript.RedactionStats}),
		})
	})
}

func (s *Store) GetTranscript(ctx context.Context, jobID string) (core.Transcript, error) {
	row, err := s.queries.GetTranscript(ctx, db.GetTranscriptParams{JobID: jobID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return core.Transcript{}, notFound(err, "transcript for job %s", jobID)
	}
	var stats core.RedactionStats
	if err := json.Unmarshal(row.RedactionStats, &stats); err != nil {
		return core.Transcript{}, fmt.Errorf("decode transcript redaction stats: %w", err)
	}
	return core.Transcript{JobID: row.JobID, URI: row.Uri, RedactionStats: stats, CreatedAt: row.CreatedAt.Time}, nil
}

// activityWorkOrderColumns preserves scanWorkOrder's shape while omitting the
// potentially large review-authority snapshots that board projections never
// inspect or render.
const activityWorkOrderColumns = `id, task_id, job_id, stage, state, claimant_id,
session_id, attempt_id, client_token_hash, agent, model, worker_id, lease_expires_at,
				review_round, review_seat, required_model, required_harness, required_effort, required_harness_config, execution_timeout, model_enforcement,
				reason_code, review_kind, review_scope, baseline_sha, head_sha, verification_context_id,
queue_entered_at, queue_deadline, queue_blocked_at, execution_started_at, execution_deadline,
last_attempt_id, last_attempt_outcome, last_failure_category, last_failure_message, last_failure_detail, last_failure_exit_status, last_failure_at,
automatic_retry_count, next_retry_at, retry_suppressed, retry_suppression_reason,
redispatch_count, operator_direction, checkpoint, continuation_session_id, continuation_attempt_id, continuation_harness, continuation_launch_environment, progress, cost_usd, tokens_in, tokens_out, usage_reported, self_reported,
rate_limit, rate_limit_observed_at, created_at, updated_at, NULL::jsonb, NULL::jsonb`

func nullableTimeValue(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx, *db.Queries) error) error {
	var (
		tx  pgx.Tx
		err error
	)
	if conn, ok := ctx.Value(sideEffectConnKey{}).(*pgxpool.Conn); ok {
		// A lifecycle command inside WithTaskSideEffectLock must use the same
		// PostgreSQL session. Advisory locks are re-entrant within one session;
		// starting this transaction through the pool can select another session
		// and deadlock against the outer task-operation lock.
		tx, err = s.beginOn(ctx, conn)
	} else {
		tx, err = s.begin(ctx)
	}
	if err != nil {
		return translateBackendConflict(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := fn(tx, s.queries.WithTx(tx)); err != nil {
		return translateBackendConflict(err)
	}
	return translateBackendConflict(tx.Commit(ctx))
}

func insertEvent(ctx context.Context, q *db.Queries, event core.Event) error {
	_, err := insertEventWithID(ctx, q, event)
	return err
}

func insertEventWithID(ctx context.Context, q *db.Queries, event core.Event) (int64, error) {
	if strings.TrimSpace(event.TaskID) == "" {
		return 0, fmt.Errorf("task-bound event %q requires a task id; use insertWorkspaceEvent for workspace-scoped events", event.Kind)
	}
	actor := store.ActorFromContext(ctx)
	if event.ActorID == "" {
		event.ActorID = actor.ID
	}
	if event.ActorRole == "" {
		event.ActorRole = actor.Role
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.Payload == nil {
		event.Payload = json.RawMessage(`{}`)
	}
	inserted, err := q.InsertEvent(ctx, db.InsertEventParams{
		TaskID: nullableText(event.TaskID), JobID: nullableText(event.JobID), Kind: event.Kind,
		ActorID: event.ActorID, ActorRole: string(event.ActorRole),
		PayloadJson: event.Payload, At: timestamp(event.At),
	})
	if err != nil {
		return 0, err
	}
	event.ID, event.At = inserted.ID, inserted.At.Time
	projection := store.ProjectLineageEvent(inserted.WorkspaceID, event)
	for _, link := range projection.Suppresses {
		if err = q.DeleteLineageLink(ctx, db.DeleteLineageLinkParams{
			WorkspaceID: link.Workspace, SrcType: string(link.SrcType), SrcID: link.SrcID,
			DstType: string(link.DstType), DstID: link.DstID, Kind: link.Kind,
		}); err != nil {
			return 0, err
		}
	}
	for _, link := range projection.Links {
		if err = q.InsertLineageLink(ctx, db.InsertLineageLinkParams{
			WorkspaceID: link.Workspace, SrcType: string(link.SrcType), SrcID: link.SrcID,
			DstType: string(link.DstType), DstID: link.DstID, Kind: link.Kind,
			CreatedByEventID: link.CreatedByEventID, CreatedAt: timestamp(link.CreatedAt),
		}); err != nil {
			return 0, err
		}
	}
	return inserted.ID, nil
}

// insertWorkspaceEvent records one task-less event and projects its canonical
// lineage inside the caller's transaction. Task-bound and workspace-scoped
// event insertion stay deliberately separate so a missing task ID cannot turn
// an INSERT ... SELECT into a silent pgx.ErrNoRows rollback.
func insertWorkspaceEvent(ctx context.Context, q *db.Queries, event core.Event) error {
	actor := store.ActorFromContext(ctx)
	if event.ActorID == "" {
		event.ActorID = actor.ID
	}
	if event.ActorRole == "" {
		event.ActorRole = actor.Role
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.Payload == nil {
		event.Payload = json.RawMessage(`{}`)
	}
	inserted, err := q.InsertWorkspaceEvent(ctx, db.InsertWorkspaceEventParams{
		WorkspaceID: workspace(ctx), Kind: event.Kind, ActorID: event.ActorID,
		ActorRole: string(event.ActorRole), PayloadJson: event.Payload, At: timestamp(event.At),
	})
	if err != nil {
		return err
	}
	event.ID, event.At = inserted.ID, inserted.At.Time
	projection := store.ProjectLineageEvent(inserted.WorkspaceID, event)
	for _, link := range projection.Suppresses {
		if err = q.DeleteLineageLink(ctx, db.DeleteLineageLinkParams{
			WorkspaceID: link.Workspace, SrcType: string(link.SrcType), SrcID: link.SrcID,
			DstType: string(link.DstType), DstID: link.DstID, Kind: link.Kind,
		}); err != nil {
			return err
		}
	}
	for _, link := range projection.Links {
		if err = q.InsertLineageLink(ctx, db.InsertLineageLinkParams{
			WorkspaceID: link.Workspace, SrcType: string(link.SrcType), SrcID: link.SrcID,
			DstType: string(link.DstType), DstID: link.DstID, Kind: link.Kind,
			CreatedByEventID: link.CreatedByEventID, CreatedAt: timestamp(link.CreatedAt),
		}); err != nil {
			return err
		}
	}
	return nil
}

func eventFromDB(event db.Event) core.Event {
	return core.Event{
		ID: event.ID, TaskID: event.TaskID.String, JobID: event.JobID.String,
		Kind: event.Kind, ActorID: event.ActorID, ActorRole: core.ActorRole(event.ActorRole),
		Payload: append(json.RawMessage(nil), event.PayloadJson...), At: event.At.Time,
	}
}

func timestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value.UTC(), Valid: true}
}

func nullableTimestamp(value time.Time) pgtype.Timestamptz {
	if value.IsZero() {
		return pgtype.Timestamptz{}
	}
	return timestamp(value)
}

func nullableFloat(value *float64) pgtype.Float8 {
	if value == nil {
		return pgtype.Float8{}
	}
	return pgtype.Float8{Float64: *value, Valid: true}
}

func floatPointer(value pgtype.Float8) *float64 {
	if !value.Valid {
		return nil
	}
	result := value.Float64
	return &result
}

func nullableTime(value pgtype.Timestamptz) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time
}

func nullableText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func notFound(err error, format string, args ...any) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf(format+": %w", append(args, store.ErrNotFound)...)
	}
	return err
}

var _ store.Store = (*Store)(nil)
