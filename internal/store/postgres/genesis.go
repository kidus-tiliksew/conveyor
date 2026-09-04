package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/pglog"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// Genesis import: build the event log for a deployment that predates it.
//
// The legacy events table cannot rebuild state by replay (it was task-scoped
// at birth, carries no stream versions, and document bodies live in version
// tables), so the import does two things per workspace:
//
//  1. History: every legacy events row not yet in the log is appended to the
//     stream legacyStream assigns it, in legacy id order, carrying its legacy
//     id. Rows the dual-append already mirrored are skipped by that id.
//  2. Snapshots: for every entity the log core owns as a stream, one
//     log.snapshot_imported event carrying the entity's projection rows
//     verbatim (row plus child rows), hashed so a re-run with unchanged rows
//     writes nothing.
//
// Each snapshot is written in a transaction that holds the stream's head
// lock before reading the rows, so a concurrent legacy write on the same
// entity cannot slip between the read and the snapshot. The whole run holds
// the startup-migrations advisory lock so it cannot overlap daemon startup.
//
// Log-core migration plan, phase 1, task 1.3.

// GenesisOptions controls one import run.
type GenesisOptions struct {
	// Workspaces limits the run; empty means every workspace in the table.
	Workspaces []string
	// BatchSize is the number of legacy history rows per transaction.
	BatchSize int
	// Progress receives one line per completed step when set.
	Progress func(string)
}

// GenesisWorkspaceReport summarizes one workspace's import.
type GenesisWorkspaceReport struct {
	Workspace           string         `json:"workspace"`
	HistoryImported     int            `json:"history_imported"`
	HistoryAlreadyInLog int            `json:"history_already_in_log"`
	SnapshotsWritten    map[string]int `json:"snapshots_written"`
	SnapshotsUnchanged  int            `json:"snapshots_unchanged"`
	MarkerWritten       bool           `json:"marker_written"`
}

// GenesisReport is the result of ImportGenesis.
type GenesisReport struct {
	Workspaces []GenesisWorkspaceReport `json:"workspaces"`
	Deployment GenesisWorkspaceReport   `json:"deployment"`
	Duration   time.Duration            `json:"duration"`
}

type genesisChild struct {
	table   string
	fk      string
	scoped  bool // has a workspace_id column
	orderBy string
	exclude []string
}

type genesisFamily struct {
	family   string
	table    string
	idColumn string
	// scope is how rows are selected for a workspace: "workspace_id" for
	// scoped tables, "id" for the workspaces table itself, "" for
	// deployment-level tables.
	scope    string
	stream   func(string) eventlog.StreamID
	exclude  []string
	children []genesisChild
}

