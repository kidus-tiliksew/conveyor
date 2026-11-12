package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/kidus-tiliksew/conveyor/internal/stageprefs"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The stage preference tools read and write the caller's own set, which
// applies in every workspace. workspace_id only admits the call under the
// ordinary view_workspace check; it never selects the set
// (component-stage-preferences; component-mcp-protocol invariants 5 and 7).
const (
	getStagePreferencesTool = "get_stage_preferences"
	setStagePreferenceTool  = "set_stage_preference"
)

func isStagePreferenceMCPTool(name string) bool {
	return name == getStagePreferencesTool || name == setStagePreferenceTool
}

// stagePreferenceMCPTools lists both tools. The set's shape matches the REST
// routes; null is unset.
func stagePreferenceMCPTools() []map[string]any {
	str := map[string]any{"type": "string"}
	entry := map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"harness", "model"},
		"properties": map[string]any{
			"harness": map[string]any{"type": "string", "minLength": 1, "maxLength": stageprefs.MaxNameBytes, "description": "Harness name as free text, such as claude-code or codex. At most 128 bytes, no control characters."},
			"model":   map[string]any{"type": "string", "minLength": 1, "maxLength": stageprefs.MaxNameBytes, "description": "Model name as free text. At most 128 bytes, no control characters."},
			"effort":  map[string]any{"type": []string{"string", "null"}, "maxLength": stageprefs.MaxEffortBytes, "description": "Optional effort as free text. At most 32 bytes, no control characters."},
		},
	}
	value := map[string]any{
		"description": "The stage's whole value; null clears the stage. For plan, implement, and verify: one entry {harness, model, effort}. For review: {primary, seats}, where primary is an entry or null and seats lists at most 8 entries, seat N at position N.",
		"anyOf": []any{
			map[string]any{"type": "null"},
			entry,
			map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"primary": map[string]any{"anyOf": []any{map[string]any{"type": "null"}, entry}},
					"seats":   map[string]any{"type": []string{"array", "null"}, "maxItems": stageprefs.MaxReviewSeats, "items": entry},
				},
			},
		},
	}
	return []map[string]any{
		{
			"name":        getStagePreferencesTool,
			"description": "Read your person's stage preferences: the harness, model, and effort to use for plan, implement, verify, and review. The result has version, updated_at, and stages with exactly the keys plan, implement, verify, and review. A stage that is null is unset. review holds primary and seats; seat N uses seat entry N or else the primary. The same set applies in every workspace, and workspace_id only admits the call. The set is advice to your own sessions: no factory decision reads it.",
			"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
			"inputSchema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"workspace_id": str}, "required": []string{"workspace_id"}},
		},
		{
			"name":        setStagePreferenceTool,
			"description": "Replace or clear one stage of your person's stage preferences, in one transaction that keeps every other stage. Save a stage only on your operator's direct answer or instruction. stage is plan, implement, verify, or review. value is the stage's whole value, and null clears it. The result is the whole set after the change. The same set applies in every workspace, and workspace_id only admits the call. A field other than harness, model, and effort, such as a host, sandbox, command, or credential, is refused.",
			"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": false},
			"inputSchema": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"workspace_id": str,
					"stage":        map[string]any{"type": "string", "enum": stageprefs.Stages},
					"value":        value,
				},
				"required": []string{"workspace_id", "stage", "value"},
			},
		},
	}
}

