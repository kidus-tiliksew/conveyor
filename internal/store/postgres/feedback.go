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

var _ store.FeedbackStore = (*Store)(nil)

// lockFeedbackKey takes the environment row lock that serializes the signals
// of one environment, the first one included, and the key row lock, and
// returns the key's label. An archive waits for the first lock and a
// revocation for the second, so a signal commits before either or is refused
// after it. FOR NO KEY UPDATE does not block an insert of a row that
// references the environment.
func lockFeedbackKey(ctx context.Context, tx pgx.Tx, workspaceID string, signal store.FeedbackSignal) (string, error) {
	var archived bool
	err := tx.QueryRow(ctx, `SELECT archived FROM environments WHERE workspace_id=$1 AND id=$2 FOR NO KEY UPDATE`, workspaceID, signal.EnvironmentID).Scan(&archived)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && archived) {
		return "", store.ErrInvalidEnvironmentKey
	}
	if err != nil {
		return "", err
	}
	var label string
	err = tx.QueryRow(ctx, `SELECT label FROM environment_keys WHERE workspace_id=$1 AND id=$2 AND environment_id=$3 AND revoked_at IS NULL FOR NO KEY UPDATE`, workspaceID, signal.KeyID, signal.EnvironmentID).Scan(&label)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", store.ErrInvalidEnvironmentKey
	}
	return label, err
}

const feedbackRunningSQL = `SELECT environment_id, service, reported_repository, reported_commit, reported_version, resolved, deploy_id, deployed_at, reported_at FROM feedback_running_versions WHERE workspace_id=$1 AND environment_id=$2`

