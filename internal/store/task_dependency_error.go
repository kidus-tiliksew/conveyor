package store

import "fmt"

// TaskDependencyError is a successor-intake dependency failure. Unwrap returns
// ErrNotFound when Reason is "not found".
type TaskDependencyError struct {
	DependencyID string
	Reason       string
	Err          error
}

func NewTaskDependencyError(id, reason string) *TaskDependencyError {
	return &TaskDependencyError{DependencyID: id, Reason: reason}
}

func (e *TaskDependencyError) Error() string {
	if e == nil {
		return ""
	}
	switch e.Reason {
	case "not found":
		return fmt.Sprintf("dependency task %s not found", e.DependencyID)
	case "belongs to another workspace":
		return fmt.Sprintf("dependency task %s belongs to another workspace", e.DependencyID)
	case "is not open":
		return fmt.Sprintf("dependency task %s is not open", e.DependencyID)
	case "empty or duplicate":
		return "depends_on contains an empty or duplicate task id"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.DependencyID == "" {
		return e.Reason
	}
	return fmt.Sprintf("dependency task %s %s", e.DependencyID, e.Reason)
}

func (e *TaskDependencyError) Unwrap() error {
	if e != nil && e.Reason == "not found" {
		return ErrNotFound
	}
	return nil
}
