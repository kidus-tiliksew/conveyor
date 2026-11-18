package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// DocumentVersionSummaryReadStore serves current and pending version
// summaries for exact document IDs, instead of every content body in the
// workspace (component-persistence; component-document-corpus). Each method
// reads the workspace from the context, deduplicates the requested IDs, and
// returns an empty map without a statement for an empty selection. A
// requested ID outside the workspace, or one without versions, is absent
// from the result.
type DocumentVersionSummaryReadStore interface {
	// ListRequirementVersionSummaries returns every version of each
	// requested requirement in version order. Current and pending rows keep
	// their content and statements; a historical row keeps its metadata and
	// statements, and its content is empty. The requirement list reads its
	// current and pending content and its historical origin sessions from it.
	ListRequirementVersionSummaries(ctx context.Context, requirementIDs []string) (map[string][]core.RequirementVersion, error)
	// ListSystemDesignVersionSummaries returns only the current and pending
	// versions of each requested System Design, in version order, with their
	// content. A historical or dismissed version is absent. The System
	// Design list and the requirement insight documents read it.
	ListSystemDesignVersionSummaries(ctx context.Context, documentIDs []string) (map[string][]core.SystemDesignVersion, error)
}

// UniqueDocumentIDs returns ids without repeats, in first-seen order.
// Durable backends bind the result as one set argument.
func UniqueDocumentIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (m *memory) ListRequirementVersionSummaries(ctx context.Context, requirementIDs []string) (map[string][]core.RequirementVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := map[string][]core.RequirementVersion{}
	for _, id := range UniqueDocumentIDs(requirementIDs) {
		key := memoryScopedKey{workspace: workspace, id: id}
		requirement, ok := m.requirements[key]
		if !ok {
			continue
		}
		versions := m.requirementVersions[key]
		if len(versions) == 0 {
			continue
		}
		summaries := make([]core.RequirementVersion, 0, len(versions))
		for _, version := range versions {
			if version.Version != requirement.CurrentVersion && (version.Confirmed || version.Retired) {
				version.Content = ""
			}
			summaries = append(summaries, version)
		}
		out[id] = summaries
	}
	return out, nil
}

func (m *memory) ListSystemDesignVersionSummaries(ctx context.Context, documentIDs []string) (map[string][]core.SystemDesignVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := map[string][]core.SystemDesignVersion{}
	for _, id := range UniqueDocumentIDs(documentIDs) {
		key := memoryScopedKey{workspace: workspace, id: id}
		document, ok := m.systemDesigns[key]
		if !ok {
			continue
		}
		versions := m.systemDesignVersions[key]
		summaries := make([]core.SystemDesignVersion, 0, len(versions))
		for _, version := range versions {
			if version.Version != document.CurrentVersion && (version.Confirmed || version.Dismissed) {
				continue
			}
			summaries = append(summaries, version)
		}
		if len(summaries) > 0 {
			out[id] = summaries
		}
	}
	return out, nil
}
