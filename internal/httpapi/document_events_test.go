package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestDocumentEventHTTP(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	req, design, _ := storetest.SeedDocumentEventMeasurement(t, st, ctx)
	guard := &boundedDocumentDetailStore{Store: st, t: t}
	server := NewServer(guard)
	server.Workspace = "demo"
	server.BearerToken = "token"
	handler := server.Handler()
	call := func(path string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if auth {
			r.Header.Set("Authorization", "Bearer token")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, collection := range []string{"requirements", "system-designs"} {
		t.Run(collection, func(t *testing.T) {
			id, kind := req, core.LineageRequirement
			if collection == "system-designs" {
				id, kind = design, core.LineageSystemDesign
			}
			path := "/v1/" + collection + "/" + id
			guard.eventPages = 0
			detail := call(path, true)
			if detail.Code != 200 {
				t.Fatal(detail.Body.String())
			}
			// The detail carries no document events: the History view reads
			// them from the events route alone (component-web-document-surfaces).
			if guard.eventPages != 0 {
				t.Fatalf("detail read %d document event pages", guard.eventPages)
			}
			assertDetailHasNoDocumentEvents(t, detail.Body.Bytes())
			assertDetailFields(t, collection, detail.Body.Bytes())
			var view struct {
				Blueprints []map[string]any `json:"serving_blueprints"`
			}
			if err := json.Unmarshal(detail.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(detail.Body.String(), `"lineage_graph"`) {
				t.Fatal("detail carries graph")
			}
			for _, blueprint := range view.Blueprints {
				if _, ok := blueprint["events"]; ok {
					t.Fatal("blueprint carries events")
				}
			}
			if collection == "requirements" && detail.Body.Len() >= 200000 {
				t.Fatalf("detail exceeds size target: %d", detail.Body.Len())
			}
			// The first events page takes no snapshot, defaults to 50 rows,
			// and pins the snapshot that every later page passes.
			firstResponse := call(path+"/events", true)
			if firstResponse.Code != 200 {
				t.Fatal(firstResponse.Body.String())
			}
			var first store.DocumentEventPage
			if err := json.Unmarshal(firstResponse.Body.Bytes(), &first); err != nil {
				t.Fatal(err)
			}
			if len(first.Events) != 50 || first.Total < 600 || first.SnapshotID == 0 || first.Limit != 50 || first.Offset != 0 {
				t.Fatalf("unbounded first page: events=%d total=%d snapshot=%d limit=%d offset=%d", len(first.Events), first.Total, first.SnapshotID, first.Limit, first.Offset)
			}
			if firstResponse.Header().Get("X-Conveyor-Total") != strconv.Itoa(first.Total) || firstResponse.Header().Get("X-Conveyor-Limit") != "50" || firstResponse.Header().Get("X-Conveyor-Offset") != "0" {
				t.Fatalf("first page headers=%v", firstResponse.Header())
			}
			all := append([]core.Event{}, first.Events...)
			for offset := 50; offset < first.Total; offset += 200 {
				response := call(fmt.Sprintf("%s/events?limit=200&offset=%d&snapshot_id=%d", path, offset, first.SnapshotID), true)
				if response.Code != 200 {
					t.Fatal(response.Body.String())
				}
				var page store.DocumentEventPage
				if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				if page.Total != first.Total || page.SnapshotID != first.SnapshotID || page.Limit != 200 || page.Offset != offset || response.Header().Get("X-Conveyor-Total") != strconv.Itoa(first.Total) || response.Header().Get("X-Conveyor-Limit") != "200" || response.Header().Get("X-Conveyor-Offset") != strconv.Itoa(offset) {
					t.Fatalf("page metadata=%+v headers=%v", page, response.Header())
				}
				all = append(all, page.Events...)
			}
			expected := []core.Event{}
			for offset := 0; offset < first.Total; offset += 200 {
				page, err := st.ListDocumentEventPage(ctx, kind, id, store.DocumentEventQuery{Limit: 200, Offset: offset, SnapshotID: first.SnapshotID})
				if err != nil {
					t.Fatal(err)
				}
				expected = append(expected, page.Events...)
			}
			if kind == core.LineageRequirement {
				expected = annotateBackfilledEvents(expected)
			}
			if !reflect.DeepEqual(all, expected) {
				t.Fatal("event pages differ from store history")
			}
			empty := call(path+"/events?limit=50&offset=9999", true)
			if empty.Code != 200 || !strings.Contains(empty.Body.String(), `"events":[]`) {
				t.Fatalf("empty page: %s", empty.Body.String())
			}
			for _, query := range []string{"limit=0", "limit=201", "limit=1&limit=2", "limit=1&offset=-1", "limit=1&offset=2147483648", "offset=1", "snapshot_id=-1", "snapshot_id=x", "snapshot_id=1&snapshot_id=2"} {
				if got := call(path+"/events?"+query, true); got.Code != 400 {
					t.Fatalf("%s: %d %s", query, got.Code, got.Body.String())
				}
			}
			if got := call(path+"/events", true); got.Code != 200 {
				t.Fatal(got.Body.String())
			}
			if got := call(path+"/events", false); got.Code != http.StatusUnauthorized {
				t.Fatalf("unauthorized=%d", got.Code)
			}
			if got := call("/v1/"+collection+"/missing/events", true); got.Code != http.StatusNotFound {
				t.Fatalf("missing=%d", got.Code)
			}
			if got := call(path+"/events?workspace_id=foreign", true); got.Code == 200 {
				t.Fatal("foreign workspace was accepted")
			}
		})
	}
}

// Detail must never fetch a task's full activity or all System Design events.
// It counts document event pages so a test can prove the detail reads none.
type boundedDocumentDetailStore struct {
	store.Store
	t          *testing.T
	eventPages int
}

func (s *boundedDocumentDetailStore) ListDocumentEventPage(ctx context.Context, kind core.LineageNodeType, id string, query store.DocumentEventQuery) (store.DocumentEventPage, error) {
	s.eventPages++
	return s.Store.ListDocumentEventPage(ctx, kind, id, query)
}

// assertDetailFields proves the detail keeps every other top-level field of
// its contract after the document event keys left it. current_version is
// omitted while a document has no confirmed version.
func assertDetailFields(t *testing.T, collection string, body []byte) {
	t.Helper()
	want := []string{"document", "pending_versions", "versions", "drift"}
	if collection == "requirements" {
		want = []string{"requirement", "pending_versions", "serving_blueprints", "serving_tasks", "requirement_links", "planning_sessions", "artifacts", "staleness", "migrated_seed", "confirmation_eligible"}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(fields))
	for key := range fields {
		if key != "current_version" {
			got = append(got, key)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s detail fields=%v want=%v (plus optional current_version)", collection, got, want)
	}
}

// assertDetailHasNoDocumentEvents proves a requirement or System Design
// detail body carries none of the removed document event keys.
func assertDetailHasNoDocumentEvents(t *testing.T, body []byte) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"lineage", "lineage_total", "lineage_snapshot_id"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("detail carries %q", key)
		}
	}
}

func (s *boundedDocumentDetailStore) ListEvents(context.Context, string) ([]core.Event, error) {
	s.t.Fatal("detail read full task events")
	return nil, nil
}
func (s *boundedDocumentDetailStore) ListCompactEvents(context.Context, string) ([]core.Event, error) {
	s.t.Fatal("detail read compact task events")
	return nil, nil
}
func (s *boundedDocumentDetailStore) ListSystemDesignEvents(context.Context, string) ([]core.Event, error) {
	s.t.Fatal("detail read full System Design events")
	return nil, nil
}

func TestDocumentEventHTTPMixedMembership(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	fixture := storetest.SeedDocumentEventMembership(t, st, ctx)
	guard := &boundedDocumentDetailStore{Store: st, t: t}
	server := NewServer(guard)
	server.Workspace, server.BearerToken = "demo", "token"
	handler := server.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
		return response
	}
	for _, kind := range []core.LineageNodeType{core.LineageRequirement, core.LineageSystemDesign} {
		path := "/v1/requirements/" + fixture.Requirement
		if kind == core.LineageSystemDesign {
			path = "/v1/system-designs/" + fixture.Design
		}
		guard.eventPages = 0
		assertDetailHasNoDocumentEvents(t, get(path).Body.Bytes())
		if guard.eventPages != 0 {
			t.Fatalf("%s detail read %d document event pages", kind, guard.eventPages)
		}
		var page store.DocumentEventPage
		if err := json.Unmarshal(get(path+"/events").Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		expected := fixture.Expected[kind]
		var snapshot int64
		for _, event := range expected {
			snapshot = max(snapshot, event.ID)
		}
		if kind == core.LineageRequirement {
			expected = annotateBackfilledEvents(expected)
		}
		// Round-trip the expected events so time locations and JSON encoding
		// match the wire representation, without consulting the page method.
		encoded, err := json.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &expected); err != nil {
			t.Fatal(err)
		}
		if page.Total != len(expected) || page.SnapshotID != snapshot || !reflect.DeepEqual(page.Events, expected) {
			t.Fatalf("%s event page contract differs: total=%d want=%d snapshot=%d want=%d\ngot=%+v\nwant=%+v", kind, page.Total, len(expected), page.SnapshotID, snapshot, page.Events, expected)
		}
	}
}
