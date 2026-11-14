package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

const joinedJobColumns = `j.id,j.task_id,j.stage,j.harness,j.model_tier,j.auth_mode,j.runner,j.pack_version,j.confinement_tier,j.cost_usd,j.tokens_in,j.tokens_out,j.state,j.started_at,j.ended_at`
const jobColumns = `id,task_id,stage,harness,model_tier,auth_mode,runner,pack_version,confinement_tier,cost_usd,tokens_in,tokens_out,state,started_at,ended_at`

func scanJob(row interface{ Scan(...any) error }) (core.Job, error) {
	var j core.Job
	var start, end *time.Time
	err := row.Scan(&j.ID, &j.TaskID, &j.Stage, &j.Harness, &j.ModelTier, &j.AuthMode, &j.Runner, &j.PackVersion, &j.Confinement, &j.CostUSD, &j.TokensIn, &j.TokensOut, &j.State, &start, &end)
	if start != nil {
		j.StartedAt = *start
	}
	if end != nil {
		j.EndedAt = *end
	}
	return j, err
}
func jobValues(j core.Job) map[string]any {
	return map[string]any{
		"stage":            j.Stage,
		"harness":          j.Harness,
		"model_tier":       j.ModelTier,
		"auth_mode":        j.AuthMode,
		"runner":           j.Runner,
		"pack_version":     j.PackVersion,
		"confinement_tier": j.Confinement,
		"cost_usd":         j.CostUSD,
		"tokens_in":        j.TokensIn,
		"tokens_out":       j.TokensOut,
		"state":            j.State,
		"started_at":       nullableTimeValue(j.StartedAt),
		"ended_at":         nullableTimeValue(j.EndedAt),
	}
}
func insertJobRow(ctx context.Context, tx *sql.Tx, j core.Job) error {
	if _, err := getTaskRow(ctx, tx, j.TaskID); err != nil {
		return err
	}
	if err := lockKey(ctx, tx, "job-id:"+j.ID); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE id=?`, j.ID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return store.ErrDispatchJobConflict
	}
	values := jobValues(j)
	values["id"] = j.ID
	values["task_id"] = j.TaskID
	values["workspace_id"] = documentWorkspace(ctx)
	_, err := writeRow(ctx, tx, rowWrite{table: "jobs", operation: "INSERT", values: values})
	return err
}
func (s *Store) CreateJob(ctx context.Context, j core.Job) error {
	ctx = dbmetrics.WithReadLabel(ctx, "CreateJob")
	return s.taskTx(ctx, j.TaskID, func(tx *sql.Tx) error {
		if err := insertJobRow(ctx, tx, j); err != nil {
			return err
		}
		return taskEvent(ctx, tx, core.Event{TaskID: j.TaskID, JobID: j.ID, Kind: "job.created", Payload: core.JSONPayload(j)})
	})
}
func (s *Store) UpdateJob(ctx context.Context, j core.Job) error {
	ctx = dbmetrics.WithReadLabel(ctx, "UpdateJob")
	var id string
	if err := documentRow(ctx, s.db, `SELECT task_id FROM jobs WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), j.ID).Scan(&id); err != nil {
		return notFound(err, "job %s", j.ID)
	}
	return s.taskTx(ctx, id, func(tx *sql.Tx) error {
		current, err := scanJob(documentRow(ctx, tx, `SELECT `+jobColumns+` FROM jobs WHERE workspace_id=? AND id=? FOR UPDATE`, documentWorkspace(ctx), j.ID))
		if err != nil {
			return notFound(err, "job %s", j.ID)
		}
		if err = store.ValidateJobTransition(current.State, j.State); err != nil {
			return err
		}
		values := jobValues(j)
		values["updated_at"] = time.Now().UTC()
		if _, err = writeRow(ctx, tx, rowWrite{table: "jobs", operation: "UPDATE", values: values, where: map[string]any{"workspace_id": documentWorkspace(ctx), "id": j.ID}}); err != nil {
			return err
		}
		return taskEvent(ctx, tx, core.Event{TaskID: current.TaskID, JobID: j.ID, Kind: "job.updated", Payload: core.JSONPayload(j)})
	})
}
func (s *Store) ListJobs(ctx context.Context, id string) ([]core.Job, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListJobs")
	rows, err := documentRows(ctx, s.db, `SELECT `+joinedJobColumns+` FROM jobs j LEFT JOIN work_orders wo ON wo.workspace_id=j.workspace_id AND wo.job_id=j.id WHERE j.workspace_id=? AND j.task_id=? ORDER BY COALESCE(j.started_at,wo.queue_entered_at) IS NULL,COALESCE(j.started_at,wo.queue_entered_at),j.id`, documentWorkspace(ctx), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListJobPage reads one page of ListJobs with its task-wide count. The count
// runs first, so a page past the end still states it.
func (s *Store) ListJobPage(ctx context.Context, id string, page store.PageQuery) (store.JobPage, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListJobPage")
	if err := page.Validate(); err != nil {
		return store.JobPage{}, err
	}
	var total int
	if err := documentRow(ctx, s.db, `SELECT COUNT(*) FROM jobs WHERE workspace_id=? AND task_id=?`, documentWorkspace(ctx), id).Scan(&total); err != nil {
		return store.JobPage{}, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT `+joinedJobColumns+` FROM jobs j LEFT JOIN work_orders wo ON wo.workspace_id=j.workspace_id AND wo.job_id=j.id WHERE j.workspace_id=? AND j.task_id=? ORDER BY COALESCE(j.started_at,wo.queue_entered_at) IS NULL,COALESCE(j.started_at,wo.queue_entered_at),j.id LIMIT ? OFFSET ?`, documentWorkspace(ctx), id, page.Limit, page.Offset)
	if err != nil {
		return store.JobPage{}, err
	}
	defer rows.Close()
	jobs := []core.Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return store.JobPage{}, err
		}
		jobs = append(jobs, job)
	}
	return store.JobPage{Jobs: jobs, Total: total}, rows.Err()
}

func (s *Store) GetLatestJob(ctx context.Context, id string) (core.Job, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetLatestJob")
	j, err := scanJob(documentRow(ctx, s.db, `SELECT `+joinedJobColumns+` FROM jobs j LEFT JOIN work_orders wo ON wo.workspace_id=j.workspace_id AND wo.job_id=j.id WHERE j.workspace_id=? AND j.task_id=? ORDER BY COALESCE(j.started_at,wo.queue_entered_at) IS NULL DESC,COALESCE(j.started_at,wo.queue_entered_at) DESC,j.id DESC LIMIT 1`, documentWorkspace(ctx), id))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Job{}, false, nil
	}
	return j, err == nil, err
}

const specColumns = `task_id,version,content,acceptance_count,acceptance,decomposition,approved,created_at,approved_at,agent,model,review_seats,origin,base_commit`

func scanSpec(row interface{ Scan(...any) error }) (core.SpecVersion, error) {
	var s core.SpecVersion
	var at *time.Time
	var reviewSeats sql.NullInt64
	var baseCommit sql.NullString
	err := row.Scan(&s.TaskID, &s.Version, &s.Content, &s.AcceptanceCount, &s.Acceptance, &s.Decomposition, &s.Approved, &s.CreatedAt, &at, &s.Agent, &s.Model, &reviewSeats, &s.Origin, &baseCommit)
	if at != nil {
		s.ApprovedAt = *at
	}
	if baseCommit.Valid {
		commit := baseCommit.String
		s.BaseCommit = &commit
	}
	s.ReviewSeats = seatCountFromNull(reviewSeats)
	return s, err
}
func (s *Store) CreateSpecVersion(ctx context.Context, spec core.SpecVersion) (core.SpecVersion, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CreateSpecVersion")
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now().UTC()
	}
	if err := store.PrepareSpecVersionOrigin(&spec); err != nil {
		return core.SpecVersion{}, err
	}
	err := s.taskTx(ctx, spec.TaskID, func(tx *sql.Tx) error {
		if _, err := getTaskRow(ctx, tx, spec.TaskID); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM task_specs WHERE workspace_id=? AND task_id=?`, documentWorkspace(ctx), spec.TaskID).Scan(&spec.Version); err != nil {
			return err
		}
		if len(spec.Acceptance) == 0 {
			spec.Acceptance = json.RawMessage(`[]`)
		}
		if len(spec.Decomposition) == 0 {
			spec.Decomposition = json.RawMessage(`[]`)
		}
		spec.Approved = false
		spec.ApprovedAt = time.Time{}
		_, err := writeRow(ctx, tx, rowWrite{table: "task_specs", operation: "INSERT", values: specRowValues(ctx, spec)})
		if err != nil {
			return err
		}
		return taskEvent(ctx, tx, core.Event{TaskID: spec.TaskID, Kind: "spec.version_created", Payload: core.JSONPayload(map[string]any{"version": spec.Version, "acceptance_count": spec.AcceptanceCount})})
	})
	if err != nil {
		return core.SpecVersion{}, err
	}
	return spec, nil
}
func (s *Store) readSpec(ctx context.Context, id string, version int, approved bool) (core.SpecVersion, bool, error) {
	query := `SELECT ` + specColumns + ` FROM task_specs WHERE workspace_id=? AND task_id=?`
	args := []any{documentWorkspace(ctx), id}
	if version > 0 {
		query += ` AND version=?`
		args = append(args, version)
	}
	if approved {
		query += ` AND approved`
	}
	query += ` ORDER BY version DESC LIMIT 1`
	spec, err := scanSpec(documentRow(ctx, s.db, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return core.SpecVersion{}, false, nil
	}
	if err != nil {
		return core.SpecVersion{}, false, err
	}
	err = documentRow(ctx, s.db, `SELECT EXISTS(SELECT 1 FROM legacy_spec_gate_versions WHERE workspace_id=? AND task_id=? AND spec_version=?)`, documentWorkspace(ctx), id, spec.Version).Scan(&spec.LegacyGate)
	return spec, true, err
}
func (s *Store) GetLatestSpecVersion(ctx context.Context, id string) (core.SpecVersion, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetLatestSpecVersion")
	return s.readSpec(ctx, id, 0, false)
}
func (s *Store) GetApprovedSpecVersion(ctx context.Context, id string) (core.SpecVersion, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetApprovedSpecVersion")
	return s.readSpec(ctx, id, 0, true)
}
func (s *Store) GetSpecVersion(ctx context.Context, id string, version int) (core.SpecVersion, bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "GetSpecVersion")
	if version <= 0 {
		return core.SpecVersion{}, false, nil
	}
	return s.readSpec(ctx, id, version, false)
}
func approveSpecTx(ctx context.Context, tx *sql.Tx, id string, version int) error {
	if _, err := getTaskRow(ctx, tx, id); err != nil {
		return err
	}
	var latest int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM task_specs WHERE workspace_id=? AND task_id=?`, documentWorkspace(ctx), id).Scan(&latest); err != nil {
		return err
	}
	if version <= 0 || version != latest {
		return fmt.Errorf("spec version %d for task %s not found or superseded", version, id)
	}
	_, err := tx.ExecContext(ctx, `UPDATE task_specs SET approved=TRUE,approved_at=? WHERE workspace_id=? AND task_id=? AND version=?`, time.Now().UTC(), documentWorkspace(ctx), id, version)
	return err
}
func (s *Store) ApproveSpecVersion(ctx context.Context, id string, version int) error {
	ctx = dbmetrics.WithReadLabel(ctx, "ApproveSpecVersion")
	return s.taskTx(ctx, id, func(tx *sql.Tx) error {
		if err := approveSpecTx(ctx, tx, id, version); err != nil {
			return err
		}
		if err := taskEvent(ctx, tx, core.Event{TaskID: id, Kind: "spec.version_approved", Payload: core.JSONPayload(map[string]int{"version": version})}); err != nil {
			return err
		}
		if err := approvePlanStateTx(ctx, tx, id); err != nil {
			return err
		}
		if err := recordPlanRiskTx(ctx, tx, id, version); err != nil {
			return err
		}
		// An approval can make a risk agent rating of this version count, so
		// a tier that reaches high cancels the open risk orders in the same
		// transaction (req-risk-based-review AC-4.3, AC-12.3).
		return cancelRiskOrdersIfHighTx(ctx, tx, id, time.Now().UTC())
	})
}

// approvePlanStateTx sets planning completed with cause plan_approved in the
// transaction that records a plan approval (component-task-lifecycle
// invariant 54; component-plan-state invariant 3; req-task-plan-state AC-1.3).
func approvePlanStateTx(ctx context.Context, tx *sql.Tx, id string) error {
	task, err := getTaskRow(ctx, tx, id)
	if err != nil {
		return err
	}
	move := store.PlanStateMoveEvent(id, task.PlanState, core.PlanCompleted, core.PlanCausePlanApproved)
	if move == nil {
		return nil
	}
	if err := taskWrite(ctx, tx, id, map[string]any{"plan_state": string(core.PlanCompleted)}); err != nil {
		return err
	}
	return taskEvent(ctx, tx, *move)
}

// specRowValues is the task_specs insert of one plan version. SingleStore has
// no CHECK constraint, so origin is validated by store.PrepareSpecVersionOrigin
// before every write (component-plan-state, Columns).
func specRowValues(ctx context.Context, spec core.SpecVersion) map[string]any {
	if len(spec.Acceptance) == 0 {
		spec.Acceptance = json.RawMessage(`[]`)
	}
	if len(spec.Decomposition) == 0 {
		spec.Decomposition = json.RawMessage(`[]`)
	}
	var baseCommit any
	if spec.BaseCommit != nil {
		baseCommit = *spec.BaseCommit
	}
	return map[string]any{"workspace_id": documentWorkspace(ctx), "task_id": spec.TaskID, "version": spec.Version, "content": spec.Content, "acceptance_count": spec.AcceptanceCount, "acceptance": []byte(spec.Acceptance), "decomposition": []byte(spec.Decomposition), "created_at": spec.CreatedAt, "agent": spec.Agent, "model": spec.Model, "review_seats": nullSeatCount(spec.ReviewSeats), "origin": string(spec.Origin), "base_commit": baseCommit}
}
