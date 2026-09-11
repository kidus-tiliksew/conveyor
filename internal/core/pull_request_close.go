package core

import "fmt"

// PullRequestClose is a post-commit publication, independent of the successor's
// lifecycle (req-task-lifecycle-and-queue AC-7.4; component-git-delivery).
type PullRequestClose struct {
	WorkspaceID          string           `json:"workspace_id"`
	TaskID               string           `json:"task_id"`
	Repository           string           `json:"repository"`
	Branch               string           `json:"branch"`
	SuccessorID          string           `json:"successor_id"`
	SuccessorBranch      string           `json:"successor_branch"`
	Reason               string           `json:"reason"`
	RestartingOperatorID string           `json:"restarting_operator_id"`
	ForgeAuthorClass     ForgeAuthorClass `json:"forge_author_class"`
	Number               int              `json:"number,omitempty"`
	URL                  string           `json:"url,omitempty"`
	State                string           `json:"state"`
	Attempts             int              `json:"attempts"`
	ForgeErrorCategory   string           `json:"forge_error_category,omitempty"`
	LastError            string           `json:"last_error,omitempty"`
	Outcome              string           `json:"outcome,omitempty"`
}

const PullRequestCloseMaxAttempts = 5

func (p PullRequestClose) Terminal() bool {
	return p.State == "closed" || p.State == "skipped" || p.State == "failed"
}
func (p PullRequestClose) Comment() string {
	return fmt.Sprintf("Closed by Conveyor: this task was started over as %s (%s). Reason: %s", p.SuccessorID, p.SuccessorBranch, p.Reason)
}
