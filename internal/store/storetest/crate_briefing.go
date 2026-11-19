package storetest

// Crate briefing conformance (feature-crate-insights CI-2; component-crates
// invariant 29, through component-requirement-insights invariants 14 to 16;
// req-crate-insights AC-5.2, AC-5.4, AC-5.6; DEC-142(2)). Every backend
// stores one briefing per workspace and crate, compares and writes it
// atomically, leaves one winner among concurrent writers of one
// fingerprint, and changes nothing else.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func runCrateBriefings(t *testing.T, factory Factory) {
	t.Run("CrateBriefingInsertAndReplace", func(t *testing.T) { runCrateBriefingInsertAndReplace(t, factory.fresh(t, nil)) })
	t.Run("CrateBriefingEqualFingerprintNoWrite", func(t *testing.T) { runCrateBriefingEqualFingerprint(t, factory.fresh(t, nil)) })
	t.Run("CrateBriefingWorkspaceIsolation", func(t *testing.T) { runCrateBriefingWorkspaceIsolation(t, factory.fresh(t, nil)) })
	t.Run("CrateBriefingConcurrentSameFingerprint", func(t *testing.T) { runCrateBriefingConcurrent(t, factory.fresh(t, nil)) })
	t.Run("CrateBriefingWritesNoEventsOrControlState", func(t *testing.T) { runCrateBriefingControlState(t, factory.fresh(t, nil)) })
}

func crateBriefingRow(crateID, fingerprint, headline string, entries ...store.CrateBriefingEntry) store.CrateBriefing {
	return store.CrateBriefing{
		CrateID: crateID, Fingerprint: fingerprint, Headline: headline, Account: "Account for " + fingerprint + ".",
		LookFirst: entries, Model: "model-" + fingerprint, TokensIn: int64(len(headline)), TokensOut: int64(len(fingerprint)),
		GeneratedBy: "user:briefing-" + fingerprint,
	}
}

func sameCrateBriefing(a, b store.CrateBriefing) bool {
	return a.WorkspaceID == b.WorkspaceID && a.CrateID == b.CrateID && a.Fingerprint == b.Fingerprint && a.Headline == b.Headline &&
		a.Account == b.Account && reflect.DeepEqual(store.CloneCrateBriefingEntries(a.LookFirst), store.CloneCrateBriefingEntries(b.LookFirst)) &&
		a.Model == b.Model && a.TokensIn == b.TokensIn && a.TokensOut == b.TokensOut && a.GeneratedBy == b.GeneratedBy && a.GeneratedAt.Equal(b.GeneratedAt)
}

func getCrateBriefing(t *testing.T, x Fixture, ctx context.Context, crateID string) store.CrateBriefing {
	t.Helper()
	briefing, err := x.Backend.GetCrateBriefing(ctx, crateID)
	requireOK(t, err)
	return briefing
}

func briefingCrate(t *testing.T, x Fixture, name string) store.Crate {
	t.Helper()
	crate, err := x.Backend.CreateCrate(x.Context, store.CrateInput{Name: name})
	requireOK(t, err)
	return crate
}

// runCrateBriefingInsertAndReplace: an absent briefing is ErrNotFound; a
// write stores every field with the bound workspace and a fresh UTC storage
// time whatever the caller sends; a new fingerprint replaces every field;
// the entries read back in order; and invalid input is refused with the
// stored row unchanged.
func runCrateBriefingInsertAndReplace(t *testing.T, x Fixture) {
	crate := briefingCrate(t, x, "Briefing insert")
	member := crateTask(t, x, core.TaskRunning, crate.ID)
	if _, err := x.Backend.GetCrateBriefing(x.Context, crate.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent briefing err=%v, want ErrNotFound", err)
	}
	before := time.Now().UTC().Add(-time.Second)
	first := crateBriefingRow(crate.ID, "fp-one", "First headline.",
		store.CrateBriefingEntry{TaskID: member.ID, Tone: "attention", Reason: "It waits for review."},
		store.CrateBriefingEntry{TaskID: member.ID, Tone: "positive", Reason: "Ünïcode reason ✓."})
	first.WorkspaceID = "caller-supplied-workspace"
	first.GeneratedAt = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	stored, err := x.Backend.RecordCrateBriefing(x.Context, first)
	requireOK(t, err)
	want := first
	want.WorkspaceID, want.GeneratedAt = x.Workspace, stored.GeneratedAt
	if !sameCrateBriefing(stored, want) || stored.GeneratedAt.Before(before) || stored.GeneratedAt.Location() != time.UTC {
		t.Fatalf("stored %+v, want %+v with a fresh UTC storage time", stored, want)
	}
	if got := getCrateBriefing(t, x, x.Context, crate.ID); !sameCrateBriefing(got, stored) {
		t.Fatalf("read back %+v, want %+v", got, stored)
	}
	// The returned entries are copies: changing them changes no stored row.
	stored.LookFirst[0].Reason = "changed by the caller"
	if got := getCrateBriefing(t, x, x.Context, crate.ID); got.LookFirst[0].Reason != "It waits for review." {
		t.Fatalf("stored entries alias the caller's slice: %+v", got.LookFirst)
	}
	second := crateBriefingRow(crate.ID, "fp-two", "Second headline.")
	replaced, err := x.Backend.RecordCrateBriefing(x.Context, second)
	requireOK(t, err)
	want = second
	want.WorkspaceID, want.GeneratedAt = x.Workspace, replaced.GeneratedAt
	if !sameCrateBriefing(replaced, want) || replaced.LookFirst == nil || len(replaced.LookFirst) != 0 || replaced.GeneratedAt.Before(getCrateBriefing(t, x, x.Context, crate.ID).GeneratedAt.Add(-time.Hour)) {
		t.Fatalf("replaced %+v, want every field of %+v and an empty entry list", replaced, want)
	}
	got := getCrateBriefing(t, x, x.Context, crate.ID)
	if !sameCrateBriefing(got, replaced) || got.LookFirst == nil {
		t.Fatalf("read back %+v, want %+v", got, replaced)
	}
	negative := crateBriefingRow(crate.ID, "fp-negative", "x")
	negative.TokensIn = -1
	for _, invalid := range []store.CrateBriefing{crateBriefingRow("", "fp", "x"), crateBriefingRow(crate.ID, " ", "x"), negative} {
		if _, err := x.Backend.RecordCrateBriefing(x.Context, invalid); err == nil {
			t.Fatalf("invalid briefing %+v was accepted", invalid)
		}
	}
	if got := getCrateBriefing(t, x, x.Context, crate.ID); !sameCrateBriefing(got, replaced) {
		t.Fatalf("refused writes changed the row to %+v", got)
	}
}