func queryFeedbackRunning(ctx context.Context, q environmentQuerier, sql string, args ...any) ([]store.FeedbackRunningVersion, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
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

const feedbackDeploySQL = `SELECT id, environment_id, service, reported_repository, reported_commit, reported_version, resolved, deployed_at, source, source_label, actor_id, report, stopped_at, stop_source, stop_source_label, stop_actor_id, stop_report FROM feedback_deploys WHERE workspace_id=$1 AND environment_id=$2`

func queryFeedbackDeploys(ctx context.Context, q environmentQuerier, sql string, args ...any) ([]store.FeedbackDeploy, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deploys := []store.FeedbackDeploy{}
	for rows.Next() {
		var deploy store.FeedbackDeploy
		var stoppedAt pgtype.Timestamptz
		var stopSource, stopLabel, stopActor *string
		var stopReport *int64
		if err := rows.Scan(&deploy.ID, &deploy.EnvironmentID, &deploy.Service, &deploy.Repository, &deploy.Commit, &deploy.Version, &deploy.Resolved, &deploy.DeployedAt,
			&deploy.Origin.Source, &deploy.Origin.Label, &deploy.Origin.Actor, &deploy.Origin.Report, &stoppedAt, &stopSource, &stopLabel, &stopActor, &stopReport); err != nil {
			return nil, err
		}
		deploy.DeployedAt = store.EnvironmentTime(deploy.DeployedAt)
		if stoppedAt.Valid && stopSource != nil && stopLabel != nil && stopActor != nil && stopReport != nil {
			at := store.EnvironmentTime(stoppedAt.Time)
			deploy.StoppedAt, deploy.StopOrigin = &at, &store.FeedbackOrigin{Source: *stopSource, Label: *stopLabel, Actor: *stopActor, Report: *stopReport}
		}
		deploys = append(deploys, deploy)
	}
	return deploys, rows.Err()
}

// readFeedbackHistory reads what PlanFeedbackChanges needs. Only a signal that
// a later arrival overtook reads the accepted signals and the deploys.
func readFeedbackHistory(ctx context.Context, tx pgx.Tx, workspaceID string, signal store.FeedbackSignal) (store.FeedbackHistory, error) {
	var history store.FeedbackHistory
	digests := make([]string, len(signal.Services))
	for i, service := range signal.Services {
		digests[i] = store.FeedbackServiceDigest(service.Name)
	}
	var err error
	if history.Running, err = queryFeedbackRunning(ctx, tx, feedbackRunningSQL+` AND service_digest=ANY($3::text[])`, workspaceID, signal.EnvironmentID, digests); err != nil {
		return history, err
	}
	late := store.FeedbackLateServices(signal, history.Running)
	if len(late) == 0 {
		return history, nil
	}
	if history.Deploys, err = queryFeedbackDeploys(ctx, tx, feedbackDeploySQL+` AND (stopped_at IS NULL OR stopped_at > $3)`, workspaceID, signal.EnvironmentID, signal.ArrivedAt); err != nil {
		return history, err
	}
	rows, err := tx.Query(ctx, `SELECT arrived_at, report, source_label, service_digests::text FROM feedback_accepted_signals WHERE workspace_id=$1 AND environment_id=$2 AND arrived_at > $3 ORDER BY arrived_at, report`, workspaceID, signal.EnvironmentID, signal.ArrivedAt)
	if err != nil {
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

func feedbackStopColumns(deploy store.FeedbackDeploy) (source, label, actor *string, report *int64) {
	if deploy.StopOrigin == nil {
		return nil, nil, nil, nil
	}
	return &deploy.StopOrigin.Source, &deploy.StopOrigin.Label, &deploy.StopOrigin.Actor, &deploy.StopOrigin.Report
}

// writeFeedbackChanges writes the planned changes: the stops, the new deploys
// and running versions, the earlier records an overtaken signal moves, and the
// newest report of each service.
func writeFeedbackChanges(ctx context.Context, tx pgx.Tx, workspaceID string, signal store.FeedbackSignal, origin store.FeedbackOrigin, changes store.FeedbackChanges) error {
	if len(changes.Stopped) > 0 {
		stopped := make([]string, len(changes.Stopped))
		for i, version := range changes.Stopped {
			stopped[i] = version.DeployID
		}
		if _, err := tx.Exec(ctx, `UPDATE feedback_deploys SET stopped_at=$3, stop_source=$4, stop_source_label=$5, stop_actor_id=$6, stop_report=$7 WHERE workspace_id=$1 AND id=ANY($2::text[]) AND stopped_at IS NULL`,
			workspaceID, stopped, signal.ArrivedAt, origin.Source, origin.Label, origin.Actor, origin.Report); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM feedback_running_versions WHERE workspace_id=$1 AND environment_id=$2 AND deploy_id=ANY($3::text[])`, workspaceID, signal.EnvironmentID, stopped); err != nil {
			return err
		}
	}
	if len(changes.Deploys) > 0 {
		var ids, services, repositories, commits, versions, sources, labels, actors []string
		var resolved []bool
		var deployed []time.Time
		var stopped []*time.Time
		var stopSources, stopLabels, stopActors []*string
		var reports []int64
		var stopReports []*int64
		for _, deploy := range changes.Deploys {
			ids, services = append(ids, deploy.ID), append(services, deploy.Service)
			repositories, commits, versions = append(repositories, deploy.Repository), append(commits, deploy.Commit), append(versions, deploy.Version)
			sources, labels, actors = append(sources, deploy.Origin.Source), append(labels, deploy.Origin.Label), append(actors, deploy.Origin.Actor)
			resolved, deployed, stopped = append(resolved, deploy.Resolved), append(deployed, deploy.DeployedAt), append(stopped, deploy.StoppedAt)
			source, label, actor, report := feedbackStopColumns(deploy)
			stopSources, stopLabels, stopActors = append(stopSources, source), append(stopLabels, label), append(stopActors, actor)
			reports, stopReports = append(reports, deploy.Origin.Report), append(stopReports, report)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO feedback_deploys (workspace_id, environment_id, id, service, reported_repository, reported_commit, reported_version, resolved, deployed_at, source, source_label, actor_id, report, stopped_at, stop_source, stop_source_label, stop_actor_id, stop_report)
SELECT $1::text, $2::text, d.* FROM unnest($3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::boolean[], $9::timestamptz[], $10::text[], $11::text[], $12::text[], $13::bigint[], $14::timestamptz[], $15::text[], $16::text[], $17::text[], $18::bigint[]) AS d`,
			workspaceID, signal.EnvironmentID, ids, services, repositories, commits, versions, resolved, deployed, sources, labels, actors, reports, stopped, stopSources, stopLabels, stopActors, stopReports); err != nil {
			return err
		}
	}
	if len(changes.Started) > 0 {
		var ids, services, serviceDigests, versionDigests, repositories, commits, versions []string
		for _, version := range changes.Started {
			ids, services = append(ids, version.DeployID), append(services, version.Service)
			serviceDigests, versionDigests = append(serviceDigests, store.FeedbackServiceDigest(version.Service)), append(versionDigests, version.Digest())
			repositories, commits, versions = append(repositories, version.Repository), append(commits, version.Commit), append(versions, version.Version)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO feedback_running_versions (workspace_id, environment_id, deployed_at, reported_at, deploy_id, service_digest, version_digest, service, reported_repository, reported_commit, reported_version)
SELECT $1::text, $2::text, $3::timestamptz, $3::timestamptz, d.* FROM unnest($4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[]) AS d`,
			workspaceID, signal.EnvironmentID, signal.ArrivedAt, ids, serviceDigests, versionDigests, services, repositories, commits, versions); err != nil {
			return err
		}
	}
	for _, deploy := range changes.Updated {
		source, label, actor, report := feedbackStopColumns(deploy)
		if _, err := tx.Exec(ctx, `UPDATE feedback_deploys SET deployed_at=$3, source=$4, source_label=$5, actor_id=$6, stopped_at=$7, stop_source=$8, stop_source_label=$9, stop_actor_id=$10, report=$11, stop_report=$12 WHERE workspace_id=$1 AND id=$2`,
			workspaceID, deploy.ID, deploy.DeployedAt, deploy.Origin.Source, deploy.Origin.Label, deploy.Origin.Actor, deploy.StoppedAt, source, label, actor, deploy.Origin.Report, report); err != nil {
			return err
		}
	}
	for _, version := range changes.Moved {
		if _, err := tx.Exec(ctx, `UPDATE feedback_running_versions SET deploy_id=$5, deployed_at=$6 WHERE workspace_id=$1 AND environment_id=$2 AND service_digest=$3 AND version_digest=$4`,
			workspaceID, signal.EnvironmentID, store.FeedbackServiceDigest(version.Service), version.Digest(), version.DeployID, version.DeployedAt); err != nil {
			return err
		}
	}
	reported := make([]string, len(changes.Reported))
	for i, service := range changes.Reported {
		reported[i] = store.FeedbackServiceDigest(service)
	}
	_, err := tx.Exec(ctx, `UPDATE feedback_running_versions SET reported_at=$4 WHERE workspace_id=$1 AND environment_id=$2 AND service_digest=ANY($3::text[])`, workspaceID, signal.EnvironmentID, reported, signal.ArrivedAt)
	return err
}

// recordFeedbackAccepted keeps the signal as a boundary of the deploy history.
func recordFeedbackAccepted(ctx context.Context, tx pgx.Tx, workspaceID string, signal store.FeedbackSignal, origin store.FeedbackOrigin) error {
	accepted := store.NewFeedbackAcceptedSignal(signal, origin)
	_, err := tx.Exec(ctx, `INSERT INTO feedback_accepted_signals (workspace_id, environment_id, arrived_at, report, source_label, service_digests) VALUES ($1, $2, $3, $4, $5, $6::text::jsonb)`,
		workspaceID, signal.EnvironmentID, accepted.ArrivedAt, accepted.Report, accepted.Label, store.FeedbackAcceptedServices(accepted))
	return err
}

func feedbackRepositories(ctx context.Context, q feedbackRowQuerier, workspaceID string) (map[string]bool, error) {
	var configYAML string
	if err := q.QueryRow(ctx, `SELECT config_yaml FROM workspaces WHERE id=$1`, workspaceID).Scan(&configYAML); err != nil {
		return nil, notFound(err, "workspace %s", workspaceID)
	}
	return store.EnvironmentRepositories(configYAML)
}

// feedbackRowQuerier is a transaction or the store's error-translating pool.
type feedbackRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CheckFeedbackRepositories implements store.FeedbackStore.
func (s *Store) CheckFeedbackRepositories(ctx context.Context, signal store.FeedbackSignal) error {
	ctx = dbmetrics.WithReadLabel(ctx, "CheckFeedbackRepositories")
	repositories, err := feedbackRepositories(ctx, s.boundary, workspace(ctx))
	if err != nil {
		return translateBackendConflict(err)
	}
	return store.CheckFeedbackRepositories(signal, repositories)
}

// RecordFeedbackSignal implements store.FeedbackStore.
func (s *Store) RecordFeedbackSignal(ctx context.Context, signal store.FeedbackSignal) error {
	ctx = dbmetrics.WithReadLabel(ctx, "RecordFeedbackSignal")
	signal, err := store.NormalizeFeedbackSignal(signal)
	if err != nil {
		return err
	}
	workspaceID := workspace(ctx)
	return s.inTx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		label, err := lockFeedbackKey(ctx, tx, workspaceID, signal)
		if err != nil {
			return err
		}
		repositories, err := feedbackRepositories(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if err := store.CheckFeedbackRepositories(signal, repositories); err != nil {
			return err
		}
		history, err := readFeedbackHistory(ctx, tx, workspaceID, signal)
		if err != nil {
			return err
		}
		// The environment row lock makes this the next number.
		var last int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(last_report), 0) FROM feedback_signals WHERE workspace_id=$1 AND environment_id=$2`, workspaceID, signal.EnvironmentID).Scan(&last); err != nil {
			return err
		}
		origin := store.FeedbackSignalOrigin(label, last+1)
		changes, err := store.PlanFeedbackChanges(signal, origin, history)
		if err != nil {
			return err
		}
		if err := writeFeedbackChanges(ctx, tx, workspaceID, signal, origin, changes); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE environment_keys SET last_used_at=now() WHERE workspace_id=$1 AND id=$2 AND (last_used_at IS NULL OR last_used_at < now() - $3::bigint * interval '1 microsecond')`,
			workspaceID, signal.KeyID, store.EnvironmentKeyUseGranularity.Microseconds()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO feedback_signals (workspace_id, environment_id, last_signal_at, next_signal_seconds, source, source_label, actor_id, last_report) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (workspace_id, environment_id) DO UPDATE SET last_report=EXCLUDED.last_report`,
			workspaceID, signal.EnvironmentID, signal.ArrivedAt, signal.NextSignalSeconds, origin.Source, origin.Label, origin.Actor, origin.Report); err != nil {
			return err
		}
		// An arrival older than the stored one matches no row to update.
		if _, err := tx.Exec(ctx, `UPDATE feedback_signals SET last_signal_at=$3, next_signal_seconds=$4, source=$5, source_label=$6, actor_id=$7 WHERE workspace_id=$1 AND environment_id=$2 AND last_signal_at <= $3`,
			workspaceID, signal.EnvironmentID, signal.ArrivedAt, signal.NextSignalSeconds, origin.Source, origin.Label, origin.Actor); err != nil {
			return err
		}
		return recordFeedbackAccepted(ctx, tx, workspaceID, signal, origin)
	})
}

