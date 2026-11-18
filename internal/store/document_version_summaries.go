package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// DocumentVersionSelection names one listed document and the current
// version number its document row returned. A summary read keeps the content
// of that version, not of a version confirmed after the row read, so a list
// never pairs a current version with empty or missing content
// (component-document-corpus).
type DocumentVersionSelection struct {
	ID             string
	CurrentVersion int
}

// DocumentVersionSummaryReadStore serves current and pending version
// summaries for exact document IDs, instead of every content body in the
// workspace (component-persistence; component-document-corpus). Each method
// reads the workspace from the context, deduplicates the selections by ID
// (the first selection of an ID wins), and returns an empty map without a
// statement for an empty selection. A requested ID outside the workspace, or
// one without versions, is absent from the result. The current version is
// the selection's CurrentVersion, never the document row's value at the time
// of the summary read.
type DocumentVersionSummaryReadStore interface {
	// ListRequirementVersionSummaries returns every version of each
	// requested requirement in version order. The selected current version
	// and pending rows keep their content and statements; a historical row
	// keeps its metadata and statements, and its content is empty. The
	// requirement list reads its current and pending content and its
	// historical origin sessions from it.
	ListRequirementVersionSummaries(ctx context.Context, selections []DocumentVersionSelection) (map[string][]core.RequirementVersion, error)
	// ListSystemDesignVersionSummaries returns only the selected current
	// version and the pending versions of each requested System Design, in
	// version order, with their content. Any other version is absent. The
	// System Design list and the requirement insight documents read it.
	ListSystemDesignVersionSummaries(ctx context.Context, selections []DocumentVersionSelection) (map[string][]core.SystemDesignVersion, error)
}

// UniqueDocumentVersionSelections returns selections without a repeated ID,
// in first-seen order. Durable backends bind the result as set arguments.
func UniqueDocumentVersionSelections(selections []DocumentVersionSelection) []DocumentVersionSelection {
	seen := make(map[string]bool, len(selections))
	out := make([]DocumentVersionSelection, 0, len(selections))
	for _, selection := range selections {
		if seen[selection.ID] {
			continue
		}
		seen[selection.ID] = true
		out = append(out, selection)
	}
	return out
}

func (m *memory) ListRequirementVersionSummaries(ctx context.Context, selections []DocumentVersionSelection) (map[string][]core.RequirementVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := map[string][]core.RequirementVersion{}
	for _, selection := range UniqueDocumentVersionSelections(selections) {
		id := selection.ID
		key := memoryScopedKey{workspace: workspace, id: id}
		if _, ok := m.requirements[key]; !ok {
			continue
		}
		versions := m.requirementVersions[key]
		if len(versions) == 0 {
			continue
		}
		summaries := make([]core.RequirementVersion, 0, len(versions))
		for _, version := range versions {
			if version.Version != selection.CurrentVersion && (version.Confirmed || version.Retired) {
				version.Content = ""
			}
			summaries = append(summaries, version)
		}
		out[id] = summaries
	}
	return out, nil
}

func (m *memory) ListSystemDesignVersionSummaries(ctx context.Context, selections []DocumentVersionSelection) (map[string][]core.SystemDesignVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := map[string][]core.SystemDesignVersion{}
	for _, selection := range UniqueDocumentVersionSelections(selections) {
		id := selection.ID
		key := memoryScopedKey{workspace: workspace, id: id}
		if _, ok := m.systemDesigns[key]; !ok {
			continue
		}
		versions := m.systemDesignVersions[key]
		summaries := make([]core.SystemDesignVersion, 0, len(versions))
		for _, version := range versions {
			if version.Version != selection.CurrentVersion && (version.Confirmed || version.Dismissed) {
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
