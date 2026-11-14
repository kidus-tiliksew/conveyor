package postgres

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// stagePreferenceRiskMigration is the migration that adds the risk column.
const stagePreferenceRiskMigration = 168

// TestStagePreferencesRiskMigrationIntegration upgrades a ledger from the
// version before the risk column through migration 168
// (req-stage-preferences AC-1.1, AC-2.4; component-stage-preferences v2;
// DEC-135(13)). A set saved before the migration keeps its stages, version,
// and update time, reads risk as unset through the store, and accepts a risk
// write afterward. The migration replays without touching data.
func TestStagePreferencesRiskMigrationIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "stage_preferences_risk_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	if err = migrateControlPlaneToVersion(ctx, pool, stagePreferenceRiskMigration-1); err != nil {
		t.Fatalf("migrate to %d: %v", stagePreferenceRiskMigration-1, err)
	}
	var columns int
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='user_stage_preferences' AND column_name='risk_json'`, schema).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatalf("version %d already has the risk column", stagePreferenceRiskMigration-1)
	}

	const userID = "usr_stage_preferences_legacy"
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,email,display_name,status) VALUES($1,'stage-preferences-legacy@example.test','Legacy','active')`, userID); err != nil {
		t.Fatal(err)
	}
	updated := time.Date(2026, 11, 13, 9, 30, 15, 123456000, time.UTC)
	if _, err = pool.Exec(ctx, `INSERT INTO user_stage_preferences (user_id, version, plan_json, implement_json, verify_json, review_json, updated_at)
		VALUES ($1, 3, $2, NULL, $3, $4, $5)`, userID,
		`{"harness":"codex","model":"gpt-6.1-sol","effort":"medium"}`,
		`{"harness":"opencode","model":"local","effort":""}`,
		`{"primary":{"harness":"claude-code","model":"claude-opus-5-5","effort":""},"seats":[{"harness":"cursor","model":"auto","effort":""}]}`,
		updated); err != nil {
		t.Fatal(err)
	}

	if err = migrateControlPlane(ctx, pool); err != nil {
		t.Fatal(err)
	}
	want := store.StagePreferences{
		Version:   3,
		UpdatedAt: &updated,
		Stages: store.StagePreferenceStages{
			Plan:   &store.StagePreferenceEntry{Harness: "codex", Model: "gpt-6.1-sol", Effort: "medium"},
			Verify: &store.StagePreferenceEntry{Harness: "opencode", Model: "local"},
			Review: &store.ReviewStagePreference{
				Primary: &store.StagePreferenceEntry{Harness: "claude-code", Model: "claude-opus-5-5"},
				Seats:   []store.StagePreferenceEntry{{Harness: "cursor", Model: "auto"}},
			},
		},
	}
	st := newStore(pool)
	read := func(step string) store.StagePreferences {
		t.Helper()
		got, err := st.GetStagePreferences(ctx, userID)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		return got
	}
	if got := read("after migration"); got.Version != want.Version || got.UpdatedAt == nil || !got.UpdatedAt.Equal(updated) || !reflect.DeepEqual(got.Stages, want.Stages) {
		t.Fatalf("legacy set after migration %d = %+v, want %+v with risk unset", stagePreferenceRiskMigration, got, want)
	}

	risk := &store.StagePreferenceEntry{Harness: "claude-code", Model: "claude-haiku-5-5", Effort: "low"}
	saved, err := st.UpdateStagePreferences(ctx, userID, func(current store.StagePreferences) (store.StagePreferenceStages, error) {
		if current.Version != 3 || current.Stages.Risk != nil {
			t.Errorf("risk write read %+v, want version 3 with risk unset", current)
		}
		next := store.CloneStagePreferenceStages(current.Stages)
		next.Risk = risk
		return next, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantStages := want.Stages
	wantStages.Risk = risk
	if saved.Version != 4 || !reflect.DeepEqual(saved.Stages, wantStages) {
		t.Fatalf("risk write after migration = %+v, want version 4 holding %+v", saved, wantStages)
	}

	// Reopening replays the ledger without touching data.
	if err = migrateControlPlane(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := read("after replay"); got.Version != 4 || !reflect.DeepEqual(got.Stages, wantStages) {
		t.Fatalf("set after replay = %+v, want version 4 holding %+v", got, wantStages)
	}
}
