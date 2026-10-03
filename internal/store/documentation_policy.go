package store

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Documentation policy off reasons recorded when the gate is explicitly off.
const (
	DocumentationOffAbsent   = "absent"
	DocumentationOffNoGitHub = "no_github_repository"
)

// ValidateDocumentationPolicyPin checks the durable shape shared by all
// backends before a pin is stored.
func ValidateDocumentationPolicyPin(policy core.DocumentationPolicy) error {
	if policy.Enabled {
		sha, err := hex.DecodeString(policy.BaseSHA)
		if err != nil || (len(sha) != 20 && len(sha) != 32) || strings.ToLower(policy.BaseSHA) != policy.BaseSHA {
			return fmt.Errorf("documentation policy base SHA %q is invalid", policy.BaseSHA)
		}
		if !strings.HasPrefix(policy.ContentHash, "sha256:") || len(policy.ContentHash) != len("sha256:")+64 {
			return fmt.Errorf("documentation policy content hash %q is invalid", policy.ContentHash)
		}
		if len(policy.Paths) == 0 {
			return fmt.Errorf("an enabled documentation policy must declare at least one path")
		}
		if strings.TrimSpace(policy.NoneStatement) == "" {
			return fmt.Errorf("an enabled documentation policy must declare a docs-none statement")
		}
		return nil
	}
	if policy.OffReason != DocumentationOffAbsent && policy.OffReason != DocumentationOffNoGitHub {
		return fmt.Errorf("documentation policy off reason %q is invalid", policy.OffReason)
	}
	return nil
}

func NormalizeDocumentationPolicy(policy core.DocumentationPolicy) core.DocumentationPolicy {
	if policy.PinnedAt.IsZero() {
		policy.PinnedAt = time.Now().UTC()
	}
	if policy.Paths == nil {
		policy.Paths = []string{}
	}
	return policy
}

func NormalizeDocumentationGateEvidence(evidence core.DocumentationGateEvidence) (core.DocumentationGateEvidence, error) {
	if strings.TrimSpace(evidence.TaskID) == "" || strings.TrimSpace(evidence.HeadSHA) == "" {
		return evidence, fmt.Errorf("documentation gate evidence requires a task and head")
	}
	paths := make([]string, 0, len(evidence.MatchedPaths))
	seen := map[string]bool{}
	for _, raw := range evidence.MatchedPaths {
		item := strings.TrimSpace(raw)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		paths = append(paths, item)
	}
	sort.Strings(paths)
	evidence.MatchedPaths = paths
	if evidence.RecordedAt.IsZero() {
		evidence.RecordedAt = time.Now().UTC()
	}
	return evidence, nil
}

type memoryDocumentationEvidenceKey struct {
	taskID  string
	headSHA string
}

func (m *memory) PinTaskDocumentationPolicy(_ context.Context, taskID string, policy core.DocumentationPolicy) (bool, error) {
	if err := ValidateDocumentationPolicyPin(policy); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.tasks[taskID]; !exists {
		return false, fmt.Errorf("%w: task %s", ErrNotFound, taskID)
	}
	if _, exists := m.documentationPolicies[taskID]; exists {
		return false, nil
	}
	m.documentationPolicies[taskID] = NormalizeDocumentationPolicy(policy)
	return true, nil
}

func (m *memory) RecordDocumentationGateEvidence(_ context.Context, evidence core.DocumentationGateEvidence) error {
	normalized, err := NormalizeDocumentationGateEvidence(evidence)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.tasks[normalized.TaskID]; !exists {
		return fmt.Errorf("%w: task %s", ErrNotFound, normalized.TaskID)
	}
	m.documentationEvidence[memoryDocumentationEvidenceKey{taskID: normalized.TaskID, headSHA: normalized.HeadSHA}] = normalized
	return nil
}

func (m *memory) GetDocumentationGateEvidence(_ context.Context, taskID, headSHA string) (core.DocumentationGateEvidence, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	evidence, ok := m.documentationEvidence[memoryDocumentationEvidenceKey{taskID: taskID, headSHA: headSHA}]
	return evidence, ok, nil
}

func (m *memory) attachDocumentationPolicyLocked(task core.Task) core.Task {
	if policy, ok := m.documentationPolicies[task.ID]; ok {
		copy := policy
		copy.Paths = append([]string(nil), policy.Paths...)
		task.DocumentationPolicy = &copy
	}
	return task
}
