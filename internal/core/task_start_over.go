package core

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// TaskStartOverRequest carries operator input and server-resolved permission.
// CanConfirmDocuments is never accepted from an HTTP request body.
type TaskStartOverRequest struct {
	TaskID              string `json:"-"`
	RequestID           string `json:"request_id"`
	Reason              string `json:"reason"`
	Note                string `json:"note"`
	CanConfirmDocuments bool   `json:"-"`
}

func (r TaskStartOverRequest) Validate() error {
	if strings.TrimSpace(r.TaskID) == "" {
		return fmt.Errorf("task id is required")
	}
	if strings.TrimSpace(r.RequestID) == "" || utf8.RuneCountInString(r.RequestID) > 200 {
		return fmt.Errorf("request_id is required and must be at most 200 characters")
	}
	if strings.TrimSpace(r.Reason) == "" || utf8.RuneCountInString(r.Reason) > 200 {
		return fmt.Errorf("reason is required and must be at most 200 characters")
	}
	if utf8.RuneCountInString(r.Note) > 2000 {
		return fmt.Errorf("note must be at most 2000 characters")
	}
	return nil
}

type TaskStartOverResult struct {
	Task      Task `json:"task"`
	Successor Task `json:"successor"`
	Created   bool `json:"created"`
}
