package stageprefs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/stageprefs"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func newService(t *testing.T) *stageprefs.Service {
	t.Helper()
	return stageprefs.New(store.NewVolatileBackend())
}

func entry(harness, model, effort string) map[string]any {
	value := map[string]any{"harness": harness, "model": model}
	if effort != "" {
		value["effort"] = effort
	}
	return value
}

func body(t *testing.T, stages any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"stages": stages})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func requireValidation(t *testing.T, err error, field string, position int) {
	t.Helper()
	var invalid *stageprefs.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want a validation error naming %s", err, field)
	}
	if invalid.Field != field || invalid.Position != position {
		t.Fatalf("validation error = %+v, want field %q position %d", invalid, field, position)
	}
}

// TestEmptySetBeforeAnySave covers AC-2.4: every stage reads unset, at version
// 0, and the read does not fail.
func TestEmptySetBeforeAnySave(t *testing.T) {
	set, err := newService(t).Get(context.Background(), "usr_never")
	if err != nil {
		t.Fatal(err)
	}
	if set.Version != 0 || set.UpdatedAt != nil || set.Stages.Plan != nil || set.Stages.Implement != nil || set.Stages.Verify != nil || set.Stages.Review != nil {
		t.Fatalf("never-saved set = %+v", set)
	}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"version":0,"updated_at":null,"stages":{"plan":null,"implement":null,"verify":null,"review":null}}`; string(raw) != want {
		t.Fatalf("never-saved JSON = %s, want %s", raw, want)
	}
}

func TestReplaceStoresTrimmedFreeTextAndAdvancesVersion(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	seats := make([]any, stageprefs.MaxReviewSeats)
	for i := range seats {
		seats[i] = entry(fmt.Sprintf("  harness %d ", i+1), "any model, any text: ok", "")
	}
	saved, err := service.Replace(ctx, "usr_a", 0, body(t, map[string]any{
		"plan":      entry("  codex ", " gpt-6.1-sol ", " medium "),
		"implement": entry("my-own-harness", "a-model-nobody-lists", ""),
		"verify":    nil,
		"review":    map[string]any{"primary": entry("claude-code", "claude-opus-5-5", "high"), "seats": seats},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 1 || saved.UpdatedAt == nil {
		t.Fatalf("first save version = %d updated_at = %v", saved.Version, saved.UpdatedAt)
	}
	if got := *saved.Stages.Plan; got != (store.StagePreferenceEntry{Harness: "codex", Model: "gpt-6.1-sol", Effort: "medium"}) {
		t.Fatalf("plan was not trimmed: %+v", got)
	}
	if saved.Stages.Verify != nil {
		t.Fatalf("verify = %+v, want unset", saved.Stages.Verify)
	}
	if len(saved.Stages.Review.Seats) != stageprefs.MaxReviewSeats || saved.Stages.Review.Seats[7].Harness != "harness 8" {
		t.Fatalf("eight seats were not kept in order: %+v", saved.Stages.Review.Seats)
	}
	read, err := service.Get(ctx, "usr_a")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read, saved) {
		t.Fatalf("read %+v, saved %+v", read, saved)
	}
	next, err := service.Replace(ctx, "usr_a", 1, body(t, map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if next.Version != 2 || next.Stages != (store.StagePreferenceStages{}) {
		t.Fatalf("replacing with no stages = %+v, want version 2 with every stage unset", next)
	}
}

// TestReplaceVersionConflict covers AC-1.7: a stale version changes nothing.
func TestReplaceVersionConflict(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	first, err := service.Replace(ctx, "usr_a", 0, body(t, map[string]any{"plan": entry("codex", "m", "")}))
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []int64{0, 2, 7} {
		if _, err := service.Replace(ctx, "usr_a", stale, body(t, map[string]any{"verify": entry("x", "y", "")})); !errors.Is(err, stageprefs.ErrVersionConflict) {
			t.Fatalf("version %d: error = %v, want version conflict", stale, err)
		}
	}
	read, err := service.Get(ctx, "usr_a")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read, first) {
		t.Fatalf("a refused write changed the set: %+v, want %+v", read, first)
	}
}

// TestPersonIsolation covers AC-1.6: each user ID reads and writes only its
// own set.
func TestPersonIsolation(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	if _, err := service.Replace(ctx, "usr_a", 0, body(t, map[string]any{"plan": entry("codex", "a", "")})); err != nil {
		t.Fatal(err)
	}
	other, err := service.Get(ctx, "usr_b")
	if err != nil {
		t.Fatal(err)
	}
	if other.Version != 0 || other.Stages.Plan != nil {
		t.Fatalf("another person read %+v", other)
	}
	if _, err := service.SetStage(ctx, "usr_b", "plan", json.RawMessage(`{"harness":"b","model":"b"}`)); err != nil {
		t.Fatal(err)
	}
	mine, err := service.Get(ctx, "usr_a")
	if err != nil {
		t.Fatal(err)
	}
	if mine.Version != 1 || mine.Stages.Plan.Model != "a" {
		t.Fatalf("another person's write reached this set: %+v", mine)
	}
	for _, call := range []func() error{
		func() error { _, err := service.Get(ctx, " "); return err },
		func() error { _, err := service.Replace(ctx, "", 0, body(t, map[string]any{})); return err },
		func() error { _, err := service.SetStage(ctx, "", "plan", nil); return err },
	} {
		if err := call(); !errors.Is(err, stageprefs.ErrNoUser) {
			t.Fatalf("call without a user: error = %v", err)
		}
	}
}

// TestEntryValidation covers AC-1.3 and invariant 2: required, trimmed,
// byte-limited text without control characters, refused with the field and
// the seat position.
func TestEntryValidation(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	long := strings.Repeat("a", stageprefs.MaxNameBytes)
	longEffort := strings.Repeat("e", stageprefs.MaxEffortBytes)
	// "é" is two bytes, so 64 of them sit exactly at the byte limit.
	multibyte := strings.Repeat("é", stageprefs.MaxNameBytes/2)
	if _, err := service.Replace(ctx, "usr_ok", 0, body(t, map[string]any{
		"plan":      entry(long, multibyte, longEffort),
		"implement": entry("  "+long+"  ", "m", ""),
	})); err != nil {
		t.Fatalf("values at the limits were refused: %v", err)
	}
	seat := func(position int, value map[string]any) map[string]any {
		seats := make([]any, position)
		for i := range seats {
			seats[i] = entry("h", "m", "")
		}
		seats[position-1] = value
		return map[string]any{"review": map[string]any{"seats": seats}}
	}
	cases := []struct {
		name     string
		stages   map[string]any
		field    string
		position int
	}{
		{"blank harness", map[string]any{"plan": entry("   ", "m", "")}, "stages.plan.harness", 0},
		{"missing model", map[string]any{"verify": map[string]any{"harness": "h"}}, "stages.verify.model", 0},
		{"null model", map[string]any{"verify": map[string]any{"harness": "h", "model": nil}}, "stages.verify.model", 0},
		{"long harness", map[string]any{"plan": entry(long+"a", "m", "")}, "stages.plan.harness", 0},
		{"long multibyte model", map[string]any{"plan": entry("h", multibyte+"a", "")}, "stages.plan.model", 0},
		{"long effort", map[string]any{"plan": entry("h", "m", longEffort+"e")}, "stages.plan.effort", 0},
		{"control in harness", map[string]any{"implement": entry("co\x07dex", "m", "")}, "stages.implement.harness", 0},
		{"newline in model", map[string]any{"implement": entry("codex", "m\n", "")}, "stages.implement.model", 0},
		{"control in effort", map[string]any{"implement": entry("codex", "m", "hi\tgh")}, "stages.implement.effort", 0},
		{"number harness", map[string]any{"plan": map[string]any{"harness": 7, "model": "m"}}, "stages.plan.harness", 0},
		{"blank review primary model", map[string]any{"review": map[string]any{"primary": entry("h", " ", "")}}, "stages.review.primary.model", 0},
		{"third seat harness", seat(3, entry("", "m", "")), "stages.review.seats[2].harness", 3},
		{"eighth seat effort", seat(8, entry("h", "m", longEffort+"e")), "stages.review.seats[7].effort", 8},
		{"null seat", seat(2, nil), "stages.review.seats[1]", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := service.Replace(ctx, "usr_refused", 0, body(t, tc.stages))
			requireValidation(t, err, tc.field, tc.position)
		})
	}
	if set, err := service.Get(ctx, "usr_refused"); err != nil || set.Version != 0 {
		t.Fatalf("refused writes stored a set: %+v, %v", set, err)
	}
}

// TestClosedShape covers invariant 3 and AC-1.5: every field outside the
// shape is refused by name, never ignored.
func TestClosedShape(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	nine := make([]any, stageprefs.MaxReviewSeats+1)
	for i := range nine {
		nine[i] = entry("h", "m", "")
	}
	cases := []struct {
		name     string
		raw      string
		field    string
		position int
	}{
		{"execution host beside stages", `{"stages":{},"execution_host":"box"}`, "execution_host", 0},
		{"version in body", `{"stages":{},"version":1}`, "version", 0},
		{"missing stages", `{}`, "stages", 0},
		{"null stages", `{"stages":null}`, "stages", 0},
		{"body not an object", `[]`, "body", 0},
		{"unknown stage", `{"stages":{"triage":null}}`, "stages.triage", 0},
		{"sandbox in entry", `{"stages":{"plan":{"harness":"h","model":"m","sandbox":"none"}}}`, "stages.plan.sandbox", 0},
		{"command in entry", `{"stages":{"implement":{"harness":"h","model":"m","command":"run"}}}`, "stages.implement.command", 0},
		{"credential in seat", `{"stages":{"review":{"seats":[{"harness":"h","model":"m","credential":"x"}]}}}`, "stages.review.seats[0].credential", 1},
		{"concurrency in review", `{"stages":{"review":{"primary":null,"concurrency":2}}}`, "stages.review.concurrency", 0},
		{"list for plan", `{"stages":{"plan":[{"harness":"h","model":"m"}]}}`, "stages.plan", 0},
		{"list for verify", `{"stages":{"verify":[]}}`, "stages.verify", 0},
		{"string for implement", `{"stages":{"implement":"codex"}}`, "stages.implement", 0},
		{"seats not a list", `{"stages":{"review":{"seats":{"harness":"h","model":"m"}}}}`, "stages.review.seats", 0},
		{"review a list", `{"stages":{"review":[]}}`, "stages.review", 0},
		{"ninth seat", string(body(t, map[string]any{"review": map[string]any{"seats": nine}})), "stages.review.seats[8]", 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := service.Replace(ctx, "usr_shape", 0, []byte(tc.raw))
			requireValidation(t, err, tc.field, tc.position)
		})
	}
	if set, err := service.Get(ctx, "usr_shape"); err != nil || set.Version != 0 {
		t.Fatalf("refused writes stored a set: %+v, %v", set, err)
	}
}

// TestUnsetReviewNormalizes covers AC-1.2: a review with neither a primary nor
// seats is unset.
func TestUnsetReviewNormalizes(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	for i, review := range []string{`null`, `{}`, `{"primary":null}`, `{"seats":[]}`, `{"primary":null,"seats":null}`} {
		saved, err := service.Replace(ctx, "usr_review", int64(i), []byte(`{"stages":{"review":`+review+`}}`))
		if err != nil {
			t.Fatalf("%s: %v", review, err)
		}
		if saved.Stages.Review != nil {
			t.Fatalf("%s saved as %+v, want unset", review, saved.Stages.Review)
		}
	}
	saved, err := service.SetStage(ctx, "usr_review", "review", json.RawMessage(`{"primary":{"harness":"h","model":"m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if saved.Stages.Review == nil || saved.Stages.Review.Seats == nil || len(saved.Stages.Review.Seats) != 0 {
		t.Fatalf("a primary-only review = %+v, want an empty seats list", saved.Stages.Review)
	}
}

