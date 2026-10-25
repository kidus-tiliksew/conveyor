package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Bounded detail view and task history pages (task 261015-3721c6;
// component-http-api "Bounded detail view", "Task history pages";
// req-operator-next-action AC-6.10, AC-6.11). Fixtures are seeded
// directly into the memory store or a test ledger; nothing sleeps.

const (
	historyWorkspace = "demo"
	historyTokenA    = "tok-a"
	historyTokenA2   = "tok-a2"
)

type historyHarness struct {
	t       *testing.T
	st      store.Store
	srv     *Server
	handler http.Handler
	members *membershipFixture
}

func newHistoryHarness(t *testing.T, st store.Store) *historyHarness {
	t.Helper()
	srv := NewServer(st)
	srv.Credentials = staticCredentialVerifier{
		historyTokenA:  {ID: "pat_a", OwnerUserID: "usr_a", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
		historyTokenA2: {ID: "pat_a2", OwnerUserID: "usr_a", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
	}
	workspaces := []core.Workspace{{ID: historyWorkspace}, {ID: "other"}}
	srv.Workspaces = &fakeWorkspaceControl{items: workspaces}
	members := &membershipFixture{workspaces: workspaces, roles: map[string]map[string]core.WorkspaceRole{
		"usr_a": {historyWorkspace: core.WorkspaceRoleOperator, "other": core.WorkspaceRoleOperator},
	}}
	srv.Memberships = members
	return &historyHarness{t: t, st: st, srv: srv, handler: srv.Handler(), members: members}
}

func (h *historyHarness) get(token, path string) *httptest.ResponseRecorder {
	h.t.Helper()
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	request := httptest.NewRequest(http.MethodGet, path+separator+"workspace_id="+historyWorkspace, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	return response
}

type historyEventJSON struct {
	ID                    string                     `json:"id"`
	TaskID                string                     `json:"task_id"`
	JobID                 string                     `json:"job_id"`
	Kind                  string                     `json:"kind"`
	ActorID               string                     `json:"actor_id"`
	Payload               json.RawMessage            `json:"payload"`
	At                    string                     `json:"at"`
	AuditAvailable        bool                       `json:"audit_available"`
	PayloadOmitted        bool                       `json:"payload_omitted"`
	PayloadBytes          int                        `json:"payload_bytes"`
	PayloadIndex          map[string]json.RawMessage `json:"payload_index"`
	PayloadIndexTruncated bool                       `json:"payload_index_truncated"`
}

type historyPageJSON struct {
	Selection string             `json:"selection"`
	Limit     int                `json:"limit"`
	Events    []historyEventJSON `json:"events"`
	Boundary  struct {
		MaxID string `json:"max_id"`
		Count int    `json:"count"`
	} `json:"boundary"`
	HasOlder    bool   `json:"has_older"`
	HasNewer    bool   `json:"has_newer"`
	OlderCursor string `json:"older_cursor"`
	NewerCursor string `json:"newer_cursor"`
}

type boundedDetailJSON struct {
	Events    []historyEventJSON `json:"events"`
	EventPage struct {
		Limit       int    `json:"limit"`
		Returned    int    `json:"returned"`
		Kinds       string `json:"kinds"`
		HasOlder    bool   `json:"has_older"`
		OlderCursor string `json:"older_cursor"`
	} `json:"event_page"`
	Summaries taskDetailSummaries `json:"-"`
	Raw       map[string]json.RawMessage
}

func (h *historyHarness) history(token, taskID, query string) (historyPageJSON, *httptest.ResponseRecorder) {
	h.t.Helper()
	response := h.get(token, "/v1/tasks/"+taskID+"/history"+query)
	var page historyPageJSON
	if response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			h.t.Fatal(err)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			h.t.Fatalf("history Cache-Control=%q", response.Header().Get("Cache-Control"))
		}
		requireHistoryPageBounds(h.t, page, response.Body.Len())
	}
	return page, response
}

func (h *historyHarness) bounded(taskID string) (boundedDetailJSON, *httptest.ResponseRecorder) {
	h.t.Helper()
	response := h.get(historyTokenA, "/v1/tasks/"+taskID+"/activity?view=bounded")
	if response.Code != http.StatusOK {
		h.t.Fatalf("bounded view: %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Conveyor-Detail-View") != "bounded" {
		h.t.Fatal("bounded view header missing")
	}
	var detail boundedDetailJSON
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		h.t.Fatal(err)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &detail.Raw); err != nil {
		h.t.Fatal(err)
	}
	section := len(detail.Raw["events"]) + len(detail.Raw["event_page"])
	if len(detail.Events) > boundedDetailEventLimit || section > taskEventSectionMaxBytes || response.Body.Len() > 262_144 {
		h.t.Fatalf("bounded view events=%d section=%d bytes=%d", len(detail.Events), section, response.Body.Len())
	}
	if detail.EventPage.Limit != 100 || detail.EventPage.Returned != len(detail.Events) || detail.EventPage.Kinds != "human" {
		h.t.Fatalf("event_page=%+v", detail.EventPage)
	}
	if detail.EventPage.HasOlder != (detail.EventPage.OlderCursor != "") {
		h.t.Fatalf("event_page has_older=%v cursor=%q", detail.EventPage.HasOlder, detail.EventPage.OlderCursor)
	}
	requireHumanAscending(h.t, detail.Events)
	return detail, response
}

func requireHistoryPageBounds(t *testing.T, page historyPageJSON, size int) {
	t.Helper()
	if len(page.Events) > page.Limit || page.Limit > 50 || size > taskEventSectionMaxBytes+1 {
		t.Fatalf("history page events=%d limit=%d bytes=%d", len(page.Events), page.Limit, size)
	}
	for _, cursor := range []string{page.OlderCursor, page.NewerCursor} {
		if len(cursor) > taskHistoryCursorMaxBytes {
			t.Fatalf("cursor length %d", len(cursor))
		}
	}
	for i, event := range page.Events {
		data, _ := json.Marshal(event)
		if len(data) > taskEventViewMaxBytes {
			t.Fatalf("event %s presented in %d bytes", event.ID, len(data))
		}
		if i > 0 && !historyEventAfter(event, page.Events[i-1]) {
			t.Fatalf("page events out of (at, id) order at %d", i)
		}
	}
}

func historyEventAfter(a, b historyEventJSON) bool {
	if a.At != b.At {
		return a.At > b.At // fixed-width UTC microsecond layout sorts lexically
	}
	aID, _ := strconv.ParseInt(a.ID, 10, 64)
	bID, _ := strconv.ParseInt(b.ID, 10, 64)
	return aID > bID
}

func requireHumanAscending(t *testing.T, events []historyEventJSON) {
	t.Helper()
	for i, event := range events {
		if !slices.Contains(humanTimelineEventKinds, event.Kind) {
			t.Fatalf("non-human kind %q in human selection", event.Kind)
		}
		if i > 0 && !historyEventAfter(event, events[i-1]) {
			t.Fatalf("events out of (at, id) order at %d", i)
		}
	}
}

var historyEpoch = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

var historyHumanKinds = []string{"review.round_completed", "merge.requested", "dispatch.failed", "approval.stale", "task.assignee.set", "merge.confirmed"}

// seedHistoryFixture creates taskID with exactly total events, reconciled of
// them dispatch.reconciled. The first non-reconciled events are an early
// task.created, pull_request.opened, and job.summary that every bounded page
// leaves outside; the rest are human-timeline events, every bigEvery-th one
// with a payload above the 8,192-byte limit.
func seedHistoryFixture(t *testing.T, st store.Store, taskID string, total, reconciled, bigEvery int) []core.Event {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), historyWorkspace)
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: historyWorkspace, State: core.TaskRunning, Title: "History fixture", CreatedAt: historyEpoch}); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: taskID + "-impl", TaskID: taskID, Stage: core.StageImplement, State: core.JobDone, StartedAt: historyEpoch, EndedAt: historyEpoch.Add(time.Minute)}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	existing, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	other := total - reconciled - len(existing)
	if other < 3 {
		t.Fatalf("fixture needs at least three non-reconciled events, has %d", other)
	}
	// Spread the other events evenly through the reconciliation stream.
	stride := max((total-len(existing))/other, 1)
	reconciliation := json.RawMessage(`{"work_order_id":"` + taskID + `-order","state":"queued","reason":"lease reconciliation"}`)
	big := json.RawMessage(`{"review_round":1,"draft":true,"url":"https://example.test/pr/2","body":"` + strings.Repeat("b", 9000) + `"}`)
	human := 0
	for i, placed, reconciledLeft := 0, 0, reconciled; i < total-len(existing); i++ {
		event := core.Event{TaskID: taskID, At: historyEpoch.Add(time.Duration(i+1) * time.Millisecond)}
		switch {
		case placed < other && (i%stride == 0 || reconciledLeft == 0):
			switch placed {
			case 0:
				event.Kind, event.ActorID, event.ActorRole = "task.created", "user:usr_a", core.ActorHuman
			case 1:
				event.Kind, event.Payload = "pull_request.opened", json.RawMessage(`{"url":"https://example.test/pr/1","number":1,"repository":"acme/app","head_sha":"abc","base_sha":"def"}`)
			case 2:
				event.Kind, event.JobID, event.Payload = "job.summary", job.ID, json.RawMessage(`{"summary":"implemented the fixture"}`)
			default:
				human++
				event.Kind = historyHumanKinds[human%len(historyHumanKinds)]
				event.Payload = json.RawMessage(fmt.Sprintf(`{"sequence":%d,"url":"https://example.test/%d"}`, human, human))
				if bigEvery > 0 && human%bigEvery == 0 {
					event.Payload = big
				}
			}
			placed++
		default:
			event.Kind, event.Payload = "dispatch.reconciled", reconciliation
			reconciledLeft--
		}
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == "dispatch.reconciled" {
			count++
		}
	}
	if len(events) != total || count != reconciled {
		t.Fatalf("fixture events=%d reconciled=%d want %d/%d", len(events), count, total, reconciled)
	}
	return events
}

