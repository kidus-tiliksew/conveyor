// Package gitx provides API-backed planning snapshots and repository identities.
package gitx

import "strings"

const defaultAssignmentPrefix = "conveyor/task-"

func BranchName(taskID string) string { return defaultAssignmentPrefix + taskID }

// DefaultAssignmentTaskID reports the task id encoded in conveyor/task-<id>.
func DefaultAssignmentTaskID(branch string) (string, bool) {
	if !strings.HasPrefix(branch, defaultAssignmentPrefix) {
		return "", false
	}
	id := branch[len(defaultAssignmentPrefix):]
	if id == "" {
		return "", false
	}
	return id, true
}

// LegalBranchName reports whether name is a legal git branch assignment
// (component-git-delivery; req-task-branch-assignment REQ-2). It encodes
// git check-ref-format --branch plus the documented refuse list and never
// spawns git.
func LegalBranchName(name string) bool {
	if name == "" || name == "@" || strings.EqualFold(name, "HEAD") {
		return false
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "refs/heads/") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return false
	}
	if strings.ContainsAny(name, " ~^:?*[\\\x7f") {
		return false
	}
	for i := range len(name) {
		if name[i] < 0x20 {
			return false
		}
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
