package httpapi

// Handler tests for the crate briefing routes (feature-crate-insights CI-2;
// component-crates invariants 29 to 32, Public surfaces; req-crate-insights
// AC-5.2, AC-5.4, AC-5.6; DEC-142(2), (4), (6)).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/crateinsight"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

const (
	crateBriefingSecret   = "fixture-briefing-key-7d1e4c9a"
	crateBriefingHeadline = "The export work is on track."
	crateBriefingAccount  = "One task is in progress on main. Another waits for an agent."
)

// crateBriefingHarness is the crate fixture with a scripted model on the
// title route and one crate of two members.
type crateBriefingHarness struct {
	*crateFixture
	agent   *insightAgent
	crate   store.Crate
	members []core.Task
}

func newCrateBriefingHarness(t *testing.T) *crateBriefingHarness {
	t.Helper()
	h := &crateBriefingHarness{crateFixture: newCrateFixture(t)}
	h.crate = h.createCrate(t, h.editor, map[string]any{"name": "Spring release"})
	h.members = []core.Task{h.task(t, h.crate.ID), h.task(t, h.crate.ID)}
	h.agent = &insightAgent{}
	h.agent.reply = insightReply{output: h.reply(t, crateinsight.BriefingEntry{TaskID: h.members[0].ID, Tone: crateinsight.ToneAttention, Reason: "It is the next task to review."}), model: "served-model-2026", in: 900, out: 120}
	h.install(h.agent, config.TriageRoute{Model: "triage-model", Timeout: time.Minute, TimeoutText: "1m"})
	return h
}

func (h *crateBriefingHarness) install(agent inprocess.Agent, route config.TriageRoute) {
	cfg := &config.Config{Workspace: "alpha", Routing: config.Routing{Triage: route}}
	h.server.WorkOrders = &workorder.Service{Store: h.store, Dispatcher: dispatch.New(h.store, cfg, agent)}
	h.server.WorkOrders.RedactionSecrets = insightSecretSource{secrets: []string{crateBriefingSecret}}
}