func TestTaskHistoryBoundedViewAndTraversalOverReconciliationFixture(t *testing.T) {
	st := store.NewMemory()
	events := seedHistoryFixture(t, st, "incident-task", 20_219, 20_127, 9)
	h := newHistoryHarness(t, st)

	complete := h.get(historyTokenA, "/v1/tasks/incident-task/activity")
	if complete.Code != http.StatusOK || complete.Header().Get("X-Conveyor-Detail-View") != "" {
		t.Fatalf("complete view: %d", complete.Code)
	}
	var completeFields map[string]json.RawMessage
	if err := json.Unmarshal(complete.Body.Bytes(), &completeFields); err != nil {
		t.Fatal(err)
	}
	var completeEvents []core.Event
	if err := json.Unmarshal(completeFields["events"], &completeEvents); err != nil || len(completeEvents) != len(events) {
		t.Fatalf("complete view events=%d err=%v", len(completeEvents), err)
	}
	if _, ok := completeFields["summaries"]; ok {
		t.Fatal("complete view gained summaries")
	}
	if _, ok := completeFields["event_page"]; ok {
		t.Fatal("complete view gained event_page")
	}

	eventsResponse := h.get(historyTokenA, "/v1/tasks/incident-task/events")
	var listed []core.Event
	if eventsResponse.Code != http.StatusOK || json.Unmarshal(eventsResponse.Body.Bytes(), &listed) != nil || len(listed) != len(events) || !bytes.Contains(eventsResponse.Body.Bytes(), []byte(`"id":20219,`)) {
		t.Fatalf("/events: %d events=%d", eventsResponse.Code, len(listed))
	}

	detail, bounded := h.bounded("incident-task")
	// 87 cycled human events plus the early pull_request.opened; the other
	// non-reconciled events are task.created (twice), job.created, and job.summary.
	if detail.EventPage.Returned != 88 || detail.EventPage.HasOlder {
		t.Fatalf("bounded page over 88 human events: %+v", detail.EventPage)
	}
	omitted := 0
	for _, event := range detail.Events {
		if event.PayloadOmitted {
			omitted++
			if event.Payload != nil && string(event.Payload) != "null" || event.PayloadBytes <= taskEventPayloadLimit || !event.AuditAvailable {
				t.Fatalf("omitted event %+v", event)
			}
			if string(event.PayloadIndex["review_round"]) != "1" || string(event.PayloadIndex["draft"]) != "true" {
				t.Fatalf("payload_index=%v", event.PayloadIndex)
			}
		}
	}
	if omitted == 0 {
		t.Fatal("no human event with an omitted payload in the page")
	}
	var summaries taskDetailSummaries
	if err := json.Unmarshal(detail.Raw["summaries"], &summaries); err != nil {
		t.Fatal(err)
	}
	if summaries.PullRequest == nil || summaries.PullRequest.URL != "https://example.test/pr/1" {
		t.Fatalf("pull request summary=%+v", summaries.PullRequest)
	}
	if result := summaries.JobResults["incident-task-impl"]; result == nil || result.Summary == nil || result.Summary.Text != "implemented the fixture" {
		t.Fatalf("job summary outside the page=%+v", summaries.JobResults)
	}
	creator := events[slices.IndexFunc(events, func(event core.Event) bool { return event.Kind == "task.created" })]
	if summaries.Creator == nil || summaries.Creator.ActorID != creator.ActorID || summaries.Creator.ActorRole != creator.ActorRole {
		t.Fatalf("creator=%+v want the first task.created %+v", summaries.Creator, creator)
	}

	first, firstResponse := h.history(historyTokenA, "incident-task", "")
	t.Logf("20,219-event fixture (20,127 dispatch.reconciled): complete view=%d bytes, bounded view=%d bytes (event section %d bytes, %d events), first technical history page=%d bytes (%d events)",
		complete.Body.Len(), bounded.Body.Len(), len(detail.Raw["events"])+len(detail.Raw["event_page"]), len(detail.Events), firstResponse.Body.Len(), len(first.Events))
	if firstResponse.Code != http.StatusOK || first.Selection != "technical" || first.Limit != 50 || len(first.Events) != 50 || first.HasNewer || !first.HasOlder || first.NewerCursor != "" {
		t.Fatalf("first page: %d %+v", firstResponse.Code, first.Boundary)
	}
	maxID := int64(0)
	for _, event := range events {
		maxID = max(maxID, event.ID)
	}
	if first.Boundary.Count != len(events) || first.Boundary.MaxID != strconv.FormatInt(maxID, 10) {
		t.Fatalf("boundary=%+v", first.Boundary)
	}
	repeat := h.get(historyTokenA, "/v1/tasks/incident-task/history?cursor="+url.QueryEscape(first.OlderCursor))
	again := h.get(historyTokenA, "/v1/tasks/incident-task/history?cursor="+url.QueryEscape(first.OlderCursor))
	if repeat.Code != http.StatusOK || !bytes.Equal(repeat.Body.Bytes(), again.Body.Bytes()) {
		t.Fatalf("repeating a cursor: %d", repeat.Code)
	}

	// Follow every older cursor; appends during traversal stay outside it.
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), historyWorkspace)
	visited := map[string]bool{}
	page, pages := first, 1
	var newestFirstIDs []string
	for {
		for i := len(page.Events) - 1; i >= 0; i-- {
			if visited[page.Events[i].ID] {
				t.Fatalf("event %s visited twice", page.Events[i].ID)
			}
			visited[page.Events[i].ID] = true
			newestFirstIDs = append(newestFirstIDs, page.Events[i].ID)
		}
		if pages%100 == 1 {
			if err := st.AppendEvent(ctx, core.Event{TaskID: "incident-task", Kind: "dispatch.reconciled", At: historyEpoch.Add(time.Hour + time.Duration(pages)*time.Second)}); err != nil {
				t.Fatal(err)
			}
		}
		if !page.HasOlder {
			break
		}
		var response *httptest.ResponseRecorder
		page, response = h.history(historyTokenA, "incident-task", "?selection=technical&cursor="+url.QueryEscape(page.OlderCursor))
		if response.Code != http.StatusOK || !page.HasNewer || page.Boundary.Count != len(events) {
			t.Fatalf("page %d: %d %s", pages, response.Code, response.Body.String())
		}
		pages++
	}
	if len(visited) != len(events) {
		t.Fatalf("technical traversal visited %d of %d events in %d pages", len(visited), len(events), pages)
	}
	for i, event := range events {
		if want := strconv.FormatInt(event.ID, 10); newestFirstIDs[len(events)-1-i] != want {
			t.Fatalf("traversal order differs at %d: %s want %s", i, newestFirstIDs[len(events)-1-i], want)
		}
	}

	// Newer cursors walk back toward the boundary's newest, never past it.
	newer := page
	for steps := 0; steps < 3; steps++ {
		next, response := h.history(historyTokenA, "incident-task", "?cursor="+url.QueryEscape(newer.NewerCursor))
		if response.Code != http.StatusOK || !next.HasOlder || len(next.Events) == 0 || !historyEventAfter(next.Events[0], newer.Events[len(newer.Events)-1]) {
			t.Fatalf("newer page %d: %d", steps, response.Code)
		}
		newer = next
	}
}

