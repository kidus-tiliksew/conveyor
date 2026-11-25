package singlestore

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

var _ store.EnvironmentStore = (*Store)(nil)

// environmentClaimKey names the claim row that serializes the environment
// mutations of one workspace.
func environmentClaimKey(workspaceID string) string { return "environments:" + workspaceID }

// Witness tables, as a claim holder names them.
const (
	environmentWitness           = "environment"
	environmentSuggestionWitness = "suggestion"
)

// environmentClaim is the claim a mutation holds and the revision that the
// last mutation of the workspace committed under it, 0 before the first. The
// holder is
// <witness>:<row>:<revision>. It names the row that mutation stamped with the
// revision. Every environment table shares the workspace shard key, so when
// that row shows the revision, every other row the mutation wrote is visible
// too (component-persistence invariant 8).
type environmentClaim struct {
	key       string
	workspace string
	last      int64
}

// claimEnvironments takes the workspace claim and waits until the last
// committed mutation is visible to this transaction. A wait that runs out
// returns store.ErrRetryable with nothing written.
func claimEnvironments(ctx context.Context, tx *sql.Tx, ws string) (environmentClaim, error) {
	claim := environmentClaim{key: environmentClaimKey(ws), workspace: ws}
	holder, err := claimKey(ctx, tx, claim.key)
	if err != nil || holder == "" {
		return claim, err
	}
	parts := strings.Split(holder, ":")
	if len(parts) != 3 {
		return claim, fmt.Errorf("environment claim holder names no record")
	}
	if claim.last, err = strconv.ParseInt(parts[2], 10, 64); err != nil {
		return claim, fmt.Errorf("environment claim holder names no record")
	}
	var query string
	switch parts[0] {
	case environmentWitness:
		query = `SELECT COUNT(*) FROM environments WHERE workspace_id=? AND id=? AND revision>=?`
	case environmentSuggestionWitness:
		query = `SELECT COUNT(*) FROM environment_suggestions WHERE workspace_id=? AND name_digest=? AND revision>=?`
	default:
		return claim, fmt.Errorf("environment claim holder names no record")
	}
	visible, err := awaitClaimedVisibility(ctx, func() (bool, error) {
		var count int
		if err := tx.QueryRowContext(ctx, query, ws, parts[1], claim.last).Scan(&count); err != nil {
			return false, err
		}
		return count > 0, nil
	})
	if err != nil {
		return claim, err
	}
	if !visible {
		return claim, fmt.Errorf("%w: the last environment change is not visible", store.ErrRetryable)
	}
	return claim, nil
}

// next returns the revision this mutation stamps on its witness row.
func (c environmentClaim) next() int64 { return c.last + 1 }

// record names the stamped row on the claim, in the transaction that stamped
// it, so the name commits with the release.
func (c environmentClaim) record(ctx context.Context, tx *sql.Tx, witness, row string) error {
	return setClaim(ctx, tx, c.key, witness+":"+row+":"+strconv.FormatInt(c.next(), 10))
}

