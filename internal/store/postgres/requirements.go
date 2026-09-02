package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// Requirement and planning-session persistence. Every mutation commits its projection update and audit
// event in one transaction, exactly as lifecycle transitions do (design-database).

func (s *Store) CreateRequirement(ctx context.Context, requirement core.Requirement, first core.RequirementVersion) (core.Requirement, core.RequirementVersion, error) {
	if requirement.ID == "" {
		return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("requirement id is required")
	}
	if requirement.Title == "" {
		return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("requirement title is required")
	}
	if requirement.Slug == "" {
		requirement.Slug = core.RequirementSlug(requirement.Title)
	}
	if err := core.ValidateRequirementOrigin(first); err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	if err := core.ValidateRequirementStatements(first.Statements); err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	if err := store.NormalizeRequirementVersionDocument(&first); err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	now := time.Now().UTC()
	requirement.Workspace = workspace(ctx)
	// A new document is visibly pending: current_version stays NULL until an
	// operator confirms, so nothing is silently authoritative.
	requirement.CurrentVersion = 0
	requirement.StatementHighWaterMark = core.RequirementStatementHighWaterMark(first.Statements)
	if requirement.CreatedAt.IsZero() {
		requirement.CreatedAt = now
	}
	requirement.UpdatedAt = now
	first.Workspace = requirement.Workspace
	first.RequirementID = requirement.ID
	first.Version = 1
	first.Confirmed = false
	first.ConfirmedBy = ""
	first.ConfirmedAt = time.Time{}
	first.Retired = false
	first.RetiredBy = ""
	first.RetiredAt = time.Time{}
	first.RetiredByVersion = 0
	if first.CreatedAt.IsZero() {
		first.CreatedAt = now
	}
	statements, err := marshalRequirementStatements(first.Statements)
	if err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	derivedFrom, err := json.Marshal(first.DerivedFrom)
	if err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := tx.Exec(ctx, `INSERT INTO requirements
			(workspace_id,id,slug,title,current_version,statement_high_water_mark,created_at,updated_at)
			VALUES ($1,$2,$3,$4,NULL,$5,$6,$7)`,
			requirement.Workspace, requirement.ID, requirement.Slug, requirement.Title,
			requirement.StatementHighWaterMark, requirement.CreatedAt, requirement.UpdatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO requirement_versions
			(workspace_id,requirement_id,version,content,statements_json,origin,origin_session_id,origin_task_id,origin_drift_id,confirmed,created_at,derived_from)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,false,$10,$11)`,
			first.Workspace, first.RequirementID, first.Version, first.Content, statements,
			string(first.Origin), first.OriginSessionID, first.OriginTaskID, first.OriginDriftID, first.CreatedAt, derivedFrom); err != nil {
			return err
		}
		if err := insertRequirementEvent(ctx, q, "requirement.created", map[string]any{
			"workspace_id": requirement.Workspace, "requirement_id": requirement.ID,
			"slug": requirement.Slug, "title": requirement.Title,
		}); err != nil {
			return err
		}
		return insertRequirementEvent(ctx, q, "requirement.version_proposed", map[string]any{
			"workspace_id": requirement.Workspace, "requirement_id": requirement.ID,
			"version": first.Version, "origin": first.Origin,
			"origin_session_id": first.OriginSessionID, "origin_task_id": first.OriginTaskID, "origin_drift_id": first.OriginDriftID,
			"statement_count": len(first.Statements),
		})
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == "requirements_workspace_id_slug_key" {
			return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf("%w: %s", store.ErrRequirementSlugConflict, requirement.Slug)
		}
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	return requirement, first, nil
}

func (s *Store) GetRequirement(ctx context.Context, id string) (core.Requirement, error) {
	return scanRequirement(s.pool.QueryRow(ctx, requirementSelect+` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id), id)
}

func (s *Store) ListRequirements(ctx context.Context, includeArchived bool) ([]core.Requirement, error) {
	rows, err := s.pool.Query(ctx, requirementSelect+` WHERE workspace_id=$1 AND ($2 OR archived_at IS NULL) ORDER BY title,id`, workspace(ctx), includeArchived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.Requirement{}
	for rows.Next() {
		requirement, scanErr := scanRequirement(rows, "")
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, requirement)
	}
	return out, rows.Err()
}

func (s *Store) ArchiveRequirement(ctx context.Context, id, actor string) error {
	return s.setRequirementArchived(ctx, id, actor, true)
}

func (s *Store) RestoreRequirement(ctx context.Context, id, actor string) error {
	return s.setRequirementArchived(ctx, id, actor, false)
}

func (s *Store) setRequirementArchived(ctx context.Context, id, actor string, archived bool) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var current *int
		var archivedAt *time.Time
		if err := tx.QueryRow(ctx, `SELECT current_version,archived_at FROM requirements WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), id).Scan(&current, &archivedAt); err != nil {
			return notFound(err, "requirement %s", id)
		}
		if (archivedAt != nil) == archived {
			return nil
		}
		now := time.Now().UTC()
		if archived {
			if _, err := tx.Exec(ctx, `UPDATE requirements SET archived_at=$3,archived_by=$4,updated_at=$3 WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id, now, actor); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE requirements SET archived_at=NULL,archived_by='',updated_at=$3 WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id, now); err != nil {
			return err
		}
		version := 0
		if current != nil {
			version = *current
		}
		kind := "requirement.restored"
		if archived {
			kind = "requirement.archived"
		}
		return insertRequirementEvent(ctx, q, kind, map[string]any{"workspace_id": workspace(ctx), "requirement_id": id, "version": version, "actor": actor, "at": now})
	})
}