// runCrateBriefingEqualFingerprint: an equal fingerprint refuses with
// ErrCrateBriefingCurrent and changes no column.
func runCrateBriefingEqualFingerprint(t *testing.T, x Fixture) {
	crate := briefingCrate(t, x, "Briefing equal")
	member := crateTask(t, x, core.TaskRunning, crate.ID)
	stored, err := x.Backend.RecordCrateBriefing(x.Context, crateBriefingRow(crate.ID, "fp-same", "Original.", store.CrateBriefingEntry{TaskID: member.ID, Tone: "info", Reason: "Original reason."}))
	requireOK(t, err)
	refused := crateBriefingRow(crate.ID, "fp-same", "A different headline.")
	refused.Account, refused.Model, refused.TokensIn, refused.TokensOut, refused.GeneratedBy = "A different account.", "other-model", 999, 999, "user:someone-else"
	if _, err := x.Backend.RecordCrateBriefing(x.Context, refused); !errors.Is(err, store.ErrCrateBriefingCurrent) {
		t.Fatalf("equal fingerprint err=%v, want ErrCrateBriefingCurrent", err)
	}
	if store.ErrCrateBriefingCurrent.Error() != "crate_briefing_current" {
		t.Fatalf("sentinel text %q", store.ErrCrateBriefingCurrent)
	}
	if got := getCrateBriefing(t, x, x.Context, crate.ID); !sameCrateBriefing(got, stored) {
		t.Fatalf("refusal changed the row: %+v, want %+v", got, stored)
	}
}

// foreignBriefingContext binds a second workspace of the same backend.
func foreignBriefingContext(t *testing.T, x Fixture) (context.Context, string) {
	t.Helper()
	foreignID := x.Workspace + "-briefing-foreign"
	if _, err := x.Backend.BootstrapWorkspaceConfig(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), foreignID), &config.Config{Workspace: foreignID, Repos: x.Config.Repos}); err != nil {
		t.Fatal(err)
	}
	return store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: requirementConformanceActor, Role: core.ActorHuman}), foreignID), foreignID
}

// runCrateBriefingWorkspaceIsolation: a briefing is read and written only in
// its own workspace. The same crate key and fingerprint in another workspace
// is a separate row.
func runCrateBriefingWorkspaceIsolation(t *testing.T, x Fixture) {
	crate := briefingCrate(t, x, "Briefing isolation")
	home, err := x.Backend.RecordCrateBriefing(x.Context, crateBriefingRow(crate.ID, "fp-home", "Home."))
	requireOK(t, err)
	foreign, foreignID := foreignBriefingContext(t, x)
	if _, err := x.Backend.GetCrateBriefing(foreign, crate.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign read err=%v, want ErrNotFound", err)
	}
	written, err := x.Backend.RecordCrateBriefing(foreign, crateBriefingRow(crate.ID, "fp-home", "Foreign."))
	if err != nil {
		t.Fatalf("foreign record: %v", err)
	}
	if written.WorkspaceID != foreignID || written.Headline != "Foreign." {
		t.Fatalf("foreign row %+v", written)
	}
	if got := getCrateBriefing(t, x, x.Context, crate.ID); !sameCrateBriefing(got, home) {
		t.Fatalf("home row changed to %+v", got)
	}
	if got := getCrateBriefing(t, x, foreign, crate.ID); !sameCrateBriefing(got, written) {
		t.Fatalf("foreign row %+v, want %+v", got, written)
	}
}

