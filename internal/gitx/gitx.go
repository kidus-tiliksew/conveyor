// Package gitx provides API-backed planning snapshots and repository identities.
package gitx

func BranchName(taskID string) string { return "conveyor/task-" + taskID }