func (s *Store) AcknowledgeRequirementStaleness(ctx context.Context, acknowledgment core.RequirementStalenessAcknowledgment) (core.RequirementStalenessAcknowledgment, error) {
	actor := store.ActorFromContext(ctx)
	if actor.Role != core.ActorHuman {
		return core.RequirementStalenessAcknowledgment{}, fmt.Errorf("requirement staleness acknowledgments require a human operator")
	}
	if acknowledgment.SignalID == "" || acknowledgment.DeliveryTaskID == "" || acknowledgment.DeliveryEventID <= 0 || acknowledgment.AcknowledgedThrough.IsZero() {
		return core.RequirementStalenessAcknowledgment{}, fmt.Errorf("complete requirement staleness acknowledgment is required")
	}
	acknowledgment.AcknowledgedBy = actor.ID
	acknowledgment.AcknowledgedAt = time.Now().UTC()
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM requirements WHERE workspace_id=$1 AND id=$2)`, workspace(ctx), acknowledgment.RequirementID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: requirement %s", store.ErrNotFound, acknowledgment.RequirementID)
		}
		return insertRequirementEvent(ctx, q, "requirement.staleness_acknowledged", map[string]any{
			"workspace_id": workspace(ctx), "requirement_id": acknowledgment.RequirementID,
			"signal_id": acknowledgment.SignalID, "delivery_task_id": acknowledgment.DeliveryTaskID,
			"delivery_event_id": acknowledgment.DeliveryEventID, "acknowledged_through": acknowledgment.AcknowledgedThrough,
			"acknowledged_by": acknowledgment.AcknowledgedBy, "acknowledged_at": acknowledgment.AcknowledgedAt,
		})
	})
	return acknowledgment, err
}

func (s *Store) ProposeRequirementVersion(ctx context.Context, version core.RequirementVersion) (core.RequirementVersion, error) {
	if err := core.ValidateRequirementOrigin(version); err != nil {
		return core.RequirementVersion{}, err
	}
	if err := store.NormalizeRequirementVersionDocument(&version); err != nil {
		return core.RequirementVersion{}, err
	}
	version.Workspace = workspace(ctx)
	version.Confirmed = false
	version.ConfirmedBy = ""
	version.ConfirmedAt = time.Time{}
	version.Retired = false
	version.RetiredBy = ""
	version.RetiredAt = time.Time{}
	version.RetiredByVersion = 0
	if version.CreatedAt.IsZero() {
		version.CreatedAt = time.Now().UTC()
	}
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		// Lock the document so concurrent proposals cannot pick the same
		// version number or race the high-water mark forward.
		var highWaterMark int
		if err := tx.QueryRow(ctx,
			`SELECT statement_high_water_mark FROM requirements
			 WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
			version.Workspace, version.RequirementID).Scan(&highWaterMark); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("requirement %s not found", version.RequirementID)
			}
			return err
		}
		if archived, archiveErr := documentArchivedTx(ctx, tx, "requirements", version.Workspace, version.RequirementID); archiveErr != nil {
			return archiveErr
		} else if archived {
			return &store.RequirementArchivedError{RequirementID: version.RequirementID}
		}
		// Every REQ-n the document has ever issued, not just its latest
		// version's, so reinstating a statement that an unconfirmed proposal
		// dropped is not mistaken for identifier reuse.
		var latestVersion int
		var issued []string
		if err := tx.QueryRow(ctx,
			`SELECT coalesce(max(rv.version), 0),
			        coalesce(array_agg(DISTINCT ids.id) FILTER (WHERE ids.id IS NOT NULL), '{}')
			 FROM requirement_versions rv
			 LEFT JOIN LATERAL jsonb_array_elements(rv.statements_json) statement ON true
			 LEFT JOIN LATERAL (
			   SELECT statement->>'id' AS id
			   UNION ALL
			   SELECT criterion->>'id' FROM jsonb_array_elements(coalesce(statement->'acceptance_criteria','[]'::jsonb)) criterion
			 ) ids ON true
			 WHERE rv.workspace_id=$1 AND rv.requirement_id=$2`,
			version.Workspace, version.RequirementID).Scan(&latestVersion, &issued); err != nil {
			return err
		}
		if version.Origin == core.RequirementOriginImplementation {
			rows, queryErr := tx.Query(ctx, requirementVersionSelect+`
				WHERE workspace_id=$1 AND requirement_id=$2 AND origin=$3 AND origin_task_id=$4
				  AND NOT confirmed AND NOT retired ORDER BY version`, version.Workspace, version.RequirementID,
				string(version.Origin), version.OriginTaskID)
			if queryErr != nil {
				return queryErr
			}
			for rows.Next() {
				existing, scanErr := scanRequirementVersionRow(rows)
				if scanErr != nil {
					rows.Close()
					return scanErr
				}
				if existing.Content == version.Content {
					rows.Close()
					existing.Deduplicated = true
					version = existing
					return nil
				}
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				rows.Close()
				return rowsErr
			}
			rows.Close()
		}
		if err := core.ValidateRequirementRevision(highWaterMark, issued, version.Statements); err != nil {
			return err
		}
		version.Version = latestVersion + 1
		statements, err := marshalRequirementStatements(version.Statements)
		if err != nil {
			return err
		}
		derivedFrom, err := json.Marshal(version.DerivedFrom)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO requirement_versions
			(workspace_id,requirement_id,version,content,statements_json,origin,origin_session_id,origin_task_id,origin_drift_id,confirmed,created_at,derived_from)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,false,$10,$11)`,
			version.Workspace, version.RequirementID, version.Version, version.Content, statements,
			string(version.Origin), version.OriginSessionID, version.OriginTaskID, version.OriginDriftID, version.CreatedAt, derivedFrom); err != nil {
			return err
		}
		if mark := core.RequirementStatementHighWaterMark(version.Statements); mark > highWaterMark {
			highWaterMark = mark
		}
		if _, err := tx.Exec(ctx,
			`UPDATE requirements SET statement_high_water_mark=$3, updated_at=now()
			 WHERE workspace_id=$1 AND id=$2`,
			version.Workspace, version.RequirementID, highWaterMark); err != nil {
			return err
		}
		return insertRequirementEvent(ctx, q, "requirement.version_proposed", map[string]any{
			"workspace_id": version.Workspace, "requirement_id": version.RequirementID,
			"version": version.Version, "origin": version.Origin,
			"origin_session_id": version.OriginSessionID, "origin_task_id": version.OriginTaskID, "origin_drift_id": version.OriginDriftID,
			"statement_count": len(version.Statements),
		})
	})
	if err != nil {
		return core.RequirementVersion{}, err
	}
	return version, nil
}

func (s *Store) ConfirmRequirementVersion(ctx context.Context, requirementID string, version int, expectedCurrentVersion ...int) (core.Requirement, core.RequirementVersion, error) {
	var (
		requirement core.Requirement
		confirmed   core.RequirementVersion
	)
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if len(expectedCurrentVersion) > 1 {
			return fmt.Errorf("at most one expected current requirement version may be supplied")
		}
		var currentVersion *int32
		var highWaterMark int
		if err := tx.QueryRow(ctx,
			`SELECT current_version, statement_high_water_mark FROM requirements
			 WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
			workspace(ctx), requirementID).Scan(&currentVersion, &highWaterMark); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("requirement %s not found", requirementID)
			}
			return err
		}
		if archived, archiveErr := documentArchivedTx(ctx, tx, "requirements", workspace(ctx), requirementID); archiveErr != nil {
			return archiveErr
		} else if archived {
			return &store.RequirementArchivedError{RequirementID: requirementID}
		}
		current := 0
		if currentVersion != nil {
			current = int(*currentVersion)
		}
		if len(expectedCurrentVersion) == 1 && expectedCurrentVersion[0] != current {
			expected := expectedCurrentVersion[0]
			return &store.RequirementVersionConflict{
				RequirementID: requirementID, Requested: version, Current: current, Expected: &expected,
			}
		}
		stored, err := scanRequirementVersion(tx.QueryRow(ctx, requirementVersionSelect+
			` WHERE workspace_id=$1 AND requirement_id=$2 AND version=$3`,
			workspace(ctx), requirementID, version), requirementID, version)
		if err != nil {
			return err
		}
		if stored.Confirmed && version == current {
			confirmed = stored
			requirement, err = getRequirementTx(ctx, tx, requirementID)
			return err
		}
		if stored.Retired {
			return &store.RequirementVersionSuperseded{
				RequirementID: requirementID, Requested: version, Current: current,
				SupersededBy: stored.RetiredByVersion,
			}
		}
		// Confirmation is where a real statement block becomes mandatory, so a
		// migration seed cannot become current intent unedited.
		if err := core.ConfirmableRequirementVersion(stored); err != nil {
			return err
		}
		// Confirmation moves forward only: re-confirming a superseded version
		// would silently revert intent the operator already advanced past.
		if currentVersion != nil && version < int(*currentVersion) {
			return &store.RequirementVersionSuperseded{
				RequirementID: requirementID, Requested: version, Current: int(*currentVersion),
				SupersededBy: int(*currentVersion),
			}
		}
		actor := store.ActorFromContext(ctx)
		now := time.Now().UTC()
		retiredRows, err := tx.Query(ctx, `UPDATE requirement_versions
			SET retired=true, retired_by=$4, retired_at=$5, retired_by_version=$3
			WHERE workspace_id=$1 AND requirement_id=$2 AND version<$3
			  AND NOT confirmed AND NOT retired
			RETURNING version`, workspace(ctx), requirementID, version, actor.ID, now)
		if err != nil {
			return err
		}
		var retiredVersions []int
		for retiredRows.Next() {
			var retiredVersion int
			if err = retiredRows.Scan(&retiredVersion); err != nil {
				retiredRows.Close()
				return err
			}
			retiredVersions = append(retiredVersions, retiredVersion)
		}
		if err = retiredRows.Err(); err != nil {
			retiredRows.Close()
			return err
		}
		retiredRows.Close()
		for _, retiredVersion := range retiredVersions {
			if err = insertRequirementEvent(ctx, q, "requirement.version_retired", map[string]any{
				"workspace_id": workspace(ctx), "requirement_id": requirementID, "version": retiredVersion,
				"retired_by": actor.ID, "confirmed_version": version,
			}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE requirement_versions SET confirmed=true, confirmed_by=$4, confirmed_at=$5
			 WHERE workspace_id=$1 AND requirement_id=$2 AND version=$3`,
			workspace(ctx), requirementID, version, actor.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE requirements SET current_version=$3, updated_at=$4
			 WHERE workspace_id=$1 AND id=$2`,
			workspace(ctx), requirementID, version, now); err != nil {
			return err
		}
		stored.Confirmed, stored.ConfirmedBy, stored.ConfirmedAt = true, actor.ID, now
		confirmed = stored
		if requirement, err = getRequirementTx(ctx, tx, requirementID); err != nil {
			return err
		}
		payload := map[string]any{
			"workspace_id": workspace(ctx), "requirement_id": requirementID,
			"version": version, "origin": stored.Origin, "confirmed_by": actor.ID,
			"supersedes_version": current,
		}
		if stored.DerivedFrom != nil {
			payload["derived_document_id"], payload["derived_document_version"], payload["derived_section_anchor"], payload["derived_target_id"] = stored.DerivedFrom.DocumentID, stored.DerivedFrom.Version, stored.DerivedFrom.SectionAnchor, stored.DerivedFrom.TargetID
		}
		if err = insertRequirementEvent(ctx, q, "requirement.version_confirmed", payload); err != nil {
			return err
		}
		if err = recomputeDecisionSweepsForDocumentTx(ctx, tx, q, core.DecisionSweepTierRequirement, requirementID, confirmed.Content); err != nil {
			return err
		}
		return activatePendingTaskContextTx(ctx, tx, q, workspace(ctx), requirementID, version, false)
	})
	if err != nil {
		return core.Requirement{}, core.RequirementVersion{}, err
	}
	return requirement, confirmed, nil
}