func TestTaskHistoryOverHundredThousandEvents(t *testing.T) {
	st := store.NewMemory()
	events := seedHistoryFixture(t, st, "large-task", 100_000, 99_400, 50)
	h := newHistoryHarness(t, st)
	detail, bounded := h.bounded("large-task")
	if len(detail.Events) != 100 || !detail.EventPage.HasOlder {
		t.Fatalf("bounded page=%+v", detail.EventPage)
	}
	older, response := h.history(historyTokenA, "large-task", "?selection=human&cursor="+url.QueryEscape(detail.EventPage.OlderCursor))
	if response.Code != http.StatusOK || len(older.Events) != 50 || older.Limit != 50 || older.Selection != "human" || !older.HasNewer {
		t.Fatalf("initial older_cursor page: %d events=%d", response.Code, len(older.Events))
	}
	requireHumanAscending(t, older.Events)
	if !historyEventAfter(detail.Events[0], older.Events[len(older.Events)-1]) {
		t.Fatal("older human page is not strictly before the bounded page")
	}
	var summaries taskDetailSummaries
	if err := json.Unmarshal(detail.Raw["summaries"], &summaries); err != nil {
		t.Fatal(err)
	}
	// The pull request and job summary events lie far outside the page.
	if summaries.PullRequest == nil || summaries.PullRequest.URL != "https://example.test/pr/1" || summaries.JobResults["large-task-impl"] == nil {
		t.Fatalf("summaries outside the page: %s", detail.Raw["summaries"])
	}
	for _, event := range detail.Events {
		if event.Kind == "pull_request.opened" {
			t.Fatal("the early pull request event is inside the bounded page")
		}
	}
	first, firstResponse := h.history(historyTokenA, "large-task", "")
	t.Logf("100,000-event fixture: bounded view=%d bytes, first technical page=%d bytes, initial older human page=%d bytes", bounded.Body.Len(), firstResponse.Body.Len(), response.Body.Len())
	if len(first.Events) != 50 || first.Boundary.Count != len(events) {
		t.Fatalf("first technical page: %d events, boundary %+v", len(first.Events), first.Boundary)
	}

	// The human selection pages every human event exactly once.
	humanCount := 0
	for _, event := range events {
		if slices.Contains(humanTimelineEventKinds, event.Kind) {
			humanCount++
		}
	}
	seen := map[string]bool{}
	page, _ := h.history(historyTokenA, "large-task", "?selection=human")
	for {
		for _, event := range page.Events {
			if seen[event.ID] {
				t.Fatalf("human event %s repeated", event.ID)
			}
			seen[event.ID] = true
		}
		if !page.HasOlder {
			break
		}
		page, response = h.history(historyTokenA, "large-task", "?selection=human&cursor="+url.QueryEscape(page.OlderCursor))
		if response.Code != http.StatusOK {
			t.Fatalf("human page: %d %s", response.Code, response.Body.String())
		}
	}
	if len(seen) != humanCount || page.Boundary.Count != humanCount {
		t.Fatalf("human traversal saw %d of %d (boundary %d)", len(seen), humanCount, page.Boundary.Count)
	}
}

