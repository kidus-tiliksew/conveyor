package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Document identities use explicit maps to exclude unrelated machinery fields.
// req-document-operating-surfaces AC-5.2/5.4/5.8.
func documentReadIdentity(kind, id, title string, current int, archived bool, superseded []string) map[string]any {
	return map[string]any{"kind": kind, "id": id, "title": title, "current_version": current, "archived": archived, "superseded_by": superseded, "active_authority": kind != "reference" && !archived && current > 0, "informative": kind == "reference"}
}
func (s *Server) mcpDocumentList(ctx context.Context, a map[string]any) ([]any, error) {
	result := []any{}
	kind := readString(a, "kind")
	archived := readBool(a, "include_archived")
	switch kind {
	case "requirement":
		values, e := s.Store.ListRequirements(ctx, archived)
		if e != nil {
			return nil, e
		}
		for _, v := range values {
			if v.CurrentVersion > 0 && readMatches(a, v.ID+" "+v.Title) {
				result = append(result, documentReadIdentity(kind, v.ID, v.Title, v.CurrentVersion, v.Archived, v.SupersededBy))
			}
		}
	case "system_design":
		values, e := s.Store.ListSystemDesigns(ctx, archived)
		if e != nil {
			return nil, e
		}
		for _, v := range values {
			if v.CurrentVersion > 0 && readMatches(a, v.ID+" "+v.Title) {
				result = append(result, documentReadIdentity(kind, v.ID, v.Title, v.CurrentVersion, v.Archived, v.SupersededBy))
			}
		}
	case "reference":
		values, e := s.Store.ListReferenceDocuments(ctx, archived)
		if e != nil {
			return nil, e
		}
		for _, v := range values {
			if readMatches(a, v.ID+" "+v.Name) {
				result = append(result, documentReadIdentity(kind, v.ID, v.Name, v.CurrentVersion, !v.DeletedAt.IsZero(), nil))
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].(map[string]any)["id"].(string) < result[j].(map[string]any)["id"].(string)
	})
	return result, nil
}
func (s *Server) mcpDocumentIdentity(ctx context.Context, a map[string]any) (map[string]any, error) {
	kind, id := readString(a, "kind"), readString(a, "document_id")
	var p map[string]any
	switch kind {
	case "requirement":
		v, e := s.Store.GetRequirement(ctx, id)
		if e != nil {
			return nil, store.ErrNotFound
		}
		p = documentReadIdentity(kind, v.ID, v.Title, v.CurrentVersion, v.Archived, v.SupersededBy)
	case "system_design":
		v, e := s.Store.GetSystemDesign(ctx, id)
		if e != nil {
			return nil, store.ErrNotFound
		}
		p = documentReadIdentity(kind, v.ID, v.Title, v.CurrentVersion, v.Archived, v.SupersededBy)
	case "reference":
		v, e := s.Store.GetReferenceDocument(ctx, id)
		if e != nil {
			return nil, store.ErrNotFound
		}
		p = documentReadIdentity(kind, v.ID, v.Name, v.CurrentVersion, !v.DeletedAt.IsZero(), nil)
	default:
		return nil, fmt.Errorf("invalid document kind")
	}
	if p["archived"] == true && !readBool(a, "include_archived") {
		return nil, fmt.Errorf("archived document: explicit include_archived required")
	}
	return p, nil
}
func (s *Server) mcpDocumentRead(ctx context.Context, a map[string]any) (map[string]any, error) {
	p, err := s.mcpDocumentIdentity(ctx, a)
	if err != nil {
		return nil, err
	}
	version := p["current_version"].(int)
	explicit := false
	if v, ok := a["version"]; ok {
		b, _ := json.Marshal(v)
		_ = json.Unmarshal(b, &version)
		explicit = true
	}
	if version == 0 {
		p["authority_absent"] = true
		p["version_status"] = "no_confirmed_version"
		return p, nil
	}
	p["version"] = version
	p["selection"] = "current"
	if explicit {
		p["selection"] = "explicit_version"
	}
	p["historical"] = version != p["current_version"].(int)
	var confirmed, retired bool
	switch readString(a, "kind") {
	case "requirement":
		v, e := s.Store.GetRequirementVersion(ctx, readString(a, "document_id"), version)
		if e != nil {
			return nil, store.ErrNotFound
		}
		p["content"] = v.Content
		p["statements"] = v.Statements
		p["origin"] = v.Origin
		p["origin_task_id"] = v.OriginTaskID
		p["confirmed_by"] = v.ConfirmedBy
		p["created_at"] = v.CreatedAt
		p["retired"] = v.Retired
		confirmed, retired = v.Confirmed, v.Retired
	case "system_design":
		v, e := s.Store.GetSystemDesignVersion(ctx, readString(a, "document_id"), version)
		if e != nil {
			return nil, store.ErrNotFound
		}
		p["content"] = v.Content
		p["governs"] = v.Governs
		p["origin"] = v.Origin
		p["origin_task_id"] = v.OriginTaskID
		p["confirmed_by"] = v.ConfirmedBy
		p["confirmed_at"] = v.ConfirmedAt
		p["created_at"] = v.CreatedAt
		p["dismissed"] = v.Dismissed
		confirmed, retired = v.Confirmed, v.Dismissed
	case "reference":
		v, e := s.Store.GetReferenceDocumentVersion(ctx, readString(a, "document_id"), version)
		if e != nil {
			return nil, store.ErrNotFound
		}
		p["content"] = v.Content
		p["created_by"] = v.CreatedBy
		p["created_at"] = v.CreatedAt
		p["content_type"] = v.ContentType
	}
	p["confirmed"] = confirmed
	p["version_status"] = "proposed"
	if retired {
		p["version_status"] = "retired"
	}
	if confirmed {
		p["version_status"] = "confirmed"
	}
	if readString(a, "kind") == "reference" {
		p["version_status"] = "informative"
	}
	p["active_authority"] = p["active_authority"] == true && confirmed && !retired && p["historical"] == false
	return p, nil
}
