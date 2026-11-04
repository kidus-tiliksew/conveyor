package httpapi

// Consultation records follow actual reads: real MCP calls through an HTTP
// server show which calls record one and which record none
// (req-design-delivery REQ-5; DEC-103).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type consultationEvent struct {
	DocumentID  string `json:"document_id"`
	Version     int    `json:"version"`
	SessionID   string `json:"session_id"`
	WorkOrderID string `json:"work_order_id"`
}

func (f *documentReadFixture) consultations() []consultationEvent {
	f.t.Helper()
	events, err := f.store.ListEvents(f.ctx, "")
	if err != nil {
		f.t.Fatal(err)
	}
	out := []consultationEvent{}
	for _, event := range events {
		if event.Kind != "system_design.consulted" {
			continue
		}
		var record consultationEvent
		if err := json.Unmarshal(event.Payload, &record); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, record)
	}
	return out
}

func (f *documentReadFixture) wantConsultations(context string, want ...consultationEvent) {
	f.t.Helper()
	got := f.consultations()
	if len(got) != len(want) {
		f.t.Fatalf("%s: consultations=%+v, want %+v", context, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			f.t.Fatalf("%s: consultations=%+v, want %+v", context, got, want)
		}
	}
}

// claimedConsultationCallers returns a user credential and an agent credential
// of one owner, each passing that owner's live claim.
func (f *documentReadFixture) claimedConsultationCallers(name string) ([]documentCaller, string, string) {
	f.t.Helper()
	owner, session := "usr_"+name, name+"-session"
	userToken, agentToken := f.user(owner, core.WorkspaceRoleContributor)
	orderID := f.order(name, core.StageImplement)
	f.claimRun(orderID, owner, session)
	claim := map[string]any{"work_order_id": orderID, "session_id": session}
	return []documentCaller{{name: "user", token: userToken, claim: claim}, {name: "agent", token: agentToken, claim: claim}}, orderID, session
}

func TestMCPSectionReadRecordsOneConsultationPerReadKey(t *testing.T) {
	for _, index := range []int{0, 1} {
		f := newDocumentReadFixture(t)
		callers, orderID, session := f.claimedConsultationCallers("reader")
		caller := callers[index]
		t.Run(caller.name, func(t *testing.T) {
			f.t = t
			f.design("design-read", designBody("## One\nfirst version text\n\n## Two\nmore text\n"))
			second := f.confirmDesignVersion("design-read", designBody("## One\nsecond version text\n\n## Two\nmore text\n"))
			first := consultationEvent{DocumentID: "design-read", Version: second, SessionID: session, WorkOrderID: orderID}

			f.readItem(caller, "get_document", documentReadArgs("design-read", "section_id", "s0"))
			f.wantConsultations("first section read", first)

			// A repeat, another section, a whole read, and a cache hit of one
			// version are one read key.
			f.readItem(caller, "get_document", documentReadArgs("design-read", "section_id", "s0"))
			f.readItem(caller, "get_document", documentReadArgs("design-read", "section_id", "s1", "version", second))
			f.readItem(caller, "get_document", documentReadArgs("design-read", "version", second))
			f.wantConsultations("repeat reads", first)

			// The served version is the one recorded, not the current one.
			f.readItem(caller, "get_document", documentReadArgs("design-read", "section_id", "s0", "version", second-1))
			older := consultationEvent{DocumentID: "design-read", Version: second - 1, SessionID: session, WorkOrderID: orderID}
			f.wantConsultations("explicit older version", first, older)

			// A second live claim of the owner attributes independently.
			otherOrder := f.order("reader-other", core.StageImplement)
			f.claimRun(otherOrder, "usr_reader", "reader-other-session")
			other := caller
			other.claim = map[string]any{"work_order_id": otherOrder, "session_id": "reader-other-session"}
			f.readItem(other, "get_document", documentReadArgs("design-read", "section_id", "s1"))
			f.wantConsultations("distinct order and session", first, older, consultationEvent{DocumentID: "design-read", Version: second, SessionID: "reader-other-session", WorkOrderID: otherOrder})
		})
	}
}