// runCrateBriefingConcurrent races six writers of one fingerprint over an
// absent row and then over a stored row with another fingerprint. Each
// race leaves exactly one winner, the rest answer ErrCrateBriefingCurrent,
// and the stored row is the winner's. Every writer starts at once behind a
// closed channel, and no case sleeps.
func runCrateBriefingConcurrent(t *testing.T, x Fixture) {
	crate := briefingCrate(t, x, "Briefing race")
	race := func(label, fingerprint string) store.CrateBriefing {
		t.Helper()
		start := make(chan struct{})
		results := make([]store.CrateBriefing, 6)
		errs := make([]error, 6)
		var ready, done sync.WaitGroup
		for i := range results {
			ready.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				briefing := crateBriefingRow(crate.ID, fingerprint, fmt.Sprintf("%s caller %d.", label, i))
				briefing.GeneratedBy = fmt.Sprintf("user:%s-%d", label, i)
				ready.Done()
				<-start
				results[i], errs[i] = x.Backend.RecordCrateBriefing(x.Context, briefing)
			}()
		}
		ready.Wait()
		close(start)
		done.Wait()
		var winner store.CrateBriefing
		winners := 0
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
				winner = results[i]
			case errors.Is(err, store.ErrCrateBriefingCurrent):
			default:
				t.Fatalf("%s caller %d: %v", label, i, err)
			}
		}
		if winners != 1 {
			t.Fatalf("%s: %d winners, want exactly one", label, winners)
		}
		if got := getCrateBriefing(t, x, x.Context, crate.ID); !sameCrateBriefing(got, winner) {
			t.Fatalf("%s: stored %+v, want the winner %+v unchanged", label, got, winner)
		}
		return winner
	}
	first := race("absent", "fp-race-one")
	second := race("replace", "fp-race-two")
	if second.Fingerprint == first.Fingerprint || second.GeneratedAt.Before(first.GeneratedAt) {
		t.Fatalf("replacement %+v after %+v", second, first)
	}
}

// runCrateBriefingControlState: storing, replacing, and refusing a briefing
// append no task or workspace event and change no task, work order, crate
// row, crate membership, crate activity, or insight input
// (req-crate-insights AC-5.6; DEC-142(2)).
func runCrateBriefingControlState(t *testing.T, x Fixture) {
	crate := briefingCrate(t, x, "Briefing control state")
	member := crateTask(t, x, core.TaskRunning, crate.ID)
	snapshot := func() []any {
		t.Helper()
		events, err := x.Backend.ListEvents(x.Context, member.ID)
		requireOK(t, err)
		workspaceEvents, err := x.Backend.ListEvents(x.Context, "")
		requireOK(t, err)
		task, err := x.Backend.GetTask(x.Context, member.ID)
		requireOK(t, err)
		tasks, err := x.Backend.ListTasks(x.Context)
		requireOK(t, err)
		orders, err := x.Backend.ListWorkOrders(x.Context)
		requireOK(t, err)
		record, err := x.Backend.GetCrate(x.Context, crate.ID)
		requireOK(t, err)
		crates, err := x.Backend.ListCrates(x.Context, store.CrateListQuery{})
		requireOK(t, err)
		activity, err := x.Backend.ListCrateActivity(x.Context, crate.ID, store.CrateActivityQuery{Limit: 50})
		requireOK(t, err)
		inputs, err := x.Backend.ReadCrateInsightInputs(x.Context, crate.ID)
		requireOK(t, err)
		out := []any{len(events), events[len(events)-1].ID, len(workspaceEvents), task, len(tasks), len(orders), record, crates, activity, inputs}
		if x.WorkspaceEvents != nil {
			all, err := x.WorkspaceEvents(x.Context, "")
			requireOK(t, err)
			out = append(out, len(all))
		}
		return out
	}
	before := snapshot()
	_, err := x.Backend.RecordCrateBriefing(x.Context, crateBriefingRow(crate.ID, "fp-control-one", "One.", store.CrateBriefingEntry{TaskID: member.ID, Tone: "info", Reason: "One."}))
	requireOK(t, err)
	_, err = x.Backend.RecordCrateBriefing(x.Context, crateBriefingRow(crate.ID, "fp-control-two", "Two."))
	requireOK(t, err)
	if _, err := x.Backend.RecordCrateBriefing(x.Context, crateBriefingRow(crate.ID, "fp-control-two", "Refused.")); !errors.Is(err, store.ErrCrateBriefingCurrent) {
		t.Fatalf("equal fingerprint err=%v", err)
	}
	if after := snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("a briefing write changed recorded state:\nbefore %+v\nafter  %+v", before, after)
	}
}
