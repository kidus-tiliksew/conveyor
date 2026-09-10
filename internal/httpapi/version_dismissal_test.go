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

func TestOperatorDismissalNotesHTTP(t *testing.T) {
	for _, tier := range []string{"requirements", "system-designs"} {
		for _, action := range []string{"confirm", "dismiss"} {
			for _, tc := range []struct {
				name, body, want string
				status           int
			}{
				{"absent", "", "", 200}, {"empty", "{}", "", 200}, {"whitespace", `{"note":"  \n  "}`, "", 200},
				{"trimmed", `{"note":"  correct intent, malformed proposal  "}`, "correct intent, malformed proposal", 200},
				{"unicode limit", `{"note":"  ` + strings.Repeat("日", 2000) + `  "}`, strings.Repeat("日", 2000), 200},
				{"over limit", `{"note":"` + strings.Repeat("日", 2001) + `"}`, "", 400},
				{"wrong type", `{"note":5}`, "", 400}, {"malformed", `{"note":`, "", 400}, {"trailing", `{"note":"a"}{}`, "", 400},
			} {
				t.Run(tier+"/"+action+"/"+tc.name, func(t *testing.T) {
					ctx := store.WithWorkspace(t.Context(), "demo")
					st := store.NewMemory()
					id := "note-doc"
					var history func() []string
					if tier == "requirements" {
						v := core.RequirementVersion{Content: "# Intent\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Preserve intent.\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve intent."}}, Origin: core.RequirementOriginOperator}
						if _, _, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, v); err != nil {
							t.Fatal(err)
						}
						v.RequirementID = id
						for n := 2; n <= 3; n++ {
							v.Content = "# Next\n" + v.Content
							if _, err := st.ProposeRequirementVersion(ctx, v); err != nil {
								t.Fatal(err)
							}
						}
						history = func() []string {
							vs, err := st.ListRequirementVersions(ctx, id)
							if err != nil {
								t.Fatal(err)
							}
							var notes []string
							for _, v := range vs {
								if tc.status != 200 && (v.Retired || v.Confirmed) {
									t.Fatal("invalid request mutated version")
								}
								notes = append(notes, v.DismissalNote)
							}
							return notes
						}
					} else {
						v := core.SystemDesignVersion{Content: "# Intent\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator}
						if _, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: id, Category: "Architecture"}, v); err != nil {
							t.Fatal(err)
						}
						v.DocumentID = id
						for n := 2; n <= 3; n++ {
							v.Content = "Next\n" + v.Content
							if _, err := st.ProposeSystemDesignVersion(ctx, v); err != nil {
								t.Fatal(err)
							}
						}
						history = func() []string {
							vs, err := st.ListSystemDesignVersions(ctx, id)
							if err != nil {
								t.Fatal(err)
							}
							var notes []string
							for _, v := range vs {
								if tc.status != 200 && (v.Dismissed || v.Confirmed) {
									t.Fatal("invalid request mutated version")
								}
								notes = append(notes, v.DismissalNote)
							}
							return notes
						}
					}
					server := NewServer(st)
					server.Workspace, server.BearerToken = "demo", "token"
					version := "1"
					if action == "confirm" {
						version = "3"
					}
					req := httptest.NewRequest(http.MethodPost, "/v1/"+tier+"/"+id+"/versions/"+version+"/"+action, strings.NewReader(tc.body))
					req.Header.Set("Authorization", "Bearer token")
					response := httptest.NewRecorder()
					server.Handler().ServeHTTP(response, req)
					if response.Code != tc.status {
						t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
					}
					for i, n := range history() {
						want := ""
						if tc.status == 200 && (i == 0 || (i == 1 && action == "confirm")) {
							want = tc.want
						}
						if n != want {
							t.Fatalf("v%d note=%q want=%q", i+1, n, want)
						}
					}
					get := httptest.NewRequest(http.MethodGet, "/v1/"+tier+"/"+id+"/versions", nil)
					get.Header.Set("Authorization", "Bearer token")
					listed := httptest.NewRecorder()
					server.Handler().ServeHTTP(listed, get)
					if listed.Code != 200 {
						t.Fatalf("list status=%d", listed.Code)
					}
					if strings.Contains(listed.Body.String(), `"dismissal_note"`) != (tc.status == 200 && tc.want != "") {
						t.Fatalf("list=%s", listed.Body.String())
					}
				})
			}
		}
	}
}
