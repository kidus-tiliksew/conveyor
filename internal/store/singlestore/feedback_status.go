package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

var _ store.FeedbackStatusStore = (*Store)(nil)

// feedbackStatusClaimKey names the claim row that serializes the feedback
// status writes of one workspace.
func feedbackStatusClaimKey(ws string) string { return "feedback_status:" + ws }

// feedbackStatusClaim is the claim a write holds and the revision the last
// write stamped on feedback_status_state, 0 before the first. Every feedback
// table shares the workspace shard key, so when the witness row shows the
// revision, every row of that write is visible too (component-persistence
// invariant 8).
type feedbackStatusClaim struct {
	ws   string
	last int64
}

func claimFeedbackStatus(ctx context.Context, tx *sql.Tx, ws string) (feedbackStatusClaim, error) {
	claim := feedbackStatusClaim{ws: ws}
	holder, err := claimKey(ctx, tx, feedbackStatusClaimKey(ws))
	if err != nil || holder == "" {
		return claim, err
	}
	if claim.last, err = strconv.ParseInt(holder, 10, 64); err != nil {
		return claim, fmt.Errorf("feedback status claim holder names no revision")
	}
	visible, err := awaitClaimedVisibility(ctx, func() (bool, error) {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback_status_state WHERE workspace_id=? AND revision>=?`, ws, claim.last).Scan(&count); err != nil {
			return false, err
		}
		return count > 0, nil
	})
	if err != nil {
		return claim, err
	}
	if !visible {
		return claim, fmt.Errorf("%w: the last feedback status write is not visible", store.ErrRetryable)
	}
	return claim, nil
}

// stamp writes the next revision on the witness and names it on the claim.
func (c feedbackStatusClaim) stamp(ctx context.Context, tx *sql.Tx) error {
	next := c.last + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO feedback_status_state (workspace_id,revision) VALUES (?,?) ON DUPLICATE KEY UPDATE revision=VALUES(revision)`, c.ws, next); err != nil {
		return err
	}
	return setClaim(ctx, tx, feedbackStatusClaimKey(c.ws), strconv.FormatInt(next, 10))
}

// withFeedbackStatusClaim runs fn under the workspace feedback claim.
func (s *Store) withFeedbackStatusClaim(ctx context.Context, fn func(*sql.Tx, string) error) error {
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		claim, err := claimFeedbackStatus(ctx, tx, ws)
		if err != nil {
			return err
		}
		if err := fn(tx, ws); err != nil {
			return err
		}
		return claim.stamp(ctx, tx)
	})
}

const feedbackResolutionColumns = `environment_id,service,reported_repository,reported_commit,reported_version,resolved_repository,resolved_commit,reason,attempted_at,next_attempt_at`

func scanFeedbackResolution(row interface{ Scan(...any) error }) (store.FeedbackResolution, error) {
	var resolution store.FeedbackResolution
	var next sql.NullTime
	if err := row.Scan(&resolution.EnvironmentID, &resolution.Service, &resolution.Repository, &resolution.Commit, &resolution.Version, &resolution.ResolvedRepository, &resolution.ResolvedCommit, &resolution.Reason, &resolution.AttemptedAt, &next); err != nil {
		return store.FeedbackResolution{}, err
	}
	resolution.AttemptedAt = store.EnvironmentTime(resolution.AttemptedAt)
	if next.Valid {
		resolution.NextAttemptAt = store.EnvironmentTime(next.Time)
	}
	return resolution, nil
}

func readFeedbackResolution(ctx context.Context, tx *sql.Tx, ws string, ref store.FeedbackVersionRef) (store.FeedbackResolution, bool, error) {
	resolution, err := scanFeedbackResolution(tx.QueryRowContext(ctx, `SELECT `+feedbackResolutionColumns+` FROM feedback_resolutions WHERE workspace_id=? AND environment_id=? AND service_digest=? AND version_digest=?`,
		ws, ref.EnvironmentID, store.FeedbackServiceDigest(ref.Service), ref.Digest()))
	if errors.Is(err, sql.ErrNoRows) {
		return store.FeedbackResolution{}, false, nil
	}
	return resolution, err == nil, err
}

