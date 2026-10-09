package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// RequirementVersionConflict is returned when an operator confirmation was
// based on stale If-Match intent. HTTP handlers use its fields for a
// non-ambiguous 409 response.
type RequirementVersionConflict struct {
	RequirementID string
	Requested     int
	Current       int
	Expected      *int
}

type RequirementArchivedError struct{ RequirementID string }

func (e *RequirementArchivedError) Error() string {
	return fmt.Sprintf("requirement %s is archived", e.RequirementID)
}

type SystemDesignArchivedError struct{ DocumentID string }

func (e *SystemDesignArchivedError) Error() string {
	return fmt.Sprintf("system design %s is archived", e.DocumentID)
}

type SupersededByInvalidError struct {
	DocumentID string
	Reason     string
}

func (e *SupersededByInvalidError) Error() string {
	return fmt.Sprintf("superseded_by identifier %q %s", e.DocumentID, e.Reason)
}

// RequirementVersionSuperseded is the terminal result of addressing an
// unconfirmed version retired by a later confirmation. It is intentionally
// distinct from an optimistic-concurrency mismatch: refreshing cannot make
// this version actionable again.
type RequirementVersionSuperseded struct {
	RequirementID string
	Requested     int
	Current       int
	SupersededBy  int
}

type VersionDismissalConflictReason string

const (
	VersionDismissalConfirmed  VersionDismissalConflictReason = "confirmed"
	VersionDismissalDismissed  VersionDismissalConflictReason = "dismissed"
	VersionDismissalSuperseded VersionDismissalConflictReason = "superseded"
)

// RequirementVersionDismissalConflict classifies terminal version states for
// the direct operator dismissal route. Refreshing cannot make any of them
// actionable again.
type RequirementVersionDismissalConflict struct {
	RequirementID string
	Requested     int
	Current       int
	Reason        VersionDismissalConflictReason
	SupersededBy  int
}

func (e *RequirementVersionDismissalConflict) Error() string {
	switch e.Reason {
	case VersionDismissalConfirmed:
		return fmt.Sprintf("requirement %s version %d is already confirmed", e.RequirementID, e.Requested)
	case VersionDismissalDismissed:
		return fmt.Sprintf("requirement %s version %d is already dismissed", e.RequirementID, e.Requested)
	default:
		return fmt.Sprintf("requirement %s version %d was superseded by confirmed version %d", e.RequirementID, e.Requested, e.SupersededBy)
	}
}

func (e *RequirementVersionSuperseded) Error() string {
	return fmt.Sprintf("requirement %s version %d was superseded by newer confirmed version %d", e.RequirementID, e.Requested, e.SupersededBy)
}

type SystemDesignVersionConflict struct {
	DocumentID string
	Requested  int
	Current    int
	Expected   *int
}

type SystemDesignVersionDismissalConflict struct {
	DocumentID   string
	Requested    int
	Current      int
	Reason       VersionDismissalConflictReason
	SupersededBy int
}

func (e *SystemDesignVersionDismissalConflict) Error() string {
	switch e.Reason {
	case VersionDismissalConfirmed:
		return fmt.Sprintf("system design %s version %d is already confirmed", e.DocumentID, e.Requested)
	case VersionDismissalDismissed:
		return fmt.Sprintf("system design %s version %d is already dismissed", e.DocumentID, e.Requested)
	default:
		return fmt.Sprintf("system design %s version %d was superseded by confirmed version %d", e.DocumentID, e.Requested, e.SupersededBy)
	}
}

func (e *SystemDesignVersionConflict) Error() string {
	if e.Expected != nil {
		return fmt.Sprintf("system design %s current version is %d, not expected version %d", e.DocumentID, e.Current, *e.Expected)
	}
	return fmt.Sprintf("system design %s already confirmed version %d; cannot confirm superseded version %d", e.DocumentID, e.Current, e.Requested)
}

func (e *RequirementVersionConflict) Error() string {
	if e.Expected != nil {
		return fmt.Sprintf("requirement %s current version is %d, not expected version %d", e.RequirementID, e.Current, *e.Expected)
	}
	return fmt.Sprintf("requirement %s already confirmed version %d; cannot confirm superseded version %d", e.RequirementID, e.Current, e.Requested)
}

func (m *memory) ListRequirementEvents(ctx context.Context, requirementID string) ([]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	events := []core.Event{}
	for _, event := range m.events[""] {
		var payload struct {
			WorkspaceID   string `json:"workspace_id"`
			RequirementID string `json:"requirement_id"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil ||
			payload.WorkspaceID != workspace || payload.RequirementID != requirementID {
			continue
		}
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].At.Equal(events[j].At) {
			return events[i].ID < events[j].ID
		}
		return events[i].At.Before(events[j].At)
	})
	return events, nil
}

func (m *memory) ListRequirementEventsByRequirement(ctx context.Context) (map[string][]core.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := map[string][]core.Event{}
	for _, event := range m.events[""] {
		var payload struct {
			WorkspaceID   string `json:"workspace_id"`
			RequirementID string `json:"requirement_id"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.WorkspaceID != workspace || payload.RequirementID == "" {
			continue
		}
		out[payload.RequirementID] = append(out[payload.RequirementID], event)
	}
	for requirementID := range out {
		sort.Slice(out[requirementID], func(i, j int) bool {
			if out[requirementID][i].At.Equal(out[requirementID][j].At) {
				return out[requirementID][i].ID < out[requirementID][j].ID
			}
			return out[requirementID][i].At.Before(out[requirementID][j].At)
		})
	}
	return out, nil
}
