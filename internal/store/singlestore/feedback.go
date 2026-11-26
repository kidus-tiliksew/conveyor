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
		if _, err := tx.ExecContext(ctx, `UPDATE feedback_deploys SET stopped_at=?,stop_source=?,stop_source_label=?,stop_actor_id=?,stop_report=? WHERE workspace_id=? AND stopped_at IS NULL AND id IN (`+list+`)`,
			append([]any{signal.ArrivedAt, origin.Source, origin.Label, origin.Actor, origin.Report, ws}, args...)...); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM feedback_running_versions WHERE workspace_id=? AND environment_id=? AND deploy_id IN (`+list+`)`, append([]any{ws, signal.EnvironmentID}, args...)...)
		return err
	})
}

func feedbackStopColumns(deploy store.FeedbackDeploy) []any {
	if deploy.StoppedAt == nil || deploy.StopOrigin == nil {
		return []any{nil, nil, nil, nil, nil}
	}
	return []any{*deploy.StoppedAt, deploy.StopOrigin.Source, deploy.StopOrigin.Label, deploy.StopOrigin.Actor, deploy.StopOrigin.Report}
}

func feedbackRowList(row string, count int) string {
	return strings.TrimSuffix(strings.Repeat(row+",", count), ",")
}

// startFeedbackVersions inserts the new deploys and running versions.
func startFeedbackVersions(ctx context.Context, tx *sql.Tx, ws string, signal store.FeedbackSignal, changes store.FeedbackChanges) error {
	if err := eachFeedbackChunk(changes.Deploys, func(part []store.FeedbackDeploy) error {
		var args []any
		for _, deploy := range part {
			args = append(args, ws, deploy.ID, signal.EnvironmentID, deploy.Service, deploy.Repository, deploy.Commit, deploy.Version, deploy.Resolved, deploy.DeployedAt, deploy.Origin.Source, deploy.Origin.Label, deploy.Origin.Actor, deploy.Origin.Report)
			args = append(args, feedbackStopColumns(deploy)...)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO feedback_deploys (workspace_id,id,environment_id,service,reported_repository,reported_commit,reported_version,resolved,deployed_at,source,source_label,actor_id,report,stopped_at,stop_source,stop_source_label,stop_actor_id,stop_report) VALUES `+feedbackRowList(`(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, len(part)), args...)
		return err
	}); err != nil {
		return err
	}
	return eachFeedbackChunk(changes.Started, func(part []store.FeedbackRunningVersion) error {
		var args []any
		for _, version := range part {
			args = append(args, ws, signal.EnvironmentID, store.FeedbackServiceDigest(version.Service), version.Digest(), version.Service, version.Repository, version.Commit, version.Version, version.DeployID, version.DeployedAt, version.ReportedAt)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO feedback_running_versions (workspace_id,environment_id,service_digest,version_digest,service,reported_repository,reported_commit,reported_version,resolved,deploy_id,deployed_at,reported_at) VALUES `+feedbackRowList(`(?,?,?,?,?,?,?,?,0,?,?,?)`, len(part)), args...)
		return err
	})
}

// moveFeedbackRecords writes the earlier records that an overtaken signal
// changes.
func moveFeedbackRecords(ctx context.Context, tx *sql.Tx, ws string, signal store.FeedbackSignal, changes store.FeedbackChanges) error {
	for _, deploy := range changes.Updated {
		args := append([]any{deploy.DeployedAt, deploy.Origin.Source, deploy.Origin.Label, deploy.Origin.Actor, deploy.Origin.Report}, feedbackStopColumns(deploy)...)
		if _, err := tx.ExecContext(ctx, `UPDATE feedback_deploys SET deployed_at=?,source=?,source_label=?,actor_id=?,report=?,stopped_at=?,stop_source=?,stop_source_label=?,stop_actor_id=?,stop_report=? WHERE workspace_id=? AND id=?`, append(args, ws, deploy.ID)...); err != nil {
			return err
		}
	}
	for _, version := range changes.Moved {
		if _, err := tx.ExecContext(ctx, `UPDATE feedback_running_versions SET deploy_id=?,deployed_at=? WHERE workspace_id=? AND environment_id=? AND service_digest=? AND version_digest=?`,
			version.DeployID, version.DeployedAt, ws, signal.EnvironmentID, store.FeedbackServiceDigest(version.Service), version.Digest()); err != nil {
			return err
		}
	}
	return nil
}

const feedbackDeploySQL = `SELECT id,environment_id,service,reported_repository,reported_commit,reported_version,resolved,deployed_at,source,source_label,actor_id,report,stopped_at,stop_source,stop_source_label,stop_actor_id,stop_report FROM feedback_deploys WHERE workspace_id=? AND environment_id=?`

