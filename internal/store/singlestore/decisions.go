package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) ProposeDecision(ctx context.Context, decision core.Decision) (core.Decision, error) {
	if err := core.ValidateDecision(decision); err != nil {
		return decision, err
	}
	err := s.documentTx(ctx, func(tx *sql.Tx) error {
		if decision.ID == "" {
			var high int
			if _, err := documentExec(ctx, tx, `INSERT INTO decision_sequences(workspace_id,high_water_mark) VALUES(?,1) ON DUPLICATE KEY UPDATE high_water_mark=high_water_mark+1`, documentWorkspace(ctx)); err != nil {
				return err
			}
			if err := documentRow(ctx, tx, `SELECT high_water_mark FROM decision_sequences WHERE workspace_id=?`, documentWorkspace(ctx)).Scan(&high); err != nil {
				return err
			}
			decision.ID = "DEC-" + strconv.Itoa(high)
		} else {
			n, _ := strconv.Atoi(strings.TrimPrefix(decision.ID, "DEC-"))
			if _, err := documentExec(ctx, tx, `INSERT INTO decision_sequences(workspace_id,high_water_mark) VALUES(?,?) ON DUPLICATE KEY UPDATE high_water_mark=GREATEST(high_water_mark,VALUES(high_water_mark))`, documentWorkspace(ctx), n); err != nil {
				return err
			}
		}

		var idCount int
		if err := documentRow(ctx, tx, `SELECT COUNT(*) FROM decisions WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), decision.ID).Scan(&idCount); err != nil {
			return err
		}
		if idCount > 0 {
			return store.ErrDecisionIDConflict
		}
		if decision.Supersedes != "" {
			var status string
			if err := documentRow(ctx, tx, `SELECT status FROM decisions WHERE workspace_id=? AND id=? FOR UPDATE`, documentWorkspace(ctx), decision.Supersedes).Scan(&status); err != nil {
				return notFound(err, "decision %s", decision.Supersedes)
			}
			if status != string(core.DecisionConfirmed) {
				return fmt.Errorf("%w: decision %s can supersede only a confirmed decision", store.ErrDecisionSupersessionConflict, decision.ID)
			}
		}
		decision.Workspace, decision.Status, decision.CreatedAt = documentWorkspace(ctx), core.DecisionProposed, time.Now().UTC()
		decision.ConfirmedBy, decision.ConfirmedAt, decision.DismissedBy, decision.DismissedAt, decision.SupersededBy = "", time.Time{}, "", time.Time{}, ""
		if _, err := documentExec(ctx, tx, `INSERT INTO decisions(workspace_id,id,statement,context,alternatives_rejected,status,origin,origin_session_id,origin_task_id,supersedes,created_at) VALUES(?,?,?,?,?,'proposed',?,?,?,?,?)`, documentWorkspace(ctx), decision.ID, decision.Statement, decision.Context, decision.AlternativesRejected, string(decision.Origin), nullString(decision.OriginSessionID), nullString(decision.OriginTaskID), nullString(decision.Supersedes), decision.CreatedAt); err != nil {
			return err
		}
		return insertWorkspaceEvent(ctx, tx, core.Event{Kind: "decision.proposed", Payload: core.JSONPayload(map[string]any{"workspace_id": documentWorkspace(ctx), "decision_id": decision.ID, "origin": decision.Origin, "origin_session_id": decision.OriginSessionID, "origin_task_id": decision.OriginTaskID, "supersedes": decision.Supersedes})})
	})
	if err != nil {
		var pgErr *mysql.MySQLError
		if errors.As(err, &pgErr) && pgErr.Number == 1062 {
			if strings.Contains(pgErr.Message, "PRIMARY") {
				return decision, fmt.Errorf("%w: %s", store.ErrDecisionIDConflict, decision.ID)
			}
			if strings.Contains(pgErr.Message, "decisions_confirmed_supersedes_key") {
				return decision, fmt.Errorf("%w: %s", store.ErrDecisionSupersessionConflict, decision.Supersedes)
			}
		}
	}
	return decision, err
}

func (s *Store) ConfirmDecision(ctx context.Context, id string) (core.Decision, error) {
	var decision core.Decision
	err := s.documentTx(ctx, func(tx *sql.Tx) error {
		var err error
		decision, err = scanDecision(documentRow(ctx, tx, decisionSelect+` WHERE workspace_id=? AND id=? FOR UPDATE`, documentWorkspace(ctx), id), id)
		if err != nil {
			return err
		}
		if decision.Status == core.DecisionConfirmed {
			return nil
		}
		if decision.Status != core.DecisionProposed {
			return fmt.Errorf("%w: decision %s is %s and cannot be confirmed", store.ErrDecisionSupersessionConflict, id, decision.Status)
		}
		actor, now := store.ActorFromContext(ctx), time.Now().UTC()
		if decision.Supersedes != "" {
			var predecessorStatus string
			if err = documentRow(ctx, tx, `SELECT status FROM decisions WHERE workspace_id=? AND id=? FOR UPDATE`, documentWorkspace(ctx), decision.Supersedes).Scan(&predecessorStatus); err != nil {
				return notFound(err, "decision %s", decision.Supersedes)
			}
			if predecessorStatus != string(core.DecisionConfirmed) {
				return fmt.Errorf("%w: %s is no longer confirmed", store.ErrDecisionSupersessionConflict, decision.Supersedes)
			}
			if _, err = documentExec(ctx, tx, `UPDATE decisions SET status='superseded',superseded_by=? WHERE workspace_id=? AND id=? AND status='confirmed'`, id, documentWorkspace(ctx), decision.Supersedes); err != nil {
				return err
			}
		}
		if _, err = documentExec(ctx, tx, `UPDATE decisions SET status='confirmed',confirmed_by=?,confirmed_at=? WHERE workspace_id=? AND id=?`, actor.ID, now, documentWorkspace(ctx), id); err != nil {
			return err
		}
		decision.Status, decision.ConfirmedBy, decision.ConfirmedAt = core.DecisionConfirmed, actor.ID, now
		if err = insertWorkspaceEvent(ctx, tx, core.Event{Kind: "decision.confirmed", Payload: core.JSONPayload(map[string]any{"workspace_id": documentWorkspace(ctx), "decision_id": id, "confirmed_by": actor.ID, "supersedes": decision.Supersedes})}); err != nil {
			return err
		}
		return recomputeDecisionSupersessionSweepTx(ctx, tx, decision)
	})
	if err != nil {
		var pgErr *mysql.MySQLError
		if errors.As(err, &pgErr) && pgErr.Number == 1062 && strings.Contains(pgErr.Message, "decisions_confirmed_supersedes_key") {
			return decision, fmt.Errorf("%w: %s", store.ErrDecisionSupersessionConflict, decision.Supersedes)
		}
	}
	if err == nil {
		decision, err = s.GetDecision(ctx, id)
	}
	return decision, err
}

func (s *Store) DismissDecision(ctx context.Context, id string) (core.Decision, error) {
	var decision core.Decision
	err := s.documentTx(ctx, func(tx *sql.Tx) error {
		var err error
		decision, err = scanDecision(documentRow(ctx, tx, decisionSelect+` WHERE workspace_id=? AND id=? FOR UPDATE`, documentWorkspace(ctx), id), id)
		if err != nil {
			return err
		}
		if decision.Status == core.DecisionDismissed {
			return nil
		}
		if decision.Status != core.DecisionProposed {
			return fmt.Errorf("%w: decision %s is %s and cannot be dismissed", store.ErrDecisionSupersessionConflict, id, decision.Status)
		}
		actor, now := store.ActorFromContext(ctx), time.Now().UTC()
		if _, err = documentExec(ctx, tx, `UPDATE decisions SET status='dismissed',dismissed_by=?,dismissed_at=? WHERE workspace_id=? AND id=?`, actor.ID, now, documentWorkspace(ctx), id); err != nil {
			return err
		}
		decision.Status, decision.DismissedBy, decision.DismissedAt = core.DecisionDismissed, actor.ID, now
		return insertWorkspaceEvent(ctx, tx, core.Event{Kind: "decision.dismissed", Payload: core.JSONPayload(map[string]any{"workspace_id": documentWorkspace(ctx), "decision_id": id, "dismissed_by": actor.ID, "supersedes": decision.Supersedes})})
	})
	if err == nil {
		decision, err = s.GetDecision(ctx, id)
	}
	return decision, err
}

const decisionSelect = `SELECT workspace_id,id,statement,context,alternatives_rejected,status,origin,coalesce(origin_session_id,''),coalesce(origin_task_id,''),coalesce(supersedes,''),coalesce(confirmed_by,''),confirmed_at,coalesce(dismissed_by,''),dismissed_at,coalesce(superseded_by,''),created_at FROM decisions`

func scanDecision(row documentScanner, id string) (core.Decision, error) {
	var item core.Decision
	var status, origin string
	var confirmedAt, dismissedAt *time.Time
	err := row.Scan(&item.Workspace, &item.ID, &item.Statement, &item.Context, &item.AlternativesRejected, &status, &origin, &item.OriginSessionID, &item.OriginTaskID, &item.Supersedes, &item.ConfirmedBy, &confirmedAt, &item.DismissedBy, &dismissedAt, &item.SupersededBy, &item.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return item, fmt.Errorf("%w: decision %s", store.ErrNotFound, id)
	}
	item.Status, item.Origin = core.DecisionStatus(status), core.DecisionOrigin(origin)
	if confirmedAt != nil {
		item.ConfirmedAt = *confirmedAt
	}
	if dismissedAt != nil {
		item.DismissedAt = *dismissedAt
	}
	return item, err
}
func (s *Store) GetDecision(ctx context.Context, id string) (core.Decision, error) {
	item, err := scanDecision(documentRow(ctx, s.db, decisionSelect+` WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), id), id)
	if err != nil {
		return item, err
	}
	entries, err := s.listDecisionSupersessionSweeps(ctx, id)
	item.Sweep = decisionSupersessionSweep(entries)
	return item, err
}
func (s *Store) ListDecisions(ctx context.Context) ([]core.Decision, error) {
	rows, err := documentRows(ctx, s.db, decisionSelect+` WHERE workspace_id=? ORDER BY CAST(SUBSTRING(id,5) AS SIGNED)`, documentWorkspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Decision{}
	for rows.Next() {
		item, scanErr := scanDecision(rows, "")
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	entries, err := s.listDecisionSupersessionSweeps(ctx, "")
	if err != nil {
		return nil, err
	}
	byDecision := make(map[string][]core.DecisionSupersessionSweepEntry)
	for _, entry := range entries {
		byDecision[entry.DecisionID] = append(byDecision[entry.DecisionID], entry)
	}
	for i := range out {
		out[i].Sweep = decisionSupersessionSweep(byDecision[out[i].ID])
	}
	return out, nil
}

const decisionSupersessionSweepSelect = `SELECT decision_id,superseded_decision_id,document_tier,document_id,status,detected_by,detected_at,resolved_by,resolved_at FROM decision_supersession_sweeps`

type decisionSupersessionSweepScanner interface {
	Scan(dest ...any) error
}

func scanDecisionSupersessionSweep(row decisionSupersessionSweepScanner) (core.DecisionSupersessionSweepEntry, error) {
	var entry core.DecisionSupersessionSweepEntry
	var status string
	var resolvedAt *time.Time
	err := row.Scan(&entry.DecisionID, &entry.SupersededDecisionID, &entry.DocumentTier, &entry.DocumentID, &status, &entry.DetectedBy, &entry.DetectedAt, &entry.ResolvedBy, &resolvedAt)
	entry.Status = core.DecisionSupersessionSweepStatus(status)
	if resolvedAt != nil {
		entry.ResolvedAt = *resolvedAt
	}
	return entry, err
}

func (s *Store) listDecisionSupersessionSweeps(ctx context.Context, decisionID string) ([]core.DecisionSupersessionSweepEntry, error) {
	rows, err := documentRows(ctx, s.db, decisionSupersessionSweepSelect+` WHERE workspace_id=? AND decision_id=? ORDER BY document_tier,document_id`, documentWorkspace(ctx), decisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []core.DecisionSupersessionSweepEntry{}
	for rows.Next() {
		entry, e := scanDecisionSupersessionSweep(rows)
		if e != nil {
			return nil, e
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func decisionSupersessionSweep(entries []core.DecisionSupersessionSweepEntry) core.DecisionSupersessionSweep {
	clean := true
	for _, entry := range entries {
		clean = clean && entry.Status != core.DecisionSweepOpen
	}
	if entries == nil {
		entries = []core.DecisionSupersessionSweepEntry{}
	}
	return core.DecisionSupersessionSweep{Clean: clean, Entries: entries}
}

func recomputeDecisionSupersessionSweepTx(ctx context.Context, tx *sql.Tx, decision core.Decision) error {
	if decision.Supersedes == "" {
		return nil
	}
	rows, err := documentRows(ctx, tx, `SELECT 'requirement',r.id,v.content FROM requirements r JOIN requirement_versions v ON v.workspace_id=r.workspace_id AND v.requirement_id=r.id AND v.version=r.current_version WHERE r.workspace_id=? AND r.archived_at IS NULL AND v.confirmed
 UNION ALL SELECT 'system_design',d.id,v.content FROM system_designs d JOIN system_design_versions v ON v.workspace_id=d.workspace_id AND v.document_id=d.id AND v.version=d.current_version WHERE d.workspace_id=? AND d.archived_at IS NULL AND v.confirmed
 UNION ALL SELECT 'reference_document',d.id,v.content FROM reference_documents d JOIN reference_document_versions v ON v.workspace_id=d.workspace_id AND v.document_id=d.id AND v.version=d.current_version WHERE d.workspace_id=? AND d.deleted_at IS NULL`, documentWorkspace(ctx), documentWorkspace(ctx), documentWorkspace(ctx))
	if err != nil {
		return err
	}
	type doc struct{ tier, id, content string }
	var docs []doc
	for rows.Next() {
		var d doc
		if err = rows.Scan(&d.tier, &d.id, &d.content); err != nil {
			rows.Close()
			return err
		}
		docs = append(docs, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, d := range docs {
		if err = recomputeDecisionSweepsForDocumentTx(ctx, tx, d.tier, d.id, d.content); err != nil {
			return err
		}
	}
	return nil
}

func recomputeDecisionSweepsForDocumentTx(ctx context.Context, tx *sql.Tx, tier, documentID, content string) error {
	rows, err := documentRows(ctx, tx, `SELECT id,supersedes FROM decisions WHERE workspace_id=? AND supersedes IS NOT NULL AND confirmed_at IS NOT NULL`, documentWorkspace(ctx))
	if err != nil {
		return err
	}
	type pair struct{ id, supersedes string }
	var decisions []pair
	for rows.Next() {
		var d pair
		if err = rows.Scan(&d.id, &d.supersedes); err != nil {
			rows.Close()
			return err
		}
		decisions = append(decisions, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	actor, now := store.ActorFromContext(ctx), time.Now().UTC()
	for _, d := range decisions {
		current, e := scanDecisionSupersessionSweep(documentRow(ctx, tx, decisionSupersessionSweepSelect+` WHERE workspace_id=? AND decision_id=? AND document_tier=? AND document_id=? FOR UPDATE`, documentWorkspace(ctx), d.id, tier, documentID))
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		cited := regexp.MustCompile(`\b` + regexp.QuoteMeta(d.supersedes) + `\b`).MatchString(content)
		kind := ""
		if cited && (errors.Is(e, sql.ErrNoRows) || current.Status == core.DecisionSweepAutoCleared) {
			current = core.DecisionSupersessionSweepEntry{DecisionID: d.id, SupersededDecisionID: d.supersedes, DocumentTier: tier, DocumentID: documentID, Status: core.DecisionSweepOpen, DetectedBy: actor.ID, DetectedAt: now}
			_, err = documentExec(ctx, tx, `INSERT INTO decision_supersession_sweeps(workspace_id,decision_id,superseded_decision_id,document_tier,document_id,status,detected_by,detected_at) VALUES(?,?,?,?,?,'open',?,?) ON DUPLICATE KEY UPDATE status='open',detected_by=VALUES(detected_by),detected_at=VALUES(detected_at),resolved_by='',resolved_at=NULL`, documentWorkspace(ctx), d.id, d.supersedes, tier, documentID, actor.ID, now)
			kind = "decision.supersession_sweep_opened"
		} else if !cited && e == nil && current.Status == core.DecisionSweepOpen {
			current.Status, current.ResolvedBy, current.ResolvedAt = core.DecisionSweepAutoCleared, actor.ID, now
			_, err = documentExec(ctx, tx, `UPDATE decision_supersession_sweeps SET status='auto_cleared',resolved_by=?,resolved_at=? WHERE workspace_id=? AND decision_id=? AND document_tier=? AND document_id=?`, actor.ID, now, documentWorkspace(ctx), d.id, tier, documentID)
			kind = "decision.supersession_sweep_auto_cleared"
		}
		if err != nil {
			return err
		}
		if kind != "" {
			if err = insertWorkspaceEvent(ctx, tx, core.Event{Kind: kind, Payload: core.JSONPayload(current)}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) DismissDecisionSupersessionSweep(ctx context.Context, decisionID, documentTier, documentID string) (core.DecisionSupersessionSweepEntry, error) {
	var entry core.DecisionSupersessionSweepEntry
	err := s.documentTx(ctx, func(tx *sql.Tx) error {
		actor, now := store.ActorFromContext(ctx), time.Now().UTC()
		current, err := scanDecisionSupersessionSweep(documentRow(ctx, tx, decisionSupersessionSweepSelect+` WHERE workspace_id=? AND decision_id=? AND document_tier=? AND document_id=? FOR UPDATE`, documentWorkspace(ctx), decisionID, documentTier, documentID))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: decision sweep entry %s/%s/%s", store.ErrNotFound, decisionID, documentTier, documentID)
		}
		if err != nil {
			return err
		}
		if current.Status == core.DecisionSweepDismissed {
			entry = current
			return nil
		}
		if current.Status != core.DecisionSweepOpen {
			return fmt.Errorf("%w: entry is %s and cannot be dismissed", store.ErrDecisionSweepTransition, current.Status)
		}
		_, err = documentExec(ctx, tx, `UPDATE decision_supersession_sweeps SET status='dismissed',resolved_by=?,resolved_at=? WHERE workspace_id=? AND decision_id=? AND document_tier=? AND document_id=?`, actor.ID, now, documentWorkspace(ctx), decisionID, documentTier, documentID)
		entry = current
		entry.Status, entry.ResolvedBy, entry.ResolvedAt = core.DecisionSweepDismissed, actor.ID, now
		if err != nil {
			return err
		}
		return insertWorkspaceEvent(ctx, tx, core.Event{Kind: "decision.supersession_sweep_dismissed", Payload: core.JSONPayload(entry)})
	})
	return entry, err
}

func nullString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
