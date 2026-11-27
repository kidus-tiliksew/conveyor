package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

var _ store.FeedbackStatusStore = (*Store)(nil)

// ClaimFeedbackResolution implements store.FeedbackStatusStore. One statement
// inserts the pending attempt or takes over an unresolved row whose next
// attempt time the signal has reached, so two claims never both win.
func (s *Store) ClaimFeedbackResolution(ctx context.Context, ref store.FeedbackVersionRef, signalAt, now time.Time) (bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ClaimFeedbackResolution")
	pending := store.FeedbackPendingResolution(ref, now)
	var claimed bool
	err := s.boundary.QueryRow(ctx, `INSERT INTO feedback_resolutions (workspace_id, environment_id, service_digest, version_digest, service, reported_repository, reported_commit, reported_version, reason, attempted_at, next_attempt_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (workspace_id, environment_id, service_digest, version_digest) DO UPDATE
SET reason=EXCLUDED.reason, attempted_at=EXCLUDED.attempted_at, next_attempt_at=EXCLUDED.next_attempt_at
WHERE feedback_resolutions.resolved_commit = '' AND feedback_resolutions.next_attempt_at <= $12
RETURNING true`,
		workspace(ctx), ref.EnvironmentID, store.FeedbackServiceDigest(ref.Service), ref.Digest(), ref.Service, ref.Repository, ref.Commit, ref.Version,
		pending.Reason, pending.AttemptedAt, pending.NextAttemptAt, store.EnvironmentTime(signalAt)).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, translateBackendConflict(err)
	}
	return claimed, nil
}

// RecordFeedbackResolution implements store.FeedbackStatusStore.
func (s *Store) RecordFeedbackResolution(ctx context.Context, resolution store.FeedbackResolution) error {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordFeedbackResolution")
	resolution, err := store.NormalizeFeedbackResolution(resolution)
	if err != nil {
		return err
	}
	workspaceID := workspace(ctx)
	var next *time.Time
	if !resolution.Resolved() {
		next = &resolution.NextAttemptAt
	}
	return s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		tag, err := tx.Exec(ctx, `INSERT INTO feedback_resolutions (workspace_id, environment_id, service_digest, version_digest, service, reported_repository, reported_commit, reported_version, resolved_repository, resolved_commit, reason, attempted_at, next_attempt_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (workspace_id, environment_id, service_digest, version_digest) DO UPDATE
SET resolved_repository=EXCLUDED.resolved_repository, resolved_commit=EXCLUDED.resolved_commit, reason=EXCLUDED.reason, attempted_at=EXCLUDED.attempted_at, next_attempt_at=EXCLUDED.next_attempt_at
WHERE feedback_resolutions.resolved_commit = ''`,
			workspaceID, resolution.EnvironmentID, store.FeedbackServiceDigest(resolution.Service), resolution.Digest(), resolution.Service, resolution.Repository, resolution.Commit, resolution.Version,
			resolution.ResolvedRepository, resolution.ResolvedCommit, resolution.Reason, resolution.AttemptedAt, next)
		if err != nil || tag.RowsAffected() == 0 || !resolution.Resolved() {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE feedback_running_versions SET resolved=true WHERE workspace_id=$1 AND environment_id=$2 AND service_digest=$3 AND version_digest=$4`,
			workspaceID, resolution.EnvironmentID, store.FeedbackServiceDigest(resolution.Service), resolution.Digest()); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE feedback_deploys SET resolved=true WHERE workspace_id=$1 AND environment_id=$2 AND service=$3 AND reported_repository=$4 AND reported_commit=$5 AND reported_version=$6`,
			workspaceID, resolution.EnvironmentID, resolution.Service, resolution.Repository, resolution.Commit, resolution.Version)
		return err
	})
}