func TestMCPWholeReadRecordsAndRefusedReadsDoNot(t *testing.T) {
	f := newDocumentReadFixture(t)
	callers, orderID, session := f.claimedConsultationCallers("whole")
	agent := callers[1]
	f.design("design-whole", designBody("## One\nshort text\n"))
	f.design("design-large", largeDocumentBody())

	for _, args := range []map[string]any{
		documentReadArgs("design-large"),
		documentReadArgs("design-whole", "section_id", "s9"),
		documentReadArgs("design-whole", "version", 99),
		documentReadArgs("design-whole", "section_id", "s0", "version", 99),
		documentReadArgs("design-whole", "section_id", "s0", "version", 1, "byte_offset", 1<<20),
		documentReadArgs("design-absent", "section_id", "s0"),
	} {
		if text, isErr := f.call(agent.token, "get_document", agent.args(args)); !isErr {
			t.Fatalf("%v succeeded: %s", args, text)
		}
	}
	f.wantConsultations("refused reads")

	f.readItem(agent, "get_document", documentReadArgs("design-whole"))
	f.wantConsultations("whole read", consultationEvent{DocumentID: "design-whole", Version: 1, SessionID: session, WorkOrderID: orderID})

	// A page of a section too large for one response is a delivered read, and
	// every later page of that version is the same key.
	oversized := "s41"
	first := f.readItem(agent, "get_document", documentReadArgs("design-large", "section_id", oversized))
	if first["heading"] != "Oversized" || first["truncated"] != true {
		t.Fatalf("first page heading=%v truncated=%v", first["heading"], first["truncated"])
	}
	f.readItem(agent, "get_document", documentReadArgs("design-large", "section_id", oversized, "version", 1, "byte_offset", first["next_byte_offset"]))
	f.wantConsultations("large section pages", consultationEvent{DocumentID: "design-whole", Version: 1, SessionID: session, WorkOrderID: orderID}, consultationEvent{DocumentID: "design-large", Version: 1, SessionID: session, WorkOrderID: orderID})
}

func TestMCPListingsOutlinesAndSearchesRecordNoConsultation(t *testing.T) {
	f := newDocumentReadFixture(t)
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	f.server.WorkOrders.Pack = bundle
	callers, orderID, session := f.claimedConsultationCallers("lists")
	f.design("design-listed", designBody("## One\nsearchable text\n"))
	for _, caller := range callers {
		// Search fills the parsed-version cache, and that fill is not a read.
		f.readItem(caller, "get_document", documentReadArgs("design-listed", "outline", true))
		if text, isErr := f.call(caller.token, "search_documents", caller.args(map[string]any{"workspace_id": governanceWorkspace, "query": "searchable", "kind": "system_design"})); isErr || !strings.Contains(text, "design-listed") {
			t.Fatalf("%s search: %s", caller.name, text)
		}
	}
	user := callers[0]
	if text, isErr := f.call(user.token, "list_documents", map[string]any{"workspace_id": governanceWorkspace, "kind": "system_design"}); isErr || !strings.Contains(text, "design-listed") {
		t.Fatalf("list: %s", text)
	}
	for _, tool := range []string{"get_work_order", "renew_work_order", "refresh_work_order_context"} {
		if text, isErr := f.call(user.token, tool, map[string]any{"workspace_id": governanceWorkspace, "work_order_id": orderID, "session_id": session}); isErr {
			t.Fatalf("%s: %s", tool, text)
		}
	}
	queued := f.order("lists-claimed", core.StageImplement)
	if text, isErr := f.call(user.token, "claim_work_order", map[string]any{"workspace_id": governanceWorkspace, "work_order_id": queued, "session_id": "lists-claim-session", "client_token": "lists-claim-token", "agent": "codex", "model": "model"}); isErr {
		t.Fatalf("claim: %s", text)
	}
	f.wantConsultations("listing tools")

	// The cache hit that follows a search still records the read it serves.
	f.readItem(callers[1], "get_document", documentReadArgs("design-listed", "section_id", "s0"))
	f.wantConsultations("section read after cache fill", consultationEvent{DocumentID: "design-listed", Version: 1, SessionID: session, WorkOrderID: orderID})
}

