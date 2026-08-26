package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestSystemDesignViewSerializesDriftDetectedAt(t *testing.T) {
	detectedAt := time.Date(2026, 8, 26, 7, 0, 0, 0, time.UTC)
	view := buildSystemDesignView(core.SystemDesign{ID: "design-monitor"}, nil, nil, []monitor.Drift{{
		ID: "drift-1", SystemDesignID: "design-monitor", DetectedAt: detectedAt,
	}})
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Drift []map[string]json.RawMessage `json:"drift"`
	}
	if err = json.Unmarshal(raw, &response); err != nil || len(response.Drift) != 1 || len(response.Drift[0]["detected_at"]) == 0 {
		t.Fatalf("system design drift detected_at missing: json=%s err=%v", raw, err)
	}
}

type countingSystemDesignStore struct {
	store.Store
	lists, versionBatches, eventBatches, driftCounts, singularVersions, singularEvents int
}

func (s *countingSystemDesignStore) ListSystemDesigns(ctx context.Context) ([]core.SystemDesign, error) {
	s.lists++
	return s.Store.ListSystemDesigns(ctx)
}
func (s *countingSystemDesignStore) ListSystemDesignVersionsByDocument(ctx context.Context) (map[string][]core.SystemDesignVersion, error) {
	s.versionBatches++
	return s.Store.ListSystemDesignVersionsByDocument(ctx)
}
func (s *countingSystemDesignStore) ListSystemDesignEventsByDocument(ctx context.Context) (map[string][]core.Event, error) {
	s.eventBatches++
	return s.Store.ListSystemDesignEventsByDocument(ctx)
}
func (s *countingSystemDesignStore) ListActiveSystemDesignDriftCounts(ctx context.Context) (map[string]int, error) {
	s.driftCounts++
	return s.Store.ListActiveSystemDesignDriftCounts(ctx)
}
func (s *countingSystemDesignStore) ListSystemDesignVersions(ctx context.Context, id string) ([]core.SystemDesignVersion, error) {
	s.singularVersions++
	return s.Store.ListSystemDesignVersions(ctx, id)
}
func (s *countingSystemDesignStore) ListSystemDesignEvents(ctx context.Context, id string) ([]core.Event, error) {
	s.singularEvents++
	return s.Store.ListSystemDesignEvents(ctx, id)
}

func TestListSystemDesignsUsesBoundedStoreRounds(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	base := store.NewMemory()
	for _, id := range []string{"design-a", "design-b", "design-c"} {
		document, version, err := base.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: id, Category: "Architecture"}, core.SystemDesignVersion{
			Content: "# " + id + "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = base.ConfirmSystemDesignVersion(ctx, document.ID, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	counting := &countingSystemDesignStore{Store: base}
	server := NewServer(counting)
	server.Workspace = "demo"
	server.Monitor = &monitor.Service{Store: base.(monitor.Store), WorkspaceID: "demo", Enabled: true}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/system-designs", nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if counting.lists != 1 || counting.versionBatches != 1 || counting.eventBatches != 0 || counting.driftCounts != 1 || counting.singularVersions != 0 || counting.singularEvents != 0 {
		t.Fatalf("store rounds lists=%d version_batches=%d event_batches=%d drift_counts=%d singular_versions=%d singular_events=%d", counting.lists, counting.versionBatches, counting.eventBatches, counting.driftCounts, counting.singularVersions, counting.singularEvents)
	}
}