// genesisFamilies is the table-driven definition of what a snapshot holds.
// Credential stores are absent on purpose and hashed secrets are excluded
// by column: nothing that could authenticate reaches the log.
var genesisFamilies = []genesisFamily{
	{
		family: "task", table: "tasks", idColumn: "id", scope: "workspace_id", stream: eventlog.TaskStream,
		children: []genesisChild{
			{table: "task_specs", fk: "task_id", orderBy: "version"},
			{table: "task_dependencies", fk: "task_id", scoped: true, orderBy: "depends_on_task_id"},
			{table: "task_context_proposals", fk: "task_id", scoped: true, orderBy: "created_at, target_kind, target_id"},
			{table: "task_setup_changes", fk: "task_id", scoped: true, orderBy: "created_at, request_id"},
			{table: "task_dependency_additions", fk: "task_id", scoped: true, orderBy: "created_at, request_id"},
			{table: "task_dependency_removals", fk: "task_id", scoped: true, orderBy: "created_at, request_id"},
			{table: "github_lifecycles", fk: "task_id", scoped: true, orderBy: "repository"},
			{table: "artifact_links", fk: "task_id", scoped: true, orderBy: "artifact_id, role"},
			{table: "interventions", fk: "task_id", orderBy: "at, id"},
			{table: "jobs", fk: "task_id", orderBy: "started_at, id"},
		},
	},
	{
		family: "work_order", table: "work_orders", idColumn: "id", scope: "workspace_id", stream: eventlog.WorkOrderStream,
		exclude: []string{"client_token_hash"},
		children: []genesisChild{
			{table: "work_order_activity_snapshots", fk: "work_order_id", scoped: true, orderBy: "captured_at, attempt_id"},
			{table: "work_order_preemptions", fk: "work_order_id", scoped: true, orderBy: "created_at, request_id"},
			{table: "work_order_recoveries", fk: "work_order_id", scoped: true, orderBy: "created_at, request_id"},
			{table: "review_publications", fk: "review_work_order_id", scoped: true, orderBy: "created_at"},
		},
	},
	{
		family: "requirement", table: "requirements", idColumn: "id", scope: "workspace_id", stream: eventlog.RequirementStream,
		children: []genesisChild{{table: "requirement_versions", fk: "requirement_id", scoped: true, orderBy: "version"}},
	},
	{
		family: "design", table: "system_designs", idColumn: "id", scope: "workspace_id", stream: eventlog.DesignStream,
		children: []genesisChild{{table: "system_design_versions", fk: "document_id", scoped: true, orderBy: "version"}},
	},
	{
		family: "decision", table: "decisions", idColumn: "id", scope: "workspace_id", stream: eventlog.DecisionStream,
		children: []genesisChild{{table: "decision_supersession_sweeps", fk: "decision_id", scoped: true, orderBy: "superseded_decision_id, document_tier, document_id"}},
	},
	{
		family: "reference_document", table: "reference_documents", idColumn: "id", scope: "workspace_id", stream: eventlog.ReferenceDocumentStream,
		children: []genesisChild{{table: "reference_document_versions", fk: "document_id", scoped: true, orderBy: "version"}},
	},
	{
		family: "planning_session", table: "planning_sessions", idColumn: "id", scope: "workspace_id", stream: eventlog.PlanningSessionStream,
		children: []genesisChild{{table: "planning_messages", fk: "session_id", scoped: true, orderBy: "seq"}},
	},
	{
		family: "planning_bundle", table: "planning_bundles", idColumn: "id", scope: "workspace_id", stream: eventlog.PlanningBundleStream,
	},
	{
		family: "worker", table: "workers", idColumn: "id", scope: "workspace_id", stream: eventlog.WorkerStream,
		exclude: []string{"credential_hash"},
	},
	{
		family: "workspace", table: "workspaces", idColumn: "id", scope: "id", stream: eventlog.WorkspaceStream,
		children: []genesisChild{
			{table: "workspace_role_bindings", fk: "workspace_id", orderBy: "user_id"},
			{table: "repos", fk: "workspace_id", orderBy: "name"},
			{table: "features", fk: "workspace_id", orderBy: "id"},
			{table: "monitor_status", fk: "workspace_id", orderBy: "workspace_id"},
		},
	},
}

// genesisDeploymentFamilies are snapshotted once, under DeploymentWorkspace.
var genesisDeploymentFamilies = []genesisFamily{
	{family: "user", table: "users", idColumn: "id", scope: "", stream: eventlog.UserStream, exclude: []string{"password_hash"}},
}

const genesisDefaultBatch = 2000