func TestMCPConsultationRequiresTheCallersOwnLiveClaim(t *testing.T) {
	f := newDocumentReadFixture(t)
	userToken, agentToken := f.user("usr_claim", core.WorkspaceRoleContributor)
	otherUser, _ := f.user("usr_stranger", core.WorkspaceRoleContributor)
	viewer, _ := f.user("usr_viewer", core.WorkspaceRoleViewer)
	orderID := f.order("claim", core.StageImplement)
	f.claimRun(orderID, "usr_claim", "claim-session")
	viewerOrder := f.order("viewer", core.StageImplement)
	f.claimRun(viewerOrder, "usr_viewer", "viewer-session")
	expired := f.order("expired", core.StageImplement)
	f.claimRun(expired, "usr_claim", "expired-session")
	f.ageClaim(expired, func(order *core.WorkOrder) { order.LeaseExpiresAt = time.Now().Add(-time.Minute) })
	f.design("design-claimed", designBody("## One\ntext\n"))
	section := documentReadArgs("design-claimed", "section_id", "s0")

	// A user credential reads without attribution, whatever it names.
	for name, c := range map[string]documentCaller{
		"no claim":            {token: userToken},
		"wrong session":       {token: userToken, claim: map[string]any{"work_order_id": orderID, "session_id": "not-this-session"}},
		"order alone":         {token: userToken, claim: map[string]any{"work_order_id": orderID}},
		"session alone":       {token: userToken, claim: map[string]any{"session_id": "claim-session"}},
		"another owner":       {token: otherUser, claim: map[string]any{"work_order_id": orderID, "session_id": "claim-session"}},
		"unknown order":       {token: userToken, claim: map[string]any{"work_order_id": "gov-order-absent", "session_id": "claim-session"}},
		"expired lease":       {token: userToken, claim: map[string]any{"work_order_id": expired, "session_id": "expired-session"}},
		"owner without claim": {token: viewer, claim: map[string]any{"work_order_id": viewerOrder, "session_id": "viewer-session"}},
	} {
		c.name = name
		f.readItem(c, "get_document", section)
		f.wantConsultations(name)
	}

	// An agent credential is refused those same reads and records nothing.
	for name, claim := range map[string]map[string]any{
		"wrong session": {"work_order_id": orderID, "session_id": "not-this-session"},
		"expired lease": {"work_order_id": expired, "session_id": "expired-session"},
	} {
		caller := documentCaller{name: name, token: agentToken, claim: claim}
		if text, isErr := f.call(caller.token, "get_document", caller.args(section)); !isErr {
			t.Fatalf("%s read succeeded: %s", name, text)
		}
		f.wantConsultations("agent " + name)
	}

	// Another workspace answers as not found before any read.
	foreign := documentCaller{token: agentToken, claim: map[string]any{"work_order_id": orderID, "session_id": "claim-session"}}
	args := documentReadArgs("design-claimed", "section_id", "s0")
	args["workspace_id"] = "beta"
	if text, isErr := f.call(foreign.token, "get_document", foreign.args(args)); !isErr {
		t.Fatalf("cross-workspace read succeeded: %s", text)
	}
	f.wantConsultations("cross-workspace read")
}

// TestMCPReadsWithoutBodyTextRecordNoConsultation covers successful responses
// that deliver no design text, so no session read anything (AC-5.1).
func TestMCPReadsWithoutBodyTextRecordNoConsultation(t *testing.T) {
	f := newDocumentReadFixture(t)
	callers, _, _ := f.claimedConsultationCallers("nobody")
	agent := callers[1]
	if _, first, err := f.store.CreateSystemDesign(f.ctx, core.SystemDesign{ID: "design-draft", Title: "Draft", Category: "Component"}, core.SystemDesignVersion{Content: designBody("## Draft\ntext\n"), Origin: core.SystemDesignOriginOperator}); err != nil || first.Version != 1 {
		t.Fatalf("version=%d err=%v", first.Version, err)
	}
	absent := f.readItem(agent, "get_document", documentReadArgs("design-draft"))
	if absent["authority_absent"] != true {
		t.Fatalf("draft read=%v", absent)
	}
	f.design("design-end", designBody("## One\ntext\n"))
	section := f.readItem(agent, "get_document", documentReadArgs("design-end", "outline", true))
	size := outlineEntries(t, section)[0]["size_bytes"]
	end := f.readItem(agent, "get_document", documentReadArgs("design-end", "section_id", "s0", "version", 1, "byte_offset", size))
	if end["text"] != "" || end["complete"] != true {
		t.Fatalf("end page=%v", end)
	}
	f.wantConsultations("responses without body text")
}