// touchEnvironment stamps the environment row as the mutation's witness.
func (c environmentClaim) touchEnvironment(ctx context.Context, tx *sql.Tx, environmentID string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE environments SET revision=? WHERE workspace_id=? AND id=?`, c.next(), c.workspace, environmentID); err != nil {
		return err
	}
	return c.record(ctx, tx, environmentWitness, environmentID)
}

func environmentNameDigest(name string) string {
	sum := sha256.Sum256([]byte(store.EnvironmentFold(name)))
	return hex.EncodeToString(sum[:])
}

const environmentColumns = `id,name,tracked_branches,github_environments,version_rule_kind,version_rule_text,timing_window_minutes,archived,created_by,created_at`

func scanEnvironment(row interface{ Scan(...any) error }, workspaceID string) (store.Environment, error) {
	environment := store.Environment{Workspace: workspaceID}
	var branches, names string
	var ruleKind, ruleText sql.NullString
	var createdAt time.Time
	if err := row.Scan(&environment.ID, &environment.Name, &branches, &names, &ruleKind, &ruleText, &environment.TimingWindowMinutes, &environment.Archived, &environment.CreatedBy, &createdAt); err != nil {
		return store.Environment{}, err
	}
	environment.CreatedAt = store.EnvironmentTime(createdAt)
	var kind, text *string
	if ruleKind.Valid && ruleText.Valid {
		kind, text = &ruleKind.String, &ruleText.String
	}
	if err := store.ApplyEnvironmentRow(&environment, []byte(branches), []byte(names), kind, text); err != nil {
		return store.Environment{}, err
	}
	return environment, nil
}

// environmentRows is the row iteration of a transaction or of a translated
// read outside one.
type environmentRows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}

func collectEnvironments(rows environmentRows, ws string, query store.EnvironmentListQuery) ([]store.Environment, error) {
	defer rows.Close()
	environments := []store.Environment{}
	for rows.Next() {
		environment, err := scanEnvironment(rows, ws)
		if err != nil {
			return nil, err
		}
		if query.Matches(environment.Archived) {
			environments = append(environments, environment)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	store.SortEnvironments(environments)
	return environments, nil
}

func environmentsTx(ctx context.Context, tx *sql.Tx, ws string) ([]store.Environment, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+environmentColumns+` FROM environments WHERE workspace_id=?`, ws)
	if err != nil {
		return nil, err
	}
	return collectEnvironments(rows, ws, store.EnvironmentListQuery{})
}

// environmentKeysSQL reads the keys that are not revoked. It selects no hash
// column.
const environmentKeysSQL = `SELECT environment_id,id,label,created_by,created_at,last_used_at FROM environment_keys WHERE workspace_id=? AND revoked_at IS NULL`

func collectEnvironmentKeys(rows environmentRows) (map[string][]store.EnvironmentKey, error) {
	defer rows.Close()
	keys := map[string][]store.EnvironmentKey{}
	for rows.Next() {
		var owner string
		var key store.EnvironmentKey
		var createdAt time.Time
		var lastUsed sql.NullTime
		if err := rows.Scan(&owner, &key.ID, &key.Label, &key.CreatedBy, &createdAt, &lastUsed); err != nil {
			return nil, err
		}
		key.CreatedAt, key.LastUsedAt = store.EnvironmentTime(createdAt), nullTimePtr(lastUsed)
		keys[owner] = append(keys[owner], key)
	}
	return keys, rows.Err()
}

func environmentRepositoriesTx(ctx context.Context, tx *sql.Tx, ws string) (map[string]bool, error) {
	var configYAML string
	err := tx.QueryRowContext(ctx, `SELECT config_yaml FROM workspaces WHERE id=?`, ws).Scan(&configYAML)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: workspace %s", store.ErrNotFound, ws)
	}
	if err != nil {
		return nil, err
	}
	return store.EnvironmentRepositories(configYAML)
}

// forgetEnvironmentSuggestions drops the suggestion and the ignore of each
// name the unarchived environment lists (component-environments invariant
// 16).
func forgetEnvironmentSuggestions(ctx context.Context, tx *sql.Tx, environment store.Environment) error {
	if environment.Archived {
		return nil
	}
	for _, name := range environment.GitHubEnvironments {
		if _, err := tx.ExecContext(ctx, `DELETE FROM environment_suggestions WHERE workspace_id=? AND name_digest=?`, environment.Workspace, environmentNameDigest(name)); err != nil {
			return err
		}
	}
	return nil
}

func insertEnvironmentEvent(ctx context.Context, tx *sql.Tx, kind string, payload json.RawMessage, at time.Time) error {
	return insertWorkspaceEvent(ctx, tx, core.Event{Kind: kind, At: at, Payload: payload})
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
	ws, err := workspace(ctx)
	if err != nil {
		return store.EnvironmentRecord{}, err
	}
	environment, err := store.NewEnvironment(ws, actor.ID, input, time.Now())
	if err != nil {
		return store.EnvironmentRecord{}, err
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		claim, err := claimEnvironments(ctx, tx, ws)
		if err != nil {
			return err
		}
		repositories, err := environmentRepositoriesTx(ctx, tx, ws)
		if err != nil {
			return err
		}
		if err := store.CheckEnvironmentRepositories(environment.TrackedBranches, repositories); err != nil {
			return err
		}
		environments, err := environmentsTx(ctx, tx, ws)
		if err != nil {
			return err
		}
		if err := store.CheckEnvironmentConflicts(environment, environments); err != nil {
			return err
		}
		row := store.NewEnvironmentRow(environment)
		if _, err := tx.ExecContext(ctx, `INSERT INTO environments (workspace_id,id,name,name_fold,tracked_branches,github_environments,version_rule_kind,version_rule_text,timing_window_minutes,archived,created_by,created_at,revision) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			ws, environment.ID, environment.Name, row.NameFold, row.TrackedBranches, row.GitHubEnvironments, row.RuleKind, row.RuleText, environment.TimingWindowMinutes, environment.Archived, environment.CreatedBy, environment.CreatedAt, claim.next()); err != nil {
			return err
		}
		if err := forgetEnvironmentSuggestions(ctx, tx, environment); err != nil {
			return err
		}
		if err := claim.record(ctx, tx, environmentWitness, environment.ID); err != nil {
			return err
		}
		return insertEnvironmentEvent(ctx, tx, store.EnvironmentCreatedEvent, store.EnvironmentCreatedPayload(environment), environment.CreatedAt)
	})
	if err != nil {
		return store.EnvironmentRecord{}, err
	}
	return store.EnvironmentRecord{Environment: environment, Keys: []store.EnvironmentKey{}}, nil
}