// ledgerStore replaces the memory event ledger with explicit events, so tests
// can commit any ID in any order: IDs above 2^53 and a delayed lower ID.
type ledgerStore struct {
	store.Store
	mu      sync.Mutex
	ledger  map[string][]core.Event
	queries []store.TaskEventWindowQuery
	readErr error
}

func newLedgerStore(t *testing.T, taskIDs ...string) *ledgerStore {
	t.Helper()
	base := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), historyWorkspace)
	for _, id := range taskIDs {
		if err := base.CreateTask(ctx, core.Task{ID: id, Workspace: historyWorkspace, State: core.TaskRunning, Title: id}); err != nil {
			t.Fatal(err)
		}
	}
	return &ledgerStore{Store: base, ledger: map[string][]core.Event{}}
}

func (l *ledgerStore) commit(events ...core.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, event := range events {
		l.ledger[event.TaskID] = append(l.ledger[event.TaskID], event)
	}
}

func (l *ledgerStore) ListEvents(_ context.Context, taskID string) ([]core.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return sortedTaskEvents(l.ledger[taskID]), nil
}

func (l *ledgerStore) ReadTaskEventWindow(ctx context.Context, q store.TaskEventWindowQuery) (store.TaskEventWindow, error) {
	if err := store.ValidateTaskEventWindowQuery(q); err != nil {
		return store.TaskEventWindow{}, err
	}
	l.mu.Lock()
	l.queries = append(l.queries, q)
	readErr := l.readErr
	l.mu.Unlock()
	if readErr != nil {
		return store.TaskEventWindow{}, readErr
	}
	if _, err := l.Store.GetTask(ctx, q.TaskID); err != nil {
		return store.TaskEventWindow{}, store.ErrNotFound
	}
	events, _ := l.ListEvents(ctx, q.TaskID)
	var boundary store.TaskEventBoundary
	if q.Boundary != nil {
		boundary = *q.Boundary
	} else {
		for _, event := range events {
			boundary.MaxID = max(boundary.MaxID, event.ID)
		}
	}
	count := 0
	var selected []core.Event
	for _, event := range events {
		if event.ID > boundary.MaxID || !q.MatchesKind(event.Kind) {
			continue
		}
		count++
		position := store.TaskEventPosition{At: event.At, ID: event.ID}
		if q.After != nil && !store.TaskEventAfter(event, *q.After) || q.Before != nil && !store.TaskEventAfter(core.Event{At: q.Before.At, ID: q.Before.ID}, position) {
			continue
		}
		selected = append(selected, event)
	}
	if q.Boundary == nil {
		boundary.Count = count
	} else if count != q.Boundary.Count {
		return store.TaskEventWindow{}, store.ErrTaskEventHistoryChanged
	}
	if q.Descending {
		store.ReverseTaskEvents(selected)
	}
	if len(selected) > q.Limit+1 {
		selected = selected[:q.Limit+1]
	}
	return store.BudgetTaskEventWindow(q, boundary, selected)
}

