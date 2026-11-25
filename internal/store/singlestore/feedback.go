package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

var _ store.FeedbackStore = (*Store)(nil)

// feedbackWriteRows bounds the rows of one multi-row statement.
const feedbackWriteRows = 200

// feedbackList renders the placeholders of an IN list and its arguments.
func feedbackList(values []string) (string, []any) {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(values)), ","), args
}

// eachFeedbackChunk calls fn for every bounded part of the values.
func eachFeedbackChunk[T any](values []T, fn func([]T) error) error {
	for len(values) > 0 {
		count := min(len(values), feedbackWriteRows)
		if err := fn(values[:count]); err != nil {
			return err
		}
		values = values[count:]
	}
	return nil
}

// feedbackKeyLabel returns the label of the signal's key. The caller holds
// the workspace environment claim, which an archive and a revocation take
// too, so the result stays true until the signal commits.
func feedbackKeyLabel(ctx context.Context, tx *sql.Tx, ws string, signal store.FeedbackSignal) (string, error) {
	var label string
	err := tx.QueryRowContext(ctx, `SELECT k.label FROM environment_keys k JOIN environments e ON e.workspace_id=k.workspace_id AND e.id=k.environment_id WHERE k.workspace_id=? AND k.id=? AND k.environment_id=? AND k.revoked_at IS NULL AND e.archived=0`,
		ws, signal.KeyID, signal.EnvironmentID).Scan(&label)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrInvalidEnvironmentKey
	}
	return label, err
}

const feedbackRunningSQL = `SELECT environment_id,service,reported_repository,reported_commit,reported_version,resolved,deploy_id,deployed_at,reported_at FROM feedback_running_versions WHERE workspace_id=? AND environment_id=?`

func collectFeedbackRunning(rows environmentRows) ([]store.FeedbackRunningVersion, error) {
	defer rows.Close()
	running := []store.FeedbackRunningVersion{}
	for rows.Next() {
		var version store.FeedbackRunningVersion
		if err := rows.Scan(&version.EnvironmentID, &version.Service, &version.Repository, &version.Commit, &version.Version, &version.Resolved, &version.DeployID, &version.DeployedAt, &version.ReportedAt); err != nil {
			return nil, err
		}
		version.DeployedAt, version.ReportedAt = store.EnvironmentTime(version.DeployedAt), store.EnvironmentTime(version.ReportedAt)
		running = append(running, version)
	}
	return running, rows.Err()
}

func stopFeedbackVersions(ctx context.Context, tx *sql.Tx, ws string, signal store.FeedbackSignal, origin store.FeedbackOrigin, stopped []store.FeedbackRunningVersion) error {
	return eachFeedbackChunk(stopped, func(part []store.FeedbackRunningVersion) error {
		ids := make([]string, len(part))
		for i, version := range part {
			ids[i] = version.DeployID
		}
		list, args := feedbackList(ids)
		if _, err := tx.ExecContext(ctx, `UPDATE feedback_deploys SET stopped_at=?,stop_source=?,stop_source_label=?,stop_actor_id=? WHERE workspace_id=? AND stopped_at IS NULL AND id IN (`+list+`)`,
			append([]any{signal.ArrivedAt, origin.Source, origin.Label, origin.Actor, ws}, args...)...); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM feedback_running_versions WHERE workspace_id=? AND environment_id=? AND deploy_id IN (`+list+`)`, append([]any{ws, signal.EnvironmentID}, args...)...)
		return err
	})
}

// startFeedbackVersions inserts the new deploys with their running versions.
func startFeedbackVersions(ctx context.Context, tx *sql.Tx, ws string, signal store.FeedbackSignal, origin store.FeedbackOrigin, deploys []store.FeedbackDeploy) error {
	return eachFeedbackChunk(deploys, func(part []store.FeedbackDeploy) error {
		var deployArgs, runningArgs []any
		for _, deploy := range part {
			deployArgs = append(deployArgs, ws, deploy.ID, signal.EnvironmentID, deploy.Service, deploy.Repository, deploy.Commit, deploy.Version, signal.ArrivedAt, origin.Source, origin.Label, origin.Actor)
			runningArgs = append(runningArgs, ws, signal.EnvironmentID, store.FeedbackServiceDigest(deploy.Service), deploy.Digest(), deploy.Service, deploy.Repository, deploy.Commit, deploy.Version, deploy.ID, signal.ArrivedAt, signal.ArrivedAt)
		}
		rows := func(row string) string { return strings.TrimSuffix(strings.Repeat(row+",", len(part)), ",") }
		if _, err := tx.ExecContext(ctx, `INSERT INTO feedback_deploys (workspace_id,id,environment_id,service,reported_repository,reported_commit,reported_version,resolved,deployed_at,source,source_label,actor_id) VALUES `+rows(`(?,?,?,?,?,?,?,0,?,?,?,?)`), deployArgs...); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO feedback_running_versions (workspace_id,environment_id,service_digest,version_digest,service,reported_repository,reported_commit,reported_version,resolved,deploy_id,deployed_at,reported_at) VALUES `+rows(`(?,?,?,?,?,?,?,?,0,?,?,?)`), runningArgs...)
		return err
	})
}