// UpdateEnvironment implements store.EnvironmentStore.
func (s *Store) UpdateEnvironment(ctx context.Context, id string, patch store.EnvironmentPatch) (store.EnvironmentRecord, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "UpdateEnvironment")
	if _, err := store.RequireActor(ctx); err != nil {
		return store.EnvironmentRecord{}, err
	}
	patch, err := store.NormalizeEnvironmentPatch(patch)
	if err != nil {
		return store.EnvironmentRecord{}, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return store.EnvironmentRecord{}, err
	}
	id = strings.TrimSpace(id)
	var result store.EnvironmentRecord
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		claim, err := claimEnvironments(ctx, tx, ws)
		if err != nil {
			return err
		}
		environments, err := environmentsTx(ctx, tx, ws)
		if err != nil {
			return err
		}
		current, found := store.Environment{}, false
		for _, environment := range environments {
			if environment.ID == id {
				current, found = environment, true
			}
		}
		if !found {
			return &store.EnvironmentNotFoundError{ID: id}
		}
		next, kind, payload, err := store.ApplyEnvironmentPatch(current, patch)
		if err != nil {
			return err
		}
		if patch.TrackedBranches != nil {
			repositories, err := environmentRepositoriesTx(ctx, tx, ws)
			if err != nil {
				return err
			}
			if err := store.CheckEnvironmentRepositories(*patch.TrackedBranches, repositories); err != nil {
				return err
			}
		}
		if kind != "" {
			if err := store.CheckEnvironmentConflicts(next, environments); err != nil {
				return err
			}
			row := store.NewEnvironmentRow(next)
			if _, err := tx.ExecContext(ctx, `UPDATE environments SET name=?,name_fold=?,tracked_branches=?,github_environments=?,version_rule_kind=?,version_rule_text=?,timing_window_minutes=?,archived=?,revision=? WHERE workspace_id=? AND id=?`,
				next.Name, row.NameFold, row.TrackedBranches, row.GitHubEnvironments, row.RuleKind, row.RuleText, next.TimingWindowMinutes, next.Archived, claim.next(), ws, id); err != nil {
				return err
			}
			if err := forgetEnvironmentSuggestions(ctx, tx, next); err != nil {
				return err
			}
			if err := claim.record(ctx, tx, environmentWitness, id); err != nil {
				return err
			}
			if err := insertEnvironmentEvent(ctx, tx, kind, payload, store.EnvironmentTime(time.Now())); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, environmentKeysSQL+` AND environment_id=?`, ws, id)
		if err != nil {
			return err
		}
		keys, err := collectEnvironmentKeys(rows)
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