func TestSystemDesignAndDecisionHTTPConfirmationContracts(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	document, first, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-dispatch", Title: "Dispatch", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Dispatch\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	sibling := store.WithWorkspace(t.Context(), "sibling")
	if _, _, err = st.CreateSystemDesign(sibling, core.SystemDesign{ID: document.ID, Title: "Sibling", Category: "Operations"}, core.SystemDesignVersion{
		Content: "# Sibling\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - cmd/**\n```", Origin: core.SystemDesignOriginOperator,
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	handler := server.Handler()

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/system-designs", nil)))
	var views []systemDesignSummary
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &views) != nil || len(views) != 1 || views[0].Document.Title != "Dispatch" || len(views[0].PendingVersions) != 1 {
		t.Fatalf("list status=%d views=%+v body=%s", list.Code, views, list.Body.String())
	}
	if strings.Contains(list.Body.String(), "# Dispatch") || strings.Contains(list.Body.String(), `"lineage"`) || strings.Contains(list.Body.String(), `"versions"`) {
		t.Fatalf("collection leaked detail payload: %s", list.Body.String())
	}
	detail := httptest.NewRecorder()
	handler.ServeHTTP(detail, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/system-designs/design-dispatch", nil)))
	var detailView systemDesignView
	if detail.Code != http.StatusOK || json.Unmarshal(detail.Body.Bytes(), &detailView) != nil || len(detailView.Lineage) != 2 || detailView.PendingVersions[0].Content == "" {
		t.Fatalf("detail status=%d view=%+v body=%s", detail.Code, detailView, detail.Body.String())
	}

	confirm := func(version, expected int) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/system-designs/design-dispatch/versions/"+strconv.Itoa(version)+"/confirm", nil)
		request.Header.Set("Authorization", "Bearer token")
		request.Header.Set("If-Match", `"`+strconv.Itoa(expected)+`"`)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := confirm(first.Version, 0); response.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", response.Code, response.Body.String())
	}
	second, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: "# Dispatch v2\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n    - internal/workorder/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if response := confirm(second.Version, 0); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"current_version":1`) {
		t.Fatalf("stale confirm status=%d body=%s", response.Code, response.Body.String())
	}
	if response := confirm(second.Version, 1); response.Code != http.StatusOK {
		t.Fatalf("second confirm status=%d body=%s", response.Code, response.Body.String())
	}
	third, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: "# Dispatch v3\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: document.ID, Content: "# Dispatch v4\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n    - internal/workorder/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if response := confirm(fourth.Version, second.Version); response.Code != http.StatusOK {
		t.Fatalf("later confirm status=%d body=%s", response.Code, response.Body.String())
	}
	if skipped, getErr := st.GetSystemDesignVersion(ctx, document.ID, third.Version); getErr != nil || !skipped.Dismissed || skipped.Confirmed {
		t.Fatalf("skipped version=%+v err=%v", skipped, getErr)
	}

	propose := func(body string) core.Decision {
		request := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("propose decision status=%d body=%s", response.Code, response.Body.String())
		}
		var decision core.Decision
		if err := json.Unmarshal(response.Body.Bytes(), &decision); err != nil {
			t.Fatal(err)
		}
		return decision
	}
	firstDecision := propose(`{"statement":"Keep dispatch event-derived.","context":"Lineage rebuilds from history.","alternatives_rejected":"Mutable edges lose provenance.","origin":"operator"}`)
	confirmDecision := httptest.NewRequest(http.MethodPost, "/v1/decisions/"+firstDecision.ID+"/confirm", nil)
	confirmDecision.Header.Set("Authorization", "Bearer token")
	confirmedDecision := httptest.NewRecorder()
	handler.ServeHTTP(confirmedDecision, confirmDecision)
	if confirmedDecision.Code != http.StatusOK || !strings.Contains(confirmedDecision.Body.String(), `"status":"confirmed"`) {
		t.Fatalf("confirm decision status=%d body=%s", confirmedDecision.Code, confirmedDecision.Body.String())
	}
	secondDecision := propose(`{"statement":"Project governance from the event log.","context":"All edges need provenance.","alternatives_rejected":"A mutable graph can drift.","origin":"operator","supersedes":"` + firstDecision.ID + `"}`)
	if firstDecision.ID != "DEC-1" || secondDecision.ID != "DEC-2" {
		t.Fatalf("decision identities first=%s second=%s", firstDecision.ID, secondDecision.ID)
	}
	dismissDecision := httptest.NewRequest(http.MethodPost, "/v1/decisions/"+secondDecision.ID+"/dismiss", nil)
	dismissDecision.Header.Set("Authorization", "Bearer token")
	dismissDecision.Header.Set("X-Conveyor-Actor", "dashboard-operator")
	dismissedDecision := httptest.NewRecorder()
	handler.ServeHTTP(dismissedDecision, dismissDecision)
	if dismissedDecision.Code != http.StatusOK || !strings.Contains(dismissedDecision.Body.String(), `"status":"dismissed"`) || !strings.Contains(dismissedDecision.Body.String(), `"dismissed_by":"user:local-operator"`) {
		t.Fatalf("dismiss decision status=%d body=%s", dismissedDecision.Code, dismissedDecision.Body.String())
	}
	confirmDismissed := httptest.NewRequest(http.MethodPost, "/v1/decisions/"+secondDecision.ID+"/confirm", nil)
	confirmDismissed.Header.Set("Authorization", "Bearer token")
	confirmDismissedResponse := httptest.NewRecorder()
	handler.ServeHTTP(confirmDismissedResponse, confirmDismissed)
	if confirmDismissedResponse.Code != http.StatusConflict {
		t.Fatalf("confirm dismissed decision status=%d body=%s", confirmDismissedResponse.Code, confirmDismissedResponse.Body.String())
	}

	spoof := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(`{"statement":"Spoof.","context":"Bad provenance.","alternatives_rejected":"Server binding.","origin":"planning_session","origin_session_id":"forged"}`))
	spoof.Header.Set("Authorization", "Bearer token")
	spoofResponse := httptest.NewRecorder()
	handler.ServeHTTP(spoofResponse, spoof)
	if spoofResponse.Code != http.StatusBadRequest {
		t.Fatalf("spoofed decision status=%d body=%s", spoofResponse.Code, spoofResponse.Body.String())
	}
	badID := httptest.NewRequest(http.MethodPost, "/v1/system-designs", strings.NewReader("{\"id\":\"bad/id\",\"title\":\"Bad\",\"category\":\"Architecture\",\"content\":\"# Bad\\n\\n```conveyor:governs\\n- repo: conveyor\\n  paths:\\n    - internal/**\\n```\"}"))
	badID.Header.Set("Authorization", "Bearer token")
	badIDResponse := httptest.NewRecorder()
	handler.ServeHTTP(badIDResponse, badID)
	if badIDResponse.Code != http.StatusBadRequest {
		t.Fatalf("unsafe design id status=%d body=%s", badIDResponse.Code, badIDResponse.Body.String())
	}
}
