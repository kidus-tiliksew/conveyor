package httpapi

import (
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Server) listRequirementEventPage(w http.ResponseWriter, r *http.Request) {
	s.listDocumentEventPage(w, r, core.LineageRequirement)
}
func (s *Server) listSystemDesignEventPage(w http.ResponseWriter, r *http.Request) {
	s.listDocumentEventPage(w, r, core.LineageSystemDesign)
}
func (s *Server) listDocumentEventPage(w http.ResponseWriter, r *http.Request, kind core.LineageNodeType) {
	id := chi.URLParam(r, "id")
	var err error
	if kind == core.LineageRequirement {
		_, err = s.Store.GetRequirement(r.Context(), id)
	} else {
		_, err = s.Store.GetSystemDesign(r.Context(), id)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	query, _, err := parseTaskOperationsQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	var snapshot int64
	if values, ok := r.URL.Query()["snapshot_id"]; ok {
		if len(values) != 1 {
			http.Error(w, "snapshot_id must be supplied once", http.StatusBadRequest)
			return
		}
		snapshot, err = strconv.ParseInt(values[0], 10, 64)
		if err != nil || snapshot < 0 {
			http.Error(w, "snapshot_id must be non-negative", http.StatusBadRequest)
			return
		}
	}
	page, err := s.Store.ListDocumentEventPage(r.Context(), kind, id, store.DocumentEventQuery{Limit: query.Limit, Offset: query.Offset, SnapshotID: snapshot})
	if err != nil {
		log.Printf("list document event page: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if kind == core.LineageRequirement {
		page.Events = annotateBackfilledEvents(page.Events)
	}
	w.Header().Set("X-Conveyor-Total", strconv.Itoa(page.Total))
	w.Header().Set("X-Conveyor-Limit", strconv.Itoa(page.Limit))
	w.Header().Set("X-Conveyor-Offset", strconv.Itoa(page.Offset))
	writeJSON(w, http.StatusOK, page)
}