func collectFeedbackDeploys(rows environmentRows) ([]store.FeedbackDeploy, error) {
	defer rows.Close()
	deploys := []store.FeedbackDeploy{}
	for rows.Next() {
		var deploy store.FeedbackDeploy
		var stoppedAt sql.NullTime
		var stopSource, stopLabel, stopActor sql.NullString
		var stopReport sql.NullInt64
		if err := rows.Scan(&deploy.ID, &deploy.EnvironmentID, &deploy.Service, &deploy.Repository, &deploy.Commit, &deploy.Version, &deploy.Resolved, &deploy.DeployedAt,
			&deploy.Origin.Source, &deploy.Origin.Label, &deploy.Origin.Actor, &deploy.Origin.Report, &stoppedAt, &stopSource, &stopLabel, &stopActor, &stopReport); err != nil {
			return nil, err
		}
		deploy.DeployedAt = store.EnvironmentTime(deploy.DeployedAt)
		if stoppedAt.Valid {
			if !stopSource.Valid || !stopLabel.Valid || !stopActor.Valid || !stopReport.Valid {
				return nil, fmt.Errorf("a stopped deploy names no origin")
			}
			at := store.EnvironmentTime(stoppedAt.Time)
			deploy.StoppedAt, deploy.StopOrigin = &at, &store.FeedbackOrigin{Source: stopSource.String, Label: stopLabel.String, Actor: stopActor.String, Report: stopReport.Int64}
		}
		deploys = append(deploys, deploy)
	}
	return deploys, rows.Err()
}

// readFeedbackHistory reads what PlanFeedbackChanges needs. Only a signal that
// a later arrival overtook reads the accepted signals and the deploys.
func readFeedbackHistory(ctx context.Context, tx *sql.Tx, ws string, signal store.FeedbackSignal) (store.FeedbackHistory, error) {
	var history store.FeedbackHistory
	digests := make([]string, len(signal.Services))
	for i, service := range signal.Services {
		digests[i] = store.FeedbackServiceDigest(service.Name)
	}
	list, args := feedbackList(digests)
	rows, err := tx.QueryContext(ctx, feedbackRunningSQL+` AND service_digest IN (`+list+`)`, append([]any{ws, signal.EnvironmentID}, args...)...)
	if err != nil {
		return history, err
	}
	if history.Running, err = collectFeedbackRunning(rows); err != nil {
		return history, err
	}
	late := store.FeedbackLateServices(signal, history.Running)
	if len(late) == 0 {
		return history, nil
	}
	if rows, err = tx.QueryContext(ctx, feedbackDeploySQL+` AND (stopped_at IS NULL OR stopped_at>?)`, ws, signal.EnvironmentID, signal.ArrivedAt); err != nil {
		return history, err
	}
	if history.Deploys, err = collectFeedbackDeploys(rows); err != nil {
		return history, err
	}
	if rows, err = tx.QueryContext(ctx, `SELECT arrived_at,report,source_label,service_digests FROM feedback_accepted_signals WHERE workspace_id=? AND environment_id=? AND arrived_at>? ORDER BY arrived_at,report`, ws, signal.EnvironmentID, signal.ArrivedAt); err != nil {
		return history, err
	}
	defer rows.Close()
	for rows.Next() {
		var accepted store.FeedbackAcceptedSignal
		var services string
		if err := rows.Scan(&accepted.ArrivedAt, &accepted.Report, &accepted.Label, &services); err != nil {
			return history, err
		}
		accepted.ArrivedAt = store.EnvironmentTime(accepted.ArrivedAt)
		if err := store.ApplyFeedbackAcceptedServices(&accepted, []byte(services)); err != nil {
			return history, err
		}
		if history.AddLater(late, accepted) {
			break
		}
	}
	return history, rows.Err()
}

// recordFeedbackAccepted keeps the signal as a boundary of the deploy history.
func recordFeedbackAccepted(ctx context.Context, tx *sql.Tx, ws string, signal store.FeedbackSignal, origin store.FeedbackOrigin) error {
	accepted := store.NewFeedbackAcceptedSignal(signal, origin)
	_, err := tx.ExecContext(ctx, `INSERT INTO feedback_accepted_signals (workspace_id,environment_id,arrived_at,report,source_label,service_digests) VALUES (?,?,?,?,?,?)`,
		ws, signal.EnvironmentID, accepted.ArrivedAt, accepted.Report, accepted.Label, store.FeedbackAcceptedServices(accepted))
	return err
}