func ledgerEvent(taskID string, id int64, offset time.Duration, kind, payload string) core.Event {
	return core.Event{ID: id, TaskID: taskID, Kind: kind, ActorID: "system:dispatcher", ActorRole: core.ActorSystem, At: historyEpoch.Add(offset), Payload: json.RawMessage(payload)}
}

func TestTaskHistoryRestartsOnDelayedLowerIDCommit(t *testing.T) {
	st := newLedgerStore(t, "delayed-task")
	for i := int64(0); i < 120; i++ {
		st.commit(ledgerEvent("delayed-task", 1000+i*10, time.Duration(i)*time.Second, "merge.requested", `{}`))
	}
	h := newHistoryHarness(t, st)
	for round, selection := range []string{"technical", "human"} {
		first, response := h.history(historyTokenA, "delayed-task", "?selection="+selection+"&limit=20")
		if response.Code != http.StatusOK || first.OlderCursor == "" {
			t.Fatalf("%s first page: %d", selection, response.Code)
		}
		detail, _ := h.bounded("delayed-task")
		// A higher ID above the captured ceiling stays outside the traversal.
		st.commit(ledgerEvent("delayed-task", 999_999+int64(round), time.Hour, "merge.requested", `{}`))
		if _, response := h.history(historyTokenA, "delayed-task", "?cursor="+url.QueryEscape(first.OlderCursor)); response.Code != http.StatusOK {
			t.Fatalf("append above ceiling refused the traversal: %d", response.Code)
		}
		// A lower ID under the ceiling becomes visible after capture.
		st.commit(ledgerEvent("delayed-task", 1001+2*int64(round), 30*time.Second, "merge.requested", `{}`))
		for _, cursor := range []string{first.OlderCursor, detail.EventPage.OlderCursor} {
			response := h.get(historyTokenA, "/v1/tasks/delayed-task/history?cursor="+url.QueryEscape(cursor))
			var body map[string]string
			_ = json.Unmarshal(response.Body.Bytes(), &body)
			if response.Code != http.StatusConflict || body["error"] != "history_restart" || body["message"] == "" {
				t.Fatalf("%s delayed lower-ID commit: %d %s", selection, response.Code, response.Body.String())
			}
		}
	}
}