func (s *Store) DismissRequirementVersion(ctx context.Context, requirementID string, version int) (core.Requirement, core.RequirementVersion, error) {
	var (
		requirement core.Requirement
		dismissed   core.RequirementVersion
	)
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var currentVersion *int32
		if err := tx.QueryRow(ctx, `SELECT current_version FROM requirements
			WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), requirementID).Scan(&currentVersion); err != nil {
			return notFound(err, "requirement %s", requirementID)
		}
		current := 0
		if currentVersion != nil {
			current = int(*currentVersion)
		}
		var err error
		dismissed, err = scanRequirementVersion(tx.QueryRow(ctx, requirementVersionSelect+
			` WHERE workspace_id=$1 AND requirement_id=$2 AND version=$3`,
			workspace(ctx), requirementID, version), requirementID, version)
		if err != nil {
			return err
		}
		if dismissed.Confirmed {
			return &store.RequirementVersionDismissalConflict{RequirementID: requirementID, Requested: version, Current: current, Reason: store.VersionDismissalConfirmed}
		}
		if dismissed.Retired {
			reason := store.VersionDismissalDismissed
			if dismissed.RetiredByVersion > 0 {
				reason = store.VersionDismissalSuperseded
			}
			return &store.RequirementVersionDismissalConflict{RequirementID: requirementID, Requested: version, Current: current, Reason: reason, SupersededBy: dismissed.RetiredByVersion}
		}
		if version < current {
			return &store.RequirementVersionDismissalConflict{RequirementID: requirementID, Requested: version, Current: current, Reason: store.VersionDismissalSuperseded, SupersededBy: current}
		}
		actor, now := store.ActorFromContext(ctx), time.Now().UTC()
		if _, err = tx.Exec(ctx, `UPDATE requirement_versions
			SET retired=true, retired_by=$4, retired_at=$5, retired_by_version=NULL
			WHERE workspace_id=$1 AND requirement_id=$2 AND version=$3`,
			workspace(ctx), requirementID, version, actor.ID, now); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE requirements SET updated_at=$3
			WHERE workspace_id=$1 AND id=$2`, workspace(ctx), requirementID, now); err != nil {
			return err
		}
		dismissed.Retired, dismissed.RetiredBy, dismissed.RetiredAt, dismissed.RetiredByVersion = true, actor.ID, now, 0
		if requirement, err = getRequirementTx(ctx, tx, requirementID); err != nil {
			return err
		}
		return insertRequirementEvent(ctx, q, "requirement.version_dismissed", map[string]any{
			"workspace_id": workspace(ctx), "requirement_id": requirementID, "version": version, "dismissed_by": actor.ID,
		})
	})
	return requirement, dismissed, err
}

