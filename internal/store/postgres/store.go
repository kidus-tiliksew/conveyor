// Package postgres implements the Phase 2 event-sourced store with pgx and
// sqlc. Every projection mutation and its audit event commit in one
// transaction; events and interventions are append-only at the database layer
// (spec §3.1, §16, §17.0).
package postgres

import (
	"context"
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
	"github.com/kidus-tiliksew/conveyor/internal/routing"
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
	previousCredentialIDs, err := q.ListWorkspaceCredentialIDs(ctx, cfg.Workspace)
	if err != nil {
		return false, err
	}
	previousPolicyRefs, err := q.ListWorkspaceVendorPolicyRefs(ctx, cfg.Workspace)
	if err != nil {
		return false, err
	}
	if seeded {
		for _, repo := range cfg.Repos {
			if err := upsertRepo(ctx, q, cfg.Workspace, repo); err != nil {
				return false, err
			}
		}
	}
	for _, policy := range cfg.VendorPolicies {
		reviewedAt, err := time.Parse("2006-01-02", policy.ReviewedAt)
		if err != nil {
			return false, err
		}
		if err := q.UpsertVendorPolicy(ctx, db.UpsertVendorPolicyParams{
			Vendor: policy.Vendor, Harness: policy.Harness, AuthMode: policy.AuthMode,
			SubscriptionHeadless: policy.SubscriptionHeadless,
			ReviewedAt:           pgtype.Date{Time: reviewedAt, Valid: true}, SourceUrl: policy.SourceURL,
		}); err != nil {
			return false, err
		}
	}
	for _, credential := range cfg.Credentials {
		if err := q.UpsertCredential(ctx, db.UpsertCredentialParams{
			ID: credential.ID, OwnerID: credential.OwnerID, OwnerKind: credential.OwnerKind,
			Kind: credential.Kind, Vendor: credential.Vendor, Harness: credential.Harness, EncRef: credential.Ref,
		}); err != nil {
			return false, err
		}
	}
	if err := q.DeleteWorkspaceCredentialRefs(ctx, cfg.Workspace); err != nil {
		return false, err
	}
	configuredCredentialIDs := make(map[string]struct{}, len(cfg.Credentials))
	for _, credential := range cfg.Credentials {
		configuredCredentialIDs[credential.ID] = struct{}{}
		if err := q.InsertWorkspaceCredentialRef(ctx, db.InsertWorkspaceCredentialRefParams{
			WorkspaceID: cfg.Workspace, CredentialID: credential.ID,
		}); err != nil {
			return false, err
		}
		if err := q.EnableCredential(ctx, credential.ID); err != nil {
			return false, err
		}
	}
	for _, id := range previousCredentialIDs {
		if _, configured := configuredCredentialIDs[id]; configured {
			continue
		}
		if err := q.DisableCredentialIfUnreferenced(ctx, id); err != nil {
			return false, err
		}
	}
	if err := q.DeleteWorkspaceVendorPolicyRefs(ctx, cfg.Workspace); err != nil {
		return false, err
	}
	configuredPolicyKeys := make(map[string]struct{}, len(cfg.VendorPolicies))
	for _, policy := range cfg.VendorPolicies {
		configuredPolicyKeys[capacityPolicyKey(policy.Vendor, policy.Harness, policy.AuthMode)] = struct{}{}
		if err := q.InsertWorkspaceVendorPolicyRef(ctx, db.InsertWorkspaceVendorPolicyRefParams{
			WorkspaceID: cfg.Workspace, Vendor: policy.Vendor, Harness: policy.Harness, AuthMode: policy.AuthMode,
		}); err != nil {
			return false, err
		}
	}
	for _, policy := range previousPolicyRefs {
		if _, configured := configuredPolicyKeys[capacityPolicyKey(policy.Vendor, policy.Harness, policy.AuthMode)]; configured {
			continue
		}
		if err := q.RestrictVendorPolicyIfUnreferenced(ctx, db.RestrictVendorPolicyIfUnreferencedParams{
			Vendor: policy.Vendor, Harness: policy.Harness, AuthMode: policy.AuthMode,
		}); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	s.workspace = cfg.Workspace
	return seeded, nil
}

func capacityPolicyKey(vendor, harness, authMode string) string {
	return strings.Join([]string{vendor, harness, authMode}, "\x1f")
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
	if before.Workspace != after.Workspace || before.Image != after.Image || before.MaxBounces != after.MaxBounces {
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

func (s *Store) ClaimCredential(ctx context.Context, request routing.ClaimRequest) (routing.Credential, error) {
	row, err := s.queries.ClaimCredential(ctx, db.ClaimCredentialParams{
		TaskID: request.TaskID, JobID: request.JobID, LeaseSeconds: request.LeaseSeconds, Harnesses: request.Harnesses,
		OwnerID: request.OwnerID, AllowRestricted: request.AllowRestricted,
		ExcludeHarness: request.ExcludeHarness,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return routing.Credential{}, routing.ErrNoCapacity
	}
	if err != nil {
		return routing.Credential{}, err
	}
	return routing.Credential{
		ID: row.ID, OwnerID: row.OwnerID, OwnerKind: row.OwnerKind, Kind: row.Kind,
		Vendor: row.Vendor, Harness: row.Harness, Ref: row.EncRef,
	}, nil
}

func (s *Store) RescueTaskCredentialLeases(ctx context.Context, taskID, currentJobID string) error {
	_, err := s.queries.RescueTaskCredentialLeases(ctx, db.RescueTaskCredentialLeasesParams{
		TaskID: taskID, CurrentJobID: currentJobID,
	})
	return err
}

func (s *Store) ReleaseCredential(ctx context.Context, id, jobID, lastError string) error {
	rows, err := s.queries.ReleaseCredential(ctx, db.ReleaseCredentialParams{ID: id, JobID: jobID, LastError: lastError})
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("credential %s is not leased by job %s", id, jobID)
	}
	return nil
}

func (s *Store) ThrottleCredential(ctx context.Context, id, jobID, lastError string, cooldownSeconds int64) error {
	rows, err := s.queries.ThrottleCredential(ctx, db.ThrottleCredentialParams{
		ID: id, JobID: jobID, LastError: lastError, CooldownSeconds: cooldownSeconds,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("credential %s is not leased by job %s", id, jobID)
	}
	return nil
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
		NextStage: string(task.NextStage), RecoveryStage: string(task.RecoveryStage), ParentTaskID: task.ParentTaskID, CreatedAt: timestamp(task.CreatedAt),
	}
}

func taskFromDB(task db.Task) core.Task {
	return core.Task{
		ID: task.ID, Workspace: task.WorkspaceID, Source: task.Source,
		Title: task.Title, Body: task.Body, Class: task.Class,
		Level: core.EscalationLevel(task.EscalationLevel), Repo: task.RepoName,
		BaseBranch: task.BaseBranch, Branch: task.Branch,
		State: core.TaskState(task.State), NextStage: core.Stage(task.NextStage), RecoveryStage: core.Stage(task.RecoveryStage), ParentTaskID: task.ParentTaskID,
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
		ModelTier: job.ModelTier, CredentialID: job.CredentialID, AuthMode: job.AuthMode,
		Runner: job.Runner, SandboxRef: job.SandboxRef,
		PackVersion: job.PackVersion, ConfinementTier: job.Confinement,
		BudgetUsd: job.BudgetUSD, CostUsd: job.CostUSD, TokensIn: job.TokensIn,
		TokensOut: job.TokensOut, State: string(job.State),
		BootDiagnostics: diagnosticsJSON(job.BootDiagnostics),
		StartedAt:       timestamp(job.StartedAt), EndedAt: nullableTimestamp(job.EndedAt),
	}
}

func jobUpdateParams(job core.Job, workspace string) db.UpdateJobParams {
	return db.UpdateJobParams{
		ID: job.ID, Stage: string(job.Stage), Harness: job.Harness,
		ModelTier: job.ModelTier, CredentialID: job.CredentialID, AuthMode: job.AuthMode,
		Runner: job.Runner, SandboxRef: job.SandboxRef,
		PackVersion: job.PackVersion, ConfinementTier: job.Confinement,
		BudgetUsd: job.BudgetUSD, CostUsd: job.CostUSD, TokensIn: job.TokensIn,
		TokensOut: job.TokensOut, State: string(job.State),
		BootDiagnostics: diagnosticsJSON(job.BootDiagnostics),
		StartedAt:       timestamp(job.StartedAt), EndedAt: nullableTimestamp(job.EndedAt),
		WorkspaceID: workspace,
	}
}

func jobFromDB(job db.Job) core.Job {
	var diagnostics *core.BootDiagnostics
	if len(job.BootDiagnostics) != 0 {
		var decoded core.BootDiagnostics
		if json.Unmarshal(job.BootDiagnostics, &decoded) == nil {
			diagnostics = &decoded
		}
	}
	return core.Job{
		ID: job.ID, TaskID: job.TaskID, Stage: core.Stage(job.Stage), Harness: job.Harness,
		ModelTier: job.ModelTier, CredentialID: job.CredentialID, AuthMode: job.AuthMode,
		Runner: job.Runner, SandboxRef: job.SandboxRef,
		PackVersion: job.PackVersion, Confinement: job.ConfinementTier,
		BudgetUSD: job.BudgetUsd, CostUSD: job.CostUsd, TokensIn: job.TokensIn,
		TokensOut: job.TokensOut, State: core.JobState(job.State),
		BootDiagnostics: diagnostics, StartedAt: job.StartedAt.Time,
		EndedAt: nullableTime(job.EndedAt),
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

func diagnosticsJSON(diagnostics *core.BootDiagnostics) []byte {
	if diagnostics == nil {
		return nil
	}
	return core.JSONPayload(diagnostics)
}

func notFound(err error, format string, args ...any) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf(format+" not found", args...)
	}
	return err
}

var _ store.Store = (*Store)(nil)
