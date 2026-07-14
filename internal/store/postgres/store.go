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
	pool      *pgxpool.Pool
	queries   *db.Queries
	river     *river.Client[pgx.Tx]
	workspace string
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
	s.workspace = cfg.Workspace
	return seeded, nil
}

func upsertRepo(ctx context.Context, q *db.Queries, workspace string, repo config.Repo) error {
	return q.UpsertRepo(ctx, db.UpsertRepoParams{
		WorkspaceID: workspace, Name: repo.Name, Url: repo.URL,
		GithubSlug: repo.GitHub, DefaultBase: repo.Base,
	})
}

func (s *Store) WorkspaceConfig(ctx context.Context) (config.VersionedDocument, error) {
	row, err := s.queries.GetWorkspaceConfig(ctx, s.workspace)
	if err != nil {
		return config.VersionedDocument{}, notFound(err, "workspace %s", s.workspace)
	}
	var document config.WorkspaceDocument
	decoder := yaml.NewDecoder(strings.NewReader(row.ConfigYaml))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return config.VersionedDocument{}, fmt.Errorf("decode stored workspace config: %w", err)
	}
	return config.VersionedDocument{Document: document, Version: row.ConfigVersion}, nil
}

// RuntimeConfig overlays the latest database document onto immutable
// deployment settings. Callers take one value per dispatch so running jobs do
// not observe mid-flight policy changes (spec §14.1, §21.3).
func (s *Store) RuntimeConfig(ctx context.Context, deployment *config.Config) (*config.Config, error) {
	row, err := s.queries.GetWorkspaceConfig(ctx, deployment.Workspace)
	if err != nil {
		return nil, notFound(err, "workspace %s", deployment.Workspace)
	}
	cfg, _, err := config.ParseStoredWorkspaceDocument([]byte(row.ConfigYaml), deployment, "database workspace config")
	return cfg, err
}

func (s *Store) UpdateWorkspaceConfig(ctx context.Context, expectedVersion int64, next *config.Config) (config.UpdateReceipt, error) {
	data, err := config.MarshalWorkspaceDocument(next)
	if err != nil {
		return config.UpdateReceipt{}, err
	}
	var result config.UpdateReceipt
	err = s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		before, err := q.GetWorkspaceConfig(ctx, s.workspace)
		if err != nil {
			return err
		}
		updated, err := q.UpdateWorkspaceConfig(ctx, db.UpdateWorkspaceConfigParams{
			ID: s.workspace, ExpectedVersion: expectedVersion, ConfigYaml: string(data),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return config.ErrVersionConflict
		}
		if err != nil {
			return err
		}
		for _, repo := range next.Repos {
			if err := upsertRepo(ctx, q, s.workspace, repo); err != nil {
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
			WorkspaceID: s.workspace, Kind: "config.updated", ActorID: actor.ID,
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
	sections := make([]string, 0, 3)
	if before.Workspace != after.Workspace || before.MaxBounces != after.MaxBounces {
		sections = append(sections, "workspace")
	}
	if !reflect.DeepEqual(before.Routing, after.Routing) {
		sections = append(sections, "routing")
	}
	if !reflect.DeepEqual(before.Repos, after.Repos) {
		sections = append(sections, "repos")
	}
	return sections
}

func (s *Store) CreateTask(ctx context.Context, task core.Task) error {
	if task.Workspace != s.workspace {
		return fmt.Errorf("task workspace %q does not match store workspace %q", task.Workspace, s.workspace)
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC()
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
	task, err := s.queries.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: s.workspace})
	if err != nil {
		return core.Task{}, notFound(err, "task %s", id)
	}
	return taskFromDB(task), nil
}

func (s *Store) GetTaskByIntakeKey(ctx context.Context, key string) (core.Task, bool, error) {
	task, err := s.queries.GetTaskByIntakeKey(ctx, db.GetTaskByIntakeKeyParams{WorkspaceID: s.workspace, IntakeKey: nullableText(key)})
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Task{}, false, nil
	}
	if err != nil {
		return core.Task{}, false, err
	}
	return taskFromDB(task), true, nil
}

func (s *Store) ListTasks(ctx context.Context) ([]core.Task, error) {
	rows, err := s.queries.ListTasks(ctx, s.workspace)
	if err != nil {
		return nil, err
	}
	result := make([]core.Task, len(rows))
	for i := range rows {
		result[i] = taskFromDB(rows[i])
	}
	return result, nil
}

func (s *Store) UpdateTaskState(ctx context.Context, id string, state core.TaskState) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		before, err := q.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: s.workspace})
		if err != nil {
			return notFound(err, "task %s", id)
		}
		if _, err := q.UpdateTaskState(ctx, db.UpdateTaskStateParams{ID: id, WorkspaceID: s.workspace, State: string(state)}); err != nil {
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
		before, err := q.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: s.workspace})
		if err != nil {
			return notFound(err, "task %s", id)
		}
		if _, err := q.UpdateTaskTransition(ctx, db.UpdateTaskTransitionParams{
			ID: id, WorkspaceID: s.workspace, State: string(state), NextStage: string(nextStage), RecoveryStage: string(recoveryStage),
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
		if _, err := q.UpdateTaskClassification(ctx, db.UpdateTaskClassificationParams{ID: id, WorkspaceID: s.workspace, Class: class}); err != nil {
			return notFound(err, "task %s", id)
		}
		return insertEvent(ctx, q, core.Event{TaskID: id, Kind: "task.classified", Payload: core.JSONPayload(map[string]any{"class": class})})
	})
}

