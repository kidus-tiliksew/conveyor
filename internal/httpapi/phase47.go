package httpapi

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

const maxArtifactBytes = 25 << 20

func (s *Server) listWorkOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := s.Store.ListWorkOrders(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, orders)
}

func (s *Server) recoverWorkOrder(w http.ResponseWriter, r *http.Request) {
	if s.WorkOrders == nil {
		http.Error(w, "work-order service unavailable", http.StatusServiceUnavailable)
		return
	}
	var request struct {
		RequestID string `json:"request_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	if request.RequestID == "" {
		request.RequestID = r.Header.Get("X-Idempotency-Key")
	}
	order, err := s.WorkOrders.Recover(r.Context(), chi.URLParam(r, "id"), request.RequestID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

type requirementNode struct {
	Feature core.Feature       `json:"feature"`
	Tasks   []core.Task        `json:"tasks"`
	Specs   []core.SpecVersion `json:"approved_specs"`
	Events  []core.Event       `json:"events"`
}

func (s *Server) listRequirements(w http.ResponseWriter, r *http.Request) {
	features, err := s.Store.ListFeatures(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	tasks, err := s.Store.ListTasks(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	nodes := make([]requirementNode, len(features))
	byID := map[string]int{}
	for i, feature := range features {
		nodes[i].Feature = feature
		byID[feature.ID] = i
	}
	for _, task := range tasks {
		index, ok := byID[task.FeatureID]
		if !ok {
			continue
		}
		nodes[index].Tasks = append(nodes[index].Tasks, task)
		if spec, exists, _ := s.Store.GetLatestSpecVersion(r.Context(), task.ID); exists && spec.Approved {
			nodes[index].Specs = append(nodes[index].Specs, spec)
		}
		events, _ := s.Store.ListEvents(r.Context(), task.ID)
		nodes[index].Events = append(nodes[index].Events, events...)
	}
	writeJSON(w, 200, nodes)
}

func (s *Server) createFeature(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ParentID    string `json:"parent_id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" {
		http.Error(w, "name is required", 400)
		return
	}
	feature := core.Feature{ID: "feature-" + core.NewTaskID(), Workspace: s.Workspace, ParentID: request.ParentID, Name: request.Name, Description: request.Description, CreatedAt: time.Now().UTC()}
	if s.ConfigProvider != nil {
		if cfg, err := s.ConfigProvider(r.Context()); err == nil {
			feature.Workspace = cfg.Workspace
		}
	}
	if err := s.Store.CreateFeature(r.Context(), feature); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	writeJSON(w, 201, feature)
}

func (s *Server) assignTaskFeature(w http.ResponseWriter, r *http.Request) {
	var request struct {
		FeatureID string `json:"feature_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.Store.AssignTaskFeature(r.Context(), chi.URLParam(r, "id"), request.FeatureID); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	task, err := s.Store.GetTask(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	writeJSON(w, 200, task)
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	artifacts, err := s.Store.ListArtifacts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	for i := range artifacts {
		artifacts[i].DownloadURL = "/v1/artifacts/" + artifacts[i].ID
	}
	writeJSON(w, 200, artifacts)
}

func (s *Server) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxArtifactBytes+(1<<20))
	if err := r.ParseMultipartForm(maxArtifactBytes); err != nil {
		http.Error(w, "artifact exceeds 25 MiB or form is invalid", http.StatusRequestEntityTooLarge)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file is required", 400)
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxArtifactBytes+1))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if len(content) > maxArtifactBytes {
		http.Error(w, "artifact exceeds 25 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	sum := sha256.Sum256(content)
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(content)
	}
	workspace := s.Workspace
	if s.ConfigProvider != nil {
		if cfg, getErr := s.ConfigProvider(r.Context()); getErr == nil {
			workspace = cfg.Workspace
		}
	}
	artifact := core.Artifact{ID: fmt.Sprintf("%x", sum), Workspace: workspace, Name: safeFilename(header), ContentType: contentType, SizeBytes: int64(len(content)), TaskID: r.FormValue("task_id"), FeatureID: r.FormValue("feature_id"), CreatedAt: time.Now().UTC()}
	artifact, err = s.Store.CreateArtifact(r.Context(), artifact, content)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 201, artifact)
}

func safeFilename(header *multipart.FileHeader) string {
	name := strings.TrimSpace(header.Filename)
	if name == "" {
		return "artifact"
	}
	name = strings.ReplaceAll(name, "\\", "/")
	parts := strings.Split(name, "/")
	return parts[len(parts)-1]
}

func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	artifact, content, err := s.Store.GetArtifact(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	w.Header().Set("Content-Type", artifact.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", artifact.Name))
	w.Header().Set("Content-Length", fmt.Sprint(len(content)))
	_, _ = w.Write(content)
}