func writeFeedbackResolution(ctx context.Context, tx *sql.Tx, ws string, resolution store.FeedbackResolution) error {
	var next any
	if !resolution.Resolved() {
		next = resolution.NextAttemptAt
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO feedback_resolutions (workspace_id,environment_id,service_digest,version_digest,service,reported_repository,reported_commit,reported_version,resolved_repository,resolved_commit,reason,attempted_at,next_attempt_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
ON DUPLICATE KEY UPDATE resolved_repository=VALUES(resolved_repository),resolved_commit=VALUES(resolved_commit),reason=VALUES(reason),attempted_at=VALUES(attempted_at),next_attempt_at=VALUES(next_attempt_at)`,
		ws, resolution.EnvironmentID, store.FeedbackServiceDigest(resolution.Service), resolution.Digest(), resolution.Service, resolution.Repository, resolution.Commit, resolution.Version,
		resolution.ResolvedRepository, resolution.ResolvedCommit, resolution.Reason, resolution.AttemptedAt, next)
	return err
}

// ClaimFeedbackResolution implements store.FeedbackStatusStore.
func (s *Store) ClaimFeedbackResolution(ctx context.Context, ref store.FeedbackVersionRef, signalAt, now time.Time) (bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ClaimFeedbackResolution")
	if ref.EnvironmentID == "" || ref.Service == "" {
		return false, fmt.Errorf("a feedback resolution needs its environment and service")
	}
	claimed := false
	err := s.withFeedbackStatusClaim(ctx, func(tx *sql.Tx, ws string) error {
		stored, found, err := readFeedbackResolution(ctx, tx, ws, ref)
		if err != nil || !store.FeedbackClaimable(stored, found, store.EnvironmentTime(signalAt)) {
			return err
		}
		claimed = true
		return writeFeedbackResolution(ctx, tx, ws, store.FeedbackPendingResolution(ref, now))
	})
	return claimed && err == nil, err
}

// RecordFeedbackResolution implements store.FeedbackStatusStore.
func (s *Store) RecordFeedbackResolution(ctx context.Context, resolution store.FeedbackResolution) error {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordFeedbackResolution")
	resolution, err := store.NormalizeFeedbackResolution(resolution)
	if err != nil {
		return err
	}
	return s.withFeedbackStatusClaim(ctx, func(tx *sql.Tx, ws string) error {
		stored, found, err := readFeedbackResolution(ctx, tx, ws, resolution.FeedbackVersionRef)
		if err != nil || (found && stored.Resolved()) {
			return err
		}
		if err := writeFeedbackResolution(ctx, tx, ws, resolution); err != nil || !resolution.Resolved() {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE feedback_running_versions SET resolved=1 WHERE workspace_id=? AND environment_id=? AND service_digest=? AND version_digest=?`,
			ws, resolution.EnvironmentID, store.FeedbackServiceDigest(resolution.Service), resolution.Digest()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE feedback_deploys SET resolved=1 WHERE workspace_id=? AND environment_id=? AND service=? AND reported_repository=? AND reported_commit=? AND reported_version=?`,
			ws, resolution.EnvironmentID, resolution.Service, resolution.Repository, resolution.Commit, resolution.Version)
		return err
	})
}

// ReadFeedbackInputs implements store.FeedbackStatusStore.
func (s *Store) ReadFeedbackInputs(ctx context.Context, environmentIDs []string) (store.FeedbackInputs, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ReadFeedbackInputs")
	inputs := store.FeedbackInputs{Running: map[string][]store.FeedbackRunningVersion{}, Deploys: map[string][]store.FeedbackDeploy{}, Resolutions: []store.FeedbackResolution{}}
	signals, err := s.ListFeedbackSignals(ctx, environmentIDs)
	if err != nil {
		return store.FeedbackInputs{}, err
	}
	inputs.Signals = signals
	if len(environmentIDs) == 0 {
		return inputs, nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return store.FeedbackInputs{}, err
	}
	err = eachFeedbackChunk(environmentIDs, func(part []string) error {
		list, args := feedbackList(part)
		args = append([]any{ws}, args...)
		rows, err := documentRows(ctx, s.db, `SELECT environment_id,service,reported_repository,reported_commit,reported_version,resolved,deploy_id,deployed_at,reported_at FROM feedback_running_versions WHERE workspace_id=? AND environment_id IN (`+list+`)`, args...)
		if err != nil {
			return err
		}
		running, err := collectFeedbackRunning(rows)
		if err != nil {
			return err
		}
		for _, version := range running {
			inputs.Running[version.EnvironmentID] = append(inputs.Running[version.EnvironmentID], version)
		}
		if rows, err = documentRows(ctx, s.db, `SELECT id,environment_id,service,reported_repository,reported_commit,reported_version,resolved,deployed_at,source,source_label,actor_id,report,stopped_at,stop_source,stop_source_label,stop_actor_id,stop_report FROM feedback_deploys WHERE workspace_id=? AND environment_id IN (`+list+`)`, args...); err != nil {
			return err
		}
		deploys, err := collectFeedbackDeploys(rows)
		if err != nil {
			return err
		}
		for _, deploy := range deploys {
			inputs.Deploys[deploy.EnvironmentID] = append(inputs.Deploys[deploy.EnvironmentID], deploy)
		}
		if rows, err = documentRows(ctx, s.db, `SELECT `+feedbackResolutionColumns+` FROM feedback_resolutions WHERE workspace_id=? AND environment_id IN (`+list+`)`, args...); err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			resolution, err := scanFeedbackResolution(rows)
			if err != nil {
				return err
			}
			inputs.Resolutions = append(inputs.Resolutions, resolution)
		}
		return rows.Err()
	})
	if err != nil {
		return store.FeedbackInputs{}, err
	}
	for id := range inputs.Running {
		store.SortFeedbackRunningVersions(inputs.Running[id])
	}
	for id := range inputs.Deploys {
		store.SortFeedbackDeploys(inputs.Deploys[id])
	}
	store.SortFeedbackResolutions(inputs.Resolutions)
	return inputs, nil
}

// AdmitFeedbackComparisons implements store.FeedbackStatusStore. The
// workspace feedback claim serializes admissions across workers and
// replicas.
func (s *Store) AdmitFeedbackComparisons(ctx context.Context, keys []store.FeedbackComparisonKey, now time.Time, most int) ([]store.FeedbackComparisonKey, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "AdmitFeedbackComparisons")
	keys = store.NormalizeFeedbackComparisonKeys(keys)
	waited, asked := time.Now(), now
	var admitted []store.FeedbackComparisonKey
	err := s.withFeedbackStatusClaim(ctx, func(tx *sql.Tx, ws string) error {
		// The time is read after the claim is held, so a call that waited
		// counts and records when its request can start.
		now := store.FeedbackAdmissionTime(asked, waited)
		if _, err := tx.ExecContext(ctx, `DELETE FROM feedback_admissions WHERE workspace_id=? AND admitted_at<=?`, ws, now.Add(-store.FeedbackAttemptMemory)); err != nil {
			return err
		}
		var used int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback_admissions WHERE workspace_id=? AND admitted_at>?`, ws, now.Add(-store.FeedbackComparisonWindow)).Scan(&used); err != nil {
			return err
		}
		attempts := map[store.FeedbackComparisonKey]time.Time{}
		if err := eachFeedbackChunk(keys, func(part []store.FeedbackComparisonKey) error {
			conditions := make([]string, len(part))
			args := []any{ws}
			for i, key := range part {
				conditions[i] = `(repository=? AND base_commit=? AND head_ref=?)`
				args = append(args, key.Repository, key.Base, key.Head)
			}
			rows, err := tx.QueryContext(ctx, `SELECT repository,base_commit,head_ref,admitted_at FROM feedback_admissions WHERE workspace_id=? AND (`+strings.Join(conditions, " OR ")+`)`, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var key store.FeedbackComparisonKey
				var at time.Time
				if err := rows.Scan(&key.Repository, &key.Base, &key.Head, &at); err != nil {
					return err
				}
				attempts[key] = store.EnvironmentTime(at)
			}
			return rows.Err()
		}); err != nil {
			return err
		}
		comparisons, err := listFeedbackComparisons(ctx, tx, ws, keys)
		if err != nil {
			return err
		}
		stored := map[store.FeedbackComparisonKey]bool{}
		for _, comparison := range comparisons {
			stored[comparison.FeedbackComparisonKey] = true
		}
		admitted = store.PlanFeedbackAdmission(keys, stored, attempts, used, now, most)
		for _, key := range admitted {
			if _, err := tx.ExecContext(ctx, `INSERT INTO feedback_admissions (workspace_id,repository,base_commit,head_ref,admitted_at) VALUES (?,?,?,?,?) ON DUPLICATE KEY UPDATE admitted_at=VALUES(admitted_at)`,
				ws, key.Repository, key.Base, key.Head, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return admitted, nil
}

// RecordFeedbackComparisons implements store.FeedbackStatusStore. An insert
// that meets a stored answer keeps it.
func (s *Store) RecordFeedbackComparisons(ctx context.Context, comparisons []store.FeedbackComparison) error {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordFeedbackComparisons")
	normalized := make([]store.FeedbackComparison, 0, len(comparisons))
	for _, comparison := range comparisons {
		comparison, err := store.NormalizeFeedbackComparison(comparison)
		if err != nil {
			return err
		}
		normalized = append(normalized, comparison)
	}
	if len(normalized) == 0 {
		return nil
	}
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	return eachFeedbackChunk(normalized, func(part []store.FeedbackComparison) error {
		var args []any
		for _, comparison := range part {
			args = append(args, ws, comparison.Repository, comparison.Base, comparison.Head, comparison.Status, comparison.ComparedAt)
		}
		_, err := backendExec(ctx, s.db, `INSERT INTO feedback_comparisons (workspace_id,repository,base_commit,head_commit,status,compared_at) VALUES `+feedbackRowList(`(?,?,?,?,?,?)`, len(part))+` ON DUPLICATE KEY UPDATE status=status`, args...)
		return err
	})
}

type feedbackStatusQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func listFeedbackComparisons(ctx context.Context, q feedbackStatusQuerier, ws string, keys []store.FeedbackComparisonKey) ([]store.FeedbackComparison, error) {
	out := []store.FeedbackComparison{}
	err := eachFeedbackChunk(keys, func(part []store.FeedbackComparisonKey) error {
		conditions := make([]string, len(part))
		args := []any{ws}
		for i, key := range part {
			conditions[i] = `(repository=? AND base_commit=? AND head_commit=?)`
			args = append(args, key.Repository, key.Base, key.Head)
		}
		rows, err := q.QueryContext(ctx, `SELECT repository,base_commit,head_commit,status,compared_at FROM feedback_comparisons WHERE workspace_id=? AND (`+strings.Join(conditions, " OR ")+`)`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var comparison store.FeedbackComparison
			if err := rows.Scan(&comparison.Repository, &comparison.Base, &comparison.Head, &comparison.Status, &comparison.ComparedAt); err != nil {
				return err
			}
			comparison.ComparedAt = store.EnvironmentTime(comparison.ComparedAt)
			out = append(out, comparison)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	store.SortFeedbackComparisons(out)
	return out, nil
}

// ListFeedbackComparisons implements store.FeedbackStatusStore.
func (s *Store) ListFeedbackComparisons(ctx context.Context, keys []store.FeedbackComparisonKey) ([]store.FeedbackComparison, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackComparisons")
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	return listFeedbackComparisons(ctx, s.db, ws, store.NormalizeFeedbackComparisonKeys(keys))
}

// RecordFeedbackBranchHead implements store.FeedbackStatusStore.
func (s *Store) RecordFeedbackBranchHead(ctx context.Context, head store.FeedbackBranchHead) error {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordFeedbackBranchHead")
	head, err := store.NormalizeFeedbackBranchHead(head)
	if err != nil {
		return err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	_, err = backendExec(ctx, s.db, `INSERT INTO feedback_branch_heads (workspace_id,repository,branch,head_commit,observed_at) VALUES (?,?,?,?,?)
ON DUPLICATE KEY UPDATE head_commit=IF(observed_at<=VALUES(observed_at),VALUES(head_commit),head_commit),observed_at=IF(observed_at<=VALUES(observed_at),VALUES(observed_at),observed_at)`,
		ws, head.Repository, head.Branch, head.Commit, head.ObservedAt)
	return err
}

// ListFeedbackBranchHeads implements store.FeedbackStatusStore.
func (s *Store) ListFeedbackBranchHeads(ctx context.Context) ([]store.FeedbackBranchHead, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackBranchHeads")
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT repository,branch,head_commit,observed_at FROM feedback_branch_heads WHERE workspace_id=?`, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.FeedbackBranchHead{}
	for rows.Next() {
		var head store.FeedbackBranchHead
		if err := rows.Scan(&head.Repository, &head.Branch, &head.Commit, &head.ObservedAt); err != nil {
			return nil, err
		}
		head.ObservedAt = store.EnvironmentTime(head.ObservedAt)
		out = append(out, head)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	store.SortFeedbackBranchHeads(out)
	return out, nil
}

// ListFeedbackMergedTasks implements store.FeedbackStatusStore. It reads the
// merged tasks, then the merge events of those tasks, and keeps the latest
// event of each that records a usable merge commit.
func (s *Store) ListFeedbackMergedTasks(ctx context.Context, query store.FeedbackMergedTaskQuery) ([]store.FeedbackMergedTask, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackMergedTasks")
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	sqlText := `SELECT id,repo_name FROM tasks WHERE workspace_id=? AND state='merged'`
	args := []any{ws}
	if len(query.TaskIDs) > 0 {
		list, ids := feedbackList(query.TaskIDs)
		sqlText += ` AND id IN (` + list + `)`
		args = append(args, ids...)
	}
	if query.CrateID != "" {
		sqlText += ` AND crate_id=?`
		args = append(args, query.CrateID)
	}
	if query.Repository != "" {
		sqlText += ` AND repo_name=?`
		args = append(args, query.Repository)
	}
	rows, err := documentRows(ctx, s.db, sqlText, args...)
	if err != nil {
		return nil, err
	}
	tasks := []store.FeedbackMergedTask{}
	index := map[string]int{}
	for rows.Next() {
		var task store.FeedbackMergedTask
		if err := rows.Scan(&task.TaskID, &task.Repository); err != nil {
			rows.Close()
			return nil, err
		}
		index[task.TaskID] = len(tasks)
		tasks = append(tasks, task)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, len(tasks))
	for i, task := range tasks {
		ids[i] = task.TaskID
	}
	type mergeEvent struct {
		sha         string
		at          time.Time
		id          int64
		pullRequest string
		repository  string
	}
	latest := map[string]mergeEvent{}
	err = eachFeedbackChunk(ids, func(part []string) error {
		list, values := feedbackList(part)
		rows, err := documentRows(ctx, s.db, `SELECT task_id,COALESCE(JSON_EXTRACT_STRING(payload_json,'merge_commit_sha'),''),at,id,COALESCE(JSON_EXTRACT_STRING(payload_json,'pull_request'),''),COALESCE(JSON_EXTRACT_STRING(payload_json,'repository'),'') FROM events_columnstore WHERE workspace_id=? AND kind IN ('merge.confirmed','merge.reconciled') AND task_id IN (`+list+`)`, append([]any{ws}, values...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var taskID, sha, pullRequest, repository string
			var at time.Time
			var id int64
			if err := rows.Scan(&taskID, &sha, &at, &id, &pullRequest, &repository); err != nil {
				return err
			}
			// The latest merge event wins before its commit is checked.
			current, seen := latest[taskID]
			if !seen || at.After(current.at) || (at.Equal(current.at) && id > current.id) {
				latest[taskID] = mergeEvent{sha, at, id, pullRequest, repository}
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	for taskID, event := range latest {
		task := &tasks[index[taskID]]
		task.MergeCommit, task.MergedAt, task.EventID = store.FeedbackMergeCommit(event.sha), event.at.UTC(), event.id
		// The pull request number comes from the same latest event
		// (component-feedback-reports invariant 12).
		task.PullRequest = store.FeedbackMergePullRequest(event.pullRequest)
		task.GitHubRepository = store.FeedbackMergeRepository(event.repository)
	}
	store.SortFeedbackMergedTasks(tasks)
	return tasks, nil
}
