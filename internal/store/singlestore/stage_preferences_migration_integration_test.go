package singlestore

import (
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestStagePreferencesRiskMigrationIntegration replays migration 0041
// (req-stage-preferences AC-1.1, AC-2.4; component-stage-preferences v2;
// DEC-135(13)) over a set saved at the previous ledger version, when the
// table had no risk column. The column arrives NULL with no backfill, so the
// saved set keeps its stages, version, and update time and reads risk as
// unset through the store. A restart after the column add, before the ledger
// row, adds nothing twice, and a risk write then keeps every other stage.
func TestStagePreferencesRiskMigrationIntegration(t *testing.T) {
	st := integrationStore(t)
	base := store.WithActor(t.Context(), store.SystemActor())
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := st.db.ExecContext(base, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	riskColumn := func() bool {
		t.Helper()
		exists, err := st.columnExists(base, "user_stage_preferences", "risk_json")
		if err != nil {
			t.Fatal(err)
		}
		return exists
	}
	ledgerRows := func() int {
		t.Helper()
		var rows int
		if err := st.db.QueryRowContext(base, `SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=?`, stagePreferenceRiskMigrationVersion).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if !riskColumn() || ledgerRows() != 1 {
		t.Fatal("a fresh schema lacks the risk column or its ledger row")
	}

	// The previous ledger version: the risk column does not exist.
	exec(`DELETE FROM conveyor_singlestore_migrations WHERE version=?`, stagePreferenceRiskMigrationVersion)
	exec(`ALTER TABLE user_stage_preferences DROP COLUMN risk_json`)
	if riskColumn() {
		t.Fatal("the previous-version fixture still has the risk column")
	}
	const userID = "usr_stage_preferences_legacy"
	updated := time.Date(2026, 11, 13, 9, 30, 15, 123456000, time.UTC)
	exec(`INSERT INTO user_stage_preferences (user_id, version, plan_json, implement_json, verify_json, review_json, updated_at) VALUES (?, 3, ?, NULL, ?, ?, ?)`, userID,
		`{"harness":"codex","model":"gpt-6.1-sol","effort":"medium"}`,
		`{"harness":"opencode","model":"local","effort":""}`,
		`{"primary":{"harness":"claude-code","model":"claude-opus-5-5","effort":""},"seats":[{"harness":"cursor","model":"auto","effort":""}]}`,
		updated)

	want := store.StagePreferenceStages{
		Plan:   &store.StagePreferenceEntry{Harness: "codex", Model: "gpt-6.1-sol", Effort: "medium"},
		Verify: &store.StagePreferenceEntry{Harness: "opencode", Model: "local"},
		Review: &store.ReviewStagePreference{
			Primary: &store.StagePreferenceEntry{Harness: "claude-code", Model: "claude-opus-5-5"},
			Seats:   []store.StagePreferenceEntry{{Harness: "cursor", Model: "auto"}},
		},
	}
	assertLegacy := func(label string) {
		t.Helper()
		if !riskColumn() || ledgerRows() != 1 {
			t.Fatalf("%s: risk column present=%t ledger rows=%d", label, riskColumn(), ledgerRows())
		}
		got, err := st.GetStagePreferences(base, userID)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if got.Version != 3 || got.UpdatedAt == nil || !got.UpdatedAt.Equal(updated) || !reflect.DeepEqual(got.Stages, want) {
			t.Fatalf("%s: legacy set = %+v, want version 3 holding %+v with risk unset", label, got, want)
		}
	}
	if err := st.migrate(base); err != nil {
		t.Fatal(err)
	}
	assertLegacy("upgrade")

	// A restart after the column add but before the ledger row adds nothing.
	exec(`DELETE FROM conveyor_singlestore_migrations WHERE version=?`, stagePreferenceRiskMigrationVersion)
	if err := st.migrate(base); err != nil {
		t.Fatal(err)
	}
	assertLegacy("restart after the column add")
	if err := st.migrate(base); err != nil {
		t.Fatal(err)
	}
	assertLegacy("replay")

	risk := &store.StagePreferenceEntry{Harness: "claude-code", Model: "claude-haiku-5-5", Effort: "low"}
	saved, err := st.UpdateStagePreferences(base, userID, func(current store.StagePreferences) (store.StagePreferenceStages, error) {
		next := store.CloneStagePreferenceStages(current.Stages)
		next.Risk = risk
		return next, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantWithRisk := want
	wantWithRisk.Risk = risk
	if saved.Version != 4 || !reflect.DeepEqual(saved.Stages, wantWithRisk) {
		t.Fatalf("risk write after migration = %+v, want version 4 holding %+v", saved, wantWithRisk)
	}
	if got, err := st.GetStagePreferences(base, userID); err != nil || got.Version != 4 || !reflect.DeepEqual(got.Stages, wantWithRisk) {
		t.Fatalf("read after risk write = %+v err=%v", got, err)
	}
}
