package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestVersionDismissalHTTPContracts(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirement, requirementVersion, err := st.CreateRequirement(ctx,
		core.Requirement{ID: "req-dismiss", Title: "Dismiss requirement"},
		core.RequirementVersion{Content: "# Dismiss\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Dismiss pending intent.\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Dismiss pending intent."}}, Origin: core.RequirementOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	design, designVersion, err := st.CreateSystemDesign(ctx,
		core.SystemDesign{ID: "design-dismiss", Title: "Dismiss design", Category: "Architecture"},
		core.SystemDesignVersion{Content: "# Dismiss\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	call := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("X-Conveyor-Actor", "operator-dismiss")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}

	requirementResponse := call("/v1/requirements/" + requirement.ID + "/versions/1/dismiss")
	if requirementResponse.Code != http.StatusOK || !strings.Contains(requirementResponse.Body.String(), `"retired":true`) || !strings.Contains(requirementResponse.Body.String(), `"retired_by":"`) || strings.Contains(requirementResponse.Body.String(), `"current_version":`) {
		t.Fatalf("requirement dismissal status=%d body=%s", requirementResponse.Code, requirementResponse.Body.String())
	}
	requirementConflict := call("/v1/requirements/" + requirement.ID + "/versions/1/dismiss")
	if requirementConflict.Code != http.StatusConflict || !strings.Contains(requirementConflict.Body.String(), `"error":"requirement_version_dismissed"`) {
		t.Fatalf("requirement conflict status=%d body=%s", requirementConflict.Code, requirementConflict.Body.String())
	}
	if missing := call("/v1/requirements/missing/versions/1/dismiss"); missing.Code != http.StatusNotFound {
		t.Fatalf("missing requirement status=%d body=%s", missing.Code, missing.Body.String())
	}
	if invalid := call("/v1/requirements/" + requirement.ID + "/versions/nope/dismiss"); invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid requirement status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	designResponse := call("/v1/system-designs/" + design.ID + "/versions/1/dismiss")
	if designResponse.Code != http.StatusOK || !strings.Contains(designResponse.Body.String(), `"dismissed":true`) || !strings.Contains(designResponse.Body.String(), `"dismissed_by":"`) || strings.Contains(designResponse.Body.String(), `"current_version":`) {
		t.Fatalf("design dismissal status=%d body=%s", designResponse.Code, designResponse.Body.String())
	}
	designConflict := call("/v1/system-designs/" + design.ID + "/versions/1/dismiss")
	if designConflict.Code != http.StatusConflict || !strings.Contains(designConflict.Body.String(), `"error":"system_design_version_dismissed"`) {
		t.Fatalf("design conflict status=%d body=%s", designConflict.Code, designConflict.Body.String())
	}

	confirmedRequirement, confirmedRequirementVersion, err := st.CreateRequirement(ctx,
		core.Requirement{ID: "req-confirmed-dismiss", Title: "Confirmed requirement"},
		core.RequirementVersion{Content: "# Confirmed\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Keep confirmed intent.\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep confirmed intent."}}, Origin: core.RequirementOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, confirmedRequirement.ID, confirmedRequirementVersion.Version); err != nil {
		t.Fatal(err)
	}
	confirmedConflict := call("/v1/requirements/" + confirmedRequirement.ID + "/versions/1/dismiss")
	if confirmedConflict.Code != http.StatusConflict || !strings.Contains(confirmedConflict.Body.String(), `"error":"requirement_version_confirmed"`) {
		t.Fatalf("confirmed conflict status=%d body=%s", confirmedConflict.Code, confirmedConflict.Body.String())
	}

	_ = requirementVersion
	_ = designVersion
}
