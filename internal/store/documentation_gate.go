package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/docsconfig"
)

var ErrDocumentationGateUnresolved = errors.New("documentation gate unresolved")
var ErrDocumentationUnresolved = errors.New("review documentation_assessment blocks approve")

// NormalizeDocumentationGateEvidence recomputes all derived evidence before any write (AC-9.3).
func NormalizeDocumentationGateEvidence(ctx context.Context, e core.DocumentationGateEvidence) (core.DocumentationGateEvidence, error) {
	if err := ctx.Err(); err != nil {
		return e, err
	}
	if strings.TrimSpace(e.TaskID) == "" {
		return e, fmt.Errorf("documentation evidence task_id is required")
	}
	for _, sha := range []string{e.HeadSHA, e.BaseSHA} {
		b, err := hex.DecodeString(sha)
		if err != nil || len(b) != 20 {
			return e, fmt.Errorf("documentation evidence requires full base and head SHA")
		}
	}
	if e.Status != "" {
		return e, fmt.Errorf("%w: %s", ErrDocumentationGateUnresolved, e.RefusalReason)
	}
	var c docsconfig.Config
	d := json.NewDecoder(strings.NewReader(string(e.PolicyJSON)))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return e, fmt.Errorf("documentation policy: %w", err)
	}
	if err := c.Validate(); err != nil {
		return e, err
	}
	e.ChangedPaths = normalizedDocumentationPaths(e.ChangedPaths)
	evaluation, err := c.Evaluate(ctx, e.ChangedPaths)
	if err != nil {
		return e, err
	}
	e.MatchedPaths = normalizedDocumentationPaths(evaluation.MatchedPaths)
	e.AffectedDocuments = evaluation.AffectedDocuments
	reason, qualified := docsconfig.MatchNoneStatement(e.AgentAuthoredBody, c.Rule.NoneStatement, c.Rule.ReasonRequired)
	e.NoneStatement, e.NoneReason = "", ""
	if qualified {
		e.NoneStatement, e.NoneReason = c.Rule.NoneStatement, reason
	}
	if len(e.AffectedDocuments) > 0 && !qualified {
		return e, fmt.Errorf("%w: affected documents not updated: %s; governed paths: %s; update the documents or supply %q%s", ErrDocumentationGateUnresolved, strings.Join(e.AffectedDocuments, ", "), strings.Join(normalizedDocumentationPaths(evaluation.GovernedPaths), ", "), c.Rule.NoneStatement, map[bool]string{true: " with a reason"}[c.Rule.ReasonRequired])
	}
	e.PolicyJSON, _ = json.Marshal(c)
	return e, nil
}
func normalizedDocumentationPaths(items []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range items {
		item := strings.TrimSpace(raw)
		if item != "" && !seen[item] {
			out = append(out, item)
			seen[item] = true
		}
	}
	sort.Strings(out)
	return out
}

// NormalizeRecordedDocumentationGateEvidence also admits failed refresh observations.
// Such observations inform review; they never admit an implementation submission.
func NormalizeRecordedDocumentationGateEvidence(ctx context.Context, e core.DocumentationGateEvidence) (core.DocumentationGateEvidence, error) {
	if e.Status == "" {
		return NormalizeDocumentationGateEvidence(ctx, e)
	}
	if err := ctx.Err(); err != nil {
		return e, err
	}
	if e.Status != "refused" && e.Status != "unavailable" || strings.TrimSpace(e.RefusalReason) == "" {
		return e, fmt.Errorf("documentation refresh requires a refused/unavailable status and reason")
	}
	if strings.TrimSpace(e.TaskID) == "" {
		return e, fmt.Errorf("documentation evidence task_id is required")
	}
	for _, sha := range []string{e.HeadSHA, e.BaseSHA} {
		b, err := hex.DecodeString(sha)
		if err != nil || len(b) != 20 {
			return e, fmt.Errorf("documentation evidence requires full base and head SHA")
		}
	}
	observed := e
	observed.Status, observed.RefusalReason = "", ""
	if e.Status == "refused" && json.Valid(e.PolicyJSON) && string(e.PolicyJSON) != "null" {
		normalized, err := NormalizeDocumentationGateEvidence(ctx, observed)
		if err == nil {
			return e, fmt.Errorf("documentation refresh marked refused without an admission failure")
		}
		e = normalized
		e.Status, e.RefusalReason = "refused", err.Error()
	}
	if !json.Valid(e.PolicyJSON) {
		e.PolicyJSON = json.RawMessage("null")
	}
	e.ChangedPaths = normalizedDocumentationPaths(e.ChangedPaths)
	e.MatchedPaths = normalizedDocumentationPaths(e.MatchedPaths)
	e.AffectedDocuments = normalizedDocumentationPaths(e.AffectedDocuments)
	return e, nil
}

