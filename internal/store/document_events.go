package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// DocumentEventQuery pins offset pages to an append-only event snapshot.
// req-260802-72fc68 AC-5.1; req-document-operating-surfaces REQ-3.
type DocumentEventQuery struct {
	Limit, Offset int
	SnapshotID    int64
}
type DocumentEventPage struct {
	Events     []core.Event `json:"events"`
	Total      int          `json:"total"`
	Limit      int          `json:"limit"`
	Offset     int          `json:"offset"`
	SnapshotID int64        `json:"snapshot_id"`
}

func ValidateDocumentEventQuery(kind core.LineageNodeType, q DocumentEventQuery) error {
	if kind != core.LineageRequirement && kind != core.LineageSystemDesign {
		return fmt.Errorf("unsupported document kind")
	}
	if q.Limit < 1 || q.Limit > MaxTaskOperationsLimit || q.Offset < 0 || q.Offset > MaxTaskOperationsOffset || q.SnapshotID < 0 {
		return fmt.Errorf("invalid document event page")
	}
	return nil
}
func (m *memory) ListDocumentEventPage(ctx context.Context, kind core.LineageNodeType, id string, q DocumentEventQuery) (DocumentEventPage, error) {
	if err := ValidateDocumentEventQuery(kind, q); err != nil {
		return DocumentEventPage{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	tasks := map[string]bool{}
	if kind == core.LineageRequirement {
		for _, p := range m.taskContextProposals {
			if p.Workspace == workspace && p.TargetKind == core.TaskContextProposalRequirement && p.TargetID == id && p.State == core.TaskContextProposalConfirmed {
				tasks[p.TaskID] = true
			}
		}
	}
	events := []core.Event{}
	snapshot := q.SnapshotID
	for taskID, items := range m.events {
		if taskID != "" {
			if kind == core.LineageRequirement {
				if !tasks[taskID] {
					continue
				}
			} else if task, ok := m.tasks[taskID]; !ok || task.Workspace != workspace {
				continue
			}
		}
		for _, e := range items {
			if q.SnapshotID > 0 && e.ID > q.SnapshotID {
				continue
			}
			// System Design events retain document membership even on task streams.
			// Task streams use the task's workspace; document streams carry it in payload.
			if taskID == "" || kind == core.LineageSystemDesign {
				var p map[string]any
				if json.Unmarshal(e.Payload, &p) != nil || (taskID == "" && p["workspace_id"] != workspace) {
					continue
				}
				if kind == core.LineageRequirement {
					if p["requirement_id"] != id {
						continue
					}
				} else if !strings.HasPrefix(e.Kind, "system_design.") || p["document_id"] != id {
					continue
				}
			}
			events = append(events, e)
			if e.ID > snapshot {
				snapshot = e.ID
			}
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].At.Equal(events[j].At) {
			return events[i].ID > events[j].ID
		}
		return events[i].At.After(events[j].At)
	})
	page := DocumentEventPage{Events: []core.Event{}, Total: len(events), Limit: q.Limit, Offset: q.Offset, SnapshotID: snapshot}
	if q.Offset < len(events) {
		page.Events = events[q.Offset:min(q.Offset+q.Limit, len(events))]
	}
	return page, nil
}
