// Package postgres implements the Phase 2 event-sourced store with pgx and
// sqlc. Every projection mutation and its audit event commit in one
// transaction; events and interventions are append-only at the database layer
// (spec §3.1, §16, §17.0).
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"gopkg.in/yaml.v3"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

type Store struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	river   *river.Client[pgx.Tx]
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
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	riverClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("create River insert client: %w", err)
	}
	return &Store{pool: pool, queries: db.New(pool), river: riverClient}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// WithTaskLock holds a workspace-scoped Postgres advisory lock across one
// external side effect. This keeps duplicate dashboard requests and multiple
// daemon instances from issuing concurrent forge merge calls.
func (s *Store) WithTaskLock(ctx context.Context, taskID string, fn func() error) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	key := "conveyor:task-operation:" + workspace(ctx) + ":" + taskID
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", key); err != nil {
		conn.Release()
		return err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock(hashtext($1))", key); err != nil {
			// A session lock survives until its connection closes. Never return a
			// connection with a possibly-held task lock to the pool.
			_ = conn.Hijack().Close(unlockCtx)
			return
		}
		conn.Release()
	}()
	return fn()
}

func (s *Store) BootstrapConfig(ctx context.Context, cfg *config.Config) error {
	_, err := s.BootstrapWorkspaceConfig(ctx, cfg)
	return err
}

// BootstrapWorkspaceConfig imports workspace scope only when the row is
// empty. Subsequent starts reconcile only the file-owned capacity metadata
// and report seeded=false so callers can emit the required startup notice
// (spec §21.3).
func (s *Store) BootstrapWorkspaceConfig(ctx context.Context, cfg *config.Config) (bool, error) {
	configYAML, err := config.MarshalWorkspaceDocument(cfg)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
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
			canonical, marshalErr := config.MarshalWorkspaceDocument(stored)
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
		for _, repo := range cfg.Repos {
			if err := upsertRepo(ctx, q, cfg.Workspace, repo); err != nil {
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
	rows, err := s.pool.Query(ctx, `SELECT id,name,config_version,created_at FROM workspaces ORDER BY lower(name),id`)
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
	err := s.pool.QueryRow(ctx, `SELECT id,name,config_version,created_at FROM workspaces WHERE id=$1`, id).
		Scan(&item.ID, &item.Name, &item.ConfigVersion, &item.CreatedAt)
	if err != nil {
		return core.Workspace{}, notFound(err, "workspace %s", id)
	}
	return item, nil
}

// CreateWorkspace commits identity, configuration, repositories, and the
// workspace.created audit event atomically (spec §21.10).
func (s *Store) CreateWorkspace(ctx context.Context, id, name string, cfg *config.Config) (core.Workspace, error) {
	data, err := config.MarshalWorkspaceDocument(cfg)
	if err != nil {
		return core.Workspace{}, err
	}
	var created core.Workspace
	err = s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
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
		actor := store.ActorFromContext(ctx)
		if _, err := q.InsertWorkspaceEvent(ctx, db.InsertWorkspaceEventParams{
			WorkspaceID: id, Kind: "workspace.created", ActorID: actor.ID, ActorRole: string(actor.Role),
			PayloadJson: core.JSONPayload(map[string]any{"id": id, "name": name, "config_version": row.ConfigVersion}),
			At:          timestamp(time.Now().UTC()),
		}); err != nil {
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
		GithubSlug: repo.GitHub, DefaultBase: repo.Base,
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
	if document.Harnesses == nil {
		document.Harnesses = []config.Harness{}
	}
	if document.Repos == nil {
		document.Repos = []config.Repo{}
	}
	return config.VersionedDocument{Document: document, Version: row.ConfigVersion}, nil
}

// RuntimeConfig overlays the latest database document onto immutable
// deployment settings. Callers take one value per dispatch so running jobs do
// not observe mid-flight policy changes (spec §14.1, §21.3).
func (s *Store) RuntimeConfig(ctx context.Context, deployment *config.Config) (*config.Config, error) {
	id := workspace(ctx)
	row, err := s.queries.GetWorkspaceConfig(ctx, id)
	if err != nil {
		return nil, notFound(err, "workspace %s", id)
	}
	base := *deployment
	base.Workspace = id
	cfg, _, err := config.ParseStoredWorkspaceDocument([]byte(row.ConfigYaml), &base, "database workspace config")
	return cfg, err
}

func (s *Store) UpdateWorkspaceConfig(ctx context.Context, expectedVersion int64, next *config.Config) (config.UpdateReceipt, error) {
	data, err := config.MarshalWorkspaceDocument(next)
	if err != nil {
		return config.UpdateReceipt{}, err
	}
	var result config.UpdateReceipt
	err = s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		before, err := q.GetWorkspaceConfig(ctx, workspace(ctx))
		if err != nil {
			return err
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
		sections := configDiff(previous, next.WorkspaceDocument())
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
			VersionedDocument: config.VersionedDocument{Document: next.WorkspaceDocument(), Version: updated.ConfigVersion},
			EventID:           event.ID, ActorID: actor.ID, Sections: sections,
		}
		return nil
	})
	return result, err
}

func configDiff(before, after config.WorkspaceDocument) []string {
	sections := make([]string, 0, 7)
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
	if !reflect.DeepEqual(before.Harnesses, after.Harnesses) {
		sections = append(sections, "harnesses")
	}
	if !reflect.DeepEqual(before.Review, after.Review) {
		sections = append(sections, "review")
	}
	if !reflect.DeepEqual(before.Execution, after.Execution) {
		sections = append(sections, "execution")
	}
	return sections
}

func (s *Store) CreateTask(ctx context.Context, task core.Task) error {
	if task.Workspace != workspace(ctx) {
		return fmt.Errorf("task workspace %q does not match store workspace %q", task.Workspace, workspace(ctx))
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC()
	}
	if !task.Mode.Valid() {
		task.Mode, task.SpecApproval, task.MergeApproval = core.LegacyPolicy(task.Level)
	}
	if task.Level == "" {
		task.Level = core.LegacyLevel(task.Mode, task.SpecApproval, task.MergeApproval)
	}
	if task.NextStage == "" && (task.State == core.TaskQueued || task.State == core.TaskClaiming) {
		task.NextStage = core.InitialStage(task.Level)
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := q.InsertTask(ctx, taskInsertParams(task)); err != nil {
			return err
		}
		if err := insertEvent(ctx, q, core.Event{
			TaskID:  task.ID,
			Kind:    "task.created",
			Payload: core.JSONPayload(task),
			At:      task.CreatedAt,
		}); err != nil {
			return err
		}
		if task.State == core.TaskQueued {
			_, err := s.enqueueTaskTx(ctx, tx, task.ID, task.Workspace)
			return err
		}
		return nil
	})
}

func (s *Store) GetTask(ctx context.Context, id string) (core.Task, error) {
	task, err := s.queries.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: workspace(ctx)})
	if err != nil {
		return core.Task{}, notFound(err, "task %s", id)
	}
	result := taskFromDB(task)
	if err = s.hydrateGitHubLifecycle(ctx, &result); err != nil {
		return core.Task{}, err
	}
	return result, nil
}

func (s *Store) GetTaskByIntakeKey(ctx context.Context, key string) (core.Task, bool, error) {
	task, err := s.queries.GetTaskByIntakeKey(ctx, db.GetTaskByIntakeKeyParams{WorkspaceID: workspace(ctx), IntakeKey: nullableText(key)})
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Task{}, false, nil
	}
	if err != nil {
		return core.Task{}, false, err
	}
	result := taskFromDB(task)
	if err = s.hydrateGitHubLifecycle(ctx, &result); err != nil {
		return core.Task{}, false, err
	}
	return result, true, nil
}

func (s *Store) ListTasks(ctx context.Context) ([]core.Task, error) {
	rows, err := s.queries.ListTasks(ctx, workspace(ctx))
	if err != nil {
		return nil, err
	}
	result := make([]core.Task, len(rows))
	for i := range rows {
		result[i] = taskFromDB(rows[i])
		if err = s.hydrateGitHubLifecycle(ctx, &result[i]); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Store) hydrateGitHubLifecycle(ctx context.Context, task *core.Task) error {
	lifecycle, ok, err := s.GetGitHubLifecycle(ctx, task.ID)
	if err != nil {
		return err
	}
	if ok {
		task.GitHub = &lifecycle
	}
	return nil
}

func (s *Store) UpdateTaskState(ctx context.Context, id string, state core.TaskState) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		before, err := q.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", id)
		}
		if _, err := q.UpdateTaskState(ctx, db.UpdateTaskStateParams{ID: id, WorkspaceID: workspace(ctx), State: string(state)}); err != nil {
			return err
		}
		if err := insertEvent(ctx, q, core.Event{
			TaskID:  id,
			Kind:    "task.state_changed",
			Payload: core.JSONPayload(map[string]any{"from": before.State, "to": state}),
		}); err != nil {
			return err
		}
		if state == core.TaskQueued {
			_, err := s.enqueueTaskTx(ctx, tx, id, before.WorkspaceID)
			return err
		}
		return nil
	})
}

func (s *Store) SetTaskTransition(ctx context.Context, id string, state core.TaskState, nextStage, recoveryStage core.Stage) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		before, err := q.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", id)
		}
		if _, err := q.UpdateTaskTransition(ctx, db.UpdateTaskTransitionParams{
			ID: id, WorkspaceID: workspace(ctx), State: string(state), NextStage: string(nextStage), RecoveryStage: string(recoveryStage),
		}); err != nil {
			return err
		}
		if err := insertEvent(ctx, q, core.Event{TaskID: id, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": before.State, "to": state})}); err != nil {
			return err
		}
		if err := insertEvent(ctx, q, core.Event{TaskID: id, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{
			"from_stage": before.NextStage, "next_stage": nextStage, "recovery_stage": recoveryStage, "state": state,
		})}); err != nil {
			return err
		}
		if state == core.TaskQueued {
			_, err := s.enqueueTaskTx(ctx, tx, id, before.WorkspaceID)
			return err
		}
		return nil
	})
}

func (s *Store) UpdateTaskClassification(ctx context.Context, id, class string) error {
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		if _, err := q.UpdateTaskClassification(ctx, db.UpdateTaskClassificationParams{ID: id, WorkspaceID: workspace(ctx), Class: class}); err != nil {
			return notFound(err, "task %s", id)
		}
		return insertEvent(ctx, q, core.Event{TaskID: id, Kind: "task.classified", Payload: core.JSONPayload(map[string]any{"class": class})})
	})
}