func TestMCPOnlySystemDesignReadsRecordConsultation(t *testing.T) {
	f := newDocumentReadFixture(t)
	callers, _, _ := f.claimedConsultationCallers("kinds")
	agent := callers[1]
	if text, isErr := f.call(agent.token, "get_document", agent.args(map[string]any{"workspace_id": governanceWorkspace, "kind": "requirement", "document_id": governanceReqID, "version": 1})); isErr {
		t.Fatalf("requirement read: %s", text)
	}
	if _, _, err := f.store.CreateReferenceDocument(f.ctx, core.ReferenceDocument{ID: "ref-consultation", Name: "Notes"}, core.ReferenceDocumentVersion{Content: "# Notes", Filename: "notes.md", ContentType: "text/markdown"}); err != nil {
		t.Fatal(err)
	}
	if text, isErr := f.call(agent.token, "get_document", agent.args(map[string]any{"workspace_id": governanceWorkspace, "kind": "reference", "document_id": "ref-consultation"})); isErr {
		t.Fatalf("reference read: %s", text)
	}
	f.wantConsultations("requirement and reference reads")
}

// consultationLedgerStore fails the ledger write on demand and reports whether
// the boundary set a deadline on it.
type consultationLedgerStore struct {
	store.Store
	failing  atomic.Bool
	attempts atomic.Int32
	bounded  atomic.Bool
}

func (s *consultationLedgerStore) RecordSystemDesignConsulted(ctx context.Context, documentID string, version int, sessionID, workOrderID string) error {
	s.attempts.Add(1)
	_, deadline := ctx.Deadline()
	s.bounded.Store(deadline)
	if s.failing.Load() {
		return errors.New("consultation ledger unavailable")
	}
	return s.Store.RecordSystemDesignConsulted(ctx, documentID, version, sessionID, workOrderID)
}

func TestMCPConsultationLedgerFailureKeepsTheReadAndARetryRecords(t *testing.T) {
	f := newDocumentReadFixture(t)
	callers, orderID, session := f.claimedConsultationCallers("ledger")
	agent := callers[1]
	f.design("design-ledger", designBody("## One\nledger text\n"))
	section := documentReadArgs("design-ledger", "section_id", "s0")
	unbound := documentCaller{name: "unbound user", token: callers[0].token}
	healthy := f.readItem(unbound, "get_document", section)
	f.wantConsultations("unbound read")

	ledger := &consultationLedgerStore{Store: f.server.Store}
	ledger.failing.Store(true)
	f.server.Store = ledger
	var captured bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&captured)
	t.Cleanup(func() { log.SetOutput(previous) })

	failed := f.readItem(agent, "get_document", section)
	log.SetOutput(previous)
	if failed["text"] != healthy["text"] || failed["version"] != healthy["version"] {
		t.Fatalf("read during ledger failure=%v, want %v", failed, healthy)
	}
	if ledger.attempts.Load() != 1 || !ledger.bounded.Load() {
		t.Fatalf("attempts=%d bounded=%t", ledger.attempts.Load(), ledger.bounded.Load())
	}
	if logged := captured.String(); !strings.Contains(logged, "record system design consultation failed") || !strings.Contains(logged, "consultation ledger unavailable") || !strings.Contains(logged, orderID) {
		t.Fatalf("log=%q", logged)
	}
	f.wantConsultations("failed write")

	ledger.failing.Store(false)
	f.readItem(agent, "get_document", section)
	f.wantConsultations("retry after failure", consultationEvent{DocumentID: "design-ledger", Version: 1, SessionID: session, WorkOrderID: orderID})
}

func TestMCPConcurrentReadsRecordOneConsultation(t *testing.T) {
	f := newDocumentReadFixture(t)
	callers, orderID, session := f.claimedConsultationCallers("concurrent")
	agent := callers[1]
	f.design("design-concurrent", designBody("## One\nconcurrent text\n"))
	section := agent.args(documentReadArgs("design-concurrent", "section_id", "s0"))
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if text, isErr := f.call(agent.token, "get_document", section); isErr {
				t.Errorf("concurrent read: %s", text)
			}
		}()
	}
	wait.Wait()
	f.wantConsultations("concurrent reads", consultationEvent{DocumentID: "design-concurrent", Version: 1, SessionID: session, WorkOrderID: orderID})
}
