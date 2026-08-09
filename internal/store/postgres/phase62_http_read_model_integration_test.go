package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Requirement audit events have no task ID, so the production dashboard must
// read them through the workspace-scoped event path rather than ListEvents.
// This guards the PostgreSQL-backed read model that the in-memory HTTP tests
// cannot exercise.
func TestPhase62RequirementsHTTPIncludesWorkspaceRequirementLineageIntegration(t *testing.T) {
	st, ctx, workspace := newPhase62IntegrationStore(t)
	requirementID := "req-" + core.NewTaskID()
	createRequirement := func(t *testing.T, requestCtx context.Context, title string) {
		t.Helper()
		session, err := st.CreatePlanningSession(requestCtx, core.PlanningSession{
			ID: "session-" + core.NewTaskID(), Title: "Capture " + title,
		})
		if err != nil {
			t.Fatal(err)
		}
		requirement, _, err := st.CreateRequirement(requestCtx, core.Requirement{
			ID: requirementID, Title: title,
		}, core.RequirementVersion{
			Content: "Retries stay bounded.",
			Statements: []core.RequirementStatement{{
				ID: "REQ-1", Statement: "Retries stop after a finite attempt limit.",
			}},
			Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(requestCtx, requirement.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	createRequirement(t, ctx, "Bounded retries")

	// The same requirement ID in a sibling workspace proves that the read
	// model scopes by the immutable workspace column, not payload identity
	// alone.
	siblingWorkspace := "phase62-http-sibling-" + core.NewTaskID()
	siblingCtx := store.WithWorkspace(t.Context(), siblingWorkspace)
	if _, err := st.BootstrapWorkspaceConfig(siblingCtx, &config.Config{
		Workspace: siblingWorkspace,
		Repos: []config.Repo{{
			Name: "conveyor", URL: "https://example.test/conveyor", Base: "main",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	createRequirement(t, siblingCtx, "Sibling retries")

	server := httpapi.NewServer(st)
	server.Workspace = workspace
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/v1/requirements/"+requirementID, nil,
	))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var view struct {
		Lineage []core.Event `json:"lineage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	wantKinds := []string{
		"requirement.created",
		"requirement.version_proposed",
		"requirement.version_confirmed",
	}
	if len(view.Lineage) != len(wantKinds) {
		t.Fatalf("lineage=%+v", view.Lineage)
	}
	for index, want := range wantKinds {
		if view.Lineage[index].Kind != want || view.Lineage[index].TaskID != "" {
			t.Fatalf("lineage[%d]=%+v want kind=%q taskless", index, view.Lineage[index], want)
		}
	}
}