// recordFeedbackNewest keeps the newest signal of the environment and the
// number of the last accepted one. The caller
// holds the workspace environment claim, so the read and the write are one
// decision.
func recordFeedbackNewest(ctx context.Context, tx *sql.Tx, ws string, signal store.FeedbackSignal, origin store.FeedbackOrigin) error {
	var newest sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT last_signal_at FROM feedback_signals WHERE workspace_id=? AND environment_id=?`, ws, signal.EnvironmentID).Scan(&newest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `INSERT INTO feedback_signals (workspace_id,environment_id,last_signal_at,next_signal_seconds,source,source_label,actor_id,last_report) VALUES (?,?,?,?,?,?,?,?)`,
			ws, signal.EnvironmentID, signal.ArrivedAt, signal.NextSignalSeconds, origin.Source, origin.Label, origin.Actor, origin.Report)
	case err != nil:
	case store.EnvironmentTime(newest.Time).After(signal.ArrivedAt):
		_, err = tx.ExecContext(ctx, `UPDATE feedback_signals SET last_report=? WHERE workspace_id=? AND environment_id=?`, origin.Report, ws, signal.EnvironmentID)
	default:
		_, err = tx.ExecContext(ctx, `UPDATE feedback_signals SET last_signal_at=?,next_signal_seconds=?,source=?,source_label=?,actor_id=?,last_report=? WHERE workspace_id=? AND environment_id=?`,
			signal.ArrivedAt, signal.NextSignalSeconds, origin.Source, origin.Label, origin.Actor, origin.Report, ws, signal.EnvironmentID)
	}
	return err
}

// CheckFeedbackRepositories implements store.FeedbackStore.
func (s *Store) CheckFeedbackRepositories(ctx context.Context, signal store.FeedbackSignal) error {
	ctx = dbmetrics.WithReadLabel(ctx, "CheckFeedbackRepositories")
	ws, err := workspace(ctx)
	if err != nil {
		return err
	}
	var configYAML string
	err = backendRow(ctx, s.db, `SELECT config_yaml FROM workspaces WHERE id=?`, ws).Scan(&configYAML)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: workspace %s", store.ErrNotFound, ws)
	}
	if err != nil {
		return translateBackendConflict(err)
	}
	repositories, err := store.EnvironmentRepositories(configYAML)
	if err != nil {
		return err
	}
	return store.CheckFeedbackRepositories(signal, repositories)
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
		history, err := readFeedbackHistory(ctx, tx, ws, signal)
		if err != nil {
			return err
		}
		// The claim makes this the next number.
		next, err := nextFeedbackReport(ctx, tx, ws, signal.EnvironmentID)
		if err != nil {
			return err
		}
		origin := store.FeedbackSignalOrigin(label, next)
		changes, err := store.PlanFeedbackChanges(signal, origin, history)
		if err != nil {
			return err
		}
		if err := writeFeedbackChanges(ctx, tx, ws, signal, origin, changes); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE environment_keys SET last_used_at=CURRENT_TIMESTAMP(6) WHERE workspace_id=? AND id=? AND (last_used_at IS NULL OR last_used_at<DATE_SUB(CURRENT_TIMESTAMP(6),INTERVAL ? MICROSECOND))",
			ws, signal.KeyID, store.EnvironmentKeyUseGranularity.Microseconds()); err != nil {
			return err
		}
		if err := recordFeedbackNewest(ctx, tx, ws, signal, origin); err != nil {
			return err
		}
		if err := recordFeedbackAccepted(ctx, tx, ws, signal, origin); err != nil {
			return err
		}
		return claim.touchEnvironment(ctx, tx, signal.EnvironmentID)
	})
}

// writeFeedbackChanges records a planned change set: the stops, the new
// deploys and running versions, the moved records, and the newest report of
// each service.
func writeFeedbackChanges(ctx context.Context, tx *sql.Tx, ws string, signal store.FeedbackSignal, origin store.FeedbackOrigin, changes store.FeedbackChanges) error {
	if err := stopFeedbackVersions(ctx, tx, ws, signal, origin, changes.Stopped); err != nil {
		return err
	}
	if err := startFeedbackVersions(ctx, tx, ws, signal, changes); err != nil {
		return err
	}
	if err := moveFeedbackRecords(ctx, tx, ws, signal, changes); err != nil {
		return err
	}
	if len(changes.Reported) == 0 {
		return nil
	}
	reported := make([]string, len(changes.Reported))
	for i, service := range changes.Reported {
		reported[i] = store.FeedbackServiceDigest(service)
	}
	list, args := feedbackList(reported)
	_, err := tx.ExecContext(ctx, `UPDATE feedback_running_versions SET reported_at=? WHERE workspace_id=? AND environment_id=? AND service_digest IN (`+list+`)`,
		append([]any{signal.ArrivedAt, ws, signal.EnvironmentID}, args...)...)
	return err
}

// nextFeedbackReport numbers the next accepted report of an environment.
// Signals and GitHub deployment reports share the numbers, and a GitHub
// report keeps its number only on the deploys and stops it records. The
// caller holds the workspace environment claim.
func nextFeedbackReport(ctx context.Context, tx *sql.Tx, ws, environmentID string) (int64, error) {
	var signals, starts, stops int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(last_report),0) FROM feedback_signals WHERE workspace_id=? AND environment_id=?`, ws, environmentID).Scan(&signals); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(report),0), COALESCE(MAX(stop_report),0) FROM feedback_deploys WHERE workspace_id=? AND environment_id=?`, ws, environmentID).Scan(&starts, &stops); err != nil {
		return 0, err
	}
	var accepted int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(report),0) FROM feedback_accepted_signals WHERE workspace_id=? AND environment_id=?`, ws, environmentID).Scan(&accepted); err != nil {
		return 0, err
	}
	return store.NextFeedbackReport(signals, starts, stops, accepted), nil
}

