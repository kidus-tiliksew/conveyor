package postgres

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

var _ store.EnvironmentStore = (*Store)(nil)

// environmentQuerier is a transaction or the store's error-translating pool.
type environmentQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const environmentColumns = `id, name, tracked_branches::text, github_environments::text, version_rule_kind, version_rule_text, timing_window_minutes, archived, created_by, created_at`

func scanEnvironment(row pgx.Row, workspaceID string) (store.Environment, error) {
	environment := store.Environment{Workspace: workspaceID}
	var branches, names string
	var ruleKind, ruleText *string
	var createdAt time.Time
	if err := row.Scan(&environment.ID, &environment.Name, &branches, &names, &ruleKind, &ruleText, &environment.TimingWindowMinutes, &environment.Archived, &environment.CreatedBy, &createdAt); err != nil {
		return store.Environment{}, err
	}
	environment.CreatedAt = store.EnvironmentTime(createdAt)
	if err := store.ApplyEnvironmentRow(&environment, []byte(branches), []byte(names), ruleKind, ruleText); err != nil {
		return store.Environment{}, err
	}
	return environment, nil
}

func queryEnvironments(ctx context.Context, q environmentQuerier, workspaceID string, query store.EnvironmentListQuery) ([]store.Environment, error) {
	rows, err := q.Query(ctx, `SELECT `+environmentColumns+` FROM environments WHERE workspace_id=$1 AND ($2::boolean IS NULL OR archived=$2::boolean)`, workspaceID, query.Archived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	environments := []store.Environment{}
	for rows.Next() {
		environment, err := scanEnvironment(rows, workspaceID)
		if err != nil {
			return nil, err
		}
		environments = append(environments, environment)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	store.SortEnvironments(environments)
	return environments, nil
}

// queryEnvironmentKeys reads the keys that are not revoked, of one
// environment or of every environment when environmentID is empty. It selects
// no hash column.
func queryEnvironmentKeys(ctx context.Context, q environmentQuerier, workspaceID, environmentID string) (map[string][]store.EnvironmentKey, error) {
	rows, err := q.Query(ctx, `SELECT environment_id, id, label, created_by, created_at, last_used_at FROM environment_keys
WHERE workspace_id=$1 AND revoked_at IS NULL AND ($2::text = '' OR environment_id=$2::text)`, workspaceID, environmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := map[string][]store.EnvironmentKey{}
	for rows.Next() {
		var owner string
		var key store.EnvironmentKey
		var createdAt time.Time
		var lastUsed pgtype.Timestamptz
		if err := rows.Scan(&owner, &key.ID, &key.Label, &key.CreatedBy, &createdAt, &lastUsed); err != nil {
			return nil, err
		}
		key.CreatedAt, key.LastUsedAt = store.EnvironmentTime(createdAt), utcPtr(lastUsed)
		keys[owner] = append(keys[owner], key)
	}
	return keys, rows.Err()
}

// lockEnvironmentWorkspace takes the workspace row lock that serializes the
// environment mutations of one workspace with each other and with a
// configuration change, and returns the repository names of the current
// configuration. FOR NO KEY UPDATE does not block an insert of a row that
// references the workspace.
func lockEnvironmentWorkspace(ctx context.Context, tx pgx.Tx, workspaceID string) (map[string]bool, error) {
	var configYAML string
	if err := tx.QueryRow(ctx, `SELECT config_yaml FROM workspaces WHERE id=$1 FOR NO KEY UPDATE`, workspaceID).Scan(&configYAML); err != nil {
		return nil, notFound(err, "workspace %s", workspaceID)
	}
	return store.EnvironmentRepositories(configYAML)
}

// forgetEnvironmentSuggestions drops the suggestion and the ignore of each
// name the unarchived environment lists (component-environments invariant
// 16).
func forgetEnvironmentSuggestions(ctx context.Context, tx pgx.Tx, environment store.Environment) error {
	if environment.Archived || len(environment.GitHubEnvironments) == 0 {
		return nil
	}
	folds := make([]string, len(environment.GitHubEnvironments))
	for i, name := range environment.GitHubEnvironments {
		folds[i] = store.EnvironmentFold(name)
	}
	_, err := tx.Exec(ctx, `DELETE FROM environment_suggestions WHERE workspace_id=$1 AND name_fold=ANY($2::text[])`, environment.Workspace, folds)
	return err
}

func insertEnvironmentEvent(ctx context.Context, q *db.Queries, actor store.Actor, workspaceID, kind string, payload json.RawMessage, at time.Time) error {
	_, err := insertWorkspaceEventRow(ctx, q, db.InsertWorkspaceEventParams{
		WorkspaceID: workspaceID, Kind: kind, ActorID: actor.ID, ActorRole: string(actor.Role),
		PayloadJson: payload, At: timestamp(at),
	})
	return err
}

// CreateEnvironment implements store.EnvironmentStore.
func (s *Store) CreateEnvironment(ctx context.Context, input store.EnvironmentInput) (store.EnvironmentRecord, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CreateEnvironment")
	actor, err := store.RequireActor(ctx)
	if err != nil {
		return store.EnvironmentRecord{}, err
	}
	if input, err = store.NormalizeEnvironmentInput(input); err != nil {
		return store.EnvironmentRecord{}, err
	}
	workspaceID := workspace(ctx)
	environment, err := store.NewEnvironment(workspaceID, actor.ID, input, time.Now())
	if err != nil {
		return store.EnvironmentRecord{}, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		repositories, err := lockEnvironmentWorkspace(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if err := store.CheckEnvironmentRepositories(environment.TrackedBranches, repositories); err != nil {
			return err
		}
		environments, err := queryEnvironments(ctx, tx, workspaceID, store.EnvironmentListQuery{})
		if err != nil {
			return err
		}
		if err := store.CheckEnvironmentConflicts(environment, environments); err != nil {
			return err
		}
		row := store.NewEnvironmentRow(environment)
		if _, err := tx.Exec(ctx, `INSERT INTO environments (workspace_id, id, name, name_fold, tracked_branches, github_environments, version_rule_kind, version_rule_text, timing_window_minutes, archived, created_by, created_at)
VALUES ($1, $2, $3, $4, $5::text::jsonb, $6::text::jsonb, $7, $8, $9, $10, $11, $12)`, workspaceID, environment.ID, environment.Name, row.NameFold, row.TrackedBranches, row.GitHubEnvironments,
			row.RuleKind, row.RuleText, environment.TimingWindowMinutes, environment.Archived, environment.CreatedBy, environment.CreatedAt); err != nil {
			return err
		}
		if err := forgetEnvironmentSuggestions(ctx, tx, environment); err != nil {
			return err
		}
		return insertEnvironmentEvent(ctx, q, actor, workspaceID, store.EnvironmentCreatedEvent, store.EnvironmentCreatedPayload(environment), environment.CreatedAt)
	})
	if err != nil {
		return store.EnvironmentRecord{}, err
	}
	return store.EnvironmentRecord{Environment: environment, Keys: []store.EnvironmentKey{}}, nil
}

// UpdateEnvironment implements store.EnvironmentStore.
func (s *Store) UpdateEnvironment(ctx context.Context, id string, patch store.EnvironmentPatch) (store.EnvironmentRecord, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "UpdateEnvironment")
	actor, err := store.RequireActor(ctx)
	if err != nil {
		return store.EnvironmentRecord{}, err
	}
	workspaceID := workspace(ctx)
	id = strings.TrimSpace(id)
	var result store.EnvironmentRecord
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		repositories, err := lockEnvironmentWorkspace(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		current, err := scanEnvironment(tx.QueryRow(ctx, `SELECT `+environmentColumns+` FROM environments WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, id), workspaceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &store.EnvironmentNotFoundError{ID: id}
		}
		if err != nil {
			return err
		}
		if err := store.CheckEnvironmentEditable(current, patch); err != nil {
			return err
		}
		patch, err := store.NormalizeEnvironmentPatch(patch)
		if err != nil {
			return err
		}
		next, kind, payload := store.ApplyEnvironmentPatch(current, patch)
		if patch.TrackedBranches != nil {
			if err := store.CheckEnvironmentRepositories(*patch.TrackedBranches, repositories); err != nil {
				return err
			}
		}
		if kind != "" {
			environments, err := queryEnvironments(ctx, tx, workspaceID, store.EnvironmentListQuery{})
			if err != nil {
				return err
			}
			if err := store.CheckEnvironmentConflicts(next, environments); err != nil {
				return err
			}
			row := store.NewEnvironmentRow(next)
			if _, err := tx.Exec(ctx, `UPDATE environments SET name=$3, name_fold=$4, tracked_branches=$5::text::jsonb, github_environments=$6::text::jsonb, version_rule_kind=$7, version_rule_text=$8, timing_window_minutes=$9, archived=$10
WHERE workspace_id=$1 AND id=$2`, workspaceID, id, next.Name, row.NameFold, row.TrackedBranches, row.GitHubEnvironments, row.RuleKind, row.RuleText, next.TimingWindowMinutes, next.Archived); err != nil {
				return err
			}
			if err := forgetEnvironmentSuggestions(ctx, tx, next); err != nil {
				return err
			}
			if err := insertEnvironmentEvent(ctx, q, actor, workspaceID, kind, payload, store.EnvironmentTime(time.Now())); err != nil {
				return err
			}
		}
		keys, err := queryEnvironmentKeys(ctx, tx, workspaceID, id)
		if err != nil {
			return err
		}
		result = store.EnvironmentRecords([]store.Environment{next}, keys)[0]
		return nil
	})
	if err != nil {
		return store.EnvironmentRecord{}, err
	}
	return result, nil
}

// ListEnvironments implements store.EnvironmentStore.
func (s *Store) ListEnvironments(ctx context.Context, query store.EnvironmentListQuery) ([]store.Environment, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListEnvironments")
	return queryEnvironments(ctx, s.boundary, workspace(ctx), query)
}

// ListEnvironmentRecords implements store.EnvironmentStore.
func (s *Store) ListEnvironmentRecords(ctx context.Context, query store.EnvironmentListQuery) ([]store.EnvironmentRecord, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListEnvironmentRecords")
	workspaceID := workspace(ctx)
	environments, err := queryEnvironments(ctx, s.boundary, workspaceID, query)
	if err != nil {
		return nil, err
	}
	keys, err := queryEnvironmentKeys(ctx, s.boundary, workspaceID, "")
	if err != nil {
		return nil, err
	}
	return store.EnvironmentRecords(environments, keys), nil
}

// IssueEnvironmentKey implements store.EnvironmentStore.
func (s *Store) IssueEnvironmentKey(ctx context.Context, environmentID, label string) (store.IssuedEnvironmentKey, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "IssueEnvironmentKey")
	actor, err := store.RequireActor(ctx)
	if err != nil {
		return store.IssuedEnvironmentKey{}, err
	}
	if label, err = store.NormalizeEnvironmentKeyLabel(label); err != nil {
		return store.IssuedEnvironmentKey{}, err
	}
	issued, hash, err := store.MintEnvironmentKey(label, actor.ID, time.Now())
	if err != nil {
		return store.IssuedEnvironmentKey{}, err
	}
	workspaceID := workspace(ctx)
	environmentID = strings.TrimSpace(environmentID)
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := lockEnvironmentWorkspace(ctx, tx, workspaceID); err != nil {
			return err
		}
		var archived bool
		err := tx.QueryRow(ctx, `SELECT archived FROM environments WHERE workspace_id=$1 AND id=$2`, workspaceID, environmentID).Scan(&archived)
		if errors.Is(err, pgx.ErrNoRows) {
			return &store.EnvironmentNotFoundError{ID: environmentID}
		}
		if err != nil {
			return err
		}
		if archived {
			return &store.EnvironmentArchivedError{ID: environmentID}
		}
		var keys int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM environment_keys WHERE workspace_id=$1 AND environment_id=$2 AND revoked_at IS NULL`, workspaceID, environmentID).Scan(&keys); err != nil {
			return err
		}
		if keys >= store.MaxEnvironmentKeys {
			return &store.EnvironmentKeyLimitError{ID: environmentID}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO environment_keys (workspace_id, id, environment_id, label, key_hash, created_by, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			workspaceID, issued.ID, environmentID, label, hash, issued.CreatedBy, issued.CreatedAt); err != nil {
			return err
		}
		return insertEnvironmentEvent(ctx, q, actor, workspaceID, store.EnvironmentKeyIssuedEvent, store.EnvironmentKeyPayload(workspaceID, environmentID, issued.ID, label), issued.CreatedAt)
	})
	if err != nil {
		return store.IssuedEnvironmentKey{}, err
	}
	return issued, nil
}

// RevokeEnvironmentKey implements store.EnvironmentStore.
func (s *Store) RevokeEnvironmentKey(ctx context.Context, environmentID, keyID string) error {
	ctx = dbmetrics.WithReadLabel(ctx, "RevokeEnvironmentKey")
	actor, err := store.RequireActor(ctx)
	if err != nil {
		return err
	}
	workspaceID := workspace(ctx)
	environmentID, keyID = strings.TrimSpace(environmentID), strings.TrimSpace(keyID)
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := lockEnvironmentWorkspace(ctx, tx, workspaceID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM environments WHERE workspace_id=$1 AND id=$2)`, workspaceID, environmentID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return &store.EnvironmentNotFoundError{ID: environmentID}
		}
		var label string
		var revoked pgtype.Timestamptz
		err := tx.QueryRow(ctx, `SELECT label, revoked_at FROM environment_keys WHERE workspace_id=$1 AND environment_id=$2 AND id=$3 FOR UPDATE`, workspaceID, environmentID, keyID).Scan(&label, &revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return &store.EnvironmentNotFoundError{ID: environmentID, KeyID: keyID}
		}
		if err != nil || revoked.Valid {
			return err
		}
		now := store.EnvironmentTime(time.Now())
		if _, err := tx.Exec(ctx, `UPDATE environment_keys SET revoked_at=$3 WHERE workspace_id=$1 AND id=$2`, workspaceID, keyID, now); err != nil {
			return err
		}
		return insertEnvironmentEvent(ctx, q, actor, workspaceID, store.EnvironmentKeyRevokedEvent, store.EnvironmentKeyPayload(workspaceID, environmentID, keyID, label), now)
	})
}

// readEnvironmentKey resolves a key by the digest of the presented value
// alone, in one read that also checks the revocation and the archive state.
func (s *Store) readEnvironmentKey(ctx context.Context, hash []byte) (store.VerifiedEnvironmentKey, error) {
	var verified store.VerifiedEnvironmentKey
	var stored []byte
	err := s.boundary.QueryRow(ctx, `SELECT k.key_hash, k.workspace_id, k.environment_id, k.id, k.label
FROM environment_keys k JOIN environments e ON e.workspace_id=k.workspace_id AND e.id=k.environment_id
WHERE k.key_hash=$1 AND k.revoked_at IS NULL AND NOT e.archived`, hash).
		Scan(&stored, &verified.Workspace, &verified.EnvironmentID, &verified.KeyID, &verified.Label)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.VerifiedEnvironmentKey{}, store.ErrInvalidEnvironmentKey
	}
	if err != nil {
		return store.VerifiedEnvironmentKey{}, translateBackendConflict(err)
	}
	if subtle.ConstantTimeCompare(stored, hash) != 1 {
		return store.VerifiedEnvironmentKey{}, store.ErrInvalidEnvironmentKey
	}
	return verified, nil
}

// CheckEnvironmentKey implements store.EnvironmentStore.
func (s *Store) CheckEnvironmentKey(ctx context.Context, value string) (store.VerifiedEnvironmentKey, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "CheckEnvironmentKey")
	return s.readEnvironmentKey(ctx, store.EnvironmentKeyDigest(value))
}

// RecordEnvironmentReport implements store.EnvironmentStore.
func (s *Store) RecordEnvironmentReport(ctx context.Context, report store.EnvironmentReport) (bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordEnvironmentReport")
	report, err := store.NormalizeEnvironmentReport(report)
	if err != nil {
		return false, err
	}
	workspaceID := workspace(ctx)
	recorded := false
	err = s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		repositories, err := lockEnvironmentWorkspace(ctx, tx, workspaceID)
		if err != nil || !repositories[report.Repository] {
			return err
		}
		unarchived := false
		environments, err := queryEnvironments(ctx, tx, workspaceID, store.EnvironmentListQuery{Archived: &unarchived})
		if err != nil {
			return err
		}
		if _, listed := store.EnvironmentListing(environments, report.GitHubEnvironment); listed {
			return nil
		}
		// An ignored name and a report no newer than the stored one match no
		// row to update.
		tag, err := tx.Exec(ctx, `INSERT INTO environment_suggestions AS s (workspace_id, name_fold, github_environment, repository, last_reported_at) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (workspace_id, name_fold) DO UPDATE SET github_environment=EXCLUDED.github_environment, repository=EXCLUDED.repository, last_reported_at=EXCLUDED.last_reported_at
WHERE NOT s.ignored AND s.last_reported_at < EXCLUDED.last_reported_at`,
			workspaceID, store.EnvironmentFold(report.GitHubEnvironment), report.GitHubEnvironment, report.Repository, report.ReportedAt)
		if err != nil {
			return err
		}
		recorded = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return recorded, nil
}

// ListEnvironmentSuggestions implements store.EnvironmentStore.
func (s *Store) ListEnvironmentSuggestions(ctx context.Context) ([]store.EnvironmentSuggestion, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListEnvironmentSuggestions")
	rows, err := s.boundary.Query(ctx, `SELECT github_environment, repository, last_reported_at FROM environment_suggestions WHERE workspace_id=$1 AND NOT ignored`, workspace(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	suggestions := []store.EnvironmentSuggestion{}
	for rows.Next() {
		var suggestion store.EnvironmentSuggestion
		if err := rows.Scan(&suggestion.GitHubEnvironment, &suggestion.Repository, &suggestion.LastReportedAt); err != nil {
			return nil, err
		}
		suggestion.LastReportedAt = store.EnvironmentTime(suggestion.LastReportedAt)
		suggestions = append(suggestions, suggestion)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	store.SortEnvironmentSuggestions(suggestions)
	return suggestions, nil
}

// IgnoreEnvironmentSuggestion implements store.EnvironmentStore.
func (s *Store) IgnoreEnvironmentSuggestion(ctx context.Context, gitHubEnvironment string) error {
	ctx = dbmetrics.WithReadLabel(ctx, "IgnoreEnvironmentSuggestion")
	actor, err := store.RequireActor(ctx)
	if err != nil {
		return err
	}
	name, err := store.NormalizeEnvironmentGitHubName("github_environment", gitHubEnvironment)
	if err != nil {
		return err
	}
	workspaceID := workspace(ctx)
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := lockEnvironmentWorkspace(ctx, tx, workspaceID); err != nil {
			return err
		}
		unarchived := false
		environments, err := queryEnvironments(ctx, tx, workspaceID, store.EnvironmentListQuery{Archived: &unarchived})
		if err != nil {
			return err
		}
		if err := store.CheckEnvironmentNameUnlisted(environments, name); err != nil {
			return err
		}
		// An already ignored name matches no row to update and returns none.
		var recorded string
		err = tx.QueryRow(ctx, `INSERT INTO environment_suggestions AS s (workspace_id, name_fold, github_environment, ignored) VALUES ($1, $2, $3, true)
ON CONFLICT (workspace_id, name_fold) DO UPDATE SET ignored=true WHERE NOT s.ignored RETURNING s.github_environment`,
			workspaceID, store.EnvironmentFold(name), name).Scan(&recorded)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return insertEnvironmentEvent(ctx, q, actor, workspaceID, store.EnvironmentSuggestionIgnoredEvent, store.EnvironmentSuggestionIgnoredPayload(workspaceID, recorded), store.EnvironmentTime(time.Now()))
	})
}