// ImportGenesis runs the import described above and returns what it did.
func (s *Store) ImportGenesis(ctx context.Context, opts GenesisOptions) (GenesisReport, error) {
	started := time.Now()
	if opts.BatchSize <= 0 {
		opts.BatchSize = genesisDefaultBatch
	}
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	report := GenesisReport{}
	err := s.withStartupLock(ctx, func() error {
		workspaces := opts.Workspaces
		if len(workspaces) == 0 {
			rows, err := s.pool.Query(ctx, `SELECT id FROM workspaces ORDER BY id`)
			if err != nil {
				return fmt.Errorf("list workspaces: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				workspaces = append(workspaces, id)
			}
			if err := rows.Err(); err != nil {
				return err
			}
		}
		for _, workspace := range workspaces {
			wr := GenesisWorkspaceReport{Workspace: workspace, SnapshotsWritten: map[string]int{}}
			imported, skipped, err := s.importLegacyHistory(ctx, workspace, opts.BatchSize, progress)
			if err != nil {
				return fmt.Errorf("workspace %s: history: %w", workspace, err)
			}
			wr.HistoryImported, wr.HistoryAlreadyInLog = imported, skipped
			progress(fmt.Sprintf("%s: history imported=%d already_in_log=%d", workspace, imported, skipped))
			for _, family := range genesisFamilies {
				written, unchanged, err := s.snapshotFamily(ctx, workspace, family)
				if err != nil {
					return fmt.Errorf("workspace %s: snapshot %s: %w", workspace, family.family, err)
				}
				if written > 0 {
					wr.SnapshotsWritten[family.family] = written
				}
				wr.SnapshotsUnchanged += unchanged
				progress(fmt.Sprintf("%s: %s snapshots written=%d unchanged=%d", workspace, family.family, written, unchanged))
			}
			if wr.HistoryImported > 0 || len(wr.SnapshotsWritten) > 0 {
				if err := s.writeGenesisMarker(ctx, workspace, wr); err != nil {
					return fmt.Errorf("workspace %s: marker: %w", workspace, err)
				}
				wr.MarkerWritten = true
			}
			report.Workspaces = append(report.Workspaces, wr)
		}
		dr := GenesisWorkspaceReport{Workspace: eventlog.DeploymentWorkspace, SnapshotsWritten: map[string]int{}}
		for _, family := range genesisDeploymentFamilies {
			written, unchanged, err := s.snapshotFamily(ctx, eventlog.DeploymentWorkspace, family)
			if err != nil {
				return fmt.Errorf("deployment: snapshot %s: %w", family.family, err)
			}
			if written > 0 {
				dr.SnapshotsWritten[family.family] = written
			}
			dr.SnapshotsUnchanged += unchanged
		}
		report.Deployment = dr
		return nil
	})
	report.Duration = time.Since(started)
	return report, err
}

// withStartupLock holds the same session-level advisory lock Migrate takes,
// so an import never overlaps a daemon applying migrations.
func (s *Store) withStartupLock(ctx context.Context, fn func() error) error {
	pooled, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire lock connection: %w", err)
	}
	lockConn := pooled.Hijack()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock(hashtext('conveyor:startup-migrations'))"); err != nil {
		_ = lockConn.Close(ctx)
		return fmt.Errorf("lock startup migrations: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = lockConn.Exec(unlockCtx, "SELECT pg_advisory_unlock(hashtext('conveyor:startup-migrations'))")
		_ = lockConn.Close(unlockCtx)
	}()
	return fn()
}

// importLegacyHistory appends legacy events rows not yet in the log, in
// batches. Each batch is one transaction: lock the workspace position, lock
// every stream the batch touches (sorted, so two importers cannot deadlock),
// bulk-insert with versions computed from the locked heads, then advance
// heads and the workspace position.
func (s *Store) importLegacyHistory(ctx context.Context, workspace string, batchSize int, progress func(string)) (imported, skipped int, err error) {
	var alreadyInLog int
	if err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM events e
WHERE e.workspace_id = $1 AND EXISTS (SELECT 1 FROM event_log l WHERE l.legacy_event_id = e.id)`, workspace).Scan(&alreadyInLog); err != nil {
		return 0, 0, fmt.Errorf("count mirrored history: %w", err)
	}
	var lastID int64
	for {
		batch, err := s.loadHistoryBatch(ctx, workspace, lastID, batchSize)
		if err != nil {
			return imported, alreadyInLog, err
		}
		if len(batch) == 0 {
			return imported, alreadyInLog, nil
		}
		if err := s.appendHistoryBatch(ctx, workspace, batch); err != nil {
			return imported, alreadyInLog, err
		}
		imported += len(batch)
		lastID = batch[len(batch)-1].ID
		progress(fmt.Sprintf("%s: history batch through legacy id %d (%d so far)", workspace, lastID, imported))
	}
}

func (s *Store) loadHistoryBatch(ctx context.Context, workspace string, afterID int64, limit int) ([]db.Event, error) {
	rows, err := s.pool.Query(ctx, `
SELECT e.id, e.task_id, e.job_id, e.kind, e.actor_id, e.actor_role, e.payload_json, e.at, e.workspace_id
FROM events e
WHERE e.workspace_id = $1 AND e.id > $2
  AND NOT EXISTS (SELECT 1 FROM event_log l WHERE l.legacy_event_id = e.id)
ORDER BY e.id
LIMIT $3`, workspace, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("load history batch: %w", err)
	}
	defer rows.Close()
	var out []db.Event
	for rows.Next() {
		var e db.Event
		if err := rows.Scan(&e.ID, &e.TaskID, &e.JobID, &e.Kind, &e.ActorID, &e.ActorRole, &e.PayloadJson, &e.At, &e.WorkspaceID); err != nil {
			return nil, fmt.Errorf("scan history row: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) appendHistoryBatch(ctx context.Context, workspace string, batch []db.Event) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	position, err := s.log.LockWorkspace(ctx, tx, workspace)
	if err != nil {
		return err
	}
	streams := make([]eventlog.StreamID, 0, len(batch))
	streamOf := make([]eventlog.StreamID, len(batch))
	seen := map[eventlog.StreamID]bool{}
	for i, row := range batch {
		stream := legacyStream(row)
		streamOf[i] = stream
		if !seen[stream] {
			seen[stream] = true
			streams = append(streams, stream)
		}
	}
	sort.Slice(streams, func(i, j int) bool { return streams[i] < streams[j] })
	heads := make(map[eventlog.StreamID]int64, len(streams))
	for _, stream := range streams {
		head, err := s.log.LockStream(ctx, tx, workspace, stream)
		if err != nil {
			return err
		}
		heads[stream] = int64(head)
	}
	pos := int64(position)
	copyRows := make([][]any, 0, len(batch))
	for i, row := range batch {
		stream := streamOf[i]
		heads[stream]++
		pos++
		payload := row.PayloadJson
		if len(payload) == 0 {
			payload = []byte(`{}`)
		}
		copyRows = append(copyRows, []any{
			workspace, pos, string(stream), heads[stream], row.Kind, row.ActorID, row.ActorRole,
			payload, row.At.Time.UTC(), row.ID,
		})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"event_log"},
		[]string{"workspace_id", "position", "stream_id", "version", "kind", "actor_id", "actor_role", "payload", "at", "legacy_event_id"},
		pgx.CopyFromRows(copyRows)); err != nil {
		return fmt.Errorf("copy history batch: %w", err)
	}
	for _, stream := range streams {
		if _, err := tx.Exec(ctx, `UPDATE event_log_streams SET version = $3 WHERE workspace_id = $1 AND stream_id = $2`, workspace, string(stream), heads[stream]); err != nil {
			return fmt.Errorf("advance stream head: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE event_log_positions SET position = $2 WHERE workspace_id = $1`, workspace, pos); err != nil {
		return fmt.Errorf("advance workspace position: %w", err)
	}
	return tx.Commit(ctx)
}

