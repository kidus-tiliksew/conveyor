package store

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
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