func (s *Store) EnsureTaskEnqueued(ctx context.Context, id string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		task, err := q.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", id)
		}
		if core.TaskState(task.State) != core.TaskQueued {
			return fmt.Errorf("task %s is not queued", id)
		}
		inserted, err := s.enqueueTaskTx(ctx, tx, id, task.WorkspaceID)
		if err != nil || !inserted {
			return err
		}
		return insertEvent(ctx, q, core.Event{
			TaskID: id, Kind: "dispatch.reconciled",
			Payload: core.JSONPayload(map[string]string{"reason": "missing durable queue job"}),
		})
	})
}

// ReconcileQueuedTasks repairs projection/queue drift. River remains the
// execution claim, but a deleted or lost River row must not strand a durable
// queued task forever.
func (s *Store) ReconcileQueuedTasks(ctx context.Context) (int, error) {
	repaired := 0
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "conveyor:queue-reconcile:"+workspace(ctx)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
SELECT t.id
FROM tasks t
WHERE t.workspace_id = $1
  AND t.state = 'queued'
  AND NOT EXISTS (
      SELECT 1 FROM river_job r
      WHERE r.kind = 'dispatch_task'
        AND r.args->>'task_id' = t.id
        AND r.state IN ('available', 'pending', 'running', 'retryable', 'scheduled')
  )
ORDER BY t.created_at, t.id`, workspace(ctx))
		if err != nil {
			return err
		}
		var taskIDs []string
		for rows.Next() {
			var taskID string
			if err := rows.Scan(&taskID); err != nil {
				rows.Close()
				return err
			}
			taskIDs = append(taskIDs, taskID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, taskID := range taskIDs {
			inserted, err := s.enqueueTaskTx(ctx, tx, taskID, workspace(ctx))
			if err != nil {
				return err
			}
			if !inserted {
				continue
			}
			repaired++
			if err := insertEvent(ctx, q, core.Event{
				TaskID: taskID, Kind: "dispatch.reconciled",
				Payload: core.JSONPayload(map[string]string{"reason": "missing durable queue job"}),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return repaired, err
}

func (s *Store) CreateJob(ctx context.Context, job core.Job) error {
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		if _, err := q.GetTask(ctx, db.GetTaskParams{ID: job.TaskID, WorkspaceID: workspace(ctx)}); err != nil {
			return notFound(err, "task %s", job.TaskID)
		}
		if _, err := q.InsertJob(ctx, jobInsertParams(job)); err != nil {
			return err
		}
		return insertEvent(ctx, q, core.Event{
			TaskID: job.TaskID, JobID: job.ID, Kind: "job.created",
			Payload: core.JSONPayload(job),
		})
	})
}

func (s *Store) UpdateJob(ctx context.Context, job core.Job) error {
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		row, err := q.UpdateJob(ctx, jobUpdateParams(job, workspace(ctx)))
		if err != nil {
			return notFound(err, "job %s", job.ID)
		}
		return insertEvent(ctx, q, core.Event{
			TaskID: row.TaskID, JobID: row.ID, Kind: "job.updated", Payload: core.JSONPayload(job),
		})
	})
}

func (s *Store) ListJobs(ctx context.Context, taskID string) ([]core.Job, error) {
	rows, err := s.queries.ListJobs(ctx, db.ListJobsParams{TaskID: taskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return nil, err
	}
	result := make([]core.Job, len(rows))
	for i := range rows {
		result[i] = jobFromDB(rows[i])
	}
	return result, nil
}

func (s *Store) GetLatestJob(ctx context.Context, taskID string) (core.Job, bool, error) {
	row, err := s.queries.GetLatestJob(ctx, db.GetLatestJobParams{TaskID: taskID, WorkspaceID: workspace(ctx)})
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Job{}, false, nil
	}
	if err != nil {
		return core.Job{}, false, err
	}
	return jobFromDB(row), true, nil
}

func (s *Store) CreateSpecVersion(ctx context.Context, spec core.SpecVersion) (core.SpecVersion, error) {
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now().UTC()
	}
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := q.GetTask(ctx, db.GetTaskParams{ID: spec.TaskID, WorkspaceID: workspace(ctx)}); err != nil {
			return notFound(err, "task %s", spec.TaskID)
		}
		row, err := q.InsertSpecVersion(ctx, db.InsertSpecVersionParams{
			TaskID: spec.TaskID, Content: spec.Content, AcceptanceCount: int32(spec.AcceptanceCount),
			Acceptance: spec.Acceptance, Decomposition: spec.Decomposition, CreatedAt: timestamp(spec.CreatedAt),
		})
		if err != nil {
			return err
		}
		spec = specFromDB(row)
		return insertEvent(ctx, q, core.Event{TaskID: spec.TaskID, Kind: "spec.version_created", Payload: core.JSONPayload(map[string]any{"version": spec.Version, "acceptance_count": spec.AcceptanceCount})})
	})
	if err != nil {
		return core.SpecVersion{}, err
	}
	return spec, nil
}

func (s *Store) GetLatestSpecVersion(ctx context.Context, taskID string) (core.SpecVersion, bool, error) {
	row, err := s.queries.GetLatestSpecVersion(ctx, db.GetLatestSpecVersionParams{TaskID: taskID, WorkspaceID: workspace(ctx)})
	if errors.Is(err, pgx.ErrNoRows) {
		return core.SpecVersion{}, false, nil
	}
	if err != nil {
		return core.SpecVersion{}, false, err
	}
	return specFromDB(row), true, nil
}

func (s *Store) ApproveSpecVersion(ctx context.Context, taskID string, version int) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		_, err := q.ApproveLatestSpecVersion(ctx, db.ApproveLatestSpecVersionParams{TaskID: taskID, Version: int32(version), WorkspaceID: workspace(ctx)})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("spec version %d for task %s not found or superseded", version, taskID)
			}
			return err
		}
		return insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "spec.version_approved", Payload: core.JSONPayload(map[string]int{"version": version})})
	})
}

const githubLifecycleColumns = `task_id, repository, spec_version, source,
source_issue_number, issue_number, issue_url, outcome, state, create_state,
create_attempts, reconcile_misses, attempts, last_error,
created_at, updated_at`

func (s *Store) QueueGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if lifecycle.State == "" {
			lifecycle.State = core.GitHubPublicationQueued
		}
		if lifecycle.CreateState == "" {
			lifecycle.CreateState = core.GitHubCreateNotStarted
		}
		if lifecycle.CreatedAt.IsZero() {
			lifecycle.CreatedAt = time.Now().UTC()
		}
		command, err := tx.Exec(ctx, `INSERT INTO github_lifecycles (
			workspace_id, task_id, repository, spec_version, source,
			source_issue_number, state, create_state, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)
		ON CONFLICT (workspace_id, task_id) DO NOTHING`, workspace(ctx),
			lifecycle.TaskID, lifecycle.Repository, lifecycle.SpecVersion,
			lifecycle.Source, lifecycle.SourceIssueNumber, lifecycle.State, lifecycle.CreateState,
			lifecycle.CreatedAt)
		if err != nil {
			return err
		}
		if command.RowsAffected() == 1 {
			if err = insertEvent(ctx, q, core.Event{TaskID: lifecycle.TaskID, Kind: "github_issue.publication_queued", Payload: core.JSONPayload(lifecycle)}); err != nil {
				return err
			}
		}
		_, err = s.river.InsertTx(ctx, tx, queueargs.GitHubIssuePublicationArgs{WorkspaceID: workspace(ctx), TaskID: lifecycle.TaskID}, &river.InsertOpts{
			MaxAttempts: 5,
			Queue:       queueargs.GitHubIssuePublicationQueue(workspace(ctx)),
			UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
				rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
				rivertype.JobStateRetryable, rivertype.JobStateScheduled,
			}},
		})
		return err
	})
}

func (s *Store) GetGitHubLifecycle(ctx context.Context, taskID string) (core.GitHubLifecycle, bool, error) {
	lifecycle, err := scanGitHubLifecycle(s.pool.QueryRow(ctx, "SELECT "+githubLifecycleColumns+" FROM github_lifecycles WHERE workspace_id=$1 AND task_id=$2", workspace(ctx), taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.GitHubLifecycle{}, false, nil
	}
	return lifecycle, err == nil, err
}

func (s *Store) UpdateGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		command, err := tx.Exec(ctx, `UPDATE github_lifecycles SET
			issue_number=$1, issue_url=$2, outcome=$3, state=$4, attempts=$5,
			last_error=$6, create_state=$7, create_attempts=$8, reconcile_misses=$9,
			updated_at=now()
			WHERE workspace_id=$10 AND task_id=$11`, lifecycle.IssueNumber,
			lifecycle.IssueURL, lifecycle.Outcome, lifecycle.State, lifecycle.Attempts,
			lifecycle.LastError, lifecycle.CreateState, lifecycle.CreateAttempts,
			lifecycle.ReconcileMisses, workspace(ctx), lifecycle.TaskID)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return fmt.Errorf("GitHub lifecycle for task %s not found", lifecycle.TaskID)
		}
		kind := "github_issue.publication_retry"
		if lifecycle.State == core.GitHubPublicationPublished {
			kind = "github_issue.publication_published"
		} else if lifecycle.State == core.GitHubPublicationFailed {
			kind = "github_issue.publication_failed"
		}
		return insertEvent(ctx, q, core.Event{TaskID: lifecycle.TaskID, Kind: kind, Payload: core.JSONPayload(lifecycle)})
	})
}

func (s *Store) ReconcileGitHubLifecycles(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+githubLifecycleColumns+` FROM github_lifecycles
		WHERE workspace_id=$1 AND state <> 'published' ORDER BY created_at`, workspace(ctx))
	if err != nil {
		return 0, err
	}
	var pending []core.GitHubLifecycle
	for rows.Next() {
		lifecycle, scanErr := scanGitHubLifecycle(rows)
		if scanErr != nil {
			rows.Close()
			return 0, scanErr
		}
		pending = append(pending, lifecycle)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, lifecycle := range pending {
		if err = s.QueueGitHubLifecycle(ctx, lifecycle); err != nil {
			return 0, err
		}
	}
	return len(pending), nil
}

func scanGitHubLifecycle(row interface{ Scan(...any) error }) (core.GitHubLifecycle, error) {
	var lifecycle core.GitHubLifecycle
	var state, createState string
	err := row.Scan(&lifecycle.TaskID, &lifecycle.Repository, &lifecycle.SpecVersion,
		&lifecycle.Source, &lifecycle.SourceIssueNumber, &lifecycle.IssueNumber,
		&lifecycle.IssueURL, &lifecycle.Outcome, &state, &createState,
		&lifecycle.CreateAttempts, &lifecycle.ReconcileMisses, &lifecycle.Attempts, &lifecycle.LastError,
		&lifecycle.CreatedAt, &lifecycle.UpdatedAt)
	lifecycle.State = core.GitHubPublicationState(state)
	lifecycle.CreateState = core.GitHubCreateState(createState)
	return lifecycle, err
}

func (s *Store) AppendEvent(ctx context.Context, event core.Event) error {
	task, err := s.queries.GetTask(ctx, db.GetTaskParams{ID: event.TaskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return notFound(err, "task %s", event.TaskID)
	}
	if event.JobID != "" {
		job, err := s.queries.GetJob(ctx, db.GetJobParams{ID: event.JobID, WorkspaceID: workspace(ctx)})
		if err != nil || job.TaskID != task.ID {
			return fmt.Errorf("job %s does not belong to task %s in workspace %s", event.JobID, event.TaskID, workspace(ctx))
		}
	}
	return insertEvent(ctx, s.queries, event)
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

func (s *Store) ListEventsAfter(ctx context.Context, taskID string, afterID int64) ([]core.Event, error) {
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
	rows, err := s.queries.ListActivityMarkers(ctx, workspace(ctx))
	if err != nil {
		return nil, err
	}
	result := make([]store.ActivityMarker, len(rows))
	for i, row := range rows {
		result[i] = store.ActivityMarker{
			TaskID: row.TaskID, LatestStage: core.Stage(row.LatestStage), LastEventAt: row.LastEventAt.Time,
		}
	}
	return result, nil
}

func (s *Store) CreateIntervention(ctx context.Context, intervention core.Intervention) error {
	if !intervention.Action.Valid() {
		return fmt.Errorf("invalid intervention action %q", intervention.Action)
	}
	actor := store.ActorFromContext(ctx)
	if intervention.ActorID == "" {
		intervention.ActorID = actor.ID
	}
	if intervention.ActorRole == "" {
		intervention.ActorRole = actor.Role
	}
	if intervention.At.IsZero() {
		intervention.At = time.Now().UTC()
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		_, err := q.GetTask(ctx, db.GetTaskParams{ID: intervention.TaskID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", intervention.TaskID)
		}
		if intervention.JobID != "" {
			job, err := q.GetJob(ctx, db.GetJobParams{ID: intervention.JobID, WorkspaceID: workspace(ctx)})
			if err != nil || job.TaskID != intervention.TaskID {
				return fmt.Errorf("job %s does not belong to task %s in workspace %s", intervention.JobID, intervention.TaskID, workspace(ctx))
			}
		}
		if _, err := q.InsertIntervention(ctx, interventionInsertParams(intervention)); err != nil {
			return err
		}
		if err := insertEvent(ctx, q, core.Event{
			TaskID:    intervention.TaskID,
			JobID:     intervention.JobID,
			Kind:      "intervention." + string(intervention.Action),
			ActorID:   intervention.ActorID,
			ActorRole: intervention.ActorRole,
			Payload: core.JSONPayload(map[string]any{
				"reason_code": intervention.ReasonCode,
				"comment":     intervention.Comment,
			}),
			At: intervention.At,
		}); err != nil {
			return err
		}
		return nil
	})
}

func (s *Store) ListInterventions(ctx context.Context, taskID string) ([]core.Intervention, error) {
	rows, err := s.queries.ListInterventions(ctx, db.ListInterventionsParams{TaskID: taskID, WorkspaceID: workspace(ctx)})
	if err != nil {
		return nil, err
	}
	result := make([]core.Intervention, len(rows))
	for i := range rows {
		result[i] = interventionFromDB(rows[i])
	}
	return result, nil
}

func (s *Store) UpsertTranscript(ctx context.Context, transcript core.Transcript) error {
	if transcript.CreatedAt.IsZero() {
		transcript.CreatedAt = time.Now().UTC()
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
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

const workOrderColumns = `id, task_id, job_id, stage, state, claimant_id,
session_id, client_token_hash, agent, model, worker_id, lease_expires_at,
				review_round, review_seat, required_model, required_harness, required_harness_config, execution_timeout, model_enforcement,
queue_entered_at, queue_deadline, execution_started_at, execution_deadline,
last_attempt_outcome, last_failure_message, last_failure_exit_status, last_failure_at,
automatic_retry_count, next_retry_at, retry_suppressed,
redispatch_count, progress, cost_usd, tokens_in, tokens_out, self_reported,
created_at, updated_at`

func (s *Store) CreateWorkOrder(ctx context.Context, order core.WorkOrder) error {
	if order.CreatedAt.IsZero() {
		order.CreatedAt = time.Now().UTC()
	}
	if order.QueueEnteredAt.IsZero() {
		order.QueueEnteredAt = order.CreatedAt
	}
	if order.QueueDeadline.IsZero() {
		order.QueueDeadline = order.QueueEnteredAt.Add(config.DefaultWorkOrderQueueTimeout)
	}
	if order.State == "" {
		order.State = core.WorkOrderQueued
	}
	order.Claimable = order.ClaimableAt(time.Now().UTC())
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var linked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM tasks t
			JOIN jobs j ON j.task_id=t.id
			WHERE t.workspace_id=$1 AND t.id=$2 AND j.id=$3 AND j.stage=$4
		)`, workspace(ctx), order.TaskID, order.JobID, order.Stage).Scan(&linked); err != nil {
			return err
		}
		if !linked {
			return fmt.Errorf("work order task %s and job %s are not linked in workspace %s", order.TaskID, order.JobID, workspace(ctx))
		}
		_, err := tx.Exec(ctx, `INSERT INTO work_orders (
			id, workspace_id, task_id, job_id, stage, state, claimant_id,
			session_id, client_token_hash, agent, model, worker_id, lease_expires_at,
				review_round, review_seat, required_model, required_harness, required_harness_config, execution_timeout, model_enforcement,
			queue_entered_at, queue_deadline, execution_started_at, execution_deadline,
			last_attempt_outcome, last_failure_message, last_failure_exit_status, last_failure_at,
			automatic_retry_count, next_retry_at, retry_suppressed,
			redispatch_count, progress, cost_usd, tokens_in, tokens_out,
			self_reported, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$38)`,
			order.ID, workspace(ctx), order.TaskID, order.JobID, order.Stage, order.State,
			order.ClaimantID, order.SessionID, order.ClientTokenHash, order.Agent, order.Model, order.WorkerID,
			nullableTimeValue(order.LeaseExpiresAt), order.ReviewRound, order.ReviewSeat,
			order.RequiredModel, order.RequiredHarness, harnessSnapshotJSON(order.RequiredHarnessConfig), order.ExecutionTimeoutText, order.ModelEnforcement,
			order.QueueEnteredAt, order.QueueDeadline,
			nullableTimeValue(order.ExecutionStartedAt), nullableTimeValue(order.ExecutionDeadline),
			order.LastAttemptOutcome, order.LastFailureMessage, order.LastFailureExitStatus, nullableTimeValue(order.LastFailureAt),
			order.AutomaticRetryCount, nullableTimeValue(order.NextRetryAt), order.RetrySuppressed,
			order.RedispatchCount, order.Progress, order.CostUSD, order.TokensIn,
			order.TokensOut, true, order.CreatedAt)
		if err != nil {
			return err
		}
		return insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.created", Payload: core.JSONPayload(order)})
	})
}

