package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestTaskAuditRequiresMatchingWorkspaceBeforeSelectorLookup(t *testing.T) {
	st := store.NewMemory()
	if err := st.CreateTask(store.WithWorkspace(t.Context(), "foreign"), core.Task{ID: "foreign-task", Workspace: "foreign"}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(st)
	router := chi.NewRouter()
	router.Get("/tasks/{id}/audit/{kind}/{record_id}", srv.getTaskAudit)
	for _, workspace := range []string{"", "demo"} {
		for _, selector := range []string{"event/invalid", "work-order/missing"} {
			t.Run(workspace+"/"+selector, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, "/tasks/foreign-task/audit/"+selector, nil)
				if workspace != "" {
					req = req.WithContext(store.WithWorkspace(req.Context(), workspace))
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if rec.Code != http.StatusNotFound || rec.Body.String() != "404 page not found\n" {
					t.Fatalf("workspace refusal: %d %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}
