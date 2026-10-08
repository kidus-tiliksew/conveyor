package store

// Listed-title synchronization on confirmation (req-document-operating-surfaces
// AC-6.1; component-document-corpus). Every backend's requirement and System
// Design confirmation decides the new title with core.ConfirmedDocumentTitle
// and records the rename with these event kinds and payloads, so memory,
// PostgreSQL, and SingleStore write identical history.

const (
	// RequirementTitleChangedEvent records a requirement rename applied by a
	// confirmation. Its payload names the document with requirement_id.
	RequirementTitleChangedEvent = "requirement.title_changed"
	// SystemDesignTitleChangedEvent records a System Design rename applied by
	// a confirmation. Its payload names the document with document_id.
	SystemDesignTitleChangedEvent = "system_design.title_changed"
)

// RequirementTitleChangedPayload is the requirement.title_changed payload.
func RequirementTitleChangedPayload(workspace, requirementID, oldTitle, newTitle string, version int, confirmedBy string) map[string]any {
	return documentTitleChangedPayload(workspace, "requirement_id", requirementID, oldTitle, newTitle, version, confirmedBy)
}

// SystemDesignTitleChangedPayload is the system_design.title_changed payload.
func SystemDesignTitleChangedPayload(workspace, documentID, oldTitle, newTitle string, version int, confirmedBy string) map[string]any {
	return documentTitleChangedPayload(workspace, "document_id", documentID, oldTitle, newTitle, version, confirmedBy)
}

func documentTitleChangedPayload(workspace, idKey, id, oldTitle, newTitle string, version int, confirmedBy string) map[string]any {
	return map[string]any{
		"workspace_id": workspace, idKey: id,
		"old_title": oldTitle, "new_title": newTitle,
		"version": version, "confirmed_by": confirmedBy,
	}
}