func (s *Store) CreateReviewRound(ctx context.Context, taskID string, jobs []core.Job, orders []core.WorkOrder) error {
	if len(jobs) == 0 || len(jobs) != len(orders) {
		return fmt.Errorf("review round requires one job per work order")
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		before, err := q.GetTask(ctx, db.GetTaskParams{ID: taskID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", taskID)
		}
		var existing int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='review' AND review_round=$3`, workspace(ctx), taskID, orders[0].ReviewRound).Scan(&existing); err != nil {
			return err
		}
		if existing == len(orders) {
			return nil
		}
		if existing != 0 {
			return fmt.Errorf("review round %d is only partially persisted", orders[0].ReviewRound)
		}
		for i, job := range jobs {
			if job.TaskID != taskID || job.Stage != core.StageReview || orders[i].TaskID != taskID || orders[i].JobID != job.ID || orders[i].ReviewRound != orders[0].ReviewRound {
				return fmt.Errorf("invalid review round member %d", i)
			}
		}
		if _, err = q.UpdateTaskState(ctx, db.UpdateTaskStateParams{ID: taskID, WorkspaceID: workspace(ctx), State: string(core.TaskRunning)}); err != nil {
			return err
		}
		if err = insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": before.State, "to": core.TaskRunning})}); err != nil {
			return err
		}
		now := time.Now().UTC()
		for i, job := range jobs {
			if _, err = q.InsertJob(ctx, jobInsertParams(job)); err != nil {
				return err
			}
			if err = insertEvent(ctx, q, core.Event{TaskID: taskID, JobID: job.ID, Kind: "job.created", Payload: core.JSONPayload(job)}); err != nil {
				return err
			}
			order := orders[i]
			if order.CreatedAt.IsZero() {
				order.CreatedAt = now
			}
			if order.QueueEnteredAt.IsZero() {
				order.QueueEnteredAt = order.CreatedAt
			}
			if order.QueueDeadline.IsZero() {
				order.QueueDeadline = order.QueueEnteredAt.Add(config.DefaultWorkOrderQueueTimeout)
			}
			order.State, order.Claimable = core.WorkOrderQueued, true
			_, err = tx.Exec(ctx, `INSERT INTO work_orders (
				id, workspace_id, task_id, job_id, stage, state, claimant_id,
				session_id, client_token_hash, agent, model, worker_id, lease_expires_at,
				review_round, review_seat, required_model, required_harness, required_harness_config, execution_timeout, model_enforcement,
				queue_entered_at, queue_deadline, execution_started_at, execution_deadline,
				last_attempt_outcome, last_failure_message, last_failure_exit_status, last_failure_at,
				automatic_retry_count, next_retry_at, retry_suppressed,
				redispatch_count, progress, cost_usd, tokens_in, tokens_out,
				self_reported, created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,'','','','','','',NULL,$7,$8,$9,$10,$11,$12,'',$13,$14,NULL,NULL,'','',NULL,NULL,0,NULL,false,0,'',0,0,0,true,$15,$15)`,
				order.ID, workspace(ctx), taskID, job.ID, core.StageReview, core.WorkOrderQueued,
				order.ReviewRound, order.ReviewSeat, order.RequiredModel, order.RequiredHarness,
				harnessSnapshotJSON(order.RequiredHarnessConfig), order.ExecutionTimeoutText,
				order.QueueEnteredAt, order.QueueDeadline, order.CreatedAt)
			if err != nil {
				return err
			}
			if err = insertEvent(ctx, q, core.Event{TaskID: taskID, JobID: job.ID, Kind: "work_order.created", Payload: core.JSONPayload(order)}); err != nil {
				return err
			}
		}
		return insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "review.round_created", Payload: core.JSONPayload(map[string]any{"review_round": orders[0].ReviewRound, "seat_count": len(orders)})})
	})
}

func (s *Store) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	if err := s.refreshWorkOrders(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	order, err := scanWorkOrder(s.pool.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2", workspace(ctx), id))
	if err != nil {
		return core.WorkOrder{}, notFound(err, "work order %s", id)
	}
	return order, nil
}

func (s *Store) ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	if err := s.refreshWorkOrders(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 ORDER BY created_at,id", workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orders []core.WorkOrder
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (s *Store) ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	if err := s.refreshWorkOrders(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=$2 ORDER BY created_at,id", workspace(ctx), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orders []core.WorkOrder
	for rows.Next() {
		order, scanErr := scanWorkOrder(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (s *Store) ClaimWorkOrder(ctx context.Context, id string, claim core.WorkOrderClaim) (core.WorkOrder, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var taskID string
	if err := tx.QueryRow(ctx, `SELECT task_id FROM work_orders WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id).Scan(&taskID); err != nil {
		return core.WorkOrder{}, notFound(err, "work order %s", id)
	}
	// Session and client-token independence spans the implementation order and
	// every seat in a review round. Serialize all claims for one task before
	// locking an individual order so concurrent seats cannot both pass the
	// sibling check against an uncommitted peer (spec §21.12 change 4).
	lockKey := fmt.Sprintf("conveyor:work-order-claim:%s:%s", workspace(ctx), taskID)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", lockKey); err != nil {
		return core.WorkOrder{}, err
	}
	order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace(ctx), id))
	if err != nil {
		return core.WorkOrder{}, notFound(err, "work order %s", id)
	}
	now := time.Now().UTC()
	if (order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed) &&
		!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now) {
		order, err = s.transitionWorkOrderTx(ctx, tx, order, core.WorkOrderTimedOut, "work_order.timed_out", now)
		if err != nil {
			return core.WorkOrder{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return core.WorkOrder{}, err
		}
		return core.WorkOrder{}, fmt.Errorf("%w: %s", store.ErrWorkOrderTimedOut, id)
	}
	if order.State == core.WorkOrderQueued && order.ExecutionStartedAt.IsZero() &&
		!order.QueueDeadline.IsZero() && !order.QueueDeadline.After(now) {
		order, err = s.transitionWorkOrderTx(ctx, tx, order, core.WorkOrderStale, "work_order.stale", now)
		if err != nil {
			return core.WorkOrder{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return core.WorkOrder{}, err
		}
		return core.WorkOrder{}, fmt.Errorf("%w: %s", store.ErrWorkOrderStale, id)
	}
	if order.State == core.WorkOrderStale {
		return core.WorkOrder{}, fmt.Errorf("%w: %s", store.ErrWorkOrderStale, id)
	}
	if order.State == core.WorkOrderTimedOut {
		return core.WorkOrder{}, fmt.Errorf("%w: %s", store.ErrWorkOrderTimedOut, id)
	}
	if order.State == core.WorkOrderClaimed && order.LeaseExpiresAt.After(now) {
		return core.WorkOrder{}, fmt.Errorf("work order %s is already claimed", id)
	}
	if order.State == core.WorkOrderClaimed {
		if _, err = s.expireWorkOrderClaimTx(ctx, tx, order, now); err != nil {
			return core.WorkOrder{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return core.WorkOrder{}, err
		}
		return core.WorkOrder{}, fmt.Errorf("work order %s lease expired; operator recovery is required", id)
	}
	if order.State != core.WorkOrderQueued && order.State != core.WorkOrderClaimed {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not claimable", id)
	}
	if !order.ClaimableAt(now) {
		if order.RetrySuppressed {
			return core.WorkOrder{}, fmt.Errorf("work order %s automatic retry is suppressed; operator recovery is required", id)
		}
		return core.WorkOrder{}, fmt.Errorf("work order %s is in retry backoff until %s", id, order.NextRetryAt.Format(time.RFC3339Nano))
	}
	hash := ""
	if claim.ClientToken != "" {
		hash = fmt.Sprintf("%x", sha256.Sum256([]byte(claim.ClientToken)))
	}
	if order.Stage == core.StageReview {
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_orders
			WHERE workspace_id=$1 AND task_id=$2 AND id<>$3
			AND (stage='implement' OR (stage='review' AND review_round=$4))
			AND (($5 <> '' AND session_id=$5) OR ($6 <> '' AND client_token_hash=$6)))
			OR EXISTS (SELECT 1 FROM events e JOIN tasks t ON t.id=e.task_id
			WHERE t.workspace_id=$1 AND e.task_id=$2 AND e.kind='work_order.claimed'
			AND e.payload_json->>'id'<>$3
			AND (e.payload_json->>'stage'='implement' OR (e.payload_json->>'stage'='review' AND COALESCE((e.payload_json->>'review_round')::integer,0)=$4))
			AND $5<>'' AND e.payload_json->>'session_id'=$5)`,
			workspace(ctx), order.TaskID, order.ID, order.ReviewRound, claim.SessionID, hash).Scan(&blocked); err != nil {
			return core.WorkOrder{}, err
		}
		if blocked {
			return core.WorkOrder{}, fmt.Errorf("self-review forbidden: review session independence requires a fresh session and client token")
		}
		if claim.WorkerID != "" {
			if order.RequiredModel != "" && claim.Model != order.RequiredModel {
				return core.WorkOrder{}, fmt.Errorf("worker review model %q does not match pinned seat model %q", claim.Model, order.RequiredModel)
			}
			order.ModelEnforcement = "worker-pinned"
		} else {
			order.ModelEnforcement = "self-reported"
		}
	}
	if !order.ExecutionStartedAt.IsZero() && order.ExecutionDeadline.IsZero() && claim.ExecutionTimeout > 0 {
		order.ExecutionDeadline = order.ExecutionStartedAt.Add(claim.ExecutionTimeout)
		if !order.ExecutionDeadline.After(now) {
			if _, err = tx.Exec(ctx, `UPDATE work_orders SET execution_deadline=$1 WHERE workspace_id=$2 AND id=$3`, order.ExecutionDeadline, workspace(ctx), id); err != nil {
				return core.WorkOrder{}, err
			}
			order, err = s.transitionWorkOrderTx(ctx, tx, order, core.WorkOrderTimedOut, "work_order.timed_out", now)
			if err != nil {
				return core.WorkOrder{}, err
			}
			if err = tx.Commit(ctx); err != nil {
				return core.WorkOrder{}, err
			}
			return core.WorkOrder{}, fmt.Errorf("%w: %s", store.ErrWorkOrderTimedOut, id)
		}
	}
	lease := claim.Lease
	if lease <= 0 {
		lease = 15 * time.Minute
	}
	expires := now.Add(lease)
	executionStarted, executionDeadline := order.ExecutionStartedAt, order.ExecutionDeadline
	if executionStarted.IsZero() {
		executionStarted = now
		if claim.ExecutionTimeout > 0 {
			executionDeadline = now.Add(claim.ExecutionTimeout)
		}
	}
	row := tx.QueryRow(ctx, "UPDATE work_orders SET state='claimed', claimant_id=$1, session_id=$2, client_token_hash=$3, agent=$4, model=$5, worker_id=$6, lease_expires_at=$7, execution_started_at=$8, execution_deadline=$9, model_enforcement=$10, updated_at=$11 WHERE workspace_id=$12 AND id=$13 RETURNING "+workOrderColumns,
		claim.ClaimantID, claim.SessionID, hash, claim.Agent, claim.Model, claim.WorkerID, expires,
		executionStarted, nullableTimeValue(executionDeadline), order.ModelEnforcement, now, workspace(ctx), id)
	order, err = scanWorkOrder(row)
	if err != nil {
		return core.WorkOrder{}, err
	}
	q := s.queries.WithTx(tx)
	if _, err := tx.Exec(ctx, `UPDATE jobs SET state='running', model_tier=$1,
		started_at=COALESCE(started_at,$2), updated_at=$2 WHERE id=$3`, claim.Model, executionStarted, order.JobID); err != nil {
		return core.WorkOrder{}, err
	}
	if err := insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.claimed", Payload: core.JSONPayload(order)}); err != nil {
		return core.WorkOrder{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	return order, nil
}

func (s *Store) RedispatchWorkOrder(ctx context.Context, id string, queueTimeout time.Duration) (core.WorkOrder, error) {
	if queueTimeout <= 0 {
		return core.WorkOrder{}, fmt.Errorf("work-order queue timeout must be positive")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace(ctx), id))
	if err != nil {
		return core.WorkOrder{}, notFound(err, "work order %s", id)
	}
	now := time.Now().UTC()
	if order.State == core.WorkOrderClaimed && order.LeaseExpiresAt.After(now) {
		return core.WorkOrder{}, fmt.Errorf("work order %s has an active claim and cannot be redispatched", id)
	}
	if order.State == core.WorkOrderQueued && order.ExecutionStartedAt.IsZero() &&
		!order.QueueDeadline.IsZero() && !order.QueueDeadline.After(now) {
		order, err = s.transitionWorkOrderTx(ctx, tx, order, core.WorkOrderStale, "work_order.stale", now)
		if err != nil {
			return core.WorkOrder{}, err
		}
	}
	if order.State != core.WorkOrderStale {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not stale and cannot be redispatched", id)
	}
	row := tx.QueryRow(ctx, `UPDATE work_orders SET state='queued', claimant_id='',
		session_id='', client_token_hash='', agent='', model='', worker_id='', lease_expires_at=NULL, model_enforcement='',
		queue_entered_at=$1, queue_deadline=$2, execution_started_at=NULL,
		execution_deadline=NULL, redispatch_count=redispatch_count+1, progress='', updated_at=$1
		WHERE workspace_id=$3 AND id=$4 RETURNING `+workOrderColumns,
		now, now.Add(queueTimeout), workspace(ctx), id)
	order, err = scanWorkOrder(row)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET state='pending', started_at=NULL,
		ended_at=NULL, updated_at=$1 WHERE id=$2`, now, order.JobID); err != nil {
		return core.WorkOrder{}, err
	}
	q := s.queries.WithTx(tx)
	if err = insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.redispatched", Payload: core.JSONPayload(order), At: now}); err != nil {
		return core.WorkOrder{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	return order, nil
}

func (s *Store) RecoverWorkOrder(ctx context.Context, id, requestID string, queueTimeout time.Duration) (core.WorkOrder, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return core.WorkOrder{}, fmt.Errorf("recovery request_id is required")
	}
	if queueTimeout <= 0 {
		return core.WorkOrder{}, fmt.Errorf("work-order queue timeout must be positive")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.WorkOrder{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace(ctx), id))
	if err != nil {
		return core.WorkOrder{}, notFound(err, "work order %s", id)
	}
	var duplicate bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_order_recoveries WHERE workspace_id=$1 AND work_order_id=$2 AND request_id=$3)`, workspace(ctx), id, requestID).Scan(&duplicate); err != nil {
		return core.WorkOrder{}, err
	}
	if duplicate {
		if err = tx.Commit(ctx); err != nil {
			return core.WorkOrder{}, err
		}
		return order, nil
	}
	if order.State != core.WorkOrderQueued || order.SessionID != "" || order.WorkerID != "" || !order.LeaseExpiresAt.IsZero() {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not an unclaimed queued order", id)
	}
	if order.LastAttemptOutcome == "" && !order.RetrySuppressed && order.NextRetryAt.IsZero() {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not released, expired, or retry-suppressed", id)
	}
	now := time.Now().UTC()
	prior := order.LastAttemptOutcome
	order, err = scanWorkOrder(tx.QueryRow(ctx, `UPDATE work_orders SET last_attempt_outcome='',retry_suppressed=false,automatic_retry_count=0,next_retry_at=NULL,queue_entered_at=$1,queue_deadline=$2,redispatch_count=redispatch_count+1,updated_at=$1 WHERE workspace_id=$3 AND id=$4 AND state='queued' AND session_id='' AND worker_id='' AND lease_expires_at IS NULL AND (last_attempt_outcome<>'' OR retry_suppressed=true OR next_retry_at IS NOT NULL) RETURNING `+workOrderColumns, now, now.Add(queueTimeout), workspace(ctx), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.WorkOrder{}, fmt.Errorf("work order %s changed during recovery", id)
	}
	if err != nil {
		return core.WorkOrder{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO work_order_recoveries (workspace_id,work_order_id,request_id,created_at) VALUES ($1,$2,$3,$4)`, workspace(ctx), id, requestID, now); err != nil {
		return core.WorkOrder{}, err
	}
	q := s.queries.WithTx(tx)
	if err = insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.recovered", Payload: core.JSONPayload(map[string]any{"workspace_id": workspace(ctx), "work_order_id": id, "request_id": requestID, "prior_state": core.WorkOrderQueued, "prior_outcome": prior, "new_state": order.State, "reason": "operator recovery"}), At: now}); err != nil {
		return core.WorkOrder{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	return order, nil
}

