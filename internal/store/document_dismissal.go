package store

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type documentDismissalNoteKey struct{}

// WithDocumentDismissalNote carries operator attribution through the existing
// document transactions, like WithActor. Only the operator REST routes set it;
// proposal inputs and MCP mutations cannot supply it (AC-5.1, AC-1.3;
// component-document-corpus).
func WithDocumentDismissalNote(ctx context.Context, note string) (context.Context, error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > 2000 {
		return nil, fmt.Errorf("note must contain at most 2000 characters")
	}
	return context.WithValue(ctx, documentDismissalNoteKey{}, note), nil
}

func DocumentDismissalNote(ctx context.Context) string {
	note, _ := ctx.Value(documentDismissalNoteKey{}).(string)
	return note
}

// DocumentDismissalEventPayload preserves the no-note event shape (AC-5.4).
func DocumentDismissalEventPayload(ctx context.Context, payload map[string]any) map[string]any {
	if note := DocumentDismissalNote(ctx); note != "" {
		payload["note"] = note
	}
	return payload
}

// DocumentRestoreConflict preserves the previously-confirmed-version boundary
// of req-document-archive AC-1.5.
type DocumentRestoreConflict struct{ DocumentID string }

func (e *DocumentRestoreConflict) Error() string {
	return fmt.Sprintf("document %s has no confirmed version to restore to", e.DocumentID)
}

// DismissalArchiveEventPayload retains the dismissal note and distinguishes an
// automatic archive from an operator archive (component-document-corpus).
func DismissalArchiveEventPayload(ctx context.Context, workspace, idKey, id, actor string, at time.Time) map[string]any {
	return DocumentDismissalEventPayload(ctx, map[string]any{
		"workspace_id": workspace, idKey: id, "version": 0, "actor": actor, "at": at,
		"superseded_by": []string{}, "reason": core.ArchiveReasonOnlyProposalDismissed,
	})
}
