package httpapi

// Handler and MCP tests for the briefing headline on the crates list
// (component-crates invariants 14 and 17, Public surfaces; req-crates
// AC-5.1; DEC-144(2)). GET /v1/crates carries briefing: null, or exactly
// headline and generated_at. The read never computes a fingerprint and never
// calls the model, and no MCP crate read carries the briefing.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// crateListSpyStore counts the reads a fingerprint needs and can fail the
// list read.
type crateListSpyStore struct {
	crateBackend
	mu                 sync.Mutex
	insightReads       int
	briefingReads      int
	listCalls          int
	listErr            error
	listedWithBriefing []store.CrateListQuery
}

func (s *crateListSpyStore) ReadCrateInsightInputs(ctx context.Context, id string) (store.CrateInsightInputs, error) {
	s.mu.Lock()
	s.insightReads++
	s.mu.Unlock()
	return s.crateBackend.ReadCrateInsightInputs(ctx, id)
}

func (s *crateListSpyStore) GetCrateBriefing(ctx context.Context, id string) (store.CrateBriefing, error) {
	s.mu.Lock()
	s.briefingReads++
	s.mu.Unlock()
	return s.crateBackend.GetCrateBriefing(ctx, id)
}

func (s *crateListSpyStore) ListCratesWithBriefings(ctx context.Context, query store.CrateListQuery) ([]store.CrateListRow, error) {
	s.mu.Lock()
	s.listCalls++
	s.listedWithBriefing = append(s.listedWithBriefing, query)
	failure := s.listErr
	s.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	return s.crateBackend.ListCratesWithBriefings(ctx, query)
}

// crateListBriefings decodes a GET /v1/crates body into each row's raw
// briefing value by crate ID, in list order.
func crateListBriefings(t *testing.T, raw []byte) ([]string, map[string]json.RawMessage) {
	t.Helper()
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	ids := make([]string, len(rows))
	briefings := make(map[string]json.RawMessage, len(rows))
	for i, row := range rows {
		var id string
		if err := json.Unmarshal(row["id"], &id); err != nil {
			t.Fatal(err)
		}
		briefing, found := row["briefing"]
		if !found {
			t.Fatalf("row %s lacks briefing: %s", id, raw)
		}
		ids[i], briefings[id] = id, briefing
	}
	return ids, briefings
}

