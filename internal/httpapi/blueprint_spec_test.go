package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestSpecResponsesFilterMaterializedChildrenByOriginVersion(t *testing.T) {
	cfg := &config.Config{
		Workspace: "demo",
		Repos:     []config.Repo{{Name: "conveyor", Base: "main"}},
	}
	st := store.NewMemoryWithConfig(cfg)
	ctx := store.WithWorkspace(t.Context(), "demo")
	parent := core.Task{
		ID: "versioned-blueprint", Workspace: "demo", Repo: "conveyor",
		BaseBranch: "main", Branch: "conveyor/task-versioned-blueprint",
		State: core.TaskQueued, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	type item struct {
		ID        string   `json:"id"`
		Repo      string   `json:"repo"`
		Summary   string   `json:"summary"`
		DependsOn []string `json:"depends_on"`
	}
	v1, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: parent.ID, Decomposition: core.JSONPayload([]item{{
			ID: "SUB-1", Repo: "conveyor", Summary: "First",
		}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ApproveSpecVersionAndMaterialize(ctx, parent.ID, v1.Version); err != nil {
		t.Fatal(err)
	}
	v2, err := st.CreateSpecVersion(ctx, core.SpecVersion{
		TaskID: parent.ID, Decomposition: core.JSONPayload([]item{
			{ID: "SUB-1", Repo: "conveyor", Summary: "First, revised"},
			{ID: "SUB-2", Repo: "conveyor", Summary: "Second"},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ApproveSpecVersionAndMaterialize(ctx, parent.ID, v2.Version); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.Workspace = "demo"
	for _, test := range []struct {
		name string
		path string
		read func(*testing.T, []byte) []core.TaskRelation
	}{
		{
			name: "latest spec", path: "/v1/tasks/" + parent.ID + "/spec?workspace_id=demo",
			read: func(t *testing.T, body []byte) []core.TaskRelation {
				var spec core.SpecVersion
				if err := json.Unmarshal(body, &spec); err != nil {
					t.Fatal(err)
				}
				if spec.Version != v2.Version {
					t.Fatalf("spec version=%d, want %d", spec.Version, v2.Version)
				}
				return spec.MaterializedChildren
			},
		},
		{
			name: "task detail", path: "/v1/tasks/" + parent.ID + "/activity?workspace_id=demo",
			read: func(t *testing.T, body []byte) []core.TaskRelation {
				var item reviewItem
				if err := json.Unmarshal(body, &item); err != nil {
					t.Fatal(err)
				}
				if item.Spec == nil || item.Spec.Version != v2.Version {
					t.Fatalf("detail spec=%+v, want version %d", item.Spec, v2.Version)
				}
				return item.Spec.MaterializedChildren
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			children := test.read(t, response.Body.Bytes())
			if len(children) != 1 || children[0].OriginSubID != "SUB-2" ||
				children[0].OriginSpecVersion != v2.Version {
				t.Fatalf("materialized children=%+v, want only v2 SUB-2", children)
			}
		})
	}
}
