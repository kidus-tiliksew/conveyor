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

// writeFeedbackDeploys inserts the new deploys with their running versions.
func writeFeedbackDeploys(ctx context.Context, tx pgx.Tx, workspaceID string, signal store.FeedbackSignal, origin store.FeedbackOrigin, changes store.FeedbackChanges) error {
	if len(changes.Deploys) == 0 {
		return nil
	}
	var ids, services, serviceDigests, versionDigests, repositories, commits, versions []string
	for _, deploy := range changes.Deploys {
		ids = append(ids, deploy.ID)
		services = append(services, deploy.Service)
		serviceDigests = append(serviceDigests, store.FeedbackServiceDigest(deploy.Service))
		versionDigests = append(versionDigests, deploy.Digest())
		repositories = append(repositories, deploy.Repository)
		commits = append(commits, deploy.Commit)
		versions = append(versions, deploy.Version)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO feedback_deploys (workspace_id, environment_id, deployed_at, source, source_label, actor_id, id, service, reported_repository, reported_commit, reported_version)
SELECT $1::text, $2::text, $3::timestamptz, $4::text, $5::text, $6::text, d.* FROM unnest($7::text[], $8::text[], $9::text[], $10::text[], $11::text[]) AS d`,
		workspaceID, signal.EnvironmentID, signal.ArrivedAt, origin.Source, origin.Label, origin.Actor, ids, services, repositories, commits, versions); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO feedback_running_versions (workspace_id, environment_id, deployed_at, reported_at, deploy_id, service_digest, version_digest, service, reported_repository, reported_commit, reported_version)
SELECT $1::text, $2::text, $3::timestamptz, $3::timestamptz, d.* FROM unnest($4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[]) AS d`,
		workspaceID, signal.EnvironmentID, signal.ArrivedAt, ids, serviceDigests, versionDigests, services, repositories, commits, versions)
	return err
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
		var configYAML string
		if err := tx.QueryRow(ctx, `SELECT config_yaml FROM workspaces WHERE id=$1`, workspaceID).Scan(&configYAML); err != nil {
			return notFound(err, "workspace %s", workspaceID)
		}
		repositories, err := store.EnvironmentRepositories(configYAML)
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
		running, err := queryFeedbackRunning(ctx, tx, feedbackRunningSQL+` AND service_digest=ANY($3::text[])`, workspaceID, signal.EnvironmentID, digests)
		if err != nil {
			return err
		}
		origin := store.FeedbackSignalOrigin(label)
		changes, err := store.PlanFeedbackChanges(signal, origin, running)
		if err != nil {
			return err
		}
		if len(changes.Stopped) > 0 {
			stopped := make([]string, len(changes.Stopped))
			for i, version := range changes.Stopped {
				stopped[i] = version.DeployID
			}
			if _, err := tx.Exec(ctx, `UPDATE feedback_deploys SET stopped_at=$3, stop_source=$4, stop_source_label=$5, stop_actor_id=$6 WHERE workspace_id=$1 AND id=ANY($2::text[]) AND stopped_at IS NULL`,
				workspaceID, stopped, signal.ArrivedAt, origin.Source, origin.Label, origin.Actor); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM feedback_running_versions WHERE workspace_id=$1 AND environment_id=$2 AND deploy_id=ANY($3::text[])`, workspaceID, signal.EnvironmentID, stopped); err != nil {
				return err
			}
		}
		if err := writeFeedbackDeploys(ctx, tx, workspaceID, signal, origin, changes); err != nil {
			return err
		}
		reported := make([]string, len(changes.Reported))
		for i, service := range changes.Reported {
			reported[i] = store.FeedbackServiceDigest(service)
		}
		if _, err := tx.Exec(ctx, `UPDATE feedback_running_versions SET reported_at=$4 WHERE workspace_id=$1 AND environment_id=$2 AND service_digest=ANY($3::text[]) AND reported_at<$4`, workspaceID, signal.EnvironmentID, reported, signal.ArrivedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE environment_keys SET last_used_at=now() WHERE workspace_id=$1 AND id=$2 AND (last_used_at IS NULL OR last_used_at < now() - $3::bigint * interval '1 microsecond')`,
			workspaceID, signal.KeyID, store.EnvironmentKeyUseGranularity.Microseconds()); err != nil {
			return err
		}
		// An arrival older than the stored one matches no row to update.
		_, err = tx.Exec(ctx, `INSERT INTO feedback_signals AS f (workspace_id, environment_id, last_signal_at, next_signal_seconds, source, source_label, actor_id) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (workspace_id, environment_id) DO UPDATE SET last_signal_at=EXCLUDED.last_signal_at, next_signal_seconds=EXCLUDED.next_signal_seconds, source=EXCLUDED.source, source_label=EXCLUDED.source_label, actor_id=EXCLUDED.actor_id
WHERE f.last_signal_at <= EXCLUDED.last_signal_at`,
			workspaceID, signal.EnvironmentID, signal.ArrivedAt, signal.NextSignalSeconds, origin.Source, origin.Label, origin.Actor)
		return err
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
	rows, err := s.boundary.Query(ctx, `SELECT id, environment_id, service, reported_repository, reported_commit, reported_version, resolved, deployed_at, source, source_label, actor_id, stopped_at, stop_source, stop_source_label, stop_actor_id
FROM feedback_deploys WHERE workspace_id=$1 AND environment_id=$2`, workspace(ctx), environmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deploys := []store.FeedbackDeploy{}
	for rows.Next() {
		var deploy store.FeedbackDeploy
		var stoppedAt pgtype.Timestamptz
		var stopSource, stopLabel, stopActor *string
		if err := rows.Scan(&deploy.ID, &deploy.EnvironmentID, &deploy.Service, &deploy.Repository, &deploy.Commit, &deploy.Version, &deploy.Resolved, &deploy.DeployedAt,
			&deploy.Origin.Source, &deploy.Origin.Label, &deploy.Origin.Actor, &stoppedAt, &stopSource, &stopLabel, &stopActor); err != nil {
			return nil, err
		}
		deploy.DeployedAt = store.EnvironmentTime(deploy.DeployedAt)
		if stoppedAt.Valid && stopSource != nil && stopLabel != nil && stopActor != nil {
			at := store.EnvironmentTime(stoppedAt.Time.In(time.UTC))
			deploy.StoppedAt, deploy.StopOrigin = &at, &store.FeedbackOrigin{Source: *stopSource, Label: *stopLabel, Actor: *stopActor}
		}
		deploys = append(deploys, deploy)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	store.SortFeedbackDeploys(deploys)
	return deploys, nil
}