func (s *Store) listEnvironments(ctx context.Context, query store.EnvironmentListQuery) ([]store.Environment, error) {
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT `+environmentColumns+` FROM environments WHERE workspace_id=?`, ws)
	if err != nil {
		return nil, err
	}
	return collectEnvironments(rows, ws, query)
}

// ListEnvironments implements store.EnvironmentStore.
func (s *Store) ListEnvironments(ctx context.Context, query store.EnvironmentListQuery) ([]store.Environment, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListEnvironments")
	return s.listEnvironments(ctx, query)
}

// ListEnvironmentRecords implements store.EnvironmentStore.
func (s *Store) ListEnvironmentRecords(ctx context.Context, query store.EnvironmentListQuery) ([]store.EnvironmentRecord, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListEnvironmentRecords")
	environments, err := s.listEnvironments(ctx, query)
	if err != nil {
		return nil, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, environmentKeysSQL, ws)
	if err != nil {
		return nil, err
	}
	keys, err := collectEnvironmentKeys(rows)
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
	ws, err := workspace(ctx)
	if err != nil {
		return store.IssuedEnvironmentKey{}, err
	}
	issued, hash, err := store.MintEnvironmentKey(label, actor.ID, time.Now())
	if err != nil {
		return store.IssuedEnvironmentKey{}, err
	}
	// SingleStore has no CHECK constraint, so the store holds the digest size
	// (req-security-boundaries AC-2.8).
	if len(hash) != sha256.Size {
		return store.IssuedEnvironmentKey{}, fmt.Errorf("environment key digest is %d bytes, want %d", len(hash), sha256.Size)
	}
	environmentID = strings.TrimSpace(environmentID)
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		claim, err := claimEnvironments(ctx, tx, ws)
		if err != nil {
			return err
		}
		var archived bool
		err = tx.QueryRowContext(ctx, `SELECT archived FROM environments WHERE workspace_id=? AND id=?`, ws, environmentID).Scan(&archived)
		if errors.Is(err, sql.ErrNoRows) {
			return &store.EnvironmentNotFoundError{ID: environmentID}
		}
		if err != nil {
			return err
		}
		if archived {
			return &store.EnvironmentArchivedError{ID: environmentID}
		}
		var keys int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM environment_keys WHERE workspace_id=? AND environment_id=? AND revoked_at IS NULL`, ws, environmentID).Scan(&keys); err != nil {
			return err
		}
		if keys >= store.MaxEnvironmentKeys {
			return &store.EnvironmentKeyLimitError{ID: environmentID}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO environment_keys (workspace_id,id,environment_id,label,key_hash,created_by,created_at) VALUES (?,?,?,?,?,?,?)`,
			ws, issued.ID, environmentID, label, hash, issued.CreatedBy, issued.CreatedAt); err != nil {
			return err
		}
		if err := claim.touchEnvironment(ctx, tx, environmentID); err != nil {
			return err
		}
		return insertEnvironmentEvent(ctx, tx, store.EnvironmentKeyIssuedEvent, store.EnvironmentKeyPayload(ws, environmentID, issued.ID, label), issued.CreatedAt)
	})
	if err != nil {
		return store.IssuedEnvironmentKey{}, err
	}
	return issued, nil
}

// RevokeEnvironmentKey implements store.EnvironmentStore.
func (s *Store) RevokeEnvironmentKey(ctx context.Context, environmentID, keyID string) error {
	ctx = dbmetrics.WithReadLabel(ctx, "RevokeEnvironmentKey")
	if _, err := store.RequireActor(ctx); err != nil {
		return err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	environmentID, keyID = strings.TrimSpace(environmentID), strings.TrimSpace(keyID)
	return s.withTx(ctx, func(tx *sql.Tx) error {
		claim, err := claimEnvironments(ctx, tx, ws)
		if err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM environments WHERE workspace_id=? AND id=?`, ws, environmentID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return &store.EnvironmentNotFoundError{ID: environmentID}
		}
		var label string
		var revoked sql.NullTime
		err = tx.QueryRowContext(ctx, `SELECT label,revoked_at FROM environment_keys WHERE workspace_id=? AND id=? AND environment_id=? FOR UPDATE`, ws, keyID, environmentID).Scan(&label, &revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return &store.EnvironmentNotFoundError{ID: environmentID, KeyID: keyID}
		}
		if err != nil || revoked.Valid {
			return err
		}
		now := store.EnvironmentTime(time.Now())
		if _, err := tx.ExecContext(ctx, `UPDATE environment_keys SET revoked_at=? WHERE workspace_id=? AND id=?`, now, ws, keyID); err != nil {
			return err
		}
		if err := claim.touchEnvironment(ctx, tx, environmentID); err != nil {
			return err
		}
		return insertEnvironmentEvent(ctx, tx, store.EnvironmentKeyRevokedEvent, store.EnvironmentKeyPayload(ws, environmentID, keyID, label), now)
	})
}