func ValidateDocumentationAssessment(e *core.DocumentationGateEvidence, verdict string, a *core.DocumentationAssessment) error {
	if e == nil {
		if a != nil {
			return fmt.Errorf("review documentation_assessment supplied without documentation evidence")
		}
		return nil
	}
	if e.Status != "" && verdict == "approve" {
		return fmt.Errorf("%w: %s (%s); submit changes_requested", ErrDocumentationUnresolved, e.Status, e.RefusalReason)
	}
	if e.Status != "" && a == nil {
		return nil
	}
	if a == nil {
		return fmt.Errorf("review documentation_assessment is required when documentation evidence exists")
	}
	if !a.Applicable {
		return fmt.Errorf("review documentation_assessment must be applicable")
	}
	if strings.TrimSpace(a.Summary) == "" {
		return fmt.Errorf("review documentation_assessment summary is required")
	}
	seen := map[string]string{}
	for _, list := range []struct {
		name  string
		items []string
	}{{"updated_paths", a.UpdatedPaths}, {"unresolved", a.Unresolved}, {"conflicts", a.Conflicts}} {
		for _, item := range list.items {
			key := strings.TrimSpace(item)
			if key == "" {
				return fmt.Errorf("review documentation_assessment %s contains an empty finding", list.name)
			}
			if prior, ok := seen[key]; ok {
				return fmt.Errorf("review documentation_assessment finding %q appears in both %s and %s", key, prior, list.name)
			}
			seen[key] = list.name
		}
	}
	if e.Status == "" && !slices.Equal(normalizedDocumentationPaths(a.UpdatedPaths), normalizedDocumentationPaths(e.MatchedPaths)) {
		return fmt.Errorf("review documentation_assessment updated_paths do not match the recorded documentation gate evidence")
	}
	if verdict == "approve" && (len(a.Unresolved) > 0 || len(a.Conflicts) > 0) {
		return fmt.Errorf("%w: unresolved findings; correct the assessment or submit changes_requested", ErrDocumentationUnresolved)
	}
	return nil
}
func cloneDocumentationEvidence(e core.DocumentationGateEvidence) core.DocumentationGateEvidence {
	e.PolicyJSON = slices.Clone(e.PolicyJSON)
	e.ChangedPaths = slices.Clone(e.ChangedPaths)
	e.MatchedPaths = slices.Clone(e.MatchedPaths)
	e.AffectedDocuments = slices.Clone(e.AffectedDocuments)
	return e
}
func (m *memory) RecordDocumentationGateEvidence(ctx context.Context, e core.DocumentationGateEvidence) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	normalized, err := NormalizeRecordedDocumentationGateEvidence(ctx, e)
	if err != nil {
		return err
	}
	workspace, _ := WorkspaceFromContext(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if task, ok := m.tasks[e.TaskID]; !ok || (task.Workspace != "" && task.Workspace != workspace) {
		return ErrNotFound
	}
	key := memoryScopedKey{workspace: workspace, id: e.TaskID}
	if m.documentationEvidence == nil {
		m.documentationEvidence = map[memoryScopedKey]map[string]core.DocumentationGateEvidence{}
	}
	if m.documentationEvidence[key] == nil {
		m.documentationEvidence[key] = map[string]core.DocumentationGateEvidence{}
	}
	m.documentationEvidence[key][e.HeadSHA] = cloneDocumentationEvidence(normalized)
	return nil
}
func (m *memory) GetDocumentationGateEvidence(ctx context.Context, taskID, headSHA string) (core.DocumentationGateEvidence, error) {
	workspace, _ := WorkspaceFromContext(ctx)
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.documentationEvidence[memoryScopedKey{workspace: workspace, id: taskID}][headSHA]
	if !ok {
		return core.DocumentationGateEvidence{}, ErrNotFound
	}
	return cloneDocumentationEvidence(e), nil
}
