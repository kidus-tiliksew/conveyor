package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const maxReferenceDocumentBytes = 2 << 20

func referenceUpload(r *http.Request) (string, string, string, []byte, error) {
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
	name, filename, contentType, content, err := referenceUpload(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if name == "" {
		http.Error(w, "reference document name is required", 400)
		return
	}
	document, version, err := s.Store.CreateReferenceDocument(r.Context(), core.ReferenceDocument{ID: "ref-" + core.NewTaskID(), Name: name}, core.ReferenceDocumentVersion{Filename: filename, ContentType: contentType, Content: string(content)})
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	writeJSON(w, 201, map[string]any{"document": document, "version": version})
}
func (s *Server) supersedeReferenceDocument(w http.ResponseWriter, r *http.Request) {
	_, filename, contentType, content, err := referenceUpload(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	version, err := s.Store.SupersedeReferenceDocument(r.Context(), chi.URLParam(r, "id"), core.ReferenceDocumentVersion{Filename: filename, ContentType: contentType, Content: string(content)})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, 201, version)
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
	http.Error(w, err.Error(), 500)
}