// readEnvironmentKey resolves a key by the digest of the presented value
// alone, in one read that also checks the revocation and the archive state.
func (s *Store) readEnvironmentKey(ctx context.Context, hash []byte) (store.VerifiedEnvironmentKey, bool, error) {
	var verified store.VerifiedEnvironmentKey
	var stored []byte
	var stale bool
	err := backendRow(ctx, s.db, "SELECT k.key_hash,k.workspace_id,k.environment_id,k.id,k.label,k.last_used_at IS NULL OR k.last_used_at<DATE_SUB(CURRENT_TIMESTAMP(6),INTERVAL ? MICROSECOND) FROM environment_keys k JOIN environments e ON e.workspace_id=k.workspace_id AND e.id=k.environment_id WHERE k.key_hash=? AND k.revoked_at IS NULL AND e.archived=0",
		store.EnvironmentKeyUseGranularity.Microseconds(), hash).
		Scan(&stored, &verified.Workspace, &verified.EnvironmentID, &verified.KeyID, &verified.Label, &stale)
	if errors.Is(err, sql.ErrNoRows) {
		return store.VerifiedEnvironmentKey{}, false, store.ErrInvalidEnvironmentKey
	}
	if err != nil {
		return store.VerifiedEnvironmentKey{}, false, translateBackendConflict(err)
	}
	if subtle.ConstantTimeCompare(stored, hash) != 1 {
		return store.VerifiedEnvironmentKey{}, false, store.ErrInvalidEnvironmentKey
	}
	return verified, stale, nil
}

// VerifyEnvironmentKey implements store.EnvironmentStore. It reads before it
// writes and never enters a transaction. A stale last use gets one
// conditional UPDATE that repeats every validity predicate. A call whose
// UPDATE changes nothing rereads, because a concurrent call stamped first or
// the key lost its validity in between.
func (s *Store) VerifyEnvironmentKey(ctx context.Context, value string) (store.VerifiedEnvironmentKey, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "VerifyEnvironmentKey")
	hash := store.EnvironmentKeyDigest(value)
	verified, stale, err := s.readEnvironmentKey(ctx, hash)
	if err != nil || !stale {
		return verified, err
	}
	result, err := backendExec(ctx, s.db, "UPDATE environment_keys SET last_used_at=CURRENT_TIMESTAMP(6) WHERE workspace_id=? AND id=? AND key_hash=? AND revoked_at IS NULL AND (last_used_at IS NULL OR last_used_at<DATE_SUB(CURRENT_TIMESTAMP(6),INTERVAL ? MICROSECOND)) AND environment_id IN (SELECT id FROM environments WHERE workspace_id=? AND archived=0)",
		verified.Workspace, verified.KeyID, hash, store.EnvironmentKeyUseGranularity.Microseconds(), verified.Workspace)
	if err != nil {
		return store.VerifiedEnvironmentKey{}, translateBackendConflict(err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return store.VerifiedEnvironmentKey{}, translateBackendConflict(err)
	}
	if changed != 1 {
		verified, _, err = s.readEnvironmentKey(ctx, hash)
	}
	return verified, err
}

// environmentSuggestionRow is one stored suggestion. reportedAt is not valid
// when the name was ignored before any report.
type environmentSuggestionRow struct {
	github     string
	reportedAt sql.NullTime
	ignored    bool
}

func environmentSuggestionTx(ctx context.Context, tx *sql.Tx, ws, digest string) (environmentSuggestionRow, bool, error) {
	var row environmentSuggestionRow
	err := tx.QueryRowContext(ctx, `SELECT github_environment,last_reported_at,ignored FROM environment_suggestions WHERE workspace_id=? AND name_digest=? FOR UPDATE`, ws, digest).Scan(&row.github, &row.reportedAt, &row.ignored)
	if errors.Is(err, sql.ErrNoRows) {
		return environmentSuggestionRow{}, false, nil
	}
	return row, err == nil, err
}