func (s *Store) GetRequirementVersion(ctx context.Context, requirementID string, version int) (core.RequirementVersion, error) {
	return scanRequirementVersion(s.pool.QueryRow(ctx, requirementVersionSelect+
		` WHERE workspace_id=$1 AND requirement_id=$2 AND version=$3`,
		workspace(ctx), requirementID, version), requirementID, version)
}

func (s *Store) ListRequirementVersions(ctx context.Context, requirementID string) ([]core.RequirementVersion, error) {
	rows, err := s.pool.Query(ctx, requirementVersionSelect+
		` WHERE workspace_id=$1 AND requirement_id=$2 ORDER BY version`, workspace(ctx), requirementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.RequirementVersion{}
	for rows.Next() {
		version, err := scanRequirementVersionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, version)
	}
	return out, rows.Err()
}

func (s *Store) ListRequirementVersionsByRequirement(ctx context.Context) (map[string][]core.RequirementVersion, error) {
	rows, err := s.pool.Query(ctx, requirementVersionSelect+
		` WHERE workspace_id=$1 ORDER BY requirement_id,version`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]core.RequirementVersion{}
	for rows.Next() {
		version, err := scanRequirementVersionRow(rows)
		if err != nil {
			return nil, err
		}
		out[version.RequirementID] = append(out[version.RequirementID], version)
	}
	return out, rows.Err()
}

func (s *Store) ProposeRequirementServes(ctx context.Context, blueprintTaskID, requirementID string, source core.RequirementServesSource, confirm bool) (core.RequirementServesLink, error) {
	proposal, _, err := s.proposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: blueprintTaskID, TargetKind: core.TaskContextProposalRequirement,
		TargetID: requirementID, Source: core.TaskContextProposalSource(source), Justification: "This confirmed requirement is relevant task context."}, true)
	if err != nil {
		return core.RequirementServesLink{}, err
	}
	if confirm {
		proposal, err = s.transitionTaskContextProposal(ctx, blueprintTaskID, core.TaskContextProposalRequirement, requirementID, core.TaskContextProposalConfirmed, true)
	}
	return requirementServesFromTaskContextProposal(proposal), err
}

func (s *Store) ConfirmRequirementServes(ctx context.Context, blueprintTaskID, requirementID string) (core.RequirementServesLink, error) {
	proposal, err := s.transitionTaskContextProposal(ctx, blueprintTaskID, core.TaskContextProposalRequirement, requirementID, core.TaskContextProposalConfirmed, true)
	return requirementServesFromTaskContextProposal(proposal), err
}

func (s *Store) DismissRequirementServes(ctx context.Context, blueprintTaskID, requirementID string) (core.RequirementServesLink, error) {
	proposal, err := s.transitionTaskContextProposal(ctx, blueprintTaskID, core.TaskContextProposalRequirement, requirementID, core.TaskContextProposalDismissed, true)
	return requirementServesFromTaskContextProposal(proposal), err
}

func (s *Store) ListRequirementServes(ctx context.Context) ([]core.RequirementServesLink, error) {
	proposals, err := s.ListTaskContextProposals(ctx, "", "")
	if err != nil {
		return nil, err
	}
	links := []core.RequirementServesLink{}
	for _, proposal := range proposals {
		if proposal.TargetKind == core.TaskContextProposalRequirement {
			links = append(links, requirementServesFromTaskContextProposal(proposal))
		}
	}
	return links, nil
}