// snapshotFamily writes one snapshot per entity of the family whose rows
// changed since the last snapshot, or that has no snapshot at its head.
func (s *Store) snapshotFamily(ctx context.Context, workspace string, family genesisFamily) (written, unchanged int, err error) {
	ids, err := s.genesisEntityIDs(ctx, workspace, family)
	if err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		didWrite, err := s.snapshotEntity(ctx, workspace, family, id)
		if err != nil {
			return written, unchanged, fmt.Errorf("%s %s: %w", family.table, id, err)
		}
		if didWrite {
			written++
		} else {
			unchanged++
		}
	}
	return written, unchanged, nil
}

func (s *Store) genesisEntityIDs(ctx context.Context, workspace string, family genesisFamily) ([]string, error) {
	var query string
	var args []any
	switch family.scope {
	case "workspace_id":
		query = fmt.Sprintf(`SELECT %s::text FROM %s WHERE workspace_id = $1 ORDER BY 1`, family.idColumn, family.table)
		args = []any{workspace}
	case "id":
		query = fmt.Sprintf(`SELECT %s::text FROM %s WHERE %s = $1 ORDER BY 1`, family.idColumn, family.table, family.idColumn)
		args = []any{workspace}
	default:
		query = fmt.Sprintf(`SELECT %s::text FROM %s ORDER BY 1`, family.idColumn, family.table)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", family.table, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// genesisSnapshot is the payload of a log.snapshot_imported event.
type genesisSnapshot struct {
	Family      string                      `json:"family"`
	Table       string                      `json:"table"`
	ID          string                      `json:"id"`
	Row         map[string]any              `json:"row"`
	Children    map[string][]map[string]any `json:"children,omitempty"`
	ContentHash string                      `json:"content_hash"`
}

func (s *Store) snapshotEntity(ctx context.Context, workspace string, family genesisFamily, id string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	stream := family.stream(id)
	if _, err := s.log.LockWorkspace(ctx, tx, workspace); err != nil {
		return false, err
	}
	head, err := s.log.LockStream(ctx, tx, workspace, stream)
	if err != nil {
		return false, err
	}
	snapshot, err := s.readGenesisSnapshot(ctx, tx, workspace, family, id)
	if err != nil {
		return false, err
	}
	if snapshot == nil {
		// The row vanished between listing and locking; nothing to snapshot.
		return false, nil
	}
	if head > 0 {
		last, err := s.log.Read(pglogCtx(ctx, tx), workspace, stream, head-1, 1)
		if err != nil {
			return false, err
		}
		if len(last) == 1 && last[0].Kind == eventlog.SnapshotImportedKind {
			var previous genesisSnapshot
			if json.Unmarshal(last[0].Payload, &previous) == nil && previous.ContentHash == snapshot.ContentHash {
				return false, nil
			}
		}
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return false, err
	}
	actor := store.ActorFromContext(ctx)
	if actor.ID == "" {
		actor = store.Actor{ID: "conveyor:migrate-log", Role: "system"}
	}
	if _, err := s.log.AppendWith(ctx, tx, workspace, stream, head, []eventlog.NewEvent{{
		Kind: eventlog.SnapshotImportedKind, ActorID: actor.ID, ActorRole: string(actor.Role), Payload: payload,
	}}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// readGenesisSnapshot reads an entity's row and child rows as JSON objects
// with excluded columns removed, and hashes the canonical encoding.
func (s *Store) readGenesisSnapshot(ctx context.Context, tx pgx.Tx, workspace string, family genesisFamily, id string) (*genesisSnapshot, error) {
	var where string
	args := []any{id}
	switch family.scope {
	case "workspace_id":
		where = fmt.Sprintf(`%s = $1 AND workspace_id = $2`, family.idColumn)
		args = append(args, workspace)
	default:
		where = fmt.Sprintf(`%s = $1`, family.idColumn)
	}
	row, err := queryJSONRows(ctx, tx, fmt.Sprintf(`SELECT row_to_json(t) FROM %s t WHERE %s`, family.table, where), args, family.exclude)
	if err != nil {
		return nil, err
	}
	if len(row) == 0 {
		return nil, nil
	}
	if len(row) > 1 {
		return nil, fmt.Errorf("%s %s: %d rows for one id", family.table, id, len(row))
	}
	snapshot := &genesisSnapshot{Family: family.family, Table: family.table, ID: id, Row: row[0]}
	for _, child := range family.children {
		childArgs := []any{id}
		childWhere := fmt.Sprintf(`c.%s = $1`, child.fk)
		if child.scoped {
			childWhere += ` AND c.workspace_id = $2`
			childArgs = append(childArgs, workspace)
		}
		rows, err := queryJSONRows(ctx, tx, fmt.Sprintf(`SELECT row_to_json(c) FROM %s c WHERE %s ORDER BY %s`, child.table, childWhere, child.orderBy), childArgs, child.exclude)
		if err != nil {
			return nil, fmt.Errorf("child %s: %w", child.table, err)
		}
		if len(rows) > 0 {
			if snapshot.Children == nil {
				snapshot.Children = map[string][]map[string]any{}
			}
			snapshot.Children[child.table] = rows
		}
	}
	hashed, err := json.Marshal(struct {
		Row      map[string]any              `json:"row"`
		Children map[string][]map[string]any `json:"children,omitempty"`
	}{snapshot.Row, snapshot.Children})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(hashed)
	snapshot.ContentHash = "sha256:" + hex.EncodeToString(sum[:])
	return snapshot, nil
}

func queryJSONRows(ctx context.Context, tx pgx.Tx, query string, args []any, exclude []string) ([]map[string]any, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("decode row json: %w", err)
		}
		for _, column := range exclude {
			delete(decoded, column)
		}
		out = append(out, decoded)
	}
	return out, rows.Err()
}

func (s *Store) writeGenesisMarker(ctx context.Context, workspace string, wr GenesisWorkspaceReport) error {
	payload, err := json.Marshal(wr)
	if err != nil {
		return err
	}
	_, err = s.log.Append(ctx, workspace, eventlog.GenesisStream, eventlog.ExpectAny, []eventlog.NewEvent{{
		Kind: eventlog.GenesisCompletedKind, ActorID: "conveyor:migrate-log", ActorRole: "system", Payload: payload,
	}})
	return err
}

// pglogCtx binds tx to the log driver's context so reads inside the
// snapshot transaction see the locked, uncommitted state.
func pglogCtx(ctx context.Context, tx pgx.Tx) context.Context {
	return pglog.WithTx(ctx, tx)
}