// ReadFeedbackInputs implements store.FeedbackStatusStore. Each table is read
// once for every named environment.
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
	workspaceID := workspace(ctx)
	running, err := queryFeedbackRunning(ctx, s.boundary, `SELECT environment_id, service, reported_repository, reported_commit, reported_version, resolved, deploy_id, deployed_at, reported_at FROM feedback_running_versions WHERE workspace_id=$1 AND environment_id=ANY($2::text[])`, workspaceID, environmentIDs)
	if err != nil {
		return store.FeedbackInputs{}, err
	}
	for _, version := range running {
		inputs.Running[version.EnvironmentID] = append(inputs.Running[version.EnvironmentID], version)
	}
	for id := range inputs.Running {
		store.SortFeedbackRunningVersions(inputs.Running[id])
	}
	deploys, err := queryFeedbackDeploys(ctx, s.boundary, `SELECT id, environment_id, service, reported_repository, reported_commit, reported_version, resolved, deployed_at, source, source_label, actor_id, report, stopped_at, stop_source, stop_source_label, stop_actor_id, stop_report FROM feedback_deploys WHERE workspace_id=$1 AND environment_id=ANY($2::text[])`, workspaceID, environmentIDs)
	if err != nil {
		return store.FeedbackInputs{}, err
	}
	for _, deploy := range deploys {
		inputs.Deploys[deploy.EnvironmentID] = append(inputs.Deploys[deploy.EnvironmentID], deploy)
	}
	for id := range inputs.Deploys {
		store.SortFeedbackDeploys(inputs.Deploys[id])
	}
	rows, err := s.boundary.Query(ctx, `SELECT environment_id, service, reported_repository, reported_commit, reported_version, resolved_repository, resolved_commit, reason, attempted_at, next_attempt_at FROM feedback_resolutions WHERE workspace_id=$1 AND environment_id=ANY($2::text[])`, workspaceID, environmentIDs)
	if err != nil {
		return store.FeedbackInputs{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var resolution store.FeedbackResolution
		var next pgtype.Timestamptz
		if err := rows.Scan(&resolution.EnvironmentID, &resolution.Service, &resolution.Repository, &resolution.Commit, &resolution.Version, &resolution.ResolvedRepository, &resolution.ResolvedCommit, &resolution.Reason, &resolution.AttemptedAt, &next); err != nil {
			return store.FeedbackInputs{}, err
		}
		resolution.AttemptedAt = store.EnvironmentTime(resolution.AttemptedAt)
		if next.Valid {
			resolution.NextAttemptAt = store.EnvironmentTime(next.Time)
		}
		inputs.Resolutions = append(inputs.Resolutions, resolution)
	}
	if err := rows.Err(); err != nil {
		return store.FeedbackInputs{}, err
	}
	store.SortFeedbackResolutions(inputs.Resolutions)
	return inputs, nil
}