func (s *Store) EnsureTaskEnqueued(ctx context.Context, id string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		task, err := q.GetTask(ctx, db.GetTaskParams{ID: id, WorkspaceID: s.workspace})
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
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "conveyor:queue-reconcile:"+s.workspace); err != nil {
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
ORDER BY t.created_at, t.id`, s.workspace)
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
			inserted, err := s.enqueueTaskTx(ctx, tx, taskID, s.workspace)
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
	if job.StartedAt.IsZero() {
		job.StartedAt = time.Now().UTC()
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		if _, err := q.GetTask(ctx, db.GetTaskParams{ID: job.TaskID, WorkspaceID: s.workspace}); err != nil {
			return notFound(err, "task %s", job.TaskID)
		}
		if _, err := q.InsertJob(ctx, jobInsertParams(job)); err != nil {
			return err
		}
		return insertEvent(ctx, q, core.Event{
			TaskID: job.TaskID, JobID: job.ID, Kind: "job.created",
			Payload: core.JSONPayload(job), At: job.StartedAt,
		})
	})
}

func (s *Store) UpdateJob(ctx context.Context, job core.Job) error {
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		row, err := q.UpdateJob(ctx, jobUpdateParams(job, s.workspace))
		if err != nil {
			return notFound(err, "job %s", job.ID)
		}
		return insertEvent(ctx, q, core.Event{
			TaskID: row.TaskID, JobID: row.ID, Kind: "job.updated", Payload: core.JSONPayload(job),
		})
	})
}

func (s *Store) ListJobs(ctx context.Context, taskID string) ([]core.Job, error) {
	rows, err := s.queries.ListJobs(ctx, db.ListJobsParams{TaskID: taskID, WorkspaceID: s.workspace})
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
	row, err := s.queries.GetLatestJob(ctx, db.GetLatestJobParams{TaskID: taskID, WorkspaceID: s.workspace})
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
		if _, err := q.GetTask(ctx, db.GetTaskParams{ID: spec.TaskID, WorkspaceID: s.workspace}); err != nil {
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
	row, err := s.queries.GetLatestSpecVersion(ctx, db.GetLatestSpecVersionParams{TaskID: taskID, WorkspaceID: s.workspace})
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
		_, err := q.ApproveLatestSpecVersion(ctx, db.ApproveLatestSpecVersionParams{TaskID: taskID, Version: int32(version), WorkspaceID: s.workspace})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("spec version %d for task %s not found or superseded", version, taskID)
			}
			return err
		}
		return insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "spec.version_approved", Payload: core.JSONPayload(map[string]int{"version": version})})
	})
}

func (s *Store) AppendEvent(ctx context.Context, event core.Event) error {
	task, err := s.queries.GetTask(ctx, db.GetTaskParams{ID: event.TaskID, WorkspaceID: s.workspace})
	if err != nil {
		return notFound(err, "task %s", event.TaskID)
	}
	if event.JobID != "" {
		job, err := s.queries.GetJob(ctx, db.GetJobParams{ID: event.JobID, WorkspaceID: s.workspace})
		if err != nil || job.TaskID != task.ID {
			return fmt.Errorf("job %s does not belong to task %s in workspace %s", event.JobID, event.TaskID, s.workspace)
		}
	}
	return insertEvent(ctx, s.queries, event)
}

func (s *Store) ListEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	rows, err := s.queries.ListEvents(ctx, db.ListEventsParams{TaskID: nullableText(taskID), WorkspaceID: s.workspace})
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
		TaskID: nullableText(taskID), WorkspaceID: s.workspace, ID: afterID,
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

func (s *Store) CountEvents(ctx context.Context, taskID, kind string) (int, error) {
	count, err := s.queries.CountEvents(ctx, db.CountEventsParams{TaskID: nullableText(taskID), Kind: kind, WorkspaceID: s.workspace})
	return int(count), err
}

func (s *Store) ListActivityMarkers(ctx context.Context) ([]store.ActivityMarker, error) {
	rows, err := s.queries.ListActivityMarkers(ctx, s.workspace)
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
		_, err := q.GetTask(ctx, db.GetTaskParams{ID: intervention.TaskID, WorkspaceID: s.workspace})
		if err != nil {
			return notFound(err, "task %s", intervention.TaskID)
		}
		if intervention.JobID != "" {
			job, err := q.GetJob(ctx, db.GetJobParams{ID: intervention.JobID, WorkspaceID: s.workspace})
			if err != nil || job.TaskID != intervention.TaskID {
				return fmt.Errorf("job %s does not belong to task %s in workspace %s", intervention.JobID, intervention.TaskID, s.workspace)
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
	rows, err := s.queries.ListInterventions(ctx, db.ListInterventionsParams{TaskID: taskID, WorkspaceID: s.workspace})
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
		job, err := q.GetJob(ctx, db.GetJobParams{ID: transcript.JobID, WorkspaceID: s.workspace})
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
	row, err := s.queries.GetTranscript(ctx, db.GetTranscriptParams{JobID: jobID, WorkspaceID: s.workspace})
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
session_id, client_token_hash, agent, model, lease_expires_at, progress,
cost_usd, tokens_in, tokens_out, self_reported, created_at, updated_at`

func (s *Store) CreateWorkOrder(ctx context.Context, order core.WorkOrder) error {
	if order.CreatedAt.IsZero() {
		order.CreatedAt = time.Now().UTC()
	}
	if order.State == "" {
		order.State = core.WorkOrderQueued
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		_, err := tx.Exec(ctx, `INSERT INTO work_orders (
			id, workspace_id, task_id, job_id, stage, state, claimant_id,
			session_id, client_token_hash, agent, model, lease_expires_at,
			progress, cost_usd, tokens_in, tokens_out, self_reported, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$18)`,
			order.ID, s.workspace, order.TaskID, order.JobID, order.Stage, order.State,
			order.ClaimantID, order.SessionID, order.ClientTokenHash, order.Agent, order.Model,
			nullableTimeValue(order.LeaseExpiresAt), order.Progress, order.CostUSD,
			order.TokensIn, order.TokensOut, true, order.CreatedAt)
		if err != nil {
			return err
		}
		return insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.created", Payload: core.JSONPayload(order)})
	})
}

