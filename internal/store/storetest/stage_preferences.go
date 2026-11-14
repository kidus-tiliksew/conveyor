package storetest

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runStagePreferences proves one versioned set per person on every backend:
// the never-saved read, the first write, a refused write, clearing a stage,
// person isolation, the same set from two workspaces, and serialized
// concurrent writers that lose no change (req-stage-preferences AC-1.1,
// AC-1.4, AC-1.6, AC-1.7, AC-2.4; component-stage-preferences invariants 1,
// 4, and 6). It also proves the risk stage (DEC-135(13)): a set saved without
// risk reads it as unset, a risk write and a risk clear keep every other
// stage, and a returned set shares no memory with the stored one.
func runStagePreferences(t *testing.T, x Fixture) {
	st := x.Backend
	ctx, owner := bootstrapOwner(t, x)
	other := memberWith(t, st, ctx, "stage-preferences-other@example.test", x.Workspace, core.WorkspaceRoleViewer)
	fresh := memberWith(t, st, ctx, "stage-preferences-fresh@example.test", x.Workspace, core.WorkspaceRoleViewer)

	unset := func(step string, got store.StagePreferences) {
		t.Helper()
		if got.Version != 0 || got.UpdatedAt != nil || !reflect.DeepEqual(got.Stages, store.StagePreferenceStages{}) {
			t.Fatalf("%s: never-saved set = %+v, want version 0 with every stage unset", step, got)
		}
	}
	read := func(step, userID string) store.StagePreferences {
		t.Helper()
		got, err := st.GetStagePreferences(ctx, userID)
		if err != nil {
			t.Fatalf("%s: read: %v", step, err)
		}
		return got
	}
	unset("owner before any write", read("owner before any write", owner.ID))

	entry := func(harness, model, effort string) *store.StagePreferenceEntry {
		return &store.StagePreferenceEntry{Harness: harness, Model: model, Effort: effort}
	}
	first := store.StagePreferenceStages{
		Plan:   entry("codex", "gpt-6.1-sol", "medium"),
		Review: &store.ReviewStagePreference{Primary: entry("claude-code", "claude-opus-5-5", ""), Seats: []store.StagePreferenceEntry{*entry("codex", "gpt-6.1-sol", "high"), *entry("cursor", "auto", "")}},
	}
	saved, err := st.UpdateStagePreferences(ctx, owner.ID, func(current store.StagePreferences) (store.StagePreferenceStages, error) {
		unset("first write's current set", current)
		return first, nil
	})
	requireOK(t, err)
	if saved.Version != 1 || saved.UpdatedAt == nil || !reflect.DeepEqual(saved.Stages, first) {
		t.Fatalf("first write = %+v, want version 1 holding %+v", saved, first)
	}
	if got := read("after first write", owner.ID); got.Version != 1 || got.UpdatedAt == nil || !got.UpdatedAt.Equal(*saved.UpdatedAt) || !reflect.DeepEqual(got.Stages, first) {
		t.Fatalf("read after first write = %+v, want %+v", got, saved)
	}

	// A refused update returns its own error and changes nothing (AC-1.7).
	refused := errors.New("refused by the caller")
	if _, err := st.UpdateStagePreferences(ctx, owner.ID, func(current store.StagePreferences) (store.StagePreferenceStages, error) {
		next := current.Stages
		next.Plan = nil
		return next, refused
	}); !errors.Is(err, refused) {
		t.Fatalf("refused update error = %v, want the caller's error", err)
	}
	if got := read("after refused write", owner.ID); got.Version != 1 || !reflect.DeepEqual(got.Stages, first) {
		t.Fatalf("refused update changed the set: %+v", got)
	}

	// Clearing one stage leaves the others (AC-1.4).
	cleared, err := st.UpdateStagePreferences(ctx, owner.ID, func(current store.StagePreferences) (store.StagePreferenceStages, error) {
		if current.Version != 1 {
			t.Errorf("clear read version %d, want 1", current.Version)
		}
		next := current.Stages
		next.Plan = nil
		return next, nil
	})
	requireOK(t, err)
	if cleared.Version != 2 || cleared.Stages.Plan != nil || !reflect.DeepEqual(cleared.Stages.Review, first.Review) {
		t.Fatalf("clear = %+v, want version 2 with plan unset and review kept", cleared)
	}

	// The sets above were saved without risk, the shape every set had before
	// the risk stage, so risk reads unset. A risk write keeps every other
	// stage, a returned set shares no memory with the stored one, and clearing
	// risk keeps the other stages too (AC-1.1, AC-1.4; DEC-135(13)).
	if got := read("legacy-shaped set", owner.ID); got.Stages.Risk != nil {
		t.Fatalf("a set saved without risk reads risk = %+v, want unset", got.Stages.Risk)
	}
	riskEntry := entry("claude-code", "claude-haiku-5-5", "low")
	withRisk, err := st.UpdateStagePreferences(ctx, owner.ID, func(current store.StagePreferences) (store.StagePreferenceStages, error) {
		if current.Version != 2 || current.Stages.Risk != nil {
			t.Errorf("risk write read %+v, want version 2 with risk unset", current)
		}
		next := current.Stages
		next.Risk = riskEntry
		return next, nil
	})
	requireOK(t, err)
	if withRisk.Version != 3 || !reflect.DeepEqual(withRisk.Stages.Risk, riskEntry) || withRisk.Stages.Plan != nil || !reflect.DeepEqual(withRisk.Stages.Review, first.Review) {
		t.Fatalf("risk write = %+v, want version 3 with risk set and the other stages kept", withRisk)
	}
	withRisk.Stages.Risk.Model = "changed by the caller"
	withRisk.Stages.Review.Seats[0].Harness = "changed by the caller"
	if got := read("after risk write", owner.ID); got.Version != 3 || !reflect.DeepEqual(got.Stages.Risk, riskEntry) || !reflect.DeepEqual(got.Stages.Review, first.Review) {
		t.Fatalf("read after risk write = %+v, want the stored risk entry unchanged by the caller's edit", got.Stages)
	}
	cleared, err = st.UpdateStagePreferences(ctx, owner.ID, func(current store.StagePreferences) (store.StagePreferenceStages, error) {
		next := current.Stages
		next.Risk = nil
		return next, nil
	})
	requireOK(t, err)
	if cleared.Version != 4 || cleared.Stages.Risk != nil || cleared.Stages.Plan != nil || !reflect.DeepEqual(cleared.Stages.Review, first.Review) {
		t.Fatalf("risk clear = %+v, want version 4 with risk unset and review kept", cleared)
	}
	if got := read("after risk clear", owner.ID); got.Version != 4 || !reflect.DeepEqual(got.Stages, cleared.Stages) {
		t.Fatalf("read after risk clear = %+v, want %+v", got, cleared)
	}

	// One person's set is invisible to and unchanged by another (AC-1.6).
	unset("other person", read("other person", other.ID))
	otherStages := store.StagePreferenceStages{Verify: entry("opencode", "local-model", "")}
	otherSaved, err := st.UpdateStagePreferences(ctx, other.ID, func(current store.StagePreferences) (store.StagePreferenceStages, error) {
		unset("other person's current set", current)
		return otherStages, nil
	})
	requireOK(t, err)
	if otherSaved.Version != 1 || !reflect.DeepEqual(otherSaved.Stages, otherStages) {
		t.Fatalf("other person's write = %+v", otherSaved)
	}
	if got := read("owner after other's write", owner.ID); got.Version != cleared.Version || !reflect.DeepEqual(got.Stages, cleared.Stages) {
		t.Fatalf("other person's write changed the owner's set: %+v", got)
	}

	// The same person reads the same set from a second workspace (REQ-1).
	second := x.Workspace + "-stage-preferences"
	_, err = st.CreateWorkspace(systemContext(store.WithWorkspace(x.Context, second)), second, second, &config.Config{Workspace: second, Repos: x.Config.Repos})
	requireOK(t, err)
	_, err = st.GrantWorkspaceRole(ctx, owner.Email, second, core.WorkspaceRoleViewer)
	requireOK(t, err)
	elsewhere, err := st.GetStagePreferences(store.WithWorkspace(ctx, second), owner.ID)
	requireOK(t, err)
	if here := read("owner in first workspace", owner.ID); !reflect.DeepEqual(elsewhere, here) {
		t.Fatalf("second workspace read %+v, first workspace read %+v", elsewhere, here)
	}

	// Concurrent writers each add one review seat. Every write sees the one
	// before it, so no seat is lost and the version advances once per write.
	const writers = 8
	runConcurrently := func(userID string, count int, edit func(int, store.StagePreferenceStages) store.StagePreferenceStages) []error {
		var wg sync.WaitGroup
		errs := make([]error, count)
		for i := range count {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = st.UpdateStagePreferences(ctx, userID, func(current store.StagePreferences) (store.StagePreferenceStages, error) {
					return edit(i, store.CloneStagePreferenceStages(current.Stages)), nil
				})
			}()
		}
		wg.Wait()
		return errs
	}
	before := read("before concurrent seats", owner.ID)
	for _, err := range runConcurrently(owner.ID, writers, func(i int, next store.StagePreferenceStages) store.StagePreferenceStages {
		next.Review.Seats = append(next.Review.Seats, store.StagePreferenceEntry{Harness: fmt.Sprintf("harness-%d", i), Model: "model"})
		return next
	}) {
		requireOK(t, err)
	}
	after := read("after concurrent seats", owner.ID)
	if after.Version != before.Version+writers || len(after.Stages.Review.Seats) != len(before.Stages.Review.Seats)+writers {
		t.Fatalf("concurrent seat writes: version %d -> %d, seats %d -> %d; want +%d each", before.Version, after.Version, len(before.Stages.Review.Seats), len(after.Stages.Review.Seats), writers)
	}
	var added []string
	for _, seat := range after.Stages.Review.Seats[len(before.Stages.Review.Seats):] {
		added = append(added, seat.Harness)
	}
	sort.Strings(added)
	for i, harness := range added {
		if harness != fmt.Sprintf("harness-%d", i) {
			t.Fatalf("concurrent seat writes lost or repeated a seat: %v", added)
		}
	}
	if !reflect.DeepEqual(after.Stages.Review.Primary, first.Review.Primary) || after.Stages.Plan != nil || after.Stages.Risk != nil {
		t.Fatalf("concurrent seat writes changed another stage: %+v", after.Stages)
	}

	// Concurrent first writes for a person with no row each set one of the
	// five stages, and every stage survives.
	const stageCount = 5
	const firstWriters = 2 * stageCount
	unset("fresh person", read("fresh person", fresh.ID))
	for _, err := range runConcurrently(fresh.ID, firstWriters, func(i int, next store.StagePreferenceStages) store.StagePreferenceStages {
		value := entry(fmt.Sprintf("harness-%d", i), "model", "")
		switch i % stageCount {
		case 0:
			next.Plan = value
		case 1:
			next.Implement = value
		case 2:
			next.Verify = value
		case 3:
			next.Risk = value
		default:
			if next.Review == nil {
				next.Review = &store.ReviewStagePreference{Seats: []store.StagePreferenceEntry{}}
			}
			next.Review.Seats = append(next.Review.Seats, *value)
		}
		return next
	}) {
		requireOK(t, err)
	}
	got := read("fresh person after concurrent first writes", fresh.ID)
	if got.Version != firstWriters || got.Stages.Plan == nil || got.Stages.Implement == nil || got.Stages.Verify == nil || got.Stages.Risk == nil || got.Stages.Review == nil || len(got.Stages.Review.Seats) != firstWriters/stageCount {
		t.Fatalf("concurrent first writes = %+v, want version %d with every stage set and %d seats", got, firstWriters, firstWriters/stageCount)
	}
}
