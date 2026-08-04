package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const maxReferenceDocumentBytes = 2 << 20

func referenceUpload(w http.ResponseWriter, r *http.Request) (string, string, string, []byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxReferenceDocumentBytes+(1<<20))
	if err := r.ParseMultipartForm(maxReferenceDocumentBytes); err != nil {
		return "", "", "", nil, err
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		return "", "", "", nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxReferenceDocumentBytes+1))
	if err != nil {
		return "", "", "", nil, err
	}
	if len(content) > maxReferenceDocumentBytes {
		return "", "", "", nil, errors.New("reference document exceeds 2 MiB")
	}
	contentType := header.Header.Get("Content-Type")
	markdown, err := core.NormalizeReferenceMarkdown(header.Filename, contentType, content)
	if err != nil {
		return "", "", "", nil, err
	}
	return strings.TrimSpace(r.FormValue("name")), header.Filename, contentType, []byte(markdown), nil
}

func (s *Server) listReferenceDocuments(w http.ResponseWriter, r *http.Request) {
	documents, err := s.Store.ListReferenceDocuments(r.Context(), false)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, documents)
}
func (s *Server) listReferenceDocumentVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := s.Store.ListReferenceDocumentVersions(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, 200, versions)
}
func (s *Server) createReferenceDocument(w http.ResponseWriter, r *http.Request) {
	name, filename, contentType, content, err := referenceUpload(w, r)
	if err != nil {
		writeReferenceUploadError(w, err)
		return
	}
	if name == "" {
		http.Error(w, "reference document name is required", 400)
		return
	}
	document, version, err := s.Store.CreateReferenceDocument(r.Context(), core.ReferenceDocument{ID: "ref-" + core.NewTaskID(), Name: name}, core.ReferenceDocumentVersion{Filename: filename, ContentType: contentType, Content: string(content)})
	if err != nil {
		if isReferenceDocumentNameConflict(err) {
			http.Error(w, "a reference document with that name already exists", http.StatusConflict)
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"document": document, "version": version})
}
func (s *Server) supersedeReferenceDocument(w http.ResponseWriter, r *http.Request) {
	_, filename, contentType, content, err := referenceUpload(w, r)
	if err != nil {
		writeReferenceUploadError(w, err)
		return
	}
	version, err := s.Store.SupersedeReferenceDocument(r.Context(), chi.URLParam(r, "id"), core.ReferenceDocumentVersion{Filename: filename, ContentType: contentType, Content: string(content)})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, 201, version)
}

func writeReferenceUploadError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) || strings.Contains(err.Error(), "exceeds 2 MiB") || strings.Contains(err.Error(), "request body too large") {
		http.Error(w, "reference document exceeds 2 MiB or form is invalid", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

func isReferenceDocumentNameConflict(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "reference_documents_live_name_idx" {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "reference document name") && strings.Contains(strings.ToLower(err.Error()), "already exists")
}
func (s *Server) deleteReferenceDocument(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteReferenceDocument(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, err.Error(), 404)
		return
	}
	http.Error(w, "reference document store unavailable", 500)
}