func (s *Store) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE work_orders SET state='queued', claimant_id='', session_id='', client_token_hash='', agent='', model='', lease_expires_at=NULL, updated_at=now()
		WHERE workspace_id=$1 AND id=$2 AND state='claimed' AND lease_expires_at <= now()`, s.workspace, id); err != nil {
		return core.WorkOrder{}, err
	}
	order, err := scanWorkOrder(s.pool.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2", s.workspace, id))
	if err != nil {
		return core.WorkOrder{}, notFound(err, "work order %s", id)
	}
	return order, nil
}

func (s *Store) ListWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE work_orders SET state='queued', claimant_id='', session_id='', client_token_hash='', agent='', model='', lease_expires_at=NULL, updated_at=now()
		WHERE workspace_id=$1 AND state='claimed' AND lease_expires_at <= now()`, s.workspace); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 ORDER BY created_at,id", s.workspace)
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
	rows, err := s.pool.Query(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND task_id=$2 ORDER BY created_at,id", s.workspace, taskID)
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
	order, err := scanWorkOrder(tx.QueryRow(ctx, "SELECT "+workOrderColumns+" FROM work_orders WHERE workspace_id=$1 AND id=$2 FOR UPDATE", s.workspace, id))
	if err != nil {
		return core.WorkOrder{}, notFound(err, "work order %s", id)
	}
	now := time.Now().UTC()
	if order.State == core.WorkOrderClaimed && order.LeaseExpiresAt.After(now) {
		return core.WorkOrder{}, fmt.Errorf("work order %s is already claimed", id)
	}
	if order.State != core.WorkOrderQueued && order.State != core.WorkOrderClaimed {
		return core.WorkOrder{}, fmt.Errorf("work order %s is not claimable", id)
	}
	hash := ""
	if claim.ClientToken != "" {
		hash = fmt.Sprintf("%x", sha256.Sum256([]byte(claim.ClientToken)))
	}
	if order.Stage == core.StageReview {
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_orders WHERE task_id=$1 AND stage='implement' AND
			(($2 <> '' AND session_id=$2) OR ($3 <> '' AND client_token_hash=$3)))`, order.TaskID, claim.SessionID, hash).Scan(&blocked); err != nil {
			return core.WorkOrder{}, err
		}
		if blocked {
			return core.WorkOrder{}, fmt.Errorf("self-review forbidden: use a fresh agent session")
		}
	}
	lease := claim.Lease
	if lease <= 0 {
		lease = 15 * time.Minute
	}
	expires := now.Add(lease)
	row := tx.QueryRow(ctx, "UPDATE work_orders SET state='claimed', claimant_id=$1, session_id=$2, client_token_hash=$3, agent=$4, model=$5, lease_expires_at=$6, updated_at=$7 WHERE id=$8 RETURNING "+workOrderColumns,
		claim.ClaimantID, claim.SessionID, hash, claim.Agent, claim.Model, expires, now, id)
	order, err = scanWorkOrder(row)
	if err != nil {
		return core.WorkOrder{}, err
	}
	q := s.queries.WithTx(tx)
	if err := insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.claimed", Payload: core.JSONPayload(order)}); err != nil {
		return core.WorkOrder{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	return order, nil
}

func (s *Store) UpdateWorkOrder(ctx context.Context, order core.WorkOrder) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		command, err := tx.Exec(ctx, `UPDATE work_orders SET state=$1, claimant_id=$2, session_id=$3,
			client_token_hash=$4, agent=$5, model=$6, lease_expires_at=$7, progress=$8,
			cost_usd=$9, tokens_in=$10, tokens_out=$11, self_reported=$12, updated_at=now()
			WHERE workspace_id=$13 AND id=$14`, order.State, order.ClaimantID, order.SessionID,
			order.ClientTokenHash, order.Agent, order.Model, nullableTimeValue(order.LeaseExpiresAt),
			order.Progress, order.CostUSD, order.TokensIn, order.TokensOut, order.SelfReported, s.workspace, order.ID)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return fmt.Errorf("work order %s not found", order.ID)
		}
		return insertEvent(ctx, q, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.updated", Payload: core.JSONPayload(order)})
	})
}

func scanWorkOrder(row interface{ Scan(...any) error }) (core.WorkOrder, error) {
	var order core.WorkOrder
	var stage, state string
	var lease pgtype.Timestamptz
	err := row.Scan(&order.ID, &order.TaskID, &order.JobID, &stage, &state, &order.ClaimantID,
		&order.SessionID, &order.ClientTokenHash, &order.Agent, &order.Model, &lease, &order.Progress,
		&order.CostUSD, &order.TokensIn, &order.TokensOut, &order.SelfReported, &order.CreatedAt, &order.UpdatedAt)
	order.Stage, order.State = core.Stage(stage), core.WorkOrderState(state)
	if lease.Valid {
		order.LeaseExpiresAt = lease.Time
	}
	return order, err
}

func (s *Store) CreateFeature(ctx context.Context, feature core.Feature) error {
	if feature.CreatedAt.IsZero() {
		feature.CreatedAt = time.Now().UTC()
	}
	if feature.ParentID != "" {
		var belongs bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM features WHERE id=$1 AND workspace_id=$2)`, feature.ParentID, s.workspace).Scan(&belongs); err != nil {
			return err
		}
		if !belongs {
			return fmt.Errorf("parent feature %s not found in workspace %s", feature.ParentID, s.workspace)
		}
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO features (id,workspace_id,parent_id,name,description,created_at) VALUES ($1,$2,NULLIF($3,''),$4,$5,$6)`, feature.ID, s.workspace, feature.ParentID, feature.Name, feature.Description, feature.CreatedAt)
	return err
}

func (s *Store) ListFeatures(ctx context.Context) ([]core.Feature, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,workspace_id,COALESCE(parent_id,''),name,description,created_at FROM features WHERE workspace_id=$1 ORDER BY name,id`, s.workspace)
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
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM features WHERE id=$1 AND workspace_id=$2)`, featureID, s.workspace).Scan(&belongs); err != nil {
				return err
			}
			if !belongs {
				return fmt.Errorf("feature %s not found in workspace %s", featureID, s.workspace)
			}
		}
		command, err := tx.Exec(ctx, `UPDATE tasks SET feature_id=NULLIF($1,''),updated_at=now() WHERE id=$2 AND workspace_id=$3`, featureID, taskID, s.workspace)
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
	artifact.Workspace = s.workspace
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.Artifact{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `INSERT INTO artifacts (id,workspace_id,name,content_type,size_bytes,content,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(workspace_id,id) DO NOTHING`, artifact.ID, s.workspace, artifact.Name, artifact.ContentType, artifact.SizeBytes, content, artifact.CreatedAt)
	if err != nil {
		return core.Artifact{}, err
	}
	if artifact.TaskID != "" || artifact.FeatureID != "" {
		var belongs bool
		if artifact.TaskID != "" {
			err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id=$1 AND workspace_id=$2)`, artifact.TaskID, s.workspace).Scan(&belongs)
		} else {
			err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM features WHERE id=$1 AND workspace_id=$2)`, artifact.FeatureID, s.workspace).Scan(&belongs)
		}
		if err != nil {
			return core.Artifact{}, err
		}
		if !belongs {
			return core.Artifact{}, fmt.Errorf("artifact attachment does not belong to workspace %s", s.workspace)
		}
		_, err = tx.Exec(ctx, `INSERT INTO artifact_links (workspace_id,artifact_id,task_id,feature_id) VALUES ($1,$2,NULLIF($3,''),NULLIF($4,'')) ON CONFLICT DO NOTHING`, s.workspace, artifact.ID, artifact.TaskID, artifact.FeatureID)
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
	err := s.pool.QueryRow(ctx, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.content,a.created_at,COALESCE(l.task_id,''),COALESCE(l.feature_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id WHERE a.workspace_id=$1 AND a.id=$2 LIMIT 1`, s.workspace, id).Scan(&artifact.ID, &artifact.Workspace, &artifact.Name, &artifact.ContentType, &artifact.SizeBytes, &content, &artifact.CreatedAt, &artifact.TaskID, &artifact.FeatureID)
	if err != nil {
		return core.Artifact{}, nil, notFound(err, "artifact %s", id)
	}
	return artifact, content, nil
}