// RecordEnvironmentReport implements store.EnvironmentStore.
func (s *Store) RecordEnvironmentReport(ctx context.Context, report store.EnvironmentReport) (bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordEnvironmentReport")
	report, err := store.NormalizeEnvironmentReport(report)
	if err != nil {
		return false, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return false, err
	}
	digest := environmentNameDigest(report.GitHubEnvironment)
	recorded := false
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		claim, err := claimEnvironments(ctx, tx, ws)
		if err != nil {
			return err
		}
		repositories, err := environmentRepositoriesTx(ctx, tx, ws)
		if err != nil || !repositories[report.Repository] {
			return err
		}
		environments, err := environmentsTx(ctx, tx, ws)
		if err != nil {
			return err
		}
		if _, listed := store.EnvironmentListing(environments, report.GitHubEnvironment); listed {
			return nil
		}
		stored, exists, err := environmentSuggestionTx(ctx, tx, ws, digest)
		if err != nil {
			return err
		}
		switch {
		case !exists:
			_, err = tx.ExecContext(ctx, `INSERT INTO environment_suggestions (workspace_id,name_digest,github_environment,repository,last_reported_at,ignored,revision) VALUES (?,?,?,?,?,0,?)`,
				ws, digest, report.GitHubEnvironment, report.Repository, report.ReportedAt, claim.next())
		case stored.ignored || (stored.reportedAt.Valid && !report.ReportedAt.After(stored.reportedAt.Time)):
			return nil
		default:
			_, err = tx.ExecContext(ctx, `UPDATE environment_suggestions SET github_environment=?,repository=?,last_reported_at=?,revision=? WHERE workspace_id=? AND name_digest=?`,
				report.GitHubEnvironment, report.Repository, report.ReportedAt, claim.next(), ws, digest)
		}
		if err != nil {
			return err
		}
		recorded = true
		return claim.record(ctx, tx, environmentSuggestionWitness, digest)
	})
	if err != nil {
		return false, err
	}
	return recorded, nil
}

// ListEnvironmentSuggestions implements store.EnvironmentStore.
func (s *Store) ListEnvironmentSuggestions(ctx context.Context) ([]store.EnvironmentSuggestion, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListEnvironmentSuggestions")
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT github_environment,repository,last_reported_at FROM environment_suggestions WHERE workspace_id=? AND ignored=0`, ws)
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
	if _, err := store.RequireActor(ctx); err != nil {
		return err
	}
	name, err := store.NormalizeEnvironmentGitHubName("github_environment", gitHubEnvironment)
	if err != nil {
		return err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	digest := environmentNameDigest(name)
	return s.withTx(ctx, func(tx *sql.Tx) error {
		claim, err := claimEnvironments(ctx, tx, ws)
		if err != nil {
			return err
		}
		environments, err := environmentsTx(ctx, tx, ws)
		if err != nil {
			return err
		}
		if err := store.CheckEnvironmentNameUnlisted(environments, name); err != nil {
			return err
		}
		stored, exists, err := environmentSuggestionTx(ctx, tx, ws, digest)
		if err != nil || stored.ignored {
			return err
		}
		if exists {
			name = stored.github
			_, err = tx.ExecContext(ctx, `UPDATE environment_suggestions SET ignored=1,revision=? WHERE workspace_id=? AND name_digest=?`, claim.next(), ws, digest)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO environment_suggestions (workspace_id,name_digest,github_environment,ignored,revision) VALUES (?,?,?,1,?)`, ws, digest, name, claim.next())
		}
		if err != nil {
			return err
		}
		if err := claim.record(ctx, tx, environmentSuggestionWitness, digest); err != nil {
			return err
		}
		return insertEnvironmentEvent(ctx, tx, store.EnvironmentSuggestionIgnoredEvent, store.EnvironmentSuggestionIgnoredPayload(ws, name), store.EnvironmentTime(time.Now()))
	})
}
