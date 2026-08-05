package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

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
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/v1/system-designs", nil))
	var views []systemDesignView
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &views) != nil || len(views) != 1 || views[0].Document.Title != "Dispatch" || len(views[0].PendingVersions) != 1 || len(views[0].Lineage) != 2 {
		t.Fatalf("list status=%d views=%+v body=%s", list.Code, views, list.Body.String())
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
