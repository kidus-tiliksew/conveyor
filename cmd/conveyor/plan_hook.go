package main

// Pre-push review hook for the planning studio. A repository may declare an
// optional argv hook in the repo-tracked .conveyor/planning.yaml; the studio
// runs it once over the whole approved draft before the requirements layer and
// runs it again only when a normative item changed since the last passing run.
// The hook is executed without a shell, with a timeout and a blocking flag; a
// skipped or failing non-blocking hook never blocks a push.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// planningConfigPath is repo-relative; .conveyor/planning.yaml is read only
	// by the local studio and is never pinned by the server.
	planningConfigPath = ".conveyor/planning.yaml"
	// planHookStateFile lives inside the draft directory and records the
	// combined normative-item hash of the last passing hook run.
	planHookStateFile = "hook.json"
	// planHookDraftEnvVar carries the absolute draft directory to the hook.
	planHookDraftEnvVar = "CONVEYOR_PLAN_DRAFT"
	// planningConfigSchemaVersion is the only schema_version accepted.
	planningConfigSchemaVersion = 1
	// defaultPrePushReviewTimeout applies when timeout_seconds is unset; the
	// documented example uses 900 seconds.
	defaultPrePushReviewTimeout = 900 * time.Second
)

// planningConfig is the parsed .conveyor/planning.yaml. A nil PrePushReview
// (absent file, absent key, or null key) means the studio skips the hook.
type planningConfig struct {
	SchemaVersion int            `yaml:"schema_version"`
	PrePushReview *prePushReview `yaml:"pre_push_review"`
}

// prePushReview is the declared hook: an argv list run without a shell.
type prePushReview struct {
	Command        []string `yaml:"command"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
	Blocking       bool     `yaml:"blocking"`
}

// planHookRecord is the persisted pass marker read from <draftDir>/hook.json.
type planHookRecord struct {
	Hash string `json:"hash"`
}

// loadPlanningConfig reads and strictly validates <repoRoot>/.conveyor/planning.yaml.
// An absent file yields a config with a nil PrePushReview and no error. Unknown
// fields, an unsupported schema_version, an empty command, and a negative
// timeout are errors.
func loadPlanningConfig(repoRoot string) (*planningConfig, error) {
	path := filepath.Join(repoRoot, planningConfigPath)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &planningConfig{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", planningConfigPath, err)
	}
	var config planningConfig
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s is empty; schema_version %d is required", planningConfigPath, planningConfigSchemaVersion)
		}
		return nil, fmt.Errorf("parse %s: %w", planningConfigPath, err)
	}
	if config.SchemaVersion != planningConfigSchemaVersion {
		return nil, fmt.Errorf("unsupported %s schema_version %d; expected %d", planningConfigPath, config.SchemaVersion, planningConfigSchemaVersion)
	}
	if config.PrePushReview != nil {
		hook := config.PrePushReview
		if len(hook.Command) == 0 {
			return nil, fmt.Errorf("%s pre_push_review.command must not be empty", planningConfigPath)
		}
		for i, arg := range hook.Command {
			if strings.TrimSpace(arg) == "" {
				return nil, fmt.Errorf("%s pre_push_review.command[%d] must not be empty", planningConfigPath, i)
			}
		}
		if hook.TimeoutSeconds < 0 {
			return nil, fmt.Errorf("%s pre_push_review.timeout_seconds must not be negative", planningConfigPath)
		}
	}
	return &config, nil
}

// runPrePushReview runs the declared hook once over the draft when the combined
// hash of itemHashes differs from the last recorded pass in <draftDir>/hook.json.
// The hook argv is executed directly (no shell) with cwd repoRoot and the draft
// directory exported as CONVEYOR_PLAN_DRAFT. A pass is recorded only on a
// successful (zero-exit) run.
//
// ran reports whether the hook process was started. A blocking failure returns
// a non-nil error; a non-blocking failure is printed to stderr and returns a
// nil error. An absent config, absent pre_push_review key, or unchanged hash
// returns (false, nil).
func runPrePushReview(ctx context.Context, repoRoot, draftDir string, itemHashes []string, stdout, stderr io.Writer) (bool, error) {
	config, err := loadPlanningConfig(repoRoot)
	if err != nil {
		return false, err
	}
	if config.PrePushReview == nil {
		return false, nil
	}
	hook := config.PrePushReview

	combined := combinedPlanItemHash(itemHashes)
	recorded, err := readPlanHookRecord(draftDir)
	if err != nil {
		return false, err
	}
	if recorded.Hash == combined {
		return false, nil
	}

	timeout := time.Duration(hook.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultPrePushReviewTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(runCtx, hook.Command[0], hook.Command[1:]...)
	command.Dir = repoRoot
	command.Env = append(os.Environ(), planHookDraftEnvVar+"="+draftDir)
	command.Stdout = stdout
	command.Stderr = stderr

	if runErr := command.Run(); runErr != nil {
		var failure error
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			failure = fmt.Errorf("timed out after %s", timeout)
		} else {
			failure = runErr
		}
		if !hook.Blocking {
			fmt.Fprintf(stderr, "warning: pre-push review hook failed (non-blocking, continuing): %v\n", failure)
			return true, nil
		}
		return true, fmt.Errorf("pre-push review hook failed: %w", failure)
	}
	if err := writePlanHookRecord(draftDir, planHookRecord{Hash: combined}); err != nil {
		return true, err
	}
	return true, nil
}

// combinedPlanItemHash hashes every normative item hash in a stable order so a
// recorded pass is invalidated by any item change.
func combinedPlanItemHash(itemHashes []string) string {
	ordered := append([]string(nil), itemHashes...)
	sort.Strings(ordered)
	sum := sha256.Sum256([]byte(strings.Join(ordered, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func readPlanHookRecord(draftDir string) (planHookRecord, error) {
	path := filepath.Join(draftDir, planHookStateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return planHookRecord{}, nil
		}
		return planHookRecord{}, fmt.Errorf("read %s: %w", path, err)
	}
	var record planHookRecord
	if err := json.Unmarshal(data, &record); err != nil {
		// Local state is advisory: a corrupt record means "no passing run" and
		// reruns the hook rather than blocking the push.
		return planHookRecord{}, nil
	}
	return record, nil
}

func writePlanHookRecord(draftDir string, record planHookRecord) error {
	path := filepath.Join(draftDir, planHookStateFile)
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