// AdmitFeedbackComparisons implements store.FeedbackStatusStore. A
// transaction advisory lock on the workspace serializes admissions, so the
// allowance and the in-flight rule hold across workers and replicas.
func (s *Store) AdmitFeedbackComparisons(ctx context.Context, keys []store.FeedbackComparisonKey, now time.Time, most int) ([]store.FeedbackComparisonKey, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "AdmitFeedbackComparisons")
	keys = store.NormalizeFeedbackComparisonKeys(keys)
	waited, asked := time.Now(), now
	workspaceID := workspace(ctx)
	var admitted []store.FeedbackComparisonKey
	err := s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('feedback_admissions:' || $1, 0))`, workspaceID); err != nil {
			return err
		}
		// The time is read after the lock is held, so a call that waited
		// counts and records when its request can start.
		now := store.FeedbackAdmissionTime(asked, waited)
		if _, err := tx.Exec(ctx, `DELETE FROM feedback_admissions WHERE workspace_id=$1 AND admitted_at <= $2`, workspaceID, now.Add(-store.FeedbackAttemptMemory)); err != nil {
			return err
		}
		var used int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM feedback_admissions WHERE workspace_id=$1 AND admitted_at > $2`, workspaceID, now.Add(-store.FeedbackComparisonWindow)).Scan(&used); err != nil {
			return err
		}
		attempts := map[store.FeedbackComparisonKey]time.Time{}
		if len(keys) > 0 {
			repositories, bases, heads := make([]string, len(keys)), make([]string, len(keys)), make([]string, len(keys))
			for i, key := range keys {
				repositories[i], bases[i], heads[i] = key.Repository, key.Base, key.Head
			}
			rows, err := tx.Query(ctx, `SELECT a.repository, a.base_commit, a.head_ref, a.admitted_at
FROM feedback_admissions a
JOIN unnest($2::text[], $3::text[], $4::text[]) AS k(repository, base_commit, head_ref)
  ON a.repository=k.repository AND a.base_commit=k.base_commit AND a.head_ref=k.head_ref
WHERE a.workspace_id=$1`, workspaceID, repositories, bases, heads)
			if err != nil {
				return err
			}
			for rows.Next() {
				var key store.FeedbackComparisonKey
				var at time.Time
				if err := rows.Scan(&key.Repository, &key.Base, &key.Head, &at); err != nil {
					rows.Close()
					return err
				}
				attempts[key] = store.EnvironmentTime(at)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		comparisons, err := listFeedbackComparisons(ctx, tx, workspaceID, keys)
		if err != nil {
			return err
		}
		stored := map[store.FeedbackComparisonKey]bool{}
		for _, comparison := range comparisons {
			stored[comparison.FeedbackComparisonKey] = true
		}
		admitted = store.PlanFeedbackAdmission(keys, stored, attempts, used, now, most)
		for _, key := range admitted {
			if _, err := tx.Exec(ctx, `INSERT INTO feedback_admissions (workspace_id, repository, base_commit, head_ref, admitted_at) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (workspace_id, repository, base_commit, head_ref) DO UPDATE SET admitted_at=EXCLUDED.admitted_at`, workspaceID, key.Repository, key.Base, key.Head, now); err != nil {
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

// RecordFeedbackComparisons implements store.FeedbackStatusStore.
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
	workspaceID := workspace(ctx)
	return s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		for _, comparison := range normalized {
			if _, err := tx.Exec(ctx, `INSERT INTO feedback_comparisons (workspace_id, repository, base_commit, head_commit, status, compared_at) VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
				workspaceID, comparison.Repository, comparison.Base, comparison.Head, comparison.Status, comparison.ComparedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

type feedbackQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func listFeedbackComparisons(ctx context.Context, q feedbackQuerier, workspaceID string, keys []store.FeedbackComparisonKey) ([]store.FeedbackComparison, error) {
	out := []store.FeedbackComparison{}
	if len(keys) == 0 {
		return out, nil
	}
	repositories, bases, heads := make([]string, len(keys)), make([]string, len(keys)), make([]string, len(keys))
	for i, key := range keys {
		repositories[i], bases[i], heads[i] = key.Repository, key.Base, key.Head
	}
	rows, err := q.Query(ctx, `SELECT c.repository, c.base_commit, c.head_commit, c.status, c.compared_at
FROM feedback_comparisons c
JOIN unnest($2::text[], $3::text[], $4::text[]) AS k(repository, base_commit, head_commit)
  ON c.repository=k.repository AND c.base_commit=k.base_commit AND c.head_commit=k.head_commit
WHERE c.workspace_id=$1`, workspaceID, repositories, bases, heads)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var comparison store.FeedbackComparison
		if err := rows.Scan(&comparison.Repository, &comparison.Base, &comparison.Head, &comparison.Status, &comparison.ComparedAt); err != nil {
			return nil, err
		}
		comparison.ComparedAt = store.EnvironmentTime(comparison.ComparedAt)
		out = append(out, comparison)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	store.SortFeedbackComparisons(out)
	return out, nil
}

// ListFeedbackComparisons implements store.FeedbackStatusStore.
func (s *Store) ListFeedbackComparisons(ctx context.Context, keys []store.FeedbackComparisonKey) ([]store.FeedbackComparison, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackComparisons")
	return listFeedbackComparisons(ctx, s.boundary, workspace(ctx), store.NormalizeFeedbackComparisonKeys(keys))
}

// RecordFeedbackBranchHead implements store.FeedbackStatusStore.
func (s *Store) RecordFeedbackBranchHead(ctx context.Context, head store.FeedbackBranchHead) error {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordFeedbackBranchHead")
	head, err := store.NormalizeFeedbackBranchHead(head)
	if err != nil {
		return err
	}
	_, err = s.boundary.Exec(ctx, `INSERT INTO feedback_branch_heads (workspace_id, repository, branch, head_commit, observed_at) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (workspace_id, repository, branch) DO UPDATE SET head_commit=EXCLUDED.head_commit, observed_at=EXCLUDED.observed_at
WHERE feedback_branch_heads.observed_at <= EXCLUDED.observed_at`, workspace(ctx), head.Repository, head.Branch, head.Commit, head.ObservedAt)
	return err
}

// ListFeedbackBranchHeads implements store.FeedbackStatusStore.
func (s *Store) ListFeedbackBranchHeads(ctx context.Context) ([]store.FeedbackBranchHead, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackBranchHeads")
	rows, err := s.boundary.Query(ctx, `SELECT repository, branch, head_commit, observed_at FROM feedback_branch_heads WHERE workspace_id=$1`, workspace(ctx))
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

// ListFeedbackMergedTasks implements store.FeedbackStatusStore. The latest
// merge.confirmed or merge.reconciled event of each merged task that records a
// usable merge commit gives its merge commit and time.
func (s *Store) ListFeedbackMergedTasks(ctx context.Context, query store.FeedbackMergedTaskQuery) ([]store.FeedbackMergedTask, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackMergedTasks")
	ids := query.TaskIDs
	if ids == nil {
		ids = []string{}
	}
	rows, err := s.boundary.Query(ctx, `SELECT t.id, t.repo_name, COALESCE(m.sha, ''), m.at, COALESCE(m.id, 0), COALESCE(m.pull_request, ''), COALESCE(m.repository, '')
FROM tasks t
LEFT JOIN LATERAL (
  SELECT e.payload_json->>'merge_commit_sha' AS sha, e.at, e.id, e.payload_json->>'pull_request' AS pull_request, e.payload_json->>'repository' AS repository FROM events e
  WHERE e.workspace_id=t.workspace_id AND e.task_id=t.id AND e.kind IN ('merge.confirmed', 'merge.reconciled')
  ORDER BY e.at DESC, e.id DESC LIMIT 1
) m ON true
WHERE t.workspace_id=$1 AND t.state='merged'
  AND (cardinality($2::text[]) = 0 OR t.id=ANY($2::text[]))
  AND ($3::text = '' OR t.crate_id=$3::text)
  AND ($4::text = '' OR t.repo_name=$4::text)`, workspace(ctx), ids, query.CrateID, query.Repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.FeedbackMergedTask{}
	for rows.Next() {
		var task store.FeedbackMergedTask
		var sha, pullRequest, repository string
		var at pgtype.Timestamptz
		var id int64
		if err := rows.Scan(&task.TaskID, &task.Repository, &sha, &at, &id, &pullRequest, &repository); err != nil {
			return nil, err
		}
		// The latest merge event wins before its commit is checked, so an
		// unusable commit leaves MergeCommit empty with that event's time.
		// Its pull request number comes from the same event
		// (component-feedback-reports invariant 12).
		if at.Valid {
			task.MergeCommit, task.MergedAt, task.EventID = store.FeedbackMergeCommit(sha), at.Time.UTC(), id
			task.PullRequest = store.FeedbackMergePullRequest(pullRequest)
			task.GitHubRepository = store.FeedbackMergeRepository(repository)
		}
		out = append(out, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	store.SortFeedbackMergedTasks(out)
	return out, nil
}
