package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func verificationUserAccess(r *http.Request) (store.VerificationAccess, error) {
	c, ok := store.CredentialFromContext(r.Context())
	if !ok || c.Kind != core.CredentialUser || c.OwnerUserID == "" {
		return store.VerificationAccess{}, store.ErrVerificationAccess
	}
	return store.VerificationAccess{TaskID: chi.URLParam(r, "id"), UserID: c.OwnerUserID}, nil
}

func verificationReadError(w http.ResponseWriter, err error) {
	code, message := http.StatusInternalServerError, "verification request failed"
	switch {
	case errors.Is(err, store.ErrVerificationAccess):
		code, message = 404, "verification access refused"
	case errors.Is(err, store.ErrVerificationInvalid):
		code, message = 400, "invalid verification request"
	case errors.Is(err, store.ErrVerificationConflict) || errors.Is(err, store.ErrVerificationState):
		code, message = 409, "verification transition or retry conflict"
	}
	http.Error(w, message, code)
}

func verificationPageRequest(r *http.Request, kind string) (store.VerificationPageRequest, error) {
	p := store.VerificationPageRequest{Kind: kind, ContextID: chi.URLParam(r, "context_id")}
	for key, values := range r.URL.Query() {
		if len(values) != 1 {
			return p, store.ErrVerificationInvalid
		}
		switch key {
		case "workspace_id":
		case "cursor":
			p.Cursor = values[0]
		case "limit":
			n, err := strconv.Atoi(values[0])
			if err != nil || n < 1 {
				return p, store.ErrVerificationInvalid
			}
			p.Limit = n
		default:
			return p, store.ErrVerificationInvalid
		}
	}
	return p, nil
}

// feature-verification-kit-execution VK-9 / DEC-43: the initial response contains only bounded metadata. The
// first context has small overview pages; attempts and evidence are on demand.
func (s *Server) getTaskVerification(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a, err := verificationUserAccess(r)
	if err != nil {
		verificationReadError(w, err)
		return
	}
	b, ok := s.Store.(store.VerificationReader)
	if !ok {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	p, err := verificationPageRequest(r, "contexts")
	if err != nil {
		verificationReadError(w, err)
		return
	}
	if p.Limit == 0 {
		p.Limit = 5
	}
	contexts, err := b.ReadVerificationPage(r.Context(), a, p)
	if err != nil {
		verificationReadError(w, err)
		return
	}
	task, err := s.Store.GetTask(r.Context(), a.TaskID)
	if err != nil {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	latest := contexts
	if p.Cursor != "" {
		latest, err = b.ReadVerificationPage(r.Context(), a, store.VerificationPageRequest{Kind: "contexts", Limit: 1})
		if err != nil {
			verificationReadError(w, err)
			return
		}
	}
	current := ""
	if len(latest.Items) > 0 {
		var m map[string]string
		_ = json.Unmarshal(latest.Items[0].Metadata, &m)
		if m["source_sha"] == core.VerifyStageHead(task) {
			current = latest.Items[0].ID
		}
	}
	overview := map[string]store.VerificationReadPage{}
	if p.Cursor == "" && current != "" {
		for _, kind := range []string{"selections", "obligations", "assertions", "operations", "publications"} {
			page, err := b.ReadVerificationPage(r.Context(), a, store.VerificationPageRequest{Kind: kind, ContextID: current, Limit: 5})
			if err != nil {
				verificationReadError(w, err)
				return
			}
			overview[kind] = page
		}
	}
	writeJSON(w, 200, map[string]any{"head_sha": core.VerifyStageHead(task), "current_context_id": current, "contexts": contexts, "overview": overview})
}

func (s *Server) getTaskVerificationPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a, err := verificationUserAccess(r)
	if err != nil {
		verificationReadError(w, err)
		return
	}
	b, ok := s.Store.(store.VerificationReader)
	if !ok {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	p, err := verificationPageRequest(r, chi.URLParam(r, "collection"))
	if err != nil {
		verificationReadError(w, err)
		return
	}
	page, err := b.ReadVerificationPage(r.Context(), a, p)
	if err != nil {
		verificationReadError(w, err)
		return
	}
	writeJSON(w, 200, page)
}

func (s *Server) getTaskVerificationEvidence(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a, err := verificationUserAccess(r)
	if err != nil {
		verificationReadError(w, err)
		return
	}
	b, ok := s.Store.(store.VerificationReader)
	if !ok {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	detail, err := b.ReadVerificationDetail(r.Context(), a, chi.URLParam(r, "context_id"), chi.URLParam(r, "evidence_id"))
	if err != nil {
		verificationReadError(w, err)
		return
	}
	artifactID := chi.URLParam(r, "artifact_id")
	if artifactID == "" {
		writeJSON(w, 200, detail)
		return
	}
	var ref *core.VerificationArtifactReference
	for _, a := range detail.Envelope.Artifacts {
		if a.ArtifactID == artifactID {
			v := a
			ref = &v
			break
		}
	}
	if ref == nil {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	writer, ok := s.Store.(store.VerificationStore)
	if !ok {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	meta, content, err := writer.ReadVerificationArtifact(r.Context(), a, detail.Envelope.ID, artifactID)
	if err == nil {
		err = store.VerifyRetainedVerificationArtifact(*ref, content)
	}
	if err != nil {
		verificationReadError(w, err)
		return
	}
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(200)
	_, _ = w.Write(content)
}

func (s *Server) recordTaskVerificationObservation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a, err := verificationUserAccess(r)
	if err != nil {
		verificationReadError(w, err)
		return
	}
	b, ok := s.Store.(store.VerificationStore)
	if !ok {
		verificationReadError(w, store.ErrVerificationAccess)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		verificationReadError(w, store.ErrVerificationInvalid)
		return
	}
	var input store.VerificationOperatorObservation
	if core.DecodeVerificationRequest(raw, &input) != nil {
		verificationReadError(w, store.ErrVerificationInvalid)
		return
	}
	receipt, err := store.RecordVerificationOperatorObservation(r.Context(), b, a, input)
	if err != nil {
		verificationReadError(w, err)
		return
	}
	writeJSON(w, 200, receipt)
}