func TestTaskHistoryCursorRefusals(t *testing.T) {
	st := newLedgerStore(t, "cursor-task", "sibling-task")
	for _, taskID := range []string{"cursor-task", "sibling-task"} {
		for i := int64(0); i < 30; i++ {
			st.commit(ledgerEvent(taskID, 100+i, time.Duration(i)*time.Second, "merge.requested", `{}`))
		}
	}
	clock := historyEpoch
	h := newHistoryHarness(t, st)
	codec, err := newTaskHistoryCursorCodec()
	if err != nil {
		t.Fatal(err)
	}
	codec.now = func() time.Time { return clock }
	h.srv.historyCursors = codec
	first, _ := h.history(historyTokenA, "cursor-task", "?selection=technical&limit=10")
	human, _ := h.history(historyTokenA, "cursor-task", "?selection=human&limit=10")
	cursor := first.OlderCursor
	refused := func(name, token, taskID, query string, status int, code string) {
		t.Helper()
		response := h.get(token, "/v1/tasks/"+taskID+"/history"+query)
		var body map[string]string
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		if response.Code != status || body["error"] != code {
			t.Fatalf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}
	escape := url.QueryEscape
	body, tag, _ := strings.Cut(cursor, ".")
	decoded, _ := base64.RawURLEncoding.DecodeString(body)
	tampered := base64.RawURLEncoding.EncodeToString(bytes.Replace(decoded, []byte(`"l":10`), []byte(`"l":11`), 1)) + "." + tag
	for name, malformed := range map[string]string{
		"garbage": "!!!", "no tag": body, "empty tag": body + ".", "not json": base64.RawURLEncoding.EncodeToString([]byte("nope")) + "." + tag,
		"tampered": tampered, "too long": strings.Repeat("a", 1025),
	} {
		refused(name, historyTokenA, "cursor-task", "?cursor="+escape(malformed), http.StatusBadRequest, "history_cursor_invalid")
	}
	refused("foreign credential", historyTokenA2, "cursor-task", "?cursor="+escape(cursor), http.StatusBadRequest, "history_cursor_invalid")
	refused("foreign task", historyTokenA, "sibling-task", "?cursor="+escape(cursor), http.StatusBadRequest, "history_cursor_invalid")
	refused("foreign selection", historyTokenA, "cursor-task", "?selection=human&cursor="+escape(cursor), http.StatusBadRequest, "history_cursor_invalid")
	refused("foreign selection reversed", historyTokenA, "cursor-task", "?selection=technical&cursor="+escape(human.OlderCursor), http.StatusBadRequest, "history_cursor_invalid")
	foreignWorkspace, err := codec.issue(taskHistoryBinding{workspace: "other", taskID: "cursor-task", credential: "pat_a"}, "technical",
		store.TaskEventBoundary{MaxID: 129, Count: 30}, store.TaskEventPosition{At: historyEpoch, ID: 120}, taskHistoryOlder, 10, clock.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	refused("foreign workspace", historyTokenA, "cursor-task", "?cursor="+escape(foreignWorkspace), http.StatusBadRequest, "history_cursor_invalid")
	refused("unknown selection", historyTokenA, "cursor-task", "?selection=everything", http.StatusBadRequest, "invalid_history_request")
	for _, limit := range []string{"0", "51", "ten", "-1"} {
		refused("limit "+limit, historyTokenA, "cursor-task", "?limit="+limit, http.StatusBadRequest, "invalid_history_request")
	}
	refused("limit differs from cursor", historyTokenA, "cursor-task", "?limit=50&cursor="+escape(cursor), http.StatusBadRequest, "invalid_history_request")
	if _, response := h.history(historyTokenA, "cursor-task", "?limit=10&cursor="+escape(cursor)); response.Code != http.StatusOK {
		t.Fatalf("matching limit: %d", response.Code)
	}
	if page, response := h.history(historyTokenA, "cursor-task", "?cursor="+escape(human.OlderCursor)); response.Code != http.StatusOK || page.Selection != "human" {
		t.Fatalf("cursor without selection adopts its own: %d %+v", response.Code, page.Selection)
	}

	// Expiry is 15 minutes after the first page, carried by every cursor.
	clock = historyEpoch.Add(taskHistoryCursorTTL - time.Microsecond)
	next, response := h.history(historyTokenA, "cursor-task", "?cursor="+escape(cursor))
	if response.Code != http.StatusOK {
		t.Fatalf("cursor before expiry: %d", response.Code)
	}
	clock = historyEpoch.Add(taskHistoryCursorTTL)
	refused("expired", historyTokenA, "cursor-task", "?cursor="+escape(cursor), http.StatusConflict, "history_restart")
	refused("expired successor", historyTokenA, "cursor-task", "?cursor="+escape(next.OlderCursor), http.StatusConflict, "history_restart")
	clock = historyEpoch

	// Another process or replica draws another key ID.
	restarted := newHistoryHarness(t, st)
	refusedBy := func(harness *historyHarness, token string) (int, string) {
		response := harness.get(token, "/v1/tasks/cursor-task/history?cursor="+escape(cursor))
		var body map[string]string
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		return response.Code, body["error"]
	}
	if status, code := refusedBy(restarted, historyTokenA); status != http.StatusConflict || code != "history_restart" {
		t.Fatalf("restarted process: %d %s", status, code)
	}

	// Revocation refuses the next page before any cursor is decoded.
	delete(h.members.roles["usr_a"], historyWorkspace)
	if status, _ := refusedBy(h, historyTokenA); status != http.StatusNotFound {
		t.Fatalf("revoked membership: %d", status)
	}
	if response := h.get(historyTokenA, "/v1/tasks/cursor-task/activity?view=bounded"); response.Code != http.StatusNotFound {
		t.Fatalf("revoked bounded view: %d", response.Code)
	}
}

func TestTaskHistoryForeignTaskAndViewValidation(t *testing.T) {
	st := newLedgerStore(t, "home-task")
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "other")
	if err := st.Store.CreateTask(ctx, core.Task{ID: "away-task", Workspace: "other", State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	h := newHistoryHarness(t, st)
	if _, response := h.history(historyTokenA, "away-task", ""); response.Code != http.StatusNotFound {
		t.Fatalf("task outside the resolved workspace: %d", response.Code)
	}
	if _, response := h.history(historyTokenA, "missing-task", ""); response.Code != http.StatusNotFound {
		t.Fatalf("missing task: %d", response.Code)
	}
	page, response := h.history(historyTokenA, "home-task", "")
	if response.Code != http.StatusOK || len(page.Events) != 0 || page.HasOlder || page.HasNewer || page.Boundary.MaxID != "0" || page.Boundary.Count != 0 {
		t.Fatalf("empty history: %d %s", response.Code, response.Body.String())
	}
	for _, view := range []string{"complete", "", "BOUNDED"} {
		if response := h.get(historyTokenA, "/v1/tasks/home-task/activity?view="+view); response.Code != http.StatusBadRequest {
			t.Fatalf("view=%q: %d", view, response.Code)
		}
	}
	st.readErr = fmt.Errorf("database unavailable")
	if _, response := h.history(historyTokenA, "home-task", ""); response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "database") {
		t.Fatalf("store failure: %d %s", response.Code, response.Body.String())
	}
}

func TestTaskHistoryExactLargeIDsPayloadOmissionAndAuditRecovery(t *testing.T) {
	st := newLedgerStore(t, "wide-task")
	const above = int64(1)<<53 + 1 // not representable as a float64
	big := `{"review_round":3,"approved":false,"reviews":[{"review_work_order_id":"order-a"},{"review_work_order_id":"order-b"}],"summary":"` + strings.Repeat("s", 9000) + `"}`
	st.commit(
		ledgerEvent("wide-task", above+4, 0, "review.round_completed", big),
		ledgerEvent("wide-task", above+2, time.Second, "merge.requested", `{"url":"https://example.test/pr/3"}`),
		// A lower ID committed with the same instant sorts first.
		ledgerEvent("wide-task", above, time.Second, "merge.confirmed", `{"governance_snapshot":{"notes":["x"]},"url":"u"}`),
	)
	h := newHistoryHarness(t, st)
	page, response := h.history(historyTokenA, "wide-task", "?limit=2")
	if response.Code != http.StatusOK || len(page.Events) != 2 || page.Events[0].ID != strconv.FormatInt(above, 10) || page.Events[1].ID != strconv.FormatInt(above+2, 10) {
		t.Fatalf("large-ID page: %d %s", response.Code, response.Body.String())
	}
	if page.Boundary.MaxID != strconv.FormatInt(above+4, 10) || !strings.Contains(response.Body.String(), `"max_id":"9007199254740997"`) {
		t.Fatalf("boundary max_id=%s", page.Boundary.MaxID)
	}
	if !page.Events[0].AuditAvailable || strings.Contains(string(page.Events[0].Payload), "governance_snapshot") {
		t.Fatalf("snapshot not stripped: %+v", page.Events[0])
	}
	older, response := h.history(historyTokenA, "wide-task", "?cursor="+url.QueryEscape(page.OlderCursor))
	if response.Code != http.StatusOK || len(older.Events) != 1 || older.HasOlder || !older.HasNewer {
		t.Fatalf("older page: %d %s", response.Code, response.Body.String())
	}
	omitted := older.Events[0]
	if omitted.ID != "9007199254740997" || !omitted.PayloadOmitted || omitted.PayloadBytes != len(big) || !omitted.AuditAvailable || string(omitted.Payload) != "null" {
		t.Fatalf("omitted event: %+v", omitted)
	}
	if string(omitted.PayloadIndex["review_round"]) != "3" || string(omitted.PayloadIndex["approved"]) != "false" || string(omitted.PayloadIndex["review_work_order_ids"]) != `["order-a","order-b"]` || omitted.PayloadIndexTruncated {
		t.Fatalf("payload_index=%v truncated=%v", omitted.PayloadIndex, omitted.PayloadIndexTruncated)
	}
	audit := h.get(historyTokenA, "/v1/tasks/wide-task/audit/event/"+omitted.ID)
	var recovered struct {
		Event struct {
			Payload json.RawMessage `json:"payload"`
		} `json:"event"`
	}
	if audit.Code != http.StatusOK || json.Unmarshal(audit.Body.Bytes(), &recovered) != nil || string(recovered.Event.Payload) != big {
		t.Fatalf("audit recovery: %d", audit.Code)
	}
	newer, response := h.history(historyTokenA, "wide-task", "?cursor="+url.QueryEscape(older.NewerCursor))
	if response.Code != http.StatusOK || len(newer.Events) != 2 || newer.Events[0].ID != strconv.FormatInt(above, 10) || newer.HasNewer {
		t.Fatalf("newer page: %d %s", response.Code, response.Body.String())
	}
	detail, _ := h.bounded("wide-task")
	if len(detail.Events) != 3 || detail.Events[2].ID != "9007199254740995" || !detail.Events[0].PayloadOmitted || detail.Events[0].PayloadIndex["review_round"] == nil {
		t.Fatalf("bounded large-ID view: %+v", detail.Events)
	}
}

func TestTaskEventPayloadIndexBoundsAndMinimalForm(t *testing.T) {
	fields := map[string]any{"nested": map[string]any{"n": 1}, "text": "x", "list": []any{1}, "blob": strings.Repeat("p", 9000)}
	for i := 0; i < 20; i++ {
		fields[fmt.Sprintf("n%02d", i)] = i
	}
	fields["flag"] = true
	reviews := []any{map[string]any{"review_work_order_id": strings.Repeat("r", 129)}}
	for i := 0; i < 70; i++ {
		reviews = append(reviews, map[string]any{"review_work_order_id": fmt.Sprintf("order-%02d", i)})
	}
	fields["reviews"] = reviews
	payload, _ := json.Marshal(fields)
	event := core.Event{ID: 7, TaskID: "t", Kind: "review.round_completed", At: historyEpoch, Payload: payload}
	data, err := renderTaskEventView(event, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var view historyEventJSON
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatal(err)
	}
	var ids []string
	_ = json.Unmarshal(view.PayloadIndex["review_work_order_ids"], &ids)
	if !view.PayloadOmitted || !view.PayloadIndexTruncated || len(view.PayloadIndex) != 17 || len(ids) != 64 || ids[0] != "order-00" {
		t.Fatalf("index fields=%d ids=%d truncated=%v", len(view.PayloadIndex), len(ids), view.PayloadIndexTruncated)
	}
	keys := make([]string, 0, len(view.PayloadIndex))
	for key := range view.PayloadIndex {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if keys[0] != "flag" || keys[15] != "n14" || view.PayloadIndex["n15"] != nil || view.PayloadIndex["text"] != nil {
		t.Fatalf("index keys=%v", keys)
	}

	// Escaping can grow a payload under the stored limit past the event bound.
	escaped := core.Event{ID: 8, TaskID: "t", Kind: "merge.failed", At: historyEpoch, Payload: json.RawMessage(`{"count":2,"error":"` + strings.Repeat("<", 8000) + `"}`)}
	data, _ = renderTaskEventView(escaped, 0, nil)
	if err := json.Unmarshal(data, &view); err != nil || !view.PayloadOmitted || view.PayloadBytes != len(escaped.Payload) || string(view.PayloadIndex["count"]) != "2" || len(data) > taskEventViewMaxBytes {
		t.Fatalf("escaped payload view=%s", data)
	}

	// Oversized other fields fall back to the minimal form, and traversal
	// progresses past it.
	st := newLedgerStore(t, "minimal-task")
	for i := int64(1); i <= 12; i++ {
		event := ledgerEvent("minimal-task", i, time.Duration(i)*time.Second, "merge.requested", `{}`)
		if i == 6 {
			event.ActorID = strings.Repeat("a", 20_000)
		}
		st.commit(event)
	}
	h := newHistoryHarness(t, st)
	seen := 0
	page, _ := h.history(historyTokenA, "minimal-task", "?limit=5")
	for {
		for _, event := range page.Events {
			seen++
			if event.ID == "6" {
				raw, _ := json.Marshal(event)
				if event.ActorID != "" || event.TaskID != "" || !event.PayloadOmitted || !event.AuditAvailable || len(raw) > taskEventViewMaxBytes {
					t.Fatalf("minimal form=%+v", event)
				}
			}
		}
		if !page.HasOlder {
			break
		}
		page, _ = h.history(historyTokenA, "minimal-task", "?cursor="+url.QueryEscape(page.OlderCursor))
	}
	if seen != 12 {
		t.Fatalf("traversal past the minimal event saw %d of 12", seen)
	}
}

func TestTaskHistoryPageDropsFarthestEventsToFitAndContinues(t *testing.T) {
	st := newLedgerStore(t, "dense-task")
	payload := `{"detail":"` + strings.Repeat("d", 6000) + `"}` // under 8,192 bytes: kept inline
	for i := int64(1); i <= 60; i++ {
		st.commit(ledgerEvent("dense-task", i, time.Duration(i)*time.Second, "dispatch.failed", payload))
	}
	h := newHistoryHarness(t, st)
	page, response := h.history(historyTokenA, "dense-task", "")
	if response.Code != http.StatusOK || len(page.Events) >= 50 || len(page.Events) == 0 || !page.HasOlder || page.Events[len(page.Events)-1].ID != "60" {
		t.Fatalf("dense first page: %d events=%d bytes=%d", response.Code, len(page.Events), response.Body.Len())
	}
	seen := map[string]bool{}
	var pages []historyPageJSON
	for {
		pages = append(pages, page)
		for _, event := range page.Events {
			if seen[event.ID] {
				t.Fatalf("repeated %s", event.ID)
			}
			seen[event.ID] = true
		}
		if !page.HasOlder {
			break
		}
		page, _ = h.history(historyTokenA, "dense-task", "?cursor="+url.QueryEscape(page.OlderCursor))
	}
	if len(seen) != 60 || len(pages) < 3 {
		t.Fatalf("dense traversal saw %d of 60 in %d pages", len(seen), len(pages))
	}
	// An ascending page keeps the events nearest its cursor and drops its
	// newest, continuing after the last included event.
	oldest := pages[len(pages)-1]
	newer, response := h.history(historyTokenA, "dense-task", "?cursor="+url.QueryEscape(oldest.NewerCursor))
	if response.Code != http.StatusOK || len(newer.Events) == 0 || len(newer.Events) >= 50 || !newer.HasNewer || !newer.HasOlder ||
		newer.Events[0].ID != strconv.Itoa(mustAtoi(t, oldest.Events[len(oldest.Events)-1].ID)+1) {
		t.Fatalf("dense newer page: %d events=%d", response.Code, len(newer.Events))
	}
	following, _ := h.history(historyTokenA, "dense-task", "?cursor="+url.QueryEscape(newer.NewerCursor))
	if len(following.Events) == 0 || following.Events[0].ID != strconv.Itoa(mustAtoi(t, newer.Events[len(newer.Events)-1].ID)+1) {
		t.Fatal("ascending continuation skipped or repeated an event")
	}
	detail, _ := h.bounded("dense-task")
	if detail.EventPage.Returned >= 60 || !detail.EventPage.HasOlder || detail.Events[len(detail.Events)-1].ID != "60" {
		t.Fatalf("dense bounded page=%+v", detail.EventPage)
	}
	older, response := h.history(historyTokenA, "dense-task", "?selection=human&cursor="+url.QueryEscape(detail.EventPage.OlderCursor))
	if response.Code != http.StatusOK || len(older.Events) == 0 {
		t.Fatalf("dense bounded older page: %d", response.Code)
	}
	back, response := h.history(historyTokenA, "dense-task", "?selection=human&cursor="+url.QueryEscape(older.NewerCursor))
	if response.Code != http.StatusOK || len(back.Events) == 0 || back.Events[0].ID != strconv.Itoa(mustAtoi(t, older.Events[len(older.Events)-1].ID)+1) {
		t.Fatalf("dense human newer page: %d events=%d", response.Code, len(back.Events))
	}
}

func mustAtoi(t *testing.T, text string) int {
	t.Helper()
	value, err := strconv.Atoi(text)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// TestBoundedTaskActivityLoadsOneHistory keeps every derivation on the one
// complete ListEvents read and adds only the presentation window read.
func TestBoundedTaskActivityLoadsOneHistory(t *testing.T) {
	base := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), historyWorkspace)
	if err := base.CreateTask(ctx, core.Task{ID: "one-read", Workspace: historyWorkspace, State: core.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	if err := base.AppendEvent(ctx, core.Event{TaskID: "one-read", Kind: "merge.requested"}); err != nil {
		t.Fatal(err)
	}
	observed := &observedStore{Store: base}
	h := newHistoryHarness(t, observed)
	if _, response := h.bounded("one-read"); response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	if observed.listEventsCalls != 1 || observed.listJobsCalls != 1 {
		t.Fatalf("bounded detail calls events:%d jobs:%d", observed.listEventsCalls, observed.listJobsCalls)
	}
}