func (h *crateBriefingHarness) reply(t *testing.T, entries ...crateinsight.BriefingEntry) string {
	t.Helper()
	if entries == nil {
		entries = []crateinsight.BriefingEntry{}
	}
	raw, err := json.Marshal(map[string]any{"headline": crateBriefingHeadline, "account": crateBriefingAccount, "look_first": entries})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func crateBriefingPath(crateID string) string { return "/v1/crates/" + crateID + "/briefing" }

// expectedFingerprint computes the live fingerprint from the store's insight
// inputs and the next actions the crate read serves. The crate has no
// target date and no merge, so the read instant's day does not enter it.
func (h *crateBriefingHarness) expectedFingerprint(t *testing.T) (string, []byte) {
	t.Helper()
	inputs, err := h.store.(crateBackend).ReadCrateInsightInputs(h.ctx, h.crate.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, actions := h.crateReadTasks(t, h.crate.ID)
	document := crateinsight.BriefingDocument(inputs, actions, time.Now())
	return crateinsight.BriefingFingerprintOf(document), document
}

func (h *crateBriefingHarness) stored(t *testing.T) (store.CrateBriefing, bool) {
	t.Helper()
	briefing, err := h.store.(crateBackend).GetCrateBriefing(h.ctx, h.crate.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.CrateBriefing{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return briefing, true
}

// crateBriefingWire decodes the briefing view.
type crateBriefingWire struct {
	Briefing *struct {
		Headline    string                       `json:"headline"`
		Account     string                       `json:"account"`
		LookFirst   []crateinsight.BriefingEntry `json:"look_first"`
		Model       string                       `json:"model"`
		GeneratedAt time.Time                    `json:"generated_at"`
		GeneratedBy string                       `json:"generated_by"`
		Current     bool                         `json:"current"`
	} `json:"briefing"`
	Fingerprint string `json:"fingerprint"`
	CanGenerate bool   `json:"can_generate"`
}

func decodeCrateBriefing(t *testing.T, status int, raw []byte, want int) crateBriefingWire {
	t.Helper()
	if status != want {
		t.Fatalf("status=%d body=%s, want %d", status, raw, want)
	}
	var view crateBriefingWire
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return view
}

// requireCrateBriefingKeys pins the view to component-crates Public
// surfaces: briefing, fingerprint, and can_generate, and in a stored
// briefing exactly headline, account, look_first, model, generated_at,
// generated_by, and current. No token count is served.
func requireCrateBriefingKeys(t *testing.T, raw []byte, stored bool) {
	t.Helper()
	if keys := crateJSONKeys(t, raw); !slices.Equal(keys, []string{"briefing", "can_generate", "fingerprint"}) {
		t.Fatalf("view keys %v", keys)
	}
	var view map[string]json.RawMessage
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if !stored {
		if string(view["briefing"]) != "null" {
			t.Fatalf("briefing %s, want null", view["briefing"])
		}
		return
	}
	if keys := crateJSONKeys(t, view["briefing"]); !slices.Equal(keys, []string{"account", "current", "generated_at", "generated_by", "headline", "look_first", "model"}) {
		t.Fatalf("briefing keys %v", keys)
	}
	if bytes.Contains(raw, []byte("tokens")) || bytes.Contains(raw, []byte("900")) {
		t.Fatalf("the view serves token counts: %s", raw)
	}
}

// TestCrateBriefingRouteReadsWithoutModel: GET serves briefing null, the
// live fingerprint, and the caller's create_tasks capability, and never
// calls the model. A viewer and a contributor read it with can_generate
// false, and their POST is refused before any model call (DEC-142(4), (6);
// req-crate-insights AC-5.5).
func TestCrateBriefingRouteReadsWithoutModel(t *testing.T) {
	h := newCrateBriefingHarness(t)
	want, _ := h.expectedFingerprint(t)
	for token, canGenerate := range map[string]bool{h.viewer: false, h.nonMaker: false, h.editor: true} {
		status, raw := h.do(t, token, http.MethodGet, crateBriefingPath(h.crate.ID), nil)
		view := decodeCrateBriefing(t, status, raw, http.StatusOK)
		requireCrateBriefingKeys(t, raw, false)
		if view.Fingerprint != want || view.CanGenerate != canGenerate {
			t.Fatalf("%s view %+v, want fingerprint %s and can_generate %v", token, view, want, canGenerate)
		}
	}
	for _, token := range []string{h.viewer, h.nonMaker} {
		// A caller without create_tasks gets the workspace not-found refusal
		// every crate mutation gives (component-crates invariant 3).
		if status, raw := h.do(t, token, http.MethodPost, crateBriefingPath(h.crate.ID), nil); status != http.StatusNotFound || crateErrorCode(t, raw)["error"] != "workspace_not_found" {
			t.Fatalf("%s POST status=%d body=%s, want the workspace not-found refusal", token, status, raw)
		}
	}
	if calls := h.agent.callCount(); calls != 0 {
		t.Fatalf("reads and refused POSTs called the model %d times", calls)
	}
	if _, ok := h.stored(t); ok {
		t.Fatal("a read stored a briefing")
	}
}

// TestCrateBriefingRouteGeneratesOnceAndServesCurrent: an editor's POST
// sends the prompt of the document, stores the validated briefing with the
// fingerprint sent, the model the client names, the requester, and the
// token counts, and serves the view with current true and no token count. A
// second POST answers 409 crate_briefing_current before any model call. GET
// then serves the stored briefing as current.
func TestCrateBriefingRouteGeneratesOnceAndServesCurrent(t *testing.T) {
	h := newCrateBriefingHarness(t)
	fingerprint, document := h.expectedFingerprint(t)
	before := time.Now().UTC().Add(-time.Second)
	status, raw := h.do(t, h.editor, http.MethodPost, crateBriefingPath(h.crate.ID), nil)
	view := decodeCrateBriefing(t, status, raw, http.StatusOK)
	requireCrateBriefingKeys(t, raw, true)
	wantEntries := []crateinsight.BriefingEntry{{TaskID: h.members[0].ID, Tone: crateinsight.ToneAttention, Reason: "It is the next task to review."}}
	if view.Fingerprint != fingerprint || !view.CanGenerate || view.Briefing.Headline != crateBriefingHeadline || view.Briefing.Account != crateBriefingAccount ||
		!slices.Equal(view.Briefing.LookFirst, wantEntries) || view.Briefing.Model != "served-model-2026" || !view.Briefing.Current || view.Briefing.GeneratedAt.Before(before) {
		t.Fatalf("POST view %s", raw)
	}
	if calls := h.agent.callCount(); calls != 1 {
		t.Fatalf("model calls=%d, want 1", calls)
	}
	if prompt := h.agent.prompts[0]; prompt != crateinsight.BriefingInstructions+"\n\nBEGIN CRATE_DATA\n"+string(document)+"\nEND CRATE_DATA\n" {
		t.Fatalf("prompt %q", prompt)
	}
	stored, ok := h.stored(t)
	if !ok || stored.Fingerprint != fingerprint || stored.TokensIn != 900 || stored.TokensOut != 120 || stored.Model != "served-model-2026" ||
		!strings.Contains(stored.GeneratedBy, intakeOwner(core.WorkspaceRoleMaintainer)) || stored.GeneratedBy != view.Briefing.GeneratedBy || !stored.GeneratedAt.Equal(view.Briefing.GeneratedAt) {
		t.Fatalf("stored %+v, view %s", stored, raw)
	}
	status, raw = h.do(t, h.editor, http.MethodPost, crateBriefingPath(h.crate.ID), nil)
	if code := crateErrorCode(t, raw); status != http.StatusConflict || code["error"] != "crate_briefing_current" || code["crate_id"] != h.crate.ID {
		t.Fatalf("second POST status=%d body=%s", status, raw)
	}
	if calls := h.agent.callCount(); calls != 1 {
		t.Fatalf("a current POST called the model: calls=%d", calls)
	}
	status, raw = h.do(t, h.viewer, http.MethodGet, crateBriefingPath(h.crate.ID), nil)
	read := decodeCrateBriefing(t, status, raw, http.StatusOK)
	requireCrateBriefingKeys(t, raw, true)
	if read.Briefing == nil || !read.Briefing.Current || read.Briefing.Headline != crateBriefingHeadline || read.CanGenerate || read.Fingerprint != fingerprint {
		t.Fatalf("GET after POST %s", raw)
	}
}

// TestCrateBriefingRouteReadsFreshness: a change to the crate's recorded
// state makes the stored briefing out of date on the next read, and the
// editor can regenerate. A change while the model runs is stamped with the
// fingerprint actually sent, so the next read reports it out of date
// (component-requirement-insights invariant 15, through component-crates
// invariant 29).
func TestCrateBriefingRouteReadsFreshness(t *testing.T) {
	h := newCrateBriefingHarness(t)
	if status, raw := h.do(t, h.editor, http.MethodPost, crateBriefingPath(h.crate.ID), nil); status != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", status, raw)
	}
	h.task(t, h.crate.ID)
	status, raw := h.do(t, h.viewer, http.MethodGet, crateBriefingPath(h.crate.ID), nil)
	view := decodeCrateBriefing(t, status, raw, http.StatusOK)
	if view.Briefing == nil || view.Briefing.Current {
		t.Fatalf("after a new member the briefing reads %s, want out of date", raw)
	}
	// The regeneration's model call adds another member while it runs.
	h.agent.replies = nil
	h.install(&mutatingAgent{inner: h.agent, mutate: func() { h.task(t, h.crate.ID) }}, config.TriageRoute{Model: "triage-model"})
	sent, _ := h.expectedFingerprint(t)
	status, raw = h.do(t, h.editor, http.MethodPost, crateBriefingPath(h.crate.ID), nil)
	posted := decodeCrateBriefing(t, status, raw, http.StatusOK)
	if posted.Fingerprint != sent || !posted.Briefing.Current {
		t.Fatalf("regenerate view %s, want the sent fingerprint %s as current", raw, sent)
	}
	if stored, _ := h.stored(t); stored.Fingerprint != sent {
		t.Fatalf("stored fingerprint %s, want the one sent %s", stored.Fingerprint, sent)
	}
	status, raw = h.do(t, h.viewer, http.MethodGet, crateBriefingPath(h.crate.ID), nil)
	if view := decodeCrateBriefing(t, status, raw, http.StatusOK); view.Briefing.Current || view.Fingerprint == sent {
		t.Fatalf("a change during generation reads as current: %s", raw)
	}
}

// mutatingAgent changes recorded state during the model call, then answers
// as inner does.
type mutatingAgent struct {
	inner  inprocess.Agent
	mutate func()
}

func (agent *mutatingAgent) Run(ctx context.Context, model string, input inprocess.Input) (inprocess.Result, error) {
	agent.mutate()
	return agent.inner.Run(ctx, model, input)
}

// TestCrateBriefingRouteEmptyAndMissingCrates: a crate with no member
// answers 409 crate_empty before any model call, and a missing or foreign
// crate answers 404 crate_not_found on both routes.
func TestCrateBriefingRouteEmptyAndMissingCrates(t *testing.T) {
	h := newCrateBriefingHarness(t)
	empty := h.createCrate(t, h.editor, map[string]any{"name": "Empty"})
	status, raw := h.do(t, h.editor, http.MethodPost, crateBriefingPath(empty.ID), nil)
	if code := crateErrorCode(t, raw); status != http.StatusConflict || code["error"] != "crate_empty" || code["crate_id"] != empty.ID {
		t.Fatalf("empty POST status=%d body=%s", status, raw)
	}
	status, raw = h.do(t, h.viewer, http.MethodGet, crateBriefingPath(empty.ID), nil)
	if view := decodeCrateBriefing(t, status, raw, http.StatusOK); view.Briefing != nil || view.Fingerprint == "" {
		t.Fatalf("empty GET %s", raw)
	}
	for _, token := range []string{h.editor, h.foreign} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			id := "crate-000000-000000"
			if token == h.foreign {
				id = h.crate.ID
			}
			status, raw := h.do(t, token, method, crateBriefingPath(id), nil)
			if code := crateErrorCode(t, raw); status != http.StatusNotFound || code["error"] != "crate_not_found" {
				t.Fatalf("%s %s %s status=%d body=%s", token, method, id, status, raw)
			}
		}
	}
	if calls := h.agent.callCount(); calls != 0 {
		t.Fatalf("model calls=%d, want 0", calls)
	}
}

// TestCrateBriefingRouteUnconfigured: no work-order service, no dispatcher,
// no in-process agent, and a triage route without a model each answer 503
// crate_briefing_unconfigured and store nothing.
func TestCrateBriefingRouteUnconfigured(t *testing.T) {
	h := newCrateBriefingHarness(t)
	for name, install := range map[string]func(){
		"no work-order service": func() { h.server.WorkOrders = nil },
		"no dispatcher":         func() { h.server.WorkOrders = &workorder.Service{Store: h.store} },
		"no agent":              func() { h.install(nil, config.TriageRoute{Model: "triage-model"}) },
		"no route model":        func() { h.install(h.agent, config.TriageRoute{Model: " ", Timeout: time.Minute}) },
	} {
		t.Run(name, func(t *testing.T) {
			install()
			status, raw := h.do(t, h.editor, http.MethodPost, crateBriefingPath(h.crate.ID), nil)
			if code := crateErrorCode(t, raw); status != http.StatusServiceUnavailable || code["error"] != "crate_briefing_unconfigured" {
				t.Fatalf("status=%d body=%s", status, raw)
			}
			if _, ok := h.stored(t); ok || h.agent.callCount() != 0 {
				t.Fatalf("an unconfigured POST stored a briefing or called the model")
			}
		})
	}
}

// TestCrateBriefingRouteFailuresKeepStoredBriefing: a provider error, an
// invalid reply, a reply that breaks a sentence limit of
// req-crate-insights AC-5.1, and a failed redaction each answer 502
// crate_briefing_failed with a redacted detail that never echoes the reply,
// and leave the stored briefing exactly as it was (AC-5.4).
func TestCrateBriefingRouteFailuresKeepStoredBriefing(t *testing.T) {
	const echo = "ECHO-OF-THE-REPLY"
	for _, test := range []struct {
		name       string
		reply      insightReply
		redaction  error
		wantDetail func(string) bool
	}{
		{"provider error", insightReply{err: fmt.Errorf("Responses API rejected key %s", crateBriefingSecret)}, nil, func(detail string) bool {
			return strings.Contains(detail, "Responses API rejected key") && !strings.Contains(detail, crateBriefingSecret)
		}},
		{"invalid reply", insightReply{output: `{"headline":"` + echo + `","account":"","look_first":[]}`, model: "served"}, nil, func(detail string) bool {
			return strings.Contains(detail, "the account is empty")
		}},
		{"nonmember entry", insightReply{output: `{"headline":"H.","account":"A ` + echo + `.","look_first":[{"task_id":"261119-ffffff","tone":"info","reason":"` + echo + `"}]}`, model: "served"}, nil, func(detail string) bool {
			return strings.Contains(detail, "not a member of the crate")
		}},
		{"two-sentence headline", insightReply{output: `{"headline":"Work started. ` + echo + ` follows.","account":"A.","look_first":[]}`, model: "served"}, nil, func(detail string) bool {
			return strings.Contains(detail, "the headline is not exactly one sentence")
		}},
		{"non-breaking-space headline", insightReply{output: `{"headline":"Work started.\u00a0` + echo + ` follows.","account":"A.","look_first":[]}`, model: "served"}, nil, func(detail string) bool {
			return strings.Contains(detail, "the headline is not exactly one sentence")
		}},
		{"four-sentence account", insightReply{output: `{"headline":"H.","account":"One merged. One is in review. One waits. The ` + echo + ` holds.","look_first":[]}`, model: "served"}, nil, func(detail string) bool {
			return strings.Contains(detail, "the account holds more than three sentences")
		}},
		{"redaction unavailable", insightReply{err: fmt.Errorf("Responses API rejected key %s", crateBriefingSecret)}, errors.New("secret store offline"), func(detail string) bool {
			return detail == crateBriefingFailureWithheld
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newCrateBriefingHarness(t)
			if status, raw := h.do(t, h.editor, http.MethodPost, crateBriefingPath(h.crate.ID), nil); status != http.StatusOK {
				t.Fatalf("first POST status=%d body=%s", status, raw)
			}
			kept, _ := h.stored(t)
			h.task(t, h.crate.ID)
			h.agent.replies = []insightReply{h.agent.reply, test.reply}
			if test.redaction != nil {
				h.server.WorkOrders.RedactionSecrets = insightSecretSource{err: test.redaction}
			}
			var captured bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&captured)
			status, raw := h.do(t, h.editor, http.MethodPost, crateBriefingPath(h.crate.ID), nil)
			log.SetOutput(previous)
			code := crateErrorCode(t, raw)
			if status != http.StatusBadGateway || code["error"] != "crate_briefing_failed" || !test.wantDetail(code["detail"]) {
				t.Fatalf("status=%d body=%s", status, raw)
			}
			for _, leaked := range []string{echo, crateBriefingSecret, "261119-ffffff"} {
				if strings.Contains(string(raw), leaked) || strings.Contains(captured.String(), leaked) {
					t.Fatalf("the failure leaks %q: body=%s log=%s", leaked, raw, captured.String())
				}
			}
			if stored, _ := h.stored(t); !sameStoredCrateBriefing(stored, kept) {
				t.Fatalf("a failure changed the stored briefing: %+v, want %+v", stored, kept)
			}
			status, raw = h.do(t, h.viewer, http.MethodGet, crateBriefingPath(h.crate.ID), nil)
			if view := decodeCrateBriefing(t, status, raw, http.StatusOK); view.Briefing == nil || view.Briefing.Headline != kept.Headline || view.Briefing.Current {
				t.Fatalf("GET after failure %s", raw)
			}
		})
	}
}