// callStagePreferenceMCP runs after callMCPTool admitted the workspace. The
// subject is always the credential's owner (req-stage-preferences AC-1.6,
// AC-2.3).
func (s *Server) callStagePreferenceMCP(ctx context.Context, name string, args map[string]any) (any, error) {
	allowed := []string{"workspace_id"}
	if name == setStagePreferenceTool {
		allowed = append(allowed, "stage", "value")
	}
	if err := stagePreferenceArgs(args, allowed); err != nil {
		return nil, err
	}
	if workspace, _ := args["workspace_id"].(string); strings.TrimSpace(workspace) == "" {
		return nil, errors.New("workspace_id is required")
	}
	service := s.stagePreferenceService()
	if service == nil {
		return nil, errors.New("stage preferences unavailable")
	}
	credential, ok := store.CredentialFromContext(ctx)
	if !ok || credential.OwnerUserID == "" {
		return nil, errWorkspaceNotVisible
	}
	if name == getStagePreferencesTool {
		set, err := service.Get(ctx, credential.OwnerUserID)
		return mcpStagePreferenceResult(ctx, name, set, err)
	}
	stage, ok := args["stage"].(string)
	if !ok {
		return nil, mcpStagePreferenceError(&stageprefs.ValidationError{Field: "stage", Message: "is required: plan, implement, verify, or review"})
	}
	raw, present := args["value"]
	if !present {
		return nil, mcpStagePreferenceError(&stageprefs.ValidationError{Field: "value", Message: "is required; send null to clear the stage"})
	}
	value, err := json.Marshal(raw)
	if err != nil {
		return nil, mcpStagePreferenceError(&stageprefs.ValidationError{Field: "value", Message: "must be JSON"})
	}
	set, err := service.SetStage(ctx, credential.OwnerUserID, stage, value)
	return mcpStagePreferenceResult(ctx, name, set, err)
}

// stagePreferenceArgs refuses an argument outside the tool's schema by name,
// so a host, sandbox, or credential is never silently ignored (AC-1.5).
func stagePreferenceArgs(args map[string]any, allowed []string) error {
	var unknown []string
	for key := range args {
		known := false
		for _, name := range allowed {
			known = known || key == name
		}
		if !known {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return mcpStagePreferenceError(&stageprefs.ValidationError{Field: unknown[0], Message: "is not a stage preference argument; only " + strings.Join(allowed, ", ") + " are allowed"})
}

// errStagePreferenceStore is the generic answer to a stage preference store
// failure. It wraps store.ErrBackendOperation, so the MCP response appends the
// request ID and carries no driver, host, or SQL text (component-persistence
// Errors invariants 11 to 13; component-mcp-protocol Transport invariant 2).
var errStagePreferenceStore = fmt.Errorf("stage preferences: %w", store.ErrBackendOperation)

// mcpStagePreferenceResult answers one tool call. Validation, the service's
// own refusals, retryable conflicts, and cancellation keep their meaning. Any
// other failure, including a driver or transport error a backend passed
// through untranslated, is logged redacted and bounded with the request ID,
// and the caller receives only the generic backend error.
func mcpStagePreferenceResult(ctx context.Context, tool string, set store.StagePreferences, err error) (any, error) {
	if err == nil {
		return set, nil
	}
	var invalid *stageprefs.ValidationError
	switch {
	case errors.As(err, &invalid):
		return nil, mcpStagePreferenceError(invalid)
	case errors.Is(err, context.Canceled):
		return nil, context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return nil, context.DeadlineExceeded
	case errors.Is(err, stageprefs.ErrVersionConflict):
		return nil, stageprefs.ErrVersionConflict
	case errors.Is(err, stageprefs.ErrNoUser):
		return nil, stageprefs.ErrNoUser
	case errors.Is(err, store.ErrRetryable):
		return nil, store.ErrRetryable
	}
	log.Printf("mcp: stage preferences store failure: request_id=%q tool=%q error=%q", middleware.GetReqID(ctx), tool, redactedErrorLogText(err))
	return nil, errStagePreferenceStore
}

// mcpStagePreferenceError names the code, the field, and a seat's position.
func mcpStagePreferenceError(invalid *stageprefs.ValidationError) error {
	if invalid.Position > 0 {
		return fmt.Errorf("validation_failed: %s (review seat %d) %s", invalid.Field, invalid.Position, invalid.Message)
	}
	return fmt.Errorf("validation_failed: %s %s", invalid.Field, invalid.Message)
}