// TestSetStage covers invariant 4 for the one-stage write: it replaces or
// clears one stage and keeps the others.
func TestSetStage(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	if _, err := service.SetStage(ctx, "usr_a", "plan", json.RawMessage(`{"harness":"codex","model":"m"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetStage(ctx, "usr_a", "review", json.RawMessage(`{"seats":[{"harness":"h","model":"m"}]}`)); err != nil {
		t.Fatal(err)
	}
	cleared, err := service.SetStage(ctx, "usr_a", "plan", json.RawMessage(`null`))
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Version != 3 || cleared.Stages.Plan != nil || cleared.Stages.Review == nil || len(cleared.Stages.Review.Seats) != 1 {
		t.Fatalf("clearing plan = %+v", cleared)
	}
	for _, tc := range []struct {
		stage, value, field string
	}{
		{"triage", `null`, "stage"},
		{"plan", `[{"harness":"h","model":"m"}]`, "value"},
		{"verify", `{"harness":"h","model":"m","host":"x"}`, "value.host"},
	} {
		_, err := service.SetStage(ctx, "usr_a", tc.stage, json.RawMessage(tc.value))
		requireValidation(t, err, tc.field, 0)
	}
	if set, _ := service.Get(ctx, "usr_a"); set.Version != 3 {
		t.Fatalf("refused stage writes changed the version to %d", set.Version)
	}
}

// TestConcurrentStageWritesKeepEveryStage proves the lost-update guard:
// concurrent one-stage writes, the first write included, each land without
// dropping another (invariant 4).
func TestConcurrentStageWritesKeepEveryStage(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, len(stageprefs.Stages))
	for i, stage := range stageprefs.Stages {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value := fmt.Sprintf(`{"harness":"h-%s","model":"m"}`, stage)
			if stage == stageprefs.StageReview {
				value = `{"primary":` + value + `}`
			}
			_, errs[i] = service.SetStage(ctx, "usr_race", stage, json.RawMessage(value))
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	set, err := service.Get(ctx, "usr_race")
	if err != nil {
		t.Fatal(err)
	}
	if set.Version != 4 || set.Stages.Plan == nil || set.Stages.Implement == nil || set.Stages.Verify == nil || set.Stages.Review == nil || set.Stages.Review.Primary == nil {
		t.Fatalf("concurrent stage writes lost a stage: %+v", set)
	}
}

// TestConcurrentReplaceAdmitsOneWriter proves that two whole-set writers based
// on the same version cannot both succeed: one wins and the other gets a
// version conflict, so neither silently overwrites the other (AC-1.7).
func TestConcurrentReplaceAdmitsOneWriter(t *testing.T) {
	service := newService(t)
	ctx := context.Background()
	const writers = 6
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = service.Replace(ctx, "usr_race", 0, body(t, map[string]any{"plan": entry(fmt.Sprintf("h%d", i), "m", "")}))
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, stageprefs.ErrVersionConflict):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d writers based on version 0 succeeded, want exactly 1", won)
	}
	if set, _ := service.Get(ctx, "usr_race"); set.Version != 1 {
		t.Fatalf("version = %d, want 1", set.Version)
	}
}

// failingStore fails every call, so the service's store-failure path is
// observable.
type failingStore struct{ err error }

func (f failingStore) GetStagePreferences(context.Context, string) (store.StagePreferences, error) {
	return store.StagePreferences{}, f.err
}

func (f failingStore) UpdateStagePreferences(context.Context, string, store.StagePreferenceUpdate) (store.StagePreferences, error) {
	return store.StagePreferences{}, f.err
}

// TestStoreFailureIsReturned covers the component's store-failure rule: no
// call reports success, or a cached set, when the store fails.
func TestStoreFailureIsReturned(t *testing.T) {
	failure := errors.New("database down")
	service := stageprefs.New(failingStore{err: failure})
	ctx := context.Background()
	if _, err := service.Get(ctx, "usr_a"); !errors.Is(err, failure) {
		t.Fatalf("Get error = %v", err)
	}
	if _, err := service.Replace(ctx, "usr_a", 0, body(t, map[string]any{})); !errors.Is(err, failure) {
		t.Fatalf("Replace error = %v", err)
	}
	if _, err := service.SetStage(ctx, "usr_a", "plan", json.RawMessage(`null`)); !errors.Is(err, failure) {
		t.Fatalf("SetStage error = %v", err)
	}
}