func sameStoredCrateBriefing(a, b store.CrateBriefing) bool {
	return a.Fingerprint == b.Fingerprint && a.Headline == b.Headline && a.Account == b.Account && slices.Equal(a.LookFirst, b.LookFirst) &&
		a.Model == b.Model && a.TokensIn == b.TokensIn && a.TokensOut == b.TokensOut && a.GeneratedBy == b.GeneratedBy && a.GeneratedAt.Equal(b.GeneratedAt)
}

// crateBriefingFaultStore injects briefing store failures and a
// cross-process winner.
type crateBriefingFaultStore struct {
	crateBackend
	inputsErr, getErr, recordErr error
	// winner, when set, is recorded through the inner store just before the
	// handler's own write, as a second conveyord process would.
	winner func(store.CrateBriefing) store.CrateBriefing
}

func (f *crateBriefingFaultStore) ReadCrateInsightInputs(ctx context.Context, id string) (store.CrateInsightInputs, error) {
	if f.inputsErr != nil {
		return store.CrateInsightInputs{}, f.inputsErr
	}
	return f.crateBackend.ReadCrateInsightInputs(ctx, id)
}

func (f *crateBriefingFaultStore) GetCrateBriefing(ctx context.Context, id string) (store.CrateBriefing, error) {
	if f.getErr != nil {
		return store.CrateBriefing{}, f.getErr
	}
	return f.crateBackend.GetCrateBriefing(ctx, id)
}