func (s *Store) refreshWorkOrders(ctx context.Context) error {
	return s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		rows, err := tx.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND state IN ('queued','claimed') FOR UPDATE", workspace(ctx))
		if err != nil {
			return err
		}
		var orders []core.WorkOrder
		for rows.Next() {
			order, scanErr := scanWorkOrder(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			orders = append(orders, order)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		now := time.Now().UTC()
		for _, order := range orders {
			if !order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now) {
				if _, err = s.transitionWorkOrderTx(ctx, tx, order, core.WorkOrderTimedOut, "work_order.timed_out", now); err != nil {
					return err
				}
				continue
			}
			if order.State == core.WorkOrderClaimed && !order.LeaseExpiresAt.After(now) {
				if _, err = s.expireWorkOrderClaimTx(ctx, tx, order, now); err != nil {
					return err
				}
				continue
			}
			if order.State == core.WorkOrderQueued && order.ExecutionStartedAt.IsZero() &&
				!order.QueueDeadline.IsZero() && !order.QueueDeadline.After(now) {
				if _, err = s.transitionWorkOrderTx(ctx, tx, order, core.WorkOrderStale, "work_order.stale", now); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Store) expireWorkOrderClaimTx(ctx context.Context, tx pgx.Tx, order core.WorkOrder, now time.Time) (core.WorkOrder, error) {
	updated, err := scanWorkOrder(tx.QueryRow(ctx, `UPDATE work_orders SET state='queued',claimant_id='',session_id='',client_token_hash='',agent='',model='',worker_id='',lease_expires_at=NULL,model_enforcement='',execution_started_at=NULL,execution_deadline=NULL,last_attempt_outcome=$1,next_retry_at=NULL,retry_suppressed=true,updated_at=$2 WHERE workspace_id=$3 AND id=$4 AND state='claimed' RETURNING `+workOrderColumns, core.WorkOrderOutcomeExpired, now, workspace(ctx), order.ID))
	if err != nil {
		return core.WorkOrder{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET state='pending',started_at=NULL,ended_at=NULL,updated_at=$1 WHERE id=$2`, now, order.JobID); err != nil {
		return core.WorkOrder{}, err
	}
	q := s.queries.WithTx(tx)
	if err = insertEvent(ctx, q, core.Event{TaskID: updated.TaskID, JobID: updated.JobID, Kind: "work_order.expired", Payload: core.JSONPayload(map[string]any{"outcome": updated.LastAttemptOutcome, "retry_suppressed": true}), At: now}); err != nil {
		return core.WorkOrder{}, err
	}
	return updated, nil
}

func (s *Store) transitionWorkOrderTx(ctx context.Context, tx pgx.Tx, order core.WorkOrder, state core.WorkOrderState, kind string, now time.Time) (core.WorkOrder, error) {
	row := tx.QueryRow(ctx, `UPDATE work_orders SET state=$1, lease_expires_at=NULL,
		updated_at=$2 WHERE workspace_id=$3 AND id=$4 RETURNING `+workOrderColumns,
		state, now, workspace(ctx), order.ID)
	updated, err := scanWorkOrder(row)
	if err != nil {
		return core.WorkOrder{}, err
	}
	if state == core.WorkOrderTimedOut {
		if _, err = tx.Exec(ctx, `UPDATE jobs SET state='failed', ended_at=COALESCE(ended_at,$1),
			updated_at=$1 WHERE id=$2`, now, order.JobID); err != nil {
			return core.WorkOrder{}, err
		}
	}
	q := s.queries.WithTx(tx)
	if err = insertEvent(ctx, q, core.Event{TaskID: updated.TaskID, JobID: updated.JobID, Kind: kind, Payload: core.JSONPayload(updated), At: now}); err != nil {
		return core.WorkOrder{}, err
	}
	return updated, nil
}

func (s *Store) UpdateWorkOrder(ctx context.Context, order core.WorkOrder) error {
	var lifecycleErr error
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		current, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace(ctx), order.ID))
		if err != nil {
			return notFound(err, "work order %s", order.ID)
		}
		now := time.Now().UTC()
		if (current.State == core.WorkOrderQueued || current.State == core.WorkOrderClaimed) &&
			!current.ExecutionDeadline.IsZero() && !current.ExecutionDeadline.After(now) {
			if _, err = s.transitionWorkOrderTx(ctx, tx, current, core.WorkOrderTimedOut, "work_order.timed_out", now); err != nil {
				return err
			}
			lifecycleErr = fmt.Errorf("%w: %s", store.ErrWorkOrderTimedOut, order.ID)
			return nil
		}
		if current.State == core.WorkOrderTimedOut {
			lifecycleErr = fmt.Errorf("%w: %s", store.ErrWorkOrderTimedOut, order.ID)
			return nil
		}
		if current.State == core.WorkOrderStale {
			lifecycleErr = fmt.Errorf("%w: %s", store.ErrWorkOrderStale, order.ID)
			return nil
		}
		if current.State == core.WorkOrderClaimed && !current.LeaseExpiresAt.After(now) {
			if _, err = s.expireWorkOrderClaimTx(ctx, tx, current, now); err != nil {
				return err
			}
			lifecycleErr = fmt.Errorf("work order lease expired")
			return nil
		}
		if updateRequiresClaim(order.State, current.State) &&
			(current.State != core.WorkOrderClaimed || current.SessionID == "" || current.SessionID != order.SessionID) {
			return fmt.Errorf("work order %s is not claimed by this session", order.ID)
		}
		command, err := tx.Exec(ctx, `UPDATE work_orders SET state=$1, claimant_id=$2, session_id=$3,
			client_token_hash=$4, agent=$5, model=$6, lease_expires_at=$7,
			model_enforcement=$8, queue_entered_at=$9, queue_deadline=$10, execution_started_at=$11,
			execution_deadline=$12, last_attempt_outcome=$13, last_failure_message=$14,
			last_failure_exit_status=$15, last_failure_at=$16, automatic_retry_count=$17,
			next_retry_at=$18, retry_suppressed=$19, redispatch_count=$20, progress=$21,
			cost_usd=$22, tokens_in=$23, tokens_out=$24, self_reported=$25, updated_at=now()
			WHERE workspace_id=$26 AND id=$27`, order.State, order.ClaimantID, order.SessionID,
			order.ClientTokenHash, order.Agent, order.Model, nullableTimeValue(order.LeaseExpiresAt),
			order.ModelEnforcement,
			order.QueueEnteredAt, order.QueueDeadline, nullableTimeValue(order.ExecutionStartedAt),
			nullableTimeValue(order.ExecutionDeadline), order.LastAttemptOutcome, order.LastFailureMessage,
			order.LastFailureExitStatus, nullableTimeValue(order.LastFailureAt), order.AutomaticRetryCount,
			nullableTimeValue(order.NextRetryAt), order.RetrySuppressed, order.RedispatchCount, order.Progress,
			order.CostUSD, order.TokensIn, order.TokensOut, order.SelfReported, workspace(ctx), order.ID)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return fmt.Errorf("work order %s not found", order.ID)
		}
		return insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.updated", Payload: core.JSONPayload(order)})
	})
	if err != nil {
		return err
	}
	return lifecycleErr
}

func updateRequiresClaim(next, current core.WorkOrderState) bool {
	if next == core.WorkOrderClaimed {
		return true
	}
	return current != next && (next == core.WorkOrderSubmitted || next == core.WorkOrderCompleted)
}

const reviewPublicationColumns = `review_work_order_id, task_id, job_id, verdict,
reason_code, summary, feedback, reviewed_commit_sha, reviewer_model,
reviewer_session, same_model_as_implementer, review_round, review_seat,
required_model, required_harness, required_effort, model_enforcement, state, attempts, check_run_id,
comment_id, last_error, created_at, updated_at`

func (s *Store) QueueReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		return s.queueReviewPublicationTx(ctx, tx, q, publication)
	})
}

func (s *Store) queueReviewPublicationTx(ctx context.Context, tx pgx.Tx, q *db.Queries, publication core.ReviewPublication) error {
	if publication.State == "" {
		publication.State = core.ReviewPublicationQueued
	}
	if publication.CreatedAt.IsZero() {
		publication.CreatedAt = time.Now().UTC()
	}
	publication.UpdatedAt = publication.CreatedAt
	command, err := tx.Exec(ctx, `INSERT INTO review_publications (
			review_work_order_id, workspace_id, task_id, job_id, verdict, reason_code,
			summary, feedback, reviewed_commit_sha, reviewer_model, reviewer_session,
			same_model_as_implementer, review_round, review_seat, required_model,
			required_harness, required_effort, model_enforcement, state
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		ON CONFLICT (review_work_order_id) DO NOTHING`, publication.ReviewWorkOrderID,
		workspace(ctx), publication.TaskID, publication.JobID, publication.Verdict,
		publication.ReasonCode, publication.Summary, publication.Feedback,
		publication.ReviewedCommitSHA, publication.ReviewerModel,
		publication.ReviewerSession, publication.SameModelAsImplementer,
		publication.ReviewRound, publication.ReviewSeat, publication.RequiredModel,
		publication.RequiredHarness, publication.RequiredEffort, publication.ModelEnforcement,
		publication.State)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return nil
	}
	if err := insertEvent(ctx, q, core.Event{TaskID: publication.TaskID, JobID: publication.JobID, Kind: "review.publication_queued", Payload: core.JSONPayload(publication)}); err != nil {
		return err
	}
	_, err = s.river.InsertTx(ctx, tx, queueargs.ReviewPublicationArgs{WorkspaceID: workspace(ctx), ReviewWorkOrderID: publication.ReviewWorkOrderID}, &river.InsertOpts{
		MaxAttempts: 5,
		Queue:       queueargs.ReviewPublicationQueue(workspace(ctx)),
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
			rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
			rivertype.JobStateRetryable, rivertype.JobStateScheduled,
		}},
	})
	return err
}

// AcceptReviewDecision commits the durable verdict, routing decision, and
// eligible GitHub publication job as one transaction. A retry therefore sees
// either the entire accepted decision or none of it.
func (s *Store) AcceptReviewDecision(ctx context.Context, decision core.ReviewDecision) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		lockKey := fmt.Sprintf("conveyor:review:%s:%s:%d", workspace(ctx), decision.TaskID, decision.ReviewRound)
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", lockKey); err != nil {
			return err
		}
		var completed, accepted bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM events e JOIN tasks t ON t.id=e.task_id
			WHERE t.workspace_id=$1 AND e.task_id=$2 AND e.kind='review.completed'
				AND e.payload_json->>'review_work_order_id'=$3
		), EXISTS (
			SELECT 1 FROM events e JOIN tasks t ON t.id=e.task_id
			WHERE t.workspace_id=$1 AND e.task_id=$2 AND e.kind='review.accepted'
				AND e.payload_json->>'review_work_order_id'=$3
		)`, workspace(ctx), decision.TaskID, decision.ReviewWorkOrderID).Scan(&completed, &accepted); err != nil {
			return err
		}
		if accepted {
			return nil
		}
		before, err := q.GetTask(ctx, db.GetTaskParams{ID: decision.TaskID, WorkspaceID: workspace(ctx)})
		if err != nil {
			return notFound(err, "task %s", decision.TaskID)
		}
		job, err := q.GetJob(ctx, db.GetJobParams{ID: decision.JobID, WorkspaceID: workspace(ctx)})
		if err != nil || job.TaskID != decision.TaskID {
			return fmt.Errorf("job %s does not belong to task %s in workspace %s", decision.JobID, decision.TaskID, workspace(ctx))
		}
		if !completed {
			if err := insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "review.completed", Payload: reviewDecisionPayload(decision)}); err != nil {
				return err
			}
		}
		if decision.PublicationEligible {
			if err := s.queueReviewPublicationTx(ctx, tx, q, reviewPublicationFromDecision(decision)); err != nil {
				return err
			}
		}
		if err := insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "review.accepted", Payload: core.JSONPayload(map[string]any{"review_work_order_id": decision.ReviewWorkOrderID, "review_round": decision.ReviewRound, "review_seat": decision.ReviewSeat})}); err != nil {
			return err
		}
		reviews, required, err := completedReviewRoundTx(ctx, tx, workspace(ctx), decision.TaskID, decision.ReviewRound, decision.ReviewWorkOrderID)
		if err != nil {
			return err
		}
		if len(reviews) < required {
			return insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, Kind: "review.round_pending", Payload: core.JSONPayload(map[string]any{"review_round": decision.ReviewRound, "completed": len(reviews), "required": required})})
		}
		aggregate := aggregateReviewRoundResult(decision.ReviewRound, reviews)
		if err = insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "review.round_completed", Payload: core.JSONPayload(aggregate)}); err != nil {
			return err
		}

		state, next, recovery := core.TaskAwaiting, core.Stage(""), core.StageImplement
		if aggregate.Verdict == "changes_requested" {
			count, countErr := q.CountEvents(ctx, db.CountEventsParams{TaskID: nullableText(decision.TaskID), Kind: "pipeline.bounced", WorkspaceID: workspace(ctx)})
			if countErr != nil {
				return countErr
			}
			// The check-in comparison uses bounces since the last human
			// intervention, not the lifetime count (spec §21.17); the
			// recorded count in the event payload stays lifetime.
			window, windowErr := q.CountEventsSinceHumanIntervention(ctx, db.CountEventsSinceHumanInterventionParams{TaskID: nullableText(decision.TaskID), Kind: "pipeline.bounced", WorkspaceID: workspace(ctx)})
			if windowErr != nil {
				return windowErr
			}
			count++
			window++
			now := time.Now().UTC()
			actorID := fmt.Sprintf("review:round:%d", decision.ReviewRound)
			intervention := core.Intervention{TaskID: decision.TaskID, JobID: decision.JobID, ActorID: actorID, ActorRole: core.ActorAgent, Action: core.InterventionRedirect, ReasonCode: aggregate.ReasonCode, Comment: aggregate.Feedback, At: now}
			if _, err = q.InsertIntervention(ctx, interventionInsertParams(intervention)); err != nil {
				return err
			}
			if err = insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "intervention.redirect", ActorID: actorID, ActorRole: core.ActorAgent, At: now, Payload: core.JSONPayload(map[string]any{"reason_code": aggregate.ReasonCode, "comment": aggregate.Feedback, "review_round": decision.ReviewRound, "reviews": aggregate.Reviews})}); err != nil {
				return err
			}
			if err = insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "pipeline.bounced", Payload: core.JSONPayload(map[string]any{"from": "review", "to": "implement", "reason_code": aggregate.ReasonCode, "feedback": aggregate.Feedback, "reviews": aggregate.Reviews, "count": count, "source": "mcp-review-panel", "review_round": decision.ReviewRound})}); err != nil {
				return err
			}
			if int(window) < decision.MaxBounces {
				state, next, recovery = core.TaskQueued, core.StageImplement, ""
			} else if err := insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"count": count, "window": window, "max_bounces": decision.MaxBounces, "review_round": decision.ReviewRound})}); err != nil {
				return err
			}
		} else if (decision.PolicyVersion > 0 && !decision.MergeApproval) || (decision.PolicyVersion == 0 && decision.Level == core.L0) {
			state, recovery = core.TaskApproved, ""
		}

		if _, err := q.UpdateTaskTransition(ctx, db.UpdateTaskTransitionParams{
			ID: decision.TaskID, WorkspaceID: workspace(ctx), State: string(state),
			NextStage: string(next), RecoveryStage: string(recovery),
		}); err != nil {
			return err
		}
		if err := insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": before.State, "to": state})}); err != nil {
			return err
		}
		if err := insertEvent(ctx, q, core.Event{TaskID: decision.TaskID, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{
			"from_stage": before.NextStage, "next_stage": next, "recovery_stage": recovery, "state": state,
			"review_round": decision.ReviewRound,
		})}); err != nil {
			return err
		}
		if state == core.TaskQueued {
			if _, err := s.enqueueTaskTx(ctx, tx, decision.TaskID, before.WorkspaceID); err != nil {
				return err
			}
		}
		return nil
	})
}