// requireCrateListSummary checks one served briefing: exactly headline and
// generated_at, equal to the stored row.
func requireCrateListSummary(t *testing.T, raw json.RawMessage, want store.CrateBriefing) {
	t.Helper()
	if keys := crateJSONKeys(t, raw); !slices.Equal(keys, []string{"generated_at", "headline"}) {
		t.Fatalf("briefing keys %v in %s, want generated_at and headline", keys, raw)
	}
	var summary struct {
		Headline    string    `json:"headline"`
		GeneratedAt time.Time `json:"generated_at"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Headline != want.Headline || !summary.GeneratedAt.Equal(want.GeneratedAt) {
		t.Fatalf("briefing %s, want headline %q generated at %s", raw, want.Headline, want.GeneratedAt)
	}
}

// TestCrateListRouteServesBriefingHeadline: each GET /v1/crates row carries
// the stored briefing's headline and generation time, or null, under each
// state filter. The read calls no model and reads no insight input or full
// briefing, so it computes no fingerprint. Two workspaces hold a crate with
// the same name, and the other workspace also holds a briefing under this
// workspace's crate ID: each list carries only its own headline.
func TestCrateListRouteServesBriefingHeadline(t *testing.T) {
	h := newCrateBriefingHarness(t)
	backend := h.store.(crateBackend)
	bare := h.createCrate(t, h.editor, map[string]any{"name": "No briefing yet"})
	shut := h.createCrate(t, h.editor, map[string]any{"name": "Shut with briefing"})
	home, err := backend.RecordCrateBriefing(h.ctx, store.CrateBriefing{CrateID: h.crate.ID, Fingerprint: "fp-home", Headline: "Home headline.", Account: "Home account.", Model: "m", GeneratedBy: "user:usr_maintainer"})
	if err != nil {
		t.Fatal(err)
	}
	shutBriefing, err := backend.RecordCrateBriefing(h.ctx, store.CrateBriefing{CrateID: shut.ID, Fingerprint: "fp-shut", Headline: "Closed headline.", Account: "Closed account.", Model: "m", GeneratedBy: "user:usr_maintainer"})
	if err != nil {
		t.Fatal(err)
	}
	if status, raw := h.do(t, h.editor, http.MethodPatch, "/v1/crates/"+shut.ID, `{"state":"closed"}`); status != http.StatusOK {
		t.Fatalf("close status=%d body=%s", status, raw)
	}
	twin := h.createCrate(t, h.foreign, map[string]any{"name": "Spring release"})
	twinBriefing, err := backend.RecordCrateBriefing(h.betaCtx, store.CrateBriefing{CrateID: twin.ID, Fingerprint: "fp-twin", Headline: "Beta twin headline.", Account: "Beta account.", Model: "m", GeneratedBy: "user:usr_foreign"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.RecordCrateBriefing(h.betaCtx, store.CrateBriefing{CrateID: h.crate.ID, Fingerprint: "fp-beta-same-id", Headline: "Beta headline under the alpha crate ID.", Account: "Beta account.", Model: "m", GeneratedBy: "user:usr_foreign"}); err != nil {
		t.Fatal(err)
	}
	spy := &crateListSpyStore{crateBackend: backend}
	h.server.Store = spy

	for _, test := range []struct {
		path string
		ids  []string
	}{
		{"/v1/crates", []string{bare.ID, h.crate.ID}},
		{"/v1/crates?state=closed", []string{shut.ID}},
		{"/v1/crates?state=all", []string{bare.ID, shut.ID, h.crate.ID}},
	} {
		status, raw := h.do(t, h.viewer, http.MethodGet, test.path, nil)
		if status != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", test.path, status, raw)
		}
		ids, briefings := crateListBriefings(t, raw)
		if !slices.Equal(crateSortedStrings(ids...), crateSortedStrings(test.ids...)) {
			t.Fatalf("%s ids %v, want %v", test.path, ids, test.ids)
		}
		if raw, listed := briefings[bare.ID]; listed && string(raw) != "null" {
			t.Fatalf("%s crate without briefing serves %s", test.path, raw)
		}
		if raw, listed := briefings[h.crate.ID]; listed {
			requireCrateListSummary(t, raw, home)
		}
		if raw, listed := briefings[shut.ID]; listed {
			requireCrateListSummary(t, raw, shutBriefing)
		}
		if bytes.Contains(raw, []byte("Beta")) || bytes.Contains(raw, []byte(twin.ID)) {
			t.Fatalf("%s carries the other workspace's crate or headline: %s", test.path, raw)
		}
		for _, hidden := range []string{"Home account.", "fp-home", `"current"`, `"model"`, `"generated_by"`, `"look_first"`} {
			if bytes.Contains(raw, []byte(hidden)) {
				t.Fatalf("%s serves %s beyond the headline: %s", test.path, hidden, raw)
			}
		}
	}

	status, raw := h.do(t, h.foreign, http.MethodGet, "/v1/crates?state=all", nil)
	if status != http.StatusOK {
		t.Fatalf("foreign status=%d body=%s", status, raw)
	}
	ids, briefings := crateListBriefings(t, raw)
	if !slices.Equal(ids, []string{twin.ID}) {
		t.Fatalf("foreign ids %v, want only %s", ids, twin.ID)
	}
	requireCrateListSummary(t, briefings[twin.ID], twinBriefing)

	if spy.insightReads != 0 || spy.briefingReads != 0 {
		t.Fatalf("the list read %d insight inputs and %d full briefings, want none", spy.insightReads, spy.briefingReads)
	}
	if calls := h.agent.callCount(); calls != 0 {
		t.Fatalf("the list called the model %d times", calls)
	}
	if spy.listCalls != 4 {
		t.Fatalf("list projection calls=%d, want one per list request", spy.listCalls)
	}
}

// TestCrateListRouteBriefingReadFailure: a failed list read answers the
// generic 500, logs the cause redacted with the request ID, and serves no
// partial list. Nothing is written, so nothing is lost (DEC-105(1)).
func TestCrateListRouteBriefingReadFailure(t *testing.T) {
	const secret = "hunter2hunter2hunter2hunter2"
	h := newCrateBriefingHarness(t)
	before := h.recorded(t)
	h.server.Store = &crateListSpyStore{crateBackend: h.store.(crateBackend), listErr: errors.New("driver refused the crate list password=" + secret)}
	var captured bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&captured)
	t.Cleanup(func() { log.SetOutput(previous) })
	status, raw := h.do(t, h.viewer, http.MethodGet, "/v1/crates", nil)
	log.SetOutput(previous)
	if status != http.StatusInternalServerError || string(raw) != "internal server error\n" {
		t.Fatalf("status=%d body=%q, want the generic 500", status, raw)
	}
	if strings.Contains(captured.String(), secret) || !strings.Contains(captured.String(), "driver refused the crate list") {
		t.Fatalf("log %q, want the cause redacted", captured.String())
	}
	h.server.Store = h.store
	if after := h.recorded(t); after != before {
		t.Fatalf("a failed list recorded state:\nbefore %s\nafter  %s", before, after)
	}
}

// TestMCPCrateReadsCarryNoBriefing: with a stored briefing, neither MCP
// list_crates nor get_crate carries a briefing field or its headline
// (component-crates invariant 14; DEC-144(2)).
func TestMCPCrateReadsCarryNoBriefing(t *testing.T) {
	h := newCrateBriefingHarness(t)
	const headline = "A headline only the dashboard list serves."
	if _, err := h.store.(crateBackend).RecordCrateBriefing(h.ctx, store.CrateBriefing{CrateID: h.crate.ID, Fingerprint: "fp-mcp", Headline: headline, Account: "Account.", Model: "m", GeneratedBy: "user:usr_maintainer"}); err != nil {
		t.Fatal(err)
	}
	if status, raw := h.do(t, h.viewer, http.MethodGet, "/v1/crates", nil); status != http.StatusOK || !strings.Contains(string(raw), headline) {
		t.Fatalf("REST list status=%d body=%s, want the headline", status, raw)
	}
	listed := readAs(t, h.handler, h.viewer, "list_crates", map[string]any{"workspace_id": "alpha"})
	one := readAs(t, h.handler, h.viewer, "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": h.crate.ID})
	if rows := crateMCPRows(t, listed); len(rows) != 1 || rows[0].ID != h.crate.ID {
		t.Fatalf("list_crates rows %+v", rows)
	}
	if rows := crateDetailRows(t, one); len(rows) != 1 || rows[0].ID != h.crate.ID {
		t.Fatalf("get_crate rows %+v", rows)
	}
	for name, page := range map[string]mcpReadPage{"list_crates": listed, "get_crate": one} {
		for _, item := range page.Items {
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(item, &keys); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"briefing", "headline", "generated_at"} {
				if _, found := keys[key]; found {
					t.Fatalf("%s row carries %s: %s", name, key, item)
				}
			}
			if strings.Contains(string(item), headline) {
				t.Fatalf("%s row carries the headline: %s", name, item)
			}
		}
	}
}
