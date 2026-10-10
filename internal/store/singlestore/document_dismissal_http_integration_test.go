package singlestore

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestDocumentDismissalArchiveHTTPIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "dismissal-http")
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "dismissal-http"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Dismissal", Email: "owner@example.test", DisplayName: "Owner"}, "dismissal-token"); err != nil {
		t.Fatal(err)
	}
	s := httpapi.NewServer(st)
	s.Workspaces = st
	s.Workspace = "dismissal-http"
	handler := s.Handler()
	for _, tier := range []string{"requirements", "system-designs"} {
		id := "http-" + tier
		var err error
		if tier == "requirements" {
			_, _, err = st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, core.RequirementVersion{Content: "# Proposal", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep history."}}})
		} else {
			_, _, err = st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: id, Category: "Architecture"}, core.SystemDesignVersion{Content: "# Proposal\n\n```conveyor:governs\n- repo: conveyor\n  paths: [internal/**]\n```", Origin: core.SystemDesignOriginOperator})
		}
		if err != nil {
			t.Fatal(err)
		}
		call := func(path, body string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodPost, "/v1/"+tier+"/"+id+path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer dismissal-token")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			return w
		}
		w := call("/versions/1/dismiss", `{"note":"Retain this reason"}`)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"archived":true`) || !strings.Contains(w.Body.String(), `"archive_reason":"only_proposal_dismissed"`) || !strings.Contains(w.Body.String(), `"archive_note":"Retain this reason"`) {
			t.Fatalf("%s dismiss=%d %s", tier, w.Code, w.Body)
		}
		w = call("/restore", "")
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no confirmed version to restore to") {
			t.Fatalf("%s restore=%d %s", tier, w.Code, w.Body)
		}
	}
}