func (f *crateBriefingFaultStore) RecordCrateBriefing(ctx context.Context, briefing store.CrateBriefing) (store.CrateBriefing, error) {
	if f.recordErr != nil {
		return store.CrateBriefing{}, f.recordErr
	}
	if f.winner != nil {
		if _, err := f.crateBackend.RecordCrateBriefing(ctx, f.winner(briefing)); err != nil {
			return store.CrateBriefing{}, err
		}
	}
	return f.crateBackend.RecordCrateBriefing(ctx, briefing)
}

// TestCrateBriefingRouteStoreFailures: a failed input read, briefing read,
// or briefing write answers the generic 500 with the cause logged redacted
// and no store text in the body. A failed read makes no model call. A
// failed write leaves the stored row unchanged. A writer that lost to
// another process with the same fingerprint answers 409
// crate_briefing_current (DEC-105(1); component-requirement-insights State
// and failure).
func TestCrateBriefingRouteStoreFailures(t *testing.T) {
	const secret = "hunter2hunter2hunter2hunter2"
	for _, test := range []struct {
		name      string
		fault     func(*crateBriefingFaultStore)
		methods   []string
		wantCalls int
	}{
		{"input read", func(f *crateBriefingFaultStore) {
			f.inputsErr = errors.New("driver refused the insight read password=" + secret)
		}, []string{http.MethodGet, http.MethodPost}, 0},
		{"briefing read", func(f *crateBriefingFaultStore) {
			f.getErr = errors.New("driver refused the briefing read password=" + secret)
		}, []string{http.MethodGet, http.MethodPost}, 0},
		{"briefing write", func(f *crateBriefingFaultStore) {
			f.recordErr = errors.New("driver refused the briefing write password=" + secret)
		}, []string{http.MethodPost}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newCrateBriefingHarness(t)
			before := h.recorded(t)
			fault := &crateBriefingFaultStore{crateBackend: h.store.(crateBackend)}
			test.fault(fault)
			h.server.Store = fault
			var captured bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&captured)
			t.Cleanup(func() { log.SetOutput(previous) })
			for _, method := range test.methods {
				status, raw := h.do(t, h.editor, method, crateBriefingPath(h.crate.ID), nil)
				if status != http.StatusInternalServerError || string(raw) != "internal server error\n" {
					t.Fatalf("%s status=%d body=%q, want the generic 500", method, status, raw)
				}
			}
			log.SetOutput(previous)
			if strings.Contains(captured.String(), secret) || !strings.Contains(captured.String(), "store failure") || !strings.Contains(captured.String(), "driver refused") {
				t.Fatalf("log %q, want a redacted store failure that names its cause", captured.String())
			}
			if calls := h.agent.callCount(); calls != test.wantCalls {
				t.Fatalf("model calls=%d, want %d", calls, test.wantCalls)
			}
			h.server.Store = h.store
			if _, ok := h.stored(t); ok {
				t.Fatal("a failed request stored a briefing")
			}
			if after := h.recorded(t); after != before {
				t.Fatalf("a failed request recorded state:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
	t.Run("lost same-fingerprint write", func(t *testing.T) {
		h := newCrateBriefingHarness(t)
		h.server.Store = &crateBriefingFaultStore{crateBackend: h.store.(crateBackend), winner: func(own store.CrateBriefing) store.CrateBriefing {
			own.Headline, own.GeneratedBy = "The other process won.", "user:other-process"
			return own
		}}
		status, raw := h.do(t, h.editor, http.MethodPost, crateBriefingPath(h.crate.ID), nil)
		if code := crateErrorCode(t, raw); status != http.StatusConflict || code["error"] != "crate_briefing_current" {
			t.Fatalf("status=%d body=%s", status, raw)
		}
		if stored, _ := h.stored(t); stored.Headline != "The other process won." {
			t.Fatalf("stored %+v, want the winner's row", stored)
		}
	})
}

// waitForCrateBriefingWaiters spins, without sleeping, until n requests hold
// or wait on the crate's generation mutex.
func waitForCrateBriefingWaiters(t *testing.T, server *Server, key requirementInsightKey, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		server.crateBriefingLocks.mu.Lock()
		refs := 0
		if entry := server.crateBriefingLocks.entries[key]; entry != nil {
			refs = entry.refs
		}
		server.crateBriefingLocks.mu.Unlock()
		if refs >= n {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiters for %+v=%d, want %d", key, refs, n)
		default:
			runtime.Gosched()
		}
	}
}

func (h *crateBriefingHarness) postAsync(token, crateID string) <-chan insightResult {
	done := make(chan insightResult, 1)
	go func() {
		workspace := "alpha"
		if token == h.foreign {
			workspace = "beta"
		}
		request := httptest.NewRequest(http.MethodPost, crateBriefingPath(crateID)+"?workspace_id="+workspace, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		h.handler.ServeHTTP(response, request)
		done <- insightResult{code: response.Code, body: response.Body.String()}
	}()
	return done
}

// TestCrateBriefingRouteSerializesGeneration: an overlapping POST for the
// same workspace and crate waits on the process mutex, reads again after the
// first stores its briefing, and answers 409 without a second model call.
// Another crate generates while the first holds its key, a waiter generates
// after a failed first attempt, and the lock registry empties afterwards.
func TestCrateBriefingRouteSerializesGeneration(t *testing.T) {
	h := newCrateBriefingHarness(t)
	other := h.createCrate(t, h.editor, map[string]any{"name": "Other crate"})
	otherMember := h.task(t, other.ID)
	agent := &insightAgent{started: make(chan struct{}, 8), release: make(chan struct{}), replies: []insightReply{h.agent.reply}}
	agent.reply = insightReply{output: h.reply(t, crateinsight.BriefingEntry{TaskID: otherMember.ID, Tone: crateinsight.ToneInfo, Reason: "It is the only task."}), model: "served"}
	h.install(agent, config.TriageRoute{Model: "triage-model"})
	key := requirementInsightKey{workspace: "alpha", requirement: h.crate.ID}

	first := h.postAsync(h.editor, h.crate.ID)
	<-agent.started
	waiter := h.postAsync(h.editor, h.crate.ID)
	waitForCrateBriefingWaiters(t, h.server, key, 2)
	otherCrate := h.postAsync(h.editor, other.ID)
	<-agent.started
	if calls := agent.callCount(); calls != 2 {
		t.Fatalf("model calls while the first holds its key=%d, want 2", calls)
	}
	close(agent.release)
	for name, done := range map[string]<-chan insightResult{"first": first, "other crate": otherCrate} {
		if result := <-done; result.code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", name, result.code, result.body)
		}
	}
	if result := <-waiter; result.code != http.StatusConflict || !strings.Contains(result.body, `"error":"crate_briefing_current"`) {
		t.Fatalf("waiter status=%d body=%s", result.code, result.body)
	}
	if calls := agent.callCount(); calls != 2 {
		t.Fatalf("model calls=%d, want 2: the waiter read the stored row again", calls)
	}

	h2 := newCrateBriefingHarness(t)
	failing := &insightAgent{replies: []insightReply{{err: errors.New("provider unavailable")}}, reply: h2.agent.reply, started: make(chan struct{}, 8), release: make(chan struct{})}
	h2.install(failing, config.TriageRoute{Model: "triage-model"})
	key2 := requirementInsightKey{workspace: "alpha", requirement: h2.crate.ID}
	failed := h2.postAsync(h2.editor, h2.crate.ID)
	<-failing.started
	retry := h2.postAsync(h2.editor, h2.crate.ID)
	waitForCrateBriefingWaiters(t, h2.server, key2, 2)
	close(failing.release)
	if result := <-failed; result.code != http.StatusBadGateway {
		t.Fatalf("failed status=%d body=%s", result.code, result.body)
	}
	<-failing.started
	if result := <-retry; result.code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", result.code, result.body)
	}
	for _, server := range []*Server{h.server, h2.server} {
		server.crateBriefingLocks.mu.Lock()
		remaining := len(server.crateBriefingLocks.entries)
		server.crateBriefingLocks.mu.Unlock()
		if remaining != 0 {
			t.Fatalf("lock registry kept %d entries", remaining)
		}
	}
}

// TestCrateBriefingRouteChangesNoRecordedState: generating and storing a
// briefing appends no event and changes no task, crate, or membership, and
// a member task's work-order context read afterwards carries no briefing
// text (req-crate-insights AC-5.6; DEC-142(2)).
func TestCrateBriefingRouteChangesNoRecordedState(t *testing.T) {
	h := newCrateBriefingHarness(t)
	before := h.recorded(t)
	tasksBefore := map[string]core.Task{}
	for _, member := range h.members {
		task, err := h.store.GetTask(h.ctx, member.ID)
		if err != nil {
			t.Fatal(err)
		}
		tasksBefore[member.ID] = task
	}
	if status, raw := h.do(t, h.editor, http.MethodPost, crateBriefingPath(h.crate.ID), nil); status != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", status, raw)
	}
	if after := h.recorded(t); after != before {
		t.Fatalf("storing a briefing recorded state:\nbefore %s\nafter  %s", before, after)
	}
	for id, want := range tasksBefore {
		got, err := h.store.GetTask(h.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		gotRaw, _ := json.Marshal(got)
		wantRaw, _ := json.Marshal(want)
		if !bytes.Equal(gotRaw, wantRaw) {
			t.Fatalf("task %s changed:\nbefore %s\nafter  %s", id, wantRaw, gotRaw)
		}
	}
	orders, err := h.store.ListWorkOrders(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 0 {
		t.Fatalf("storing a briefing created work orders: %+v", orders)
	}
}
