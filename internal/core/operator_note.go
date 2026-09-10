package core

import "time"

// OperatorNote is an operator's reason, not document authority (AC-5.2;
// component-work-orders). Version history outlives this task-context projection.
type OperatorNote struct {
	DocumentID  string    `json:"document_id"`
	Version     int       `json:"version"`
	Tier        string    `json:"tier"`
	Note        string    `json:"note"`
	DismissedAt time.Time `json:"dismissed_at"`
}