// recordFeedbackNewest keeps the newest signal of the environment. The caller
// holds the workspace environment claim, so the read and the write are one
// decision.
func recordFeedbackNewest(ctx context.Context, tx *sql.Tx, ws string, signal store.FeedbackSignal, origin store.FeedbackOrigin) error {
	var newest sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT last_signal_at FROM feedback_signals WHERE workspace_id=? AND environment_id=?`, ws, signal.EnvironmentID).Scan(&newest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `INSERT INTO feedback_signals (workspace_id,environment_id,last_signal_at,next_signal_seconds,source,source_label,actor_id) VALUES (?,?,?,?,?,?,?)`,
			ws, signal.EnvironmentID, signal.ArrivedAt, signal.NextSignalSeconds, origin.Source, origin.Label, origin.Actor)
	case err != nil || store.EnvironmentTime(newest.Time).After(signal.ArrivedAt):
	default:
		_, err = tx.ExecContext(ctx, `UPDATE feedback_signals SET last_signal_at=?,next_signal_seconds=?,source=?,source_label=?,actor_id=? WHERE workspace_id=? AND environment_id=?`,
			signal.ArrivedAt, signal.NextSignalSeconds, origin.Source, origin.Label, origin.Actor, ws, signal.EnvironmentID)
	}
	return err
}

// RecordFeedbackSignal implements store.FeedbackStore. It holds the workspace
// environment claim, waits until the last environment change or signal is
// visible, and stamps the environment row as its own witness. The feedback
// tables share the shard key of that row, so a later holder sees every row
// this signal wrote (component-persistence invariant 8).
func (s *Store) RecordFeedbackSignal(ctx context.Context, signal store.FeedbackSignal) error {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordFeedbackSignal")
	signal, err := store.NormalizeFeedbackSignal(signal)
	if err != nil {
		return err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	// SingleStore has no CHECK constraint, so the store holds the reference.
	if signal.EnvironmentID == "" || signal.KeyID == "" {
		return store.ErrInvalidEnvironmentKey
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		claim, err := claimEnvironments(ctx, tx, ws)
		if err != nil {
			return err
		}
		label, err := feedbackKeyLabel(ctx, tx, ws, signal)
		if err != nil {
			return err
		}
		repositories, err := environmentRepositoriesTx(ctx, tx, ws)
		if err != nil {
			return err
		}
		if err := store.CheckFeedbackRepositories(signal, repositories); err != nil {
			return err
		}
		digests := make([]string, len(signal.Services))
		for i, service := range signal.Services {
			digests[i] = store.FeedbackServiceDigest(service.Name)
		}
		list, args := feedbackList(digests)
		rows, err := tx.QueryContext(ctx, feedbackRunningSQL+` AND service_digest IN (`+list+`)`, append([]any{ws, signal.EnvironmentID}, args...)...)
		if err != nil {
			return err
		}
		running, err := collectFeedbackRunning(rows)
		if err != nil {
			return err
		}
		origin := store.FeedbackSignalOrigin(label)
		changes, err := store.PlanFeedbackChanges(signal, origin, running)
		if err != nil {
			return err
		}
		if err := stopFeedbackVersions(ctx, tx, ws, signal, origin, changes.Stopped); err != nil {
			return err
		}
		if err := startFeedbackVersions(ctx, tx, ws, signal, origin, changes.Deploys); err != nil {
			return err
		}
		if len(changes.Reported) > 0 {
			reported := make([]string, len(changes.Reported))
			for i, service := range changes.Reported {
				reported[i] = store.FeedbackServiceDigest(service)
			}
			list, args := feedbackList(reported)
			if _, err := tx.ExecContext(ctx, `UPDATE feedback_running_versions SET reported_at=? WHERE workspace_id=? AND environment_id=? AND reported_at<? AND service_digest IN (`+list+`)`,
				append([]any{signal.ArrivedAt, ws, signal.EnvironmentID, signal.ArrivedAt}, args...)...); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE environment_keys SET last_used_at=CURRENT_TIMESTAMP(6) WHERE workspace_id=? AND id=? AND (last_used_at IS NULL OR last_used_at<DATE_SUB(CURRENT_TIMESTAMP(6),INTERVAL ? MICROSECOND))",
			ws, signal.KeyID, store.EnvironmentKeyUseGranularity.Microseconds()); err != nil {
			return err
		}
		if err := recordFeedbackNewest(ctx, tx, ws, signal, origin); err != nil {
			return err
		}
		return claim.touchEnvironment(ctx, tx, signal.EnvironmentID)
	})
}

// ListFeedbackSignals implements store.FeedbackStore.
func (s *Store) ListFeedbackSignals(ctx context.Context, environmentIDs []string) (map[string]store.FeedbackSignalState, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackSignals")
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]store.FeedbackSignalState{}
	if len(environmentIDs) == 0 {
		return out, nil
	}
	list, args := feedbackList(environmentIDs)
	rows, err := documentRows(ctx, s.db, `SELECT environment_id,last_signal_at,next_signal_seconds,source,source_label,actor_id FROM feedback_signals WHERE workspace_id=? AND environment_id IN (`+list+`)`, append([]any{ws}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var state store.FeedbackSignalState
		if err := rows.Scan(&state.EnvironmentID, &state.LastSignalAt, &state.NextSignalSeconds, &state.Origin.Source, &state.Origin.Label, &state.Origin.Actor); err != nil {
			return nil, err
		}
		state.LastSignalAt = store.EnvironmentTime(state.LastSignalAt)
		out[state.EnvironmentID] = state
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListFeedbackRunningVersions implements store.FeedbackStore.
func (s *Store) ListFeedbackRunningVersions(ctx context.Context, environmentID string) ([]store.FeedbackRunningVersion, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackRunningVersions")
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, feedbackRunningSQL, ws, environmentID)
	if err != nil {
		return nil, err
	}
	running, err := collectFeedbackRunning(rows)
	if err != nil {
		return nil, err
	}
	store.SortFeedbackRunningVersions(running)
	return running, nil
}

// ListFeedbackDeploys implements store.FeedbackStore.
func (s *Store) ListFeedbackDeploys(ctx context.Context, environmentID string) ([]store.FeedbackDeploy, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackDeploys")
	ws, err := workspace(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := documentRows(ctx, s.db, `SELECT id,environment_id,service,reported_repository,reported_commit,reported_version,resolved,deployed_at,source,source_label,actor_id,stopped_at,stop_source,stop_source_label,stop_actor_id FROM feedback_deploys WHERE workspace_id=? AND environment_id=?`, ws, environmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deploys := []store.FeedbackDeploy{}
	for rows.Next() {
		var deploy store.FeedbackDeploy
		var stoppedAt sql.NullTime
		var stopSource, stopLabel, stopActor sql.NullString
		if err := rows.Scan(&deploy.ID, &deploy.EnvironmentID, &deploy.Service, &deploy.Repository, &deploy.Commit, &deploy.Version, &deploy.Resolved, &deploy.DeployedAt,
			&deploy.Origin.Source, &deploy.Origin.Label, &deploy.Origin.Actor, &stoppedAt, &stopSource, &stopLabel, &stopActor); err != nil {
			return nil, err
		}
		deploy.DeployedAt = store.EnvironmentTime(deploy.DeployedAt)
		if stoppedAt.Valid {
			if !stopSource.Valid || !stopLabel.Valid || !stopActor.Valid {
				return nil, fmt.Errorf("a stopped deploy names no origin")
			}
			at := store.EnvironmentTime(stoppedAt.Time)
			deploy.StoppedAt, deploy.StopOrigin = &at, &store.FeedbackOrigin{Source: stopSource.String, Label: stopLabel.String, Actor: stopActor.String}
		}
		deploys = append(deploys, deploy)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	store.SortFeedbackDeploys(deploys)
	return deploys, nil
}