func requirementServesFromTaskContextProposal(proposal core.TaskContextProposal) core.RequirementServesLink {
	return core.RequirementServesLink{BlueprintTaskID: proposal.TaskID, RequirementID: proposal.TargetID, State: core.RequirementServesState(proposal.State),
		Source: core.RequirementServesSource(proposal.Source), CreatedByEventID: proposal.CreatedByEventID, DecisionEventID: proposal.DecisionEventID,
		ProposedBy: proposal.ProposedBy, DecidedBy: proposal.DecidedBy, Workspace: proposal.Workspace, CreatedAt: proposal.CreatedAt, UpdatedAt: proposal.UpdatedAt}
}

func (s *Store) CreatePlanningSession(ctx context.Context, session core.PlanningSession) (core.PlanningSession, error) {
	if session.ID == "" {
		return core.PlanningSession{}, fmt.Errorf("planning session id is required")
	}
	now := time.Now().UTC()
	session.Workspace = workspace(ctx)
	session.Status = core.PlanningSessionActive
	session.ProducedRequirementID = ""
	session.ProducedTaskID = ""
	session.ProducedSystemDesignID = ""
	session.TranscriptArtifactID = ""
	session.FinalizedAt = time.Time{}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	session.UpdatedAt = now
	if session.PinnedRevisions == nil {
		session.PinnedRevisions = map[string]string{}
	}
	goal, err := core.NormalizePlanningSessionGoal(session.Goal)
	if err != nil {
		return core.PlanningSession{}, err
	}
	session.Goal = goal
	pins, err := json.Marshal(session.PinnedRevisions)
	if err != nil {
		return core.PlanningSession{}, fmt.Errorf("encode planning revisions: %w", err)
	}
	promotion, err := json.Marshal(session.Promotion)
	if err != nil {
		return core.PlanningSession{}, fmt.Errorf("encode planning promotion: %w", err)
	}
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := tx.Exec(ctx, `INSERT INTO planning_sessions
			(workspace_id,id,title,status,goal,requirement_context_id,system_design_context_id,model,effort,
			 exploration_output_tokens,exploration_tokens_used,primary_repo,pinned_revisions,
			 promotion,created_at,updated_at)
			VALUES ($1,$2,$3,'active',$4,NULLIF($5,''),NULLIF($6,''),$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			session.Workspace, session.ID, session.Title, string(session.Goal),
			session.RequirementContextID, session.SystemDesignContextID,
			session.Model, session.Effort, session.ExplorationOutputTokens,
			session.ExplorationTokensUsed, session.PrimaryRepo, pins, promotion,
			session.CreatedAt, session.UpdatedAt); err != nil {
			return err
		}
		return insertRequirementEvent(ctx, q, "planning_session.created", map[string]any{
			"workspace_id": session.Workspace, "session_id": session.ID, "title": session.Title,
			"requirement_context_id":   session.RequirementContextID,
			"system_design_context_id": session.SystemDesignContextID,
			"goal":                     string(session.Goal),
			"promotion":                session.Promotion,
			"model":                    session.Model, "effort": session.Effort,
			"exploration_output_tokens": session.ExplorationOutputTokens,
			"primary_repo":              session.PrimaryRepo, "pinned_revisions": session.PinnedRevisions,
		})
	})
	if err != nil {
		return core.PlanningSession{}, err
	}
	return session, nil
}

func (s *Store) GetPlanningSession(ctx context.Context, id string) (core.PlanningSession, error) {
	return scanPlanningSession(s.pool.QueryRow(ctx, planningSessionSelect+
		` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id), id)
}

func (s *Store) ListPlanningSessions(ctx context.Context) ([]core.PlanningSession, error) {
	rows, err := s.pool.Query(ctx, planningSessionSelect+
		` WHERE workspace_id=$1 ORDER BY updated_at DESC, id`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.PlanningSession{}
	for rows.Next() {
		session, err := scanPlanningSessionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

func (s *Store) PinPlanningSessionRepo(ctx context.Context, sessionID, repo, revision string) (core.PlanningSession, error) {
	var session core.PlanningSession
	var conflict error
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		existing, err := scanPlanningSession(tx.QueryRow(ctx, planningSessionSelect+
			` WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), sessionID), sessionID)
		if err != nil {
			return err
		}
		if existing.Status != core.PlanningSessionActive {
			return fmt.Errorf("planning session %s is %s and cannot pin repositories", sessionID, existing.Status)
		}
		if pinned := existing.PinnedRevisions[repo]; pinned != "" {
			session = existing
			if pinned != revision {
				conflict = fmt.Errorf(
					"planning repository %s is already pinned at %s; cannot repin at %s", repo, pinned, revision)
			}
			return nil
		}
		if _, err = tx.Exec(ctx, `UPDATE planning_sessions
			SET pinned_revisions=jsonb_set(pinned_revisions, ARRAY[$3], to_jsonb($4::text), true),
			    updated_at=now()
			WHERE workspace_id=$1 AND id=$2`, workspace(ctx), sessionID, repo, revision); err != nil {
			return err
		}
		if err = insertRequirementEvent(ctx, q, "planning_session.repo_pinned", map[string]any{
			"workspace_id": workspace(ctx), "session_id": sessionID,
			"repo": repo, "revision": revision,
		}); err != nil {
			return err
		}
		session, err = scanPlanningSession(tx.QueryRow(ctx, planningSessionSelect+
			` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), sessionID), sessionID)
		return err
	})
	if err == nil && conflict != nil {
		return session, conflict
	}
	return session, err
}