type completedReviewRecord struct {
	ReviewWorkOrderID string `json:"review_work_order_id"`
	Verdict           string `json:"verdict"`
	ReasonCode        string `json:"reason_code"`
	Summary           string `json:"summary"`
	Feedback          string `json:"feedback"`
	ReviewerModel     string `json:"reviewer_model"`
	ReviewRound       int    `json:"review_round"`
	ReviewSeat        int    `json:"review_seat"`
	RequiredModel     string `json:"required_model"`
	RequiredHarness   string `json:"required_harness"`
	RequiredEffort    string `json:"required_effort"`
	ModelEnforcement  string `json:"model_enforcement"`
}

type reviewRoundResultRecord struct {
	ReviewRound int                     `json:"review_round"`
	Verdict     string                  `json:"verdict"`
	ReasonCode  string                  `json:"reason_code"`
	Summary     string                  `json:"summary"`
	Feedback    string                  `json:"feedback,omitempty"`
	Reviews     []completedReviewRecord `json:"reviews"`
}

func completedReviewRoundTx(ctx context.Context, tx pgx.Tx, workspaceID, taskID string, round int, workOrderID string) ([]completedReviewRecord, int, error) {
	required := 1
	if round > 0 {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM work_orders WHERE workspace_id=$1 AND task_id=$2 AND stage='review' AND review_round=$3`, workspaceID, taskID, round).Scan(&required); err != nil {
			return nil, 0, err
		}
	}
	query := `SELECT e.payload_json FROM events e JOIN tasks t ON t.id=e.task_id WHERE t.workspace_id=$1 AND e.task_id=$2 AND e.kind='review.completed' AND COALESCE((e.payload_json->>'review_round')::integer,0)=$3`
	args := []any{workspaceID, taskID, round}
	if round == 0 {
		query += ` AND e.payload_json->>'review_work_order_id'=$4`
		args = append(args, workOrderID)
	}
	query += ` ORDER BY COALESCE((e.payload_json->>'review_seat')::integer,0), e.id`
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var reviews []completedReviewRecord
	for rows.Next() {
		var payload []byte
		if err = rows.Scan(&payload); err != nil {
			return nil, 0, err
		}
		var review completedReviewRecord
		if err = json.Unmarshal(payload, &review); err != nil {
			return nil, 0, err
		}
		reviews = append(reviews, review)
	}
	return reviews, required, rows.Err()
}

func aggregateReviewRoundResult(round int, reviews []completedReviewRecord) reviewRoundResultRecord {
	sort.Slice(reviews, func(i, j int) bool { return reviews[i].ReviewSeat < reviews[j].ReviewSeat })
	if round == 0 && len(reviews) == 1 {
		review := reviews[0]
		return reviewRoundResultRecord{ReviewRound: round, Verdict: review.Verdict, ReasonCode: review.ReasonCode, Summary: review.Summary, Feedback: review.Feedback, Reviews: reviews}
	}
	result := reviewRoundResultRecord{ReviewRound: round, Verdict: "approve", ReasonCode: "approved", Summary: "All review panel seats approved.", Reviews: reviews}
	var feedback []string
	for _, review := range reviews {
		if review.Verdict == "changes_requested" {
			result.Verdict, result.ReasonCode, result.Summary = "changes_requested", "panel_changes_requested", "The review panel requested changes."
		}
		if strings.TrimSpace(review.Feedback) != "" {
			feedback = append(feedback, fmt.Sprintf("Seat %d (%s, %s): %s", review.ReviewSeat, review.RequiredModel, review.ModelEnforcement, strings.TrimSpace(review.Feedback)))
		}
	}
	result.Feedback = strings.Join(feedback, "\n")
	return result
}

func (s *Store) GetReviewPublication(ctx context.Context, id string) (core.ReviewPublication, error) {
	publication, err := scanReviewPublication(s.pool.QueryRow(ctx, "SELECT "+reviewPublicationColumns+" FROM review_publications WHERE workspace_id=$1 AND review_work_order_id=$2", workspace(ctx), id))
	if err != nil {
		return core.ReviewPublication{}, notFound(err, "review publication %s", id)
	}
	return publication, nil
}

func (s *Store) UpdateReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		command, err := tx.Exec(ctx, `UPDATE review_publications SET state=$1, attempts=$2,
			check_run_id=$3, comment_id=$4, reviewed_commit_sha=$5, last_error=$6,
			updated_at=now() WHERE workspace_id=$7 AND review_work_order_id=$8`,
			publication.State, publication.Attempts, publication.CheckRunID,
			publication.CommentID, publication.ReviewedCommitSHA, publication.LastError,
			workspace(ctx), publication.ReviewWorkOrderID)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return fmt.Errorf("review publication %s not found", publication.ReviewWorkOrderID)
		}
		kind := "review.publication_retry"
		if publication.State == core.ReviewPublicationPublished {
			kind = "review.publication_published"
		} else if publication.State == core.ReviewPublicationFailed {
			kind = "review.publication_failed"
		}
		return insertEvent(ctx, q, core.Event{TaskID: publication.TaskID, JobID: publication.JobID, Kind: kind, Payload: core.JSONPayload(publication)})
	})
}

func (s *Store) ReconcileReviewPublications(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT e.task_id, COALESCE(e.job_id,''), e.payload_json
		FROM events e
		JOIN tasks t ON t.id=e.task_id
		LEFT JOIN review_publications p ON p.workspace_id=t.workspace_id
			AND p.review_work_order_id=e.payload_json->>'review_work_order_id'
		WHERE t.workspace_id=$1 AND e.kind='review.completed'
			AND COALESCE(e.payload_json->>'review_work_order_id','') <> ''
			AND e.payload_json @> '{"publication_eligible": true}'::jsonb
			AND p.review_work_order_id IS NULL
		ORDER BY e.id`, workspace(ctx))
	if err != nil {
		return 0, err
	}
	var missing []core.ReviewPublication
	for rows.Next() {
		var taskID, jobID string
		var payload []byte
		if err = rows.Scan(&taskID, &jobID, &payload); err != nil {
			rows.Close()
			return 0, err
		}
		var publication core.ReviewPublication
		if json.Unmarshal(payload, &publication) == nil && publication.ReviewWorkOrderID != "" {
			publication.TaskID, publication.JobID = taskID, jobID
			publication.State = core.ReviewPublicationQueued
			missing = append(missing, publication)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	created := 0
	for _, publication := range missing {
		if err = s.QueueReviewPublication(ctx, publication); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

func reviewDecisionPayload(decision core.ReviewDecision) []byte {
	return core.JSONPayload(map[string]any{
		"review_work_order_id": decision.ReviewWorkOrderID, "verdict": decision.Verdict,
		"reason_code": decision.ReasonCode, "summary": decision.Summary, "feedback": decision.Feedback,
		"reviewed_commit_sha": decision.ReviewedCommitSHA, "reviewer": decision.Reviewer,
		"reviewer_model": decision.ReviewerModel, "reviewer_session": decision.ReviewerSession,
		"same_model_as_implementer": decision.SameModelAsImplementer,
		"review_round":              decision.ReviewRound, "review_seat": decision.ReviewSeat,
		"required_model": decision.RequiredModel, "required_harness": decision.RequiredHarness,
		"required_effort":      decision.RequiredEffort,
		"model_enforcement":    decision.ModelEnforcement,
		"publication_eligible": decision.PublicationEligible,
	})
}

func reviewPublicationFromDecision(decision core.ReviewDecision) core.ReviewPublication {
	return core.ReviewPublication{
		ReviewWorkOrderID: decision.ReviewWorkOrderID, TaskID: decision.TaskID, JobID: decision.JobID,
		Verdict: decision.Verdict, ReasonCode: decision.ReasonCode, Summary: decision.Summary,
		Feedback: decision.Feedback, ReviewedCommitSHA: decision.ReviewedCommitSHA,
		ReviewerModel: decision.ReviewerModel, ReviewerSession: decision.ReviewerSession,
		SameModelAsImplementer: decision.SameModelAsImplementer,
		ReviewRound:            decision.ReviewRound, ReviewSeat: decision.ReviewSeat,
		RequiredModel: decision.RequiredModel, RequiredHarness: decision.RequiredHarness, RequiredEffort: decision.RequiredEffort,
		ModelEnforcement: decision.ModelEnforcement,
	}
}

func scanReviewPublication(row interface{ Scan(...any) error }) (core.ReviewPublication, error) {
	var publication core.ReviewPublication
	var state string
	err := row.Scan(&publication.ReviewWorkOrderID, &publication.TaskID, &publication.JobID,
		&publication.Verdict, &publication.ReasonCode, &publication.Summary,
		&publication.Feedback, &publication.ReviewedCommitSHA, &publication.ReviewerModel,
		&publication.ReviewerSession, &publication.SameModelAsImplementer,
		&publication.ReviewRound, &publication.ReviewSeat, &publication.RequiredModel,
		&publication.RequiredHarness, &publication.RequiredEffort, &publication.ModelEnforcement, &state,
		&publication.Attempts, &publication.CheckRunID, &publication.CommentID,
		&publication.LastError, &publication.CreatedAt, &publication.UpdatedAt)
	publication.State = core.ReviewPublicationState(state)
	return publication, err
}

func scanWorkOrder(row interface{ Scan(...any) error }) (core.WorkOrder, error) {
	var order core.WorkOrder
	var stage, state string
	var harnessConfig []byte
	var lease, queueEntered, queueDeadline, executionStarted, executionDeadline, lastFailureAt, nextRetryAt pgtype.Timestamptz
	err := row.Scan(&order.ID, &order.TaskID, &order.JobID, &stage, &state, &order.ClaimantID,
		&order.SessionID, &order.ClientTokenHash, &order.Agent, &order.Model, &order.WorkerID, &lease,
		&order.ReviewRound, &order.ReviewSeat, &order.RequiredModel, &order.RequiredHarness, &harnessConfig, &order.ExecutionTimeoutText, &order.ModelEnforcement,
		&queueEntered, &queueDeadline, &executionStarted, &executionDeadline,
		&order.LastAttemptOutcome, &order.LastFailureMessage, &order.LastFailureExitStatus, &lastFailureAt,
		&order.AutomaticRetryCount, &nextRetryAt, &order.RetrySuppressed,
		&order.RedispatchCount, &order.Progress, &order.CostUSD, &order.TokensIn,
		&order.TokensOut, &order.SelfReported, &order.CreatedAt, &order.UpdatedAt)
	order.Stage, order.State = core.Stage(stage), core.WorkOrderState(state)
	if len(harnessConfig) > 0 && string(harnessConfig) != "{}" {
		var snapshot core.HarnessSnapshot
		if err == nil {
			err = json.Unmarshal(harnessConfig, &snapshot)
		}
		if err == nil && snapshot.Name != "" {
			order.RequiredHarnessConfig = &snapshot
			order.RequiredEffort = snapshot.Effort
		}
	}
	if lease.Valid {
		order.LeaseExpiresAt = lease.Time
	}
	if queueEntered.Valid {
		order.QueueEnteredAt = queueEntered.Time
	}
	if queueDeadline.Valid {
		order.QueueDeadline = queueDeadline.Time
	}
	if executionStarted.Valid {
		order.ExecutionStartedAt = executionStarted.Time
	}
	if executionDeadline.Valid {
		order.ExecutionDeadline = executionDeadline.Time
	}
	if lastFailureAt.Valid {
		order.LastFailureAt = lastFailureAt.Time
	}
	if nextRetryAt.Valid {
		order.NextRetryAt = nextRetryAt.Time
	}
	order.Claimable = order.ClaimableAt(time.Now().UTC())
	return order, err
}

func harnessSnapshotJSON(snapshot *core.HarnessSnapshot) []byte {
	if snapshot == nil {
		return []byte("{}")
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func (s *Store) CreateFeature(ctx context.Context, feature core.Feature) error {
	if feature.CreatedAt.IsZero() {
		feature.CreatedAt = time.Now().UTC()
	}
	if feature.ParentID != "" {
		var belongs bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM features WHERE id=$1 AND workspace_id=$2)`, feature.ParentID, workspace(ctx)).Scan(&belongs); err != nil {
			return err
		}
		if !belongs {
			return fmt.Errorf("parent feature %s not found in workspace %s", feature.ParentID, workspace(ctx))
		}
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO features (id,workspace_id,parent_id,name,description,created_at) VALUES ($1,$2,NULLIF($3,''),$4,$5,$6)`, feature.ID, workspace(ctx), feature.ParentID, feature.Name, feature.Description, feature.CreatedAt)
	return err
}

func (s *Store) ListFeatures(ctx context.Context) ([]core.Feature, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,workspace_id,COALESCE(parent_id,''),name,description,created_at FROM features WHERE workspace_id=$1 ORDER BY name,id`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.Feature
	for rows.Next() {
		var feature core.Feature
		if err := rows.Scan(&feature.ID, &feature.Workspace, &feature.ParentID, &feature.Name, &feature.Description, &feature.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, feature)
	}
	return result, rows.Err()
}

func (s *Store) AssignTaskFeature(ctx context.Context, taskID, featureID string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if featureID != "" {
			var belongs bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM features WHERE id=$1 AND workspace_id=$2)`, featureID, workspace(ctx)).Scan(&belongs); err != nil {
				return err
			}
			if !belongs {
				return fmt.Errorf("feature %s not found in workspace %s", featureID, workspace(ctx))
			}
		}
		command, err := tx.Exec(ctx, `UPDATE tasks SET feature_id=NULLIF($1,''),updated_at=now() WHERE id=$2 AND workspace_id=$3`, featureID, taskID, workspace(ctx))
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return fmt.Errorf("task %s not found", taskID)
		}
		return insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "task.feature_assigned", Payload: core.JSONPayload(map[string]string{"feature_id": featureID})})
	})
}