// feedbackGitHubRepeat reads the accepted GitHub reports of the environment
// at the signal's time and reports whether one names its service.
func feedbackGitHubRepeat(ctx context.Context, tx *sql.Tx, ws string, signal store.FeedbackSignal) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT arrived_at,report,source_label,service_digests FROM feedback_accepted_signals WHERE workspace_id=? AND environment_id=? AND arrived_at=? AND source_label=''`, ws, signal.EnvironmentID, signal.ArrivedAt)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var accepted []store.FeedbackAcceptedSignal
	for rows.Next() {
		var report store.FeedbackAcceptedSignal
		var services []byte
		if err := rows.Scan(&report.ArrivedAt, &report.Report, &report.Label, &services); err != nil {
			return false, err
		}
		report.ArrivedAt = store.EnvironmentTime(report.ArrivedAt)
		if err := store.ApplyFeedbackAcceptedServices(&report, services); err != nil {
			return false, err
		}
		accepted = append(accepted, report)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return store.FeedbackGitHubRepeat(signal, accepted), nil
}

// RecordFeedbackGitHubDeploy implements store.FeedbackStore. It holds the
// workspace environment claim, which an archive takes too, so an archived
// environment records nothing, and it stamps the environment row as its own
// witness as a signal does (component-persistence invariant 8).
func (s *Store) RecordFeedbackGitHubDeploy(ctx context.Context, deploy store.FeedbackGitHubDeploy) (bool, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordFeedbackGitHubDeploy")
	signal, err := store.FeedbackGitHubDeploySignal(deploy)
	if err != nil {
		return false, err
	}
	ws, err := workspace(ctx)
	if err != nil {
		return false, err
	}
	recorded := false
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		recorded = false
		claim, err := claimEnvironments(ctx, tx, ws)
		if err != nil {
			return err
		}
		var archived bool
		err = tx.QueryRowContext(ctx, `SELECT archived FROM environments WHERE workspace_id=? AND id=?`, ws, signal.EnvironmentID).Scan(&archived)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && archived) {
			return nil
		}
		if err != nil {
			return err
		}
		repositories, err := environmentRepositoriesTx(ctx, tx, ws)
		if err != nil {
			return err
		}
		if !repositories[deploy.Repository] {
			return nil
		}
		history, err := readFeedbackHistory(ctx, tx, ws, signal)
		if err != nil {
			return err
		}
		next, err := nextFeedbackReport(ctx, tx, ws, signal.EnvironmentID)
		if err != nil {
			return err
		}
		origin := store.FeedbackGitHubOrigin(next)
		changes, err := store.PlanFeedbackChanges(signal, origin, history)
		if err != nil {
			return err
		}
		if !changes.Changed() {
			repeat, err := feedbackGitHubRepeat(ctx, tx, ws, signal)
			if err != nil || repeat {
				return err
			}
		}
		if err := writeFeedbackChanges(ctx, tx, ws, signal, origin, changes); err != nil {
			return err
		}
		if err := recordFeedbackAccepted(ctx, tx, ws, signal, origin); err != nil {
			return err
		}
		if err := claim.touchEnvironment(ctx, tx, signal.EnvironmentID); err != nil {
			return err
		}
		recorded = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return recorded, nil
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
	rows, err := documentRows(ctx, s.db, feedbackDeploySQL, ws, environmentID)
	if err != nil {
		return nil, err
	}
	deploys, err := collectFeedbackDeploys(rows)
	if err != nil {
		return nil, err
	}
	store.SortFeedbackDeploys(deploys)
	return deploys, nil
}
