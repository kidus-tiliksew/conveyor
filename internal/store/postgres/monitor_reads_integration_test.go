package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The adapter retains the pre-optimization read path for serialized HTTP parity.
// It deliberately performs the old identity lookup followed by point reads.
type legacyMonitorReads struct {
	*Store
	now time.Time
}

func (s *legacyMonitorReads) ListUnresolvedDrift(ctx context.Context) ([]monitor.Drift, error) {
	status, err := s.MonitorStatus(ctx, true, s.now)
	return status.Drift, err
}

func TestMonitorReadsBoundedAndHTTPParityIntegration(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	recorder := &queryRecorder{}
	cfg.ConnConfig.Tracer = recorder
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	st := newStore(pool)
	ws := "monitor-reads-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), ws)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Monitor reads"}, core.RequirementVersion{Content: "# Monitor reads", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve drift signals."}}, Origin: core.RequirementOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	design, dv, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-" + core.NewTaskID(), Title: "Monitor reads", Category: "Component design"}, core.SystemDesignVersion{Content: "# Monitor reads\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, dv.Version); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 500; i++ {
		observation := monitor.Observation{WorkspaceID: ws, Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: fmt.Sprintf("commit-%03d", i), SourceURL: "https://example.test/commit", CommitSHA: fmt.Sprintf("sha-%03d", i), ObservedAt: now.Add(time.Duration(i) * time.Second), ChangedPaths: []string{"internal/example.go"}, Context: map[string]string{"index": fmt.Sprint(i), "detail": "fixture context"}}
		if i%2 == 0 {
			observation.Hints = &monitor.HintContext{}
		}
		if _, _, err = st.Observe(ctx, observation); err != nil {
			t.Fatal(err)
		}
		// Distinct times make parity independent of the old unspecified tie order.
		if _, err = pool.Exec(ctx, `UPDATE monitor_observations SET created_at=$3 WHERE workspace_id=$1 AND identity=$2`, ws, observation.Identity(), now.Add(time.Duration(i)*time.Microsecond)); err != nil {
			t.Fatal(err)
		}
		if i < 21 {
			task := phase61Task(ws, "monitor-read-"+core.NewTaskID(), core.TaskRunning, "")
			if err = st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if _, err = st.LinkTask(ctx, observation.Identity(), task.ID, "created"); err != nil {
				t.Fatal(err)
			}
			drift := monitor.Drift{ID: fmt.Sprintf("drift-%03d", i), WorkspaceID: ws, Repository: "conveyor", Kind: monitor.DirectPush, SourceURL: observation.SourceURL, CommitSHA: observation.CommitSHA, RequirementID: requirement.ID, SystemDesignID: design.ID, SystemDesignVersion: dv.Version, MatchingPaths: observation.ChangedPaths, TaskID: task.ID, DetectedAt: now.Add(-time.Duration(i+1) * time.Minute)}
			if _, _, err = st.RecordDrift(ctx, drift); err != nil {
				t.Fatal(err)
			}
			if i == 20 {
				if _, err = st.ResolveDrift(ctx, drift.ID, "change_reverted", ""); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err = st.RecordMonitorSuccess(ctx, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = st.RecordMonitorFailure(ctx, "forge_status", "fixture error", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = st.AuditMonitor(ctx, "monitor.retry", map[string]any{"attempt": 1}); err != nil {
		t.Fatal(err)
	}
	legacy := &legacyMonitorReads{Store: st, now: now}
	server := httpapi.NewServer(st)
	server.Credentials = nil
	server.Workspace, server.BearerToken = ws, "token"
	read := func(path string, backend monitor.Store) []byte {
		t.Helper()
		server.Monitor = &monitor.Service{Store: backend, Enabled: true, Now: func() time.Time { return now }}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		return response.Body.Bytes()
	}
	for _, path := range []string{"/v1/requirements/" + requirement.ID, "/v1/system-designs/" + design.ID, "/v1/monitor"} {
		before := read(path, legacy)
		recorder.reset()
		after := read(path, st)
		if !bytes.Equal(before, after) {
			t.Fatalf("%s serialized output changed\nbefore=%s\nafter=%s", path, before, after)
		}
		counts := map[string]int{}
		for _, query := range recorder.snapshot() {
			for _, table := range []string{"monitor_status", "monitor_activity", "monitor_observations", "repository_drift"} {
				if strings.Contains(query, "FROM "+table) {
					counts[table]++
				}
			}
		}
		if path == "/v1/monitor" {
			if counts["monitor_observations"] != 1 || counts["repository_drift"] != 1 || counts["monitor_status"] != 1 || counts["monitor_activity"] != 1 {
				t.Fatalf("status query counts=%v", counts)
			}
			var status monitor.Status
			if err = json.Unmarshal(after, &status); err != nil {
				t.Fatal(err)
			}
			if len(status.Observations) != 500 || len(status.Drift) != 20 || status.DriftCount != 20 || status.OldestDriftAge != 20*time.Minute {
				t.Fatalf("status fixture counts or age changed: observations=%d drift=%d count=%d age=%s", len(status.Observations), len(status.Drift), status.DriftCount, status.OldestDriftAge)
			}
		} else if counts["repository_drift"] != 1 || counts["monitor_observations"] != 0 || counts["monitor_activity"] != 0 || counts["monitor_status"] != 0 {
			t.Fatalf("%s query counts=%v", path, counts)
		}
		t.Logf("500 observations, 20 unresolved drift: %s monitor queries=%v; serialized response byte-identical", path, counts)
	}
}

func (s *legacyMonitorReads) MonitorStatus(ctx context.Context, enabled bool, now time.Time) (monitor.Status, error) {
	status := monitor.Status{WorkspaceID: workspace(ctx), Enabled: enabled}
	var lastSuccess, backoff *time.Time
	err := s.pool.QueryRow(ctx, `
SELECT last_successful_at,current_error,forge_error_category,backoff_until
FROM monitor_status WHERE workspace_id=$1`, workspace(ctx)).
		Scan(&lastSuccess, &status.CurrentError, &status.ForgeErrorCategory, &backoff)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return monitor.Status{}, err
	}
	if lastSuccess != nil {
		status.LastSuccessfulAt = *lastSuccess
	}
	if backoff != nil {
		status.BackoffUntil = *backoff
	}
	activityRows, err := s.pool.Query(ctx, `
SELECT id,kind,payload_json,at FROM monitor_activity
WHERE workspace_id=$1 ORDER BY at,id`, workspace(ctx))
	if err != nil {
		return monitor.Status{}, err
	}
	for activityRows.Next() {
		var activity monitor.Activity
		var payload []byte
		activity.WorkspaceID = workspace(ctx)
		if err = activityRows.Scan(&activity.ID, &activity.Kind, &payload, &activity.At); err != nil {
			activityRows.Close()
			return monitor.Status{}, err
		}
		_ = json.Unmarshal(payload, &activity.Payload)
		status.Activity = append(status.Activity, activity)
	}
	if err = activityRows.Err(); err != nil {
		activityRows.Close()
		return monitor.Status{}, err
	}
	activityRows.Close()
	rows, err := s.pool.Query(ctx, `
SELECT identity FROM monitor_observations WHERE workspace_id=$1 ORDER BY created_at`, workspace(ctx))
	if err != nil {
		return monitor.Status{}, err
	}
	for rows.Next() {
		var identity string
		if err = rows.Scan(&identity); err != nil {
			rows.Close()
			return monitor.Status{}, err
		}
		record, getErr := s.getObservation(ctx, identity)
		if getErr != nil {
			rows.Close()
			return monitor.Status{}, getErr
		}
		status.Observations = append(status.Observations, record)
	}
	rows.Close()
	driftRows, err := s.pool.Query(ctx, `
SELECT id FROM repository_drift WHERE workspace_id=$1 AND resolved_at IS NULL ORDER BY detected_at`, workspace(ctx))
	if err != nil {
		return monitor.Status{}, err
	}
	defer driftRows.Close()
	for driftRows.Next() {
		var id string
		if err = driftRows.Scan(&id); err != nil {
			return monitor.Status{}, err
		}
		drift, getErr := s.getDrift(ctx, id)
		if getErr != nil {
			return monitor.Status{}, getErr
		}
		status.Drift = append(status.Drift, drift)
		status.DriftCount++
		if age := now.Sub(drift.DetectedAt); age > status.OldestDriftAge {
			status.OldestDriftAge = age
		}
	}
	return status, driftRows.Err()
}
