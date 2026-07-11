// Package jobartifact defines attempt-scoped artifact names shared by the
// sandbox shim, trusted runner, and dispatcher.
package jobartifact

import (
	"fmt"
	"path/filepath"
	"strings"
)

func EventLogPath(controlDir, jobID string) (string, error) {
	if err := validateJobID(jobID); err != nil {
		return "", err
	}
	return filepath.Join(controlDir, "events-"+jobID+".jsonl"), nil
}

func LogPath(controlDir, jobID string) (string, error) {
	if err := validateJobID(jobID); err != nil {
		return "", err
	}
	return filepath.Join(controlDir, "job-"+jobID+".log"), nil
}

func validateJobID(jobID string) error {
	if jobID == "" || jobID == "." || jobID == ".." || strings.ContainsAny(jobID, `/\`) {
		return fmt.Errorf("invalid artifact job ID %q", jobID)
	}
	return nil
}