// ListFeedbackSignals implements store.FeedbackStore.
func (s *Store) ListFeedbackSignals(ctx context.Context, environmentIDs []string) (map[string]store.FeedbackSignalState, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackSignals")
	out := map[string]store.FeedbackSignalState{}
	if len(environmentIDs) == 0 {
		return out, nil
	}
	rows, err := s.boundary.Query(ctx, `SELECT environment_id, last_signal_at, next_signal_seconds, source, source_label, actor_id FROM feedback_signals WHERE workspace_id=$1 AND environment_id=ANY($2::text[])`, workspace(ctx), environmentIDs)
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
	running, err := queryFeedbackRunning(ctx, s.boundary, feedbackRunningSQL, workspace(ctx), environmentID)
	if err != nil {
		return nil, err
	}
	store.SortFeedbackRunningVersions(running)
	return running, nil
}

// ListFeedbackDeploys implements store.FeedbackStore.
func (s *Store) ListFeedbackDeploys(ctx context.Context, environmentID string) ([]store.FeedbackDeploy, error) {
	ctx = dbmetrics.WithReadLabel(ctx, "ListFeedbackDeploys")
	deploys, err := queryFeedbackDeploys(ctx, s.boundary, feedbackDeploySQL, workspace(ctx), environmentID)
	if err != nil {
		return nil, err
	}
	store.SortFeedbackDeploys(deploys)
	return deploys, nil
}