func (s *Store) RecordPlanningExplorationTokens(ctx context.Context, sessionID string, tokens int) (core.PlanningSession, error) {
	if tokens < 0 {
		return core.PlanningSession{}, fmt.Errorf("planning exploration tokens must not be negative")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE planning_sessions
		SET exploration_tokens_used=exploration_tokens_used+$3, updated_at=now()
		WHERE workspace_id=$1 AND id=$2`, workspace(ctx), sessionID, tokens); err != nil {
		return core.PlanningSession{}, err
	}
	return s.GetPlanningSession(ctx, sessionID)
}

func (s *Store) AppendPlanningMessage(ctx context.Context, message core.PlanningMessage) (core.PlanningMessage, error) {
	if !message.Role.Valid() {
		return core.PlanningMessage{}, fmt.Errorf("invalid planning message role %q", message.Role)
	}
	message.Workspace = workspace(ctx)
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	}
	if len(message.Parts) == 0 {
		message.Parts = json.RawMessage(`[]`)
	}
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		// Lock the session so concurrent appends cannot claim the same sequence.
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM planning_sessions WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
			message.Workspace, message.SessionID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("planning session %s not found", message.SessionID)
			}
			return err
		}
		if core.PlanningSessionStatus(status) != core.PlanningSessionActive {
			return fmt.Errorf("planning session %s is %s and accepts no further messages", message.SessionID, status)
		}
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(max(seq),0)+1 FROM planning_messages
			 WHERE workspace_id=$1 AND session_id=$2`,
			message.Workspace, message.SessionID).Scan(&message.Seq); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO planning_messages
			(workspace_id,session_id,seq,role,content,parts_json,created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			message.Workspace, message.SessionID, message.Seq, string(message.Role),
			message.Content, []byte(message.Parts), message.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE planning_sessions SET updated_at=$3 WHERE workspace_id=$1 AND id=$2`,
			message.Workspace, message.SessionID, message.CreatedAt); err != nil {
			return err
		}
		return insertRequirementEvent(ctx, q, "planning_session.message_appended", map[string]any{
			"workspace_id": message.Workspace, "session_id": message.SessionID,
			"seq": message.Seq, "role": message.Role,
		})
	})
	if err != nil {
		return core.PlanningMessage{}, err
	}
	return message, nil
}

func (s *Store) ListPlanningMessages(ctx context.Context, sessionID string) ([]core.PlanningMessage, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT workspace_id,session_id,seq,role,content,parts_json,created_at
		 FROM planning_messages WHERE workspace_id=$1 AND session_id=$2 ORDER BY seq`,
		workspace(ctx), sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.PlanningMessage{}
	for rows.Next() {
		var message core.PlanningMessage
		var role string
		var parts []byte
		if err := rows.Scan(&message.Workspace, &message.SessionID, &message.Seq, &role,
			&message.Content, &parts, &message.CreatedAt); err != nil {
			return nil, err
		}
		message.Role = core.PlanningMessageRole(role)
		message.Parts = json.RawMessage(parts)
		out = append(out, message)
	}
	return out, rows.Err()
}

func (s *Store) FinalizePlanningSession(ctx context.Context, request store.PlanningFinalizeRequest) (core.PlanningSession, error) {
	if err := request.Validate(); err != nil {
		return core.PlanningSession{}, err
	}
	var session core.PlanningSession
	err := s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		existing, err := scanPlanningSession(tx.QueryRow(ctx, planningSessionSelect+
			` WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(ctx), request.SessionID), request.SessionID)
		if err != nil {
			return err
		}
		if existing.Status == core.PlanningSessionFinalized {
			// Idempotent for an identical finalize; any difference in the
			// recorded lineage — produced artifact or archived transcript — is
			// a contradiction, not a retry, so the stored lineage stands.
			if existing.ProducedRequirementID == request.RequirementID &&
				existing.ProducedTaskID == request.TaskID &&
				existing.ProducedSystemDesignID == request.SystemDesignID &&
				existing.ProducedBundleID == request.BundleID &&
				existing.TranscriptArtifactID == request.TranscriptArtifactID {
				session = existing
				return nil
			}
			return fmt.Errorf(
				"planning session %s is already finalized with different lineage", request.SessionID)
		}
		if existing.Status != core.PlanningSessionActive {
			// The row lock serializes finalize against abandon. When abandon
			// wins, its terminal state must not be overwritten by the
			// in-flight planning run.
			return fmt.Errorf(
				"planning session %s is %s and cannot be finalized", request.SessionID, existing.Status)
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE planning_sessions
			SET status='finalized', produced_requirement_id=NULLIF($3,''),
			    produced_task_id=NULLIF($4,''), produced_system_design_id=NULLIF($5,''), produced_bundle_id=NULLIF($9,''), transcript_artifact_id=NULLIF($6,''),
			    title=COALESCE(NULLIF($8,''),title),
			    finalized_at=$7, updated_at=$7
			WHERE workspace_id=$1 AND id=$2`,
			workspace(ctx), request.SessionID, request.RequirementID, request.TaskID, request.SystemDesignID,
			request.TranscriptArtifactID, now, strings.TrimSpace(request.Title), request.BundleID); err != nil {
			return err
		}
		if session, err = scanPlanningSession(tx.QueryRow(ctx, planningSessionSelect+
			` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), request.SessionID), request.SessionID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE artifact_links
			SET planning_session_id=NULL, requirement_id=NULLIF($3,''), task_id=NULLIF($4,'')
			WHERE workspace_id=$1 AND planning_session_id=$2 AND artifact_id<>$5`,
			workspace(ctx), session.ID, session.ProducedRequirementID, session.ProducedTaskID, session.TranscriptArtifactID); err != nil {
			return err
		}
		if err = insertRequirementEvent(ctx, q, "planning_session.finalized", map[string]any{
			"workspace_id": workspace(ctx), "session_id": session.ID, "title": session.Title,
			"produced_requirement_id":   session.ProducedRequirementID,
			"produced_task_id":          session.ProducedTaskID,
			"produced_system_design_id": session.ProducedSystemDesignID,
			"produced_bundle_id":        session.ProducedBundleID,
			"transcript_artifact_id":    session.TranscriptArtifactID,
		}); err != nil {
			return err
		}
		if session.RequirementContextID == "" || session.ProducedTaskID == "" {
			return nil
		}
		if _, linkErr := getTaskContextProposalTx(ctx, tx, session.ProducedTaskID, core.TaskContextProposalRequirement, session.RequirementContextID, true); linkErr == nil {
			return nil
		} else if !errors.Is(linkErr, pgx.ErrNoRows) {
			return linkErr
		}
		var slug, title string
		if err = tx.QueryRow(ctx, `SELECT slug,title FROM requirements WHERE workspace_id=$1 AND id=$2`, workspace(ctx), session.RequirementContextID).Scan(&slug, &title); err != nil {
			return err
		}
		eventID, err := insertEventWithID(ctx, q, core.Event{TaskID: session.ProducedTaskID, Kind: "task.requirement_suggested", Payload: core.JSONPayload(map[string]any{
			"requirement_id": session.RequirementContextID, "requirement_slug": slug,
			"requirement_title": title, "source": core.RequirementServesPlanning,
		})})
		if err != nil {
			return err
		}
		actor := store.ActorFromContext(ctx)
		_, err = tx.Exec(ctx, `INSERT INTO task_context_proposals
			(workspace_id,task_id,target_kind,target_id,target_title,state,source,justification,created_by_event_id,proposed_by,created_at,updated_at)
			VALUES ($1,$2,'requirement',$3,$4,'proposed','planning',$5,$6,$7,$8,$8)`,
			workspace(ctx), session.ProducedTaskID, session.RequirementContextID, title,
			"This planning requirement is relevant task context.", eventID, actor.ID, now)
		return err
	})
	if err != nil {
		return core.PlanningSession{}, err
	}
	return session, nil
}

func (s *Store) AbandonPlanningSession(ctx context.Context, sessionID string, reasons ...string) (core.PlanningSession, error) {
	var session core.PlanningSession
	err := s.withPlanningSessionLock(ctx, sessionID, func(lockedCtx context.Context) error {
		return s.inTx(lockedCtx, func(tx pgx.Tx, q *db.Queries) error {
			existing, err := scanPlanningSession(tx.QueryRow(lockedCtx, planningSessionSelect+
				` WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace(lockedCtx), sessionID), sessionID)
			if err != nil {
				return err
			}
			if existing.Status == core.PlanningSessionAbandoned {
				session = existing
				return nil
			}
			if existing.Status == core.PlanningSessionFinalized {
				// Abandoning would strand what the session produced.
				return fmt.Errorf("planning session %s is already finalized", sessionID)
			}
			if _, err := tx.Exec(lockedCtx, `UPDATE planning_sessions
				SET status='abandoned', updated_at=$3
				WHERE workspace_id=$1 AND id=$2`,
				workspace(lockedCtx), sessionID, time.Now().UTC()); err != nil {
				return err
			}
			if session, err = scanPlanningSession(tx.QueryRow(lockedCtx, planningSessionSelect+
				` WHERE workspace_id=$1 AND id=$2`, workspace(lockedCtx), sessionID), sessionID); err != nil {
				return err
			}
			payload := map[string]any{"workspace_id": workspace(lockedCtx), "session_id": session.ID}
			if reason := firstTrimmed(reasons); reason != "" {
				payload["reason"] = reason
			}
			return insertRequirementEvent(lockedCtx, q, "planning_session.abandoned", payload)
		})
	})
	if err != nil {
		return core.PlanningSession{}, err
	}
	return session, nil
}

func firstTrimmed(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func (s *Store) ListPlanningSessionEvents(ctx context.Context, sessionID string) ([]core.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,COALESCE(task_id,''),COALESCE(job_id,''),kind,actor_id,actor_role,payload_json,at
		FROM events WHERE workspace_id=$1 AND payload_json->>'session_id'=$2 ORDER BY id`, workspace(ctx), sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Event
	for rows.Next() {
		var event core.Event
		if err := rows.Scan(&event.ID, &event.TaskID, &event.JobID, &event.Kind, &event.ActorID, &event.ActorRole, &event.Payload, &event.At); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

const requirementSelect = `SELECT workspace_id,id,slug,title,current_version,statement_high_water_mark,archived_at,archived_by,created_at,updated_at FROM requirements`

const requirementVersionSelect = `SELECT workspace_id,requirement_id,version,content,statements_json,origin,origin_session_id,origin_task_id,origin_drift_id,confirmed,confirmed_by,confirmed_at,retired,retired_by,retired_at,retired_by_version,created_at,derived_from FROM requirement_versions`

const planningSessionSelect = `SELECT workspace_id,id,title,status,goal,COALESCE(requirement_context_id,''),
	COALESCE(system_design_context_id,''),COALESCE(produced_requirement_id,''),COALESCE(produced_task_id,''),COALESCE(produced_system_design_id,''),COALESCE(produced_bundle_id,''),
	COALESCE(transcript_artifact_id,''),model,effort,exploration_output_tokens,
	exploration_tokens_used,primary_repo,pinned_revisions,promotion,created_at,updated_at,finalized_at
	FROM planning_sessions`

func scanRequirement(row pgx.Row, id string) (core.Requirement, error) {
	var requirement core.Requirement
	var currentVersion *int32
	var archivedAt *time.Time
	if err := row.Scan(&requirement.Workspace, &requirement.ID, &requirement.Slug, &requirement.Title,
		&currentVersion, &requirement.StatementHighWaterMark, &archivedAt, &requirement.ArchivedBy, &requirement.CreatedAt, &requirement.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Requirement{}, fmt.Errorf("%w: requirement %s", store.ErrNotFound, id)
		}
		return core.Requirement{}, err
	}
	if currentVersion != nil {
		requirement.CurrentVersion = int(*currentVersion)
	}
	if archivedAt != nil {
		requirement.Archived, requirement.ArchivedAt = true, *archivedAt
	}
	return requirement, nil
}

func getRequirementTx(ctx context.Context, tx pgx.Tx, id string) (core.Requirement, error) {
	exists, err := archivalColumnExistsTx(ctx, tx, "requirements")
	if err != nil {
		return core.Requirement{}, err
	}
	if exists {
		return scanRequirement(tx.QueryRow(ctx, requirementSelect+` WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id), id)
	}
	var requirement core.Requirement
	var currentVersion *int32
	err = tx.QueryRow(ctx, `SELECT workspace_id,id,slug,title,current_version,statement_high_water_mark,created_at,updated_at FROM requirements WHERE workspace_id=$1 AND id=$2`, workspace(ctx), id).Scan(&requirement.Workspace, &requirement.ID, &requirement.Slug, &requirement.Title, &currentVersion, &requirement.StatementHighWaterMark, &requirement.CreatedAt, &requirement.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Requirement{}, fmt.Errorf("%w: requirement %s", store.ErrNotFound, id)
	}
	if currentVersion != nil {
		requirement.CurrentVersion = int(*currentVersion)
	}
	return requirement, err
}

func scanRequirementVersion(row pgx.Row, requirementID string, version int) (core.RequirementVersion, error) {
	stored, err := scanRequirementVersionRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.RequirementVersion{}, fmt.Errorf("%w: requirement %s has no version %d", store.ErrNotFound, requirementID, version)
	}
	return stored, err
}

func scanRequirementVersionRow(row pgx.Row) (core.RequirementVersion, error) {
	var stored core.RequirementVersion
	var origin string
	var statements []byte
	var confirmedBy string
	var confirmedAt *time.Time
	var retiredBy string
	var retiredAt *time.Time
	var retiredByVersion *int32
	var derivedFrom []byte
	if err := row.Scan(&stored.Workspace, &stored.RequirementID, &stored.Version, &stored.Content,
		&statements, &origin, &stored.OriginSessionID, &stored.OriginTaskID, &stored.OriginDriftID,
		&stored.Confirmed, &confirmedBy, &confirmedAt, &stored.Retired, &retiredBy, &retiredAt,
		&retiredByVersion, &stored.CreatedAt, &derivedFrom); err != nil {
		return core.RequirementVersion{}, err
	}
	parsed, err := unmarshalRequirementStatements(statements)
	if err != nil {
		return core.RequirementVersion{}, err
	}
	stored.Statements = parsed
	stored.Origin = core.RequirementOrigin(origin)
	if len(derivedFrom) > 0 && string(derivedFrom) != "null" {
		var derivation core.RequirementDerivation
		if err := json.Unmarshal(derivedFrom, &derivation); err != nil {
			return core.RequirementVersion{}, err
		}
		stored.DerivedFrom = &derivation
	}
	stored.ConfirmedBy = confirmedBy
	if confirmedAt != nil {
		stored.ConfirmedAt = *confirmedAt
	}
	stored.RetiredBy = retiredBy
	if retiredAt != nil {
		stored.RetiredAt = *retiredAt
	}
	if retiredByVersion != nil {
		stored.RetiredByVersion = int(*retiredByVersion)
	}
	return stored, nil
}

func scanPlanningSession(row pgx.Row, id string) (core.PlanningSession, error) {
	session, err := scanPlanningSessionRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.PlanningSession{}, fmt.Errorf("planning session %s not found", id)
	}
	return session, err
}

func scanPlanningSessionRow(row pgx.Row) (core.PlanningSession, error) {
	var session core.PlanningSession
	var status string
	var goal string
	var finalizedAt *time.Time
	var pins []byte
	var promotion []byte
	if err := row.Scan(&session.Workspace, &session.ID, &session.Title, &status, &goal,
		&session.RequirementContextID, &session.SystemDesignContextID, &session.ProducedRequirementID, &session.ProducedTaskID, &session.ProducedSystemDesignID, &session.ProducedBundleID,
		&session.TranscriptArtifactID, &session.Model, &session.Effort,
		&session.ExplorationOutputTokens, &session.ExplorationTokensUsed,
		&session.PrimaryRepo, &pins, &promotion, &session.CreatedAt, &session.UpdatedAt, &finalizedAt); err != nil {
		return core.PlanningSession{}, err
	}
	if err := json.Unmarshal(pins, &session.PinnedRevisions); err != nil {
		return core.PlanningSession{}, fmt.Errorf("decode planning revisions: %w", err)
	}
	if len(promotion) > 0 && string(promotion) != "null" {
		if err := json.Unmarshal(promotion, &session.Promotion); err != nil {
			return core.PlanningSession{}, fmt.Errorf("decode planning promotion: %w", err)
		}
	}
	session.Status = core.PlanningSessionStatus(status)
	session.Goal = core.PlanningSessionGoal(goal)
	if finalizedAt != nil {
		session.FinalizedAt = *finalizedAt
	}
	return session, nil
}

func marshalRequirementStatements(statements []core.RequirementStatement) ([]byte, error) {
	if statements == nil {
		statements = []core.RequirementStatement{}
	}
	encoded, err := json.Marshal(statements)
	if err != nil {
		return nil, fmt.Errorf("encode requirement statements: %w", err)
	}
	return encoded, nil
}

func unmarshalRequirementStatements(raw []byte) ([]core.RequirementStatement, error) {
	if len(raw) == 0 {
		return []core.RequirementStatement{}, nil
	}
	statements := []core.RequirementStatement{}
	if err := json.Unmarshal(raw, &statements); err != nil {
		return nil, fmt.Errorf("decode requirement statements: %w", err)
	}
	return statements, nil
}

// insertRequirementEvent records a workspace-scoped audit event. Requirement
// and planning mutations carry no task, so they use the workspace event path
// and the migration 046 scope allowlist.
func insertRequirementEvent(ctx context.Context, q *db.Queries, kind string, payload map[string]any) error {
	return insertWorkspaceEvent(ctx, q, core.Event{Kind: kind, Payload: core.JSONPayload(payload)})
}
