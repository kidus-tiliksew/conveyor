package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	server := NewServer(&boundedDocumentDetailStore{Store: st, t: t})
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
			detail := call(path, true)
			if detail.Code != 200 {
				t.Fatal(detail.Body.String())
			}
			var view struct {
				Lineage    []core.Event     `json:"lineage"`
				Total      int              `json:"lineage_total"`
				SnapshotID int64            `json:"lineage_snapshot_id"`
				Blueprints []map[string]any `json:"serving_blueprints"`
			}
			if err := json.Unmarshal(detail.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if len(view.Lineage) != 50 || view.Total < 600 || view.SnapshotID == 0 {
				t.Fatalf("unbounded detail: events=%d total=%d snapshot=%d", len(view.Lineage), view.Total, view.SnapshotID)
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
			all := append([]core.Event{}, view.Lineage...)
			for offset := 50; offset < view.Total; offset += 200 {
				response := call(fmt.Sprintf("%s/events?limit=200&offset=%d&snapshot_id=%d", path, offset, view.SnapshotID), true)
				if response.Code != 200 {
					t.Fatal(response.Body.String())
				}
				var page store.DocumentEventPage
				if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				if page.Total != view.Total || page.Limit != 200 || page.Offset != offset || response.Header().Get("X-Conveyor-Total") != strconv.Itoa(view.Total) || response.Header().Get("X-Conveyor-Limit") != "200" || response.Header().Get("X-Conveyor-Offset") != strconv.Itoa(offset) {
					t.Fatalf("page metadata=%+v headers=%v", page, response.Header())
				}
				all = append(all, page.Events...)
			}
			expected := []core.Event{}
			for offset := 0; offset < view.Total; offset += 200 {
				page, err := st.ListDocumentEventPage(ctx, kind, id, store.DocumentEventQuery{Limit: 200, Offset: offset, SnapshotID: view.SnapshotID})
				if err != nil {
					t.Fatal(err)
				}
				expected = append(expected, page.Events...)
			}
			if kind == core.LineageRequirement {
				expected = annotateBackfilledEvents(expected)
			}
			if !reflect.DeepEqual(all, expected) {
				t.Fatal("detail and pages differ from store history")
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
type boundedDocumentDetailStore struct {
	store.Store
	t *testing.T
}

func (s *boundedDocumentDetailStore) ListEvents(context.Context, string) ([]core.Event, error) {
	s.t.Fatal("detail read full task events")
	return nil, nil
}
func (s *boundedDocumentDetailStore) ListSystemDesignEvents(context.Context, string) ([]core.Event, error) {
	s.t.Fatal("detail read full System Design events")
	return nil, nil
}