func (s *Store) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	rows, err := s.pool.Query(ctx, `SELECT a.id,a.workspace_id,a.name,a.content_type,a.size_bytes,a.created_at,COALESCE(l.task_id,''),COALESCE(l.feature_id,'') FROM artifacts a LEFT JOIN artifact_links l ON l.workspace_id=a.workspace_id AND l.artifact_id=a.id WHERE a.workspace_id=$1 ORDER BY a.created_at,a.id`, s.workspace)
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
	result, err := s.river.InsertTx(ctx, tx, queueargs.DispatchTaskArgs{TaskID: taskID}, &river.InsertOpts{
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
		BaseBranch: task.BaseBranch, Branch: task.Branch, State: string(task.State),
		NextStage: string(task.NextStage), RecoveryStage: string(task.RecoveryStage), ParentTaskID: task.ParentTaskID, FeatureID: nullableText(task.FeatureID), IntakeKey: nullableText(task.IntakeKey), CreatedAt: timestamp(task.CreatedAt),
	}
}

func taskFromDB(task db.Task) core.Task {
	return core.Task{
		ID: task.ID, Workspace: task.WorkspaceID, Source: task.Source, IntakeKey: task.IntakeKey.String,
		Title: task.Title, Body: task.Body, Class: task.Class,
		Level: core.EscalationLevel(task.EscalationLevel), Repo: task.RepoName,
		BaseBranch: task.BaseBranch, Branch: task.Branch,
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
		StartedAt: timestamp(job.StartedAt), EndedAt: nullableTimestamp(job.EndedAt),
	}
}

func jobUpdateParams(job core.Job, workspace string) db.UpdateJobParams {
	return db.UpdateJobParams{
		ID: job.ID, Stage: string(job.Stage), Harness: job.Harness,
		ModelTier: job.ModelTier, AuthMode: job.AuthMode, Runner: job.Runner,
		PackVersion: job.PackVersion, ConfinementTier: job.Confinement,
		CostUsd: job.CostUSD, TokensIn: job.TokensIn,
		TokensOut: job.TokensOut, State: string(job.State),
		StartedAt: timestamp(job.StartedAt), EndedAt: nullableTimestamp(job.EndedAt),
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