func (s *Store) CreateArtifact(ctx context.Context, artifact core.Artifact, content []byte) (core.Artifact, error) {
	artifact.ID = fmt.Sprintf("%x", sha256.Sum256(content))
	artifact.SizeBytes = int64(len(content))
	if artifact.CreatedAt.IsZero() {
		artifact.CreatedAt = time.Now().UTC()
	}
	artifact.Workspace = workspace(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.Artifact{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `INSERT INTO artifacts (id,workspace_id,name,content_type,size_bytes,content,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(workspace_id,id) DO NOTHING`, artifact.ID, workspace(ctx), artifact.Name, artifact.ContentType, artifact.SizeBytes, content, artifact.CreatedAt)
	if err != nil {
		return core.Artifact{}, err
	}
	if artifact.TaskID != "" || artifact.FeatureID != "" {
		var belongs bool
		if artifact.TaskID != "" {
			err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id=$1 AND workspace_id=$2)`, artifact.TaskID, workspace(ctx)).Scan(&belongs)
		} else {
			err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM features WHERE id=$1 AND workspace_id=$2)`, artifact.FeatureID, workspace(ctx)).Scan(&belongs)
		}
		if err != nil {
			return core.Artifact{}, err
		}
		if !belongs {
			return core.Artifact{}, fmt.Errorf("artifact attachment does not belong to workspace %s", workspace(ctx))
		}
		_, err = tx.Exec(ctx, `INSERT INTO artifact_links (workspace_id,artifact_id,task_id,feature_id) VALUES ($1,$2,NULLIF($3,''),NULLIF($4,'')) ON CONFLICT DO NOTHING`, workspace(ctx), artifact.ID, artifact.TaskID, artifact.FeatureID)
		if err != nil {
			return core.Artifact{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return core.Artifact{}, err
	}
	return artifact, nil
}

func (s *Store) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	var artifact core.Artifact
	var content []byte
	err := s.pool.QueryRow(ctx, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.content,a.created_at,COALESCE(l.task_id,''),COALESCE(l.feature_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id WHERE a.workspace_id=$1 AND a.id=$2 LIMIT 1`, workspace(ctx), id).Scan(&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &content, &artifact.CreatedAt, &artifact.TaskID, &artifact.FeatureID)
	if err != nil {
		return core.Artifact{}, nil, notFound(err, "artifact %s", id)
	}
	return artifact, content, nil
}

func (s *Store) GetArtifactForContext(ctx context.Context, id, taskID, featureID string) (core.Artifact, []byte, error) {
	var artifact core.Artifact
	var content []byte
	err := s.pool.QueryRow(ctx, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.content,a.created_at,COALESCE(l.task_id,''),COALESCE(l.feature_id,'')
		FROM artifacts a
		JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id
		WHERE a.workspace_id=$1 AND a.id=$2
		  AND (($3 <> '' AND l.task_id=$3) OR ($4 <> '' AND l.feature_id=$4))
		LIMIT 1`, workspace(ctx), id, taskID, featureID).Scan(&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &content, &artifact.CreatedAt, &artifact.TaskID, &artifact.FeatureID)
	if err != nil {
		return core.Artifact{}, nil, notFound(err, "artifact %s", id)
	}
	return artifact, content, nil
}

func (s *Store) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	rows, err := s.pool.Query(ctx, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.task_id,''),COALESCE(l.feature_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id WHERE a.workspace_id=$1 ORDER BY a.created_at,a.id`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.Artifact
	for rows.Next() {
		var artifact core.Artifact
		if err := rows.Scan(&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &artifact.CreatedAt, &artifact.TaskID, &artifact.FeatureID); err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	return result, rows.Err()
}

func nullableTimeValue(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx, *db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := fn(tx, s.queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) enqueueTaskTx(ctx context.Context, tx pgx.Tx, taskID, workspace string) (bool, error) {
	result, err := s.river.InsertTx(ctx, tx, queueargs.DispatchTaskArgs{WorkspaceID: workspace, TaskID: taskID}, &river.InsertOpts{
		MaxAttempts: 3,
		Queue:       queueargs.DispatchQueue(workspace),
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			// Suppress duplicate work only while a dispatch is active or may
			// retry. River's default also includes completed jobs, which makes
			// an intentional human redispatch a silent no-op until job cleanup.
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateRetryable,
				rivertype.JobStateScheduled,
			},
		},
	})
	if err != nil {
		return false, fmt.Errorf("enqueue task %s: %w", taskID, err)
	}
	return !result.UniqueSkippedAsDuplicate, nil
}

func insertEvent(ctx context.Context, q *db.Queries, event core.Event) error {
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
	_, err := q.InsertEvent(ctx, db.InsertEventParams{
		TaskID: nullableText(event.TaskID), JobID: nullableText(event.JobID), Kind: event.Kind,
		ActorID: event.ActorID, ActorRole: string(event.ActorRole),
		PayloadJson: event.Payload, At: timestamp(event.At),
	})
	return err
}

func taskInsertParams(task core.Task) db.InsertTaskParams {
	return db.InsertTaskParams{
		ID: task.ID, WorkspaceID: task.Workspace, Source: task.Source,
		Title: task.Title, Body: task.Body, Class: task.Class,
		EscalationLevel: string(task.Level), RepoName: task.Repo,
		Mode: string(task.Mode), SpecApproval: task.SpecApproval, MergeApproval: task.MergeApproval,
		PolicyVersion: int32(task.PolicyVersion),
		BaseBranch:    task.BaseBranch, Branch: task.Branch, State: string(task.State),
		NextStage: string(task.NextStage), RecoveryStage: string(task.RecoveryStage), ParentTaskID: task.ParentTaskID, FeatureID: nullableText(task.FeatureID), IntakeKey: nullableText(task.IntakeKey), CreatedAt: timestamp(task.CreatedAt),
	}
}

func taskFromDB(task db.Task) core.Task {
	return core.Task{
		ID: task.ID, Workspace: task.WorkspaceID, Source: task.Source, IntakeKey: task.IntakeKey.String,
		Title: task.Title, Body: task.Body, Class: task.Class,
		Level: core.EscalationLevel(task.EscalationLevel), Repo: task.RepoName,
		Mode: core.TaskMode(task.Mode), SpecApproval: task.SpecApproval, MergeApproval: task.MergeApproval,
		PolicyVersion: int(task.PolicyVersion),
		BaseBranch:    task.BaseBranch, Branch: task.Branch,
		State: core.TaskState(task.State), NextStage: core.Stage(task.NextStage), RecoveryStage: core.Stage(task.RecoveryStage), ParentTaskID: task.ParentTaskID, FeatureID: task.FeatureID.String,
		CreatedAt: task.CreatedAt.Time,
	}
}

func specFromDB(spec db.TaskSpec) core.SpecVersion {
	return core.SpecVersion{
		TaskID: spec.TaskID, Version: int(spec.Version), Content: spec.Content,
		AcceptanceCount: int(spec.AcceptanceCount), Acceptance: append([]byte(nil), spec.Acceptance...),
		Decomposition: append([]byte(nil), spec.Decomposition...), Approved: spec.Approved,
		CreatedAt: spec.CreatedAt.Time, ApprovedAt: nullableTime(spec.ApprovedAt),
	}
}

func jobInsertParams(job core.Job) db.InsertJobParams {
	return db.InsertJobParams{
		ID: job.ID, TaskID: job.TaskID, Stage: string(job.Stage), Harness: job.Harness,
		ModelTier: job.ModelTier, AuthMode: job.AuthMode, Runner: job.Runner,
		PackVersion: job.PackVersion, ConfinementTier: job.Confinement,
		CostUsd: job.CostUSD, TokensIn: job.TokensIn,
		TokensOut: job.TokensOut, State: string(job.State),
		StartedAt: nullableTimestamp(job.StartedAt), EndedAt: nullableTimestamp(job.EndedAt),
	}
}

func jobUpdateParams(job core.Job, workspace string) db.UpdateJobParams {
	return db.UpdateJobParams{
		ID: job.ID, Stage: string(job.Stage), Harness: job.Harness,
		ModelTier: job.ModelTier, AuthMode: job.AuthMode, Runner: job.Runner,
		PackVersion: job.PackVersion, ConfinementTier: job.Confinement,
		CostUsd: job.CostUSD, TokensIn: job.TokensIn,
		TokensOut: job.TokensOut, State: string(job.State),
		StartedAt: nullableTimestamp(job.StartedAt), EndedAt: nullableTimestamp(job.EndedAt),
		WorkspaceID: workspace,
	}
}

func jobFromDB(job db.Job) core.Job {
	return core.Job{
		ID: job.ID, TaskID: job.TaskID, Stage: core.Stage(job.Stage), Harness: job.Harness,
		ModelTier: job.ModelTier, AuthMode: job.AuthMode, Runner: job.Runner,
		PackVersion: job.PackVersion, Confinement: job.ConfinementTier,
		CostUSD: job.CostUsd, TokensIn: job.TokensIn,
		TokensOut: job.TokensOut, State: core.JobState(job.State),
		StartedAt: job.StartedAt.Time, EndedAt: nullableTime(job.EndedAt),
	}
}

func eventFromDB(event db.Event) core.Event {
	return core.Event{
		ID: event.ID, TaskID: event.TaskID.String, JobID: event.JobID.String,
		Kind: event.Kind, ActorID: event.ActorID, ActorRole: core.ActorRole(event.ActorRole),
		Payload: append(json.RawMessage(nil), event.PayloadJson...), At: event.At.Time,
	}
}

func interventionInsertParams(intervention core.Intervention) db.InsertInterventionParams {
	return db.InsertInterventionParams{
		TaskID: intervention.TaskID, JobID: nullableText(intervention.JobID),
		ActorID: intervention.ActorID, ActorRole: string(intervention.ActorRole),
		Action: string(intervention.Action), ReasonCode: intervention.ReasonCode,
		Comment: intervention.Comment, At: timestamp(intervention.At),
	}
}

func interventionFromDB(intervention db.Intervention) core.Intervention {
	return core.Intervention{
		ID: intervention.ID, TaskID: intervention.TaskID, JobID: intervention.JobID.String,
		ActorID: intervention.ActorID, ActorRole: core.ActorRole(intervention.ActorRole),
		Action: core.InterventionAction(intervention.Action), ReasonCode: intervention.ReasonCode,
		Comment: intervention.Comment, At: intervention.At.Time,
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
		return fmt.Errorf(format+" not found", args...)
	}
	return err
}

var _ store.Store = (*Store)(nil)
