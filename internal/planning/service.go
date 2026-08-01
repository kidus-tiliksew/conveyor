// Package planning runs the bounded, in-process planning conversation over
// Conveyor's durable requirement, spec, artifact, and session contracts.
package planning

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const (
	DefaultMaxSteps        = 8
	DefaultMaxCallsPerStep = 4
	DefaultMaxContextBytes = 1 << 20
	DefaultMaxToolBytes    = 256 << 10
	DefaultMaxDuration     = 20 * time.Minute
)

type Emitter func(map[string]any) error

type UserMessage struct {
	Content string
	Parts   json.RawMessage
}

type BlueprintFinalizer func(
	context.Context,
	string,
	string,
	string,
	string,
	pipeline.StructuredSpec,
	string,
) (core.Task, core.SpecVersion, error)

type Service struct {
	Store             store.Store
	Agent             inprocess.Agent
	ConfigProvider    func(context.Context) (*config.Config, error)
	Git               *gitx.Manager
	FinalizeBlueprint BlueprintFinalizer
	Model             string
	Effort            string
	Prompt            string
	MaxSteps          int
	MaxCallsPerStep   int
	MaxContextBytes   int
	MaxToolBytes      int
	MaxDuration       time.Duration
}

type decision struct {
	ResponseText string     `json:"response_text"`
	ToolCalls    []toolCall `json:"tool_calls"`
}

type toolCall struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ArgumentsJSON string `json:"arguments_json"`
}

type produced struct {
	RequirementID string
	TaskID        string
}

type toolExecution struct {
	Output      any
	Produced    *produced
	Exploration bool
}

func (s *Service) CreateSession(ctx context.Context, title, requirementContextID, modelOverride string) (core.PlanningSession, error) {
	if s == nil || s.Store == nil || s.ConfigProvider == nil {
		return core.PlanningSession{}, fmt.Errorf("planning session configuration is unavailable")
	}
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return core.PlanningSession{}, err
	}
	if cfg.ExecutionSettings == nil {
		return core.PlanningSession{}, fmt.Errorf("planning execution settings are unavailable")
	}
	settings := cfg.ExecutionSettings.ControlPlane.Planning
	model := strings.TrimSpace(modelOverride)
	if model == "" {
		model = settings.Model
	}
	allowed := false
	for _, candidate := range cfg.PlanningModels {
		allowed = allowed || candidate == model
	}
	if !allowed {
		return core.PlanningSession{}, fmt.Errorf(
			"planning model %q is not allowlisted; configured models: %s",
			model, strings.Join(cfg.PlanningModels, ", "),
		)
	}
	if len(cfg.Repos) == 0 {
		return core.PlanningSession{}, fmt.Errorf("planning requires at least one configured repository")
	}
	manager := s.Git
	if manager == nil {
		manager = gitx.NewManager(cfg.CacheDir, "")
	}
	primary := cfg.Repos[0]
	snapshot, err := manager.PinSnapshot(ctx, primary.URL, primary.Base)
	if err != nil {
		return core.PlanningSession{}, fmt.Errorf("pin primary planning repository %s: %w", primary.Name, err)
	}
	return s.Store.CreatePlanningSession(ctx, core.PlanningSession{
		ID: "session-" + core.NewTaskID(), Title: strings.TrimSpace(title),
		RequirementContextID: requirementContextID,
		Model:                model, Effort: settings.Effort,
		ExplorationOutputTokens: settings.ExplorationOutputTokens,
		PrimaryRepo:             primary.Name,
		PinnedRevisions:         map[string]string{primary.Name: snapshot.Revision},
	})
}

func (s *Service) Run(ctx context.Context, sessionID string, user UserMessage, emit Emitter) error {
	if s == nil || s.Store == nil || s.Agent == nil {
		return fmt.Errorf("planning service is unavailable")
	}
	if emit == nil {
		return fmt.Errorf("planning stream emitter is required")
	}
	return s.Store.WithPlanningSessionRun(ctx, sessionID, func(lockedCtx context.Context) error {
		return s.runClaimed(lockedCtx, sessionID, user, emit)
	})
}

func (s *Service) runClaimed(ctx context.Context, sessionID string, user UserMessage, emit Emitter) (runErr error) {
	pending := map[string]toolCall{}
	defer func() {
		if len(pending) != 0 {
			if err := s.appendSyntheticToolResults(ctx, sessionID, pending, runErr); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("persist synthetic planning tool result: %w", err))
			}
		}
	}()
	session, err := s.Store.GetPlanningSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if session.Status != core.PlanningSessionActive {
		return fmt.Errorf("planning session %s is %s and accepts no further messages", sessionID, session.Status)
	}
	user.Content = strings.TrimSpace(user.Content)
	if user.Content == "" {
		return fmt.Errorf("user message content is required")
	}
	if len(user.Content) > 64<<10 {
		return fmt.Errorf("user message exceeds 64 KiB")
	}
	if len(user.Parts) == 0 {
		user.Parts = core.JSONPayload([]map[string]any{{"type": "text", "text": user.Content}})
	}
	if len(user.Parts) > 64<<10 {
		return fmt.Errorf("user message parts exceed 64 KiB; upload large context as an artifact")
	}
	var userParts []any
	if err = json.Unmarshal(user.Parts, &userParts); err != nil {
		return fmt.Errorf("user message parts must be a JSON array: %w", err)
	}
	if userParts == nil {
		return fmt.Errorf("user message parts must be a JSON array")
	}
	if _, err = s.Store.AppendPlanningMessage(ctx, core.PlanningMessage{
		SessionID: sessionID, Role: core.PlanningMessageUser,
		Content: user.Content, Parts: user.Parts,
	}); err != nil {
		return err
	}

	model, effort, routeTimeout, err := s.modelSettings(ctx, session)
	if err != nil {
		return err
	}
	maxDuration := s.MaxDuration
	if maxDuration <= 0 {
		maxDuration = DefaultMaxDuration
	}
	if routeTimeout > 0 && routeTimeout < maxDuration {
		maxDuration = routeTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, maxDuration)
	defer cancel()

	messageID := "message-" + core.NewTaskID()
	if err = emit(map[string]any{"type": "start", "messageId": messageID}); err != nil {
		return err
	}
	if err = emit(map[string]any{"type": "start-step"}); err != nil {
		return err
	}

	maxSteps := s.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}
	maxCalls := s.MaxCallsPerStep
	if maxCalls <= 0 {
		maxCalls = DefaultMaxCallsPerStep
	}
	for step := 1; step <= maxSteps; step++ {
		if step > 1 {
			if err = emit(map[string]any{"type": "start-step"}); err != nil {
				return err
			}
		}
		session, err = s.Store.GetPlanningSession(runCtx, sessionID)
		if err != nil {
			return err
		}
		if session.Status != core.PlanningSessionActive {
			return fmt.Errorf("planning session %s is %s and accepts no further messages", sessionID, session.Status)
		}
		messages, listErr := s.Store.ListPlanningMessages(runCtx, sessionID)
		if listErr != nil {
			return listErr
		}
		prompt, promptErr := s.prompt(runCtx, session, messages, step, maxSteps)
		if promptErr != nil {
			return promptErr
		}
		result, runErr := s.Agent.Run(runCtx, model, inprocess.Input{
			Prompt: prompt, Effort: effort,
			OutputSchema: &inprocess.OutputSchema{Name: "planning_step", Schema: decisionSchema()},
		})
		if runErr != nil {
			return fmt.Errorf("planning model step %d: %w", step, runErr)
		}
		next, parseErr := parseDecision(result.Output)
		if parseErr != nil {
			return fmt.Errorf("planning model step %d: %w", step, parseErr)
		}
		if len(next.ToolCalls) > maxCalls {
			return fmt.Errorf("planning model step %d requested %d tools; maximum is %d", step, len(next.ToolCalls), maxCalls)
		}
		if next.ResponseText == "" && len(next.ToolCalls) == 0 {
			return fmt.Errorf("planning model step %d returned neither text nor a tool call", step)
		}
		if containsFinalize(next.ToolCalls) && len(next.ToolCalls) != 1 {
			return fmt.Errorf("a finalize tool must be the only tool call in its step")
		}

		assistantParts := make([]map[string]any, 0, len(next.ToolCalls)+1)
		if next.ResponseText != "" {
			textID := "text-" + core.NewTaskID()
			assistantParts = append(assistantParts,
				map[string]any{"type": "text-start", "id": textID},
				map[string]any{"type": "text-delta", "id": textID, "delta": next.ResponseText},
				map[string]any{"type": "text-end", "id": textID},
			)
		}
		seenCalls := map[string]bool{}
		for _, call := range next.ToolCalls {
			if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
				return fmt.Errorf("planning tool calls require id and name")
			}
			if seenCalls[call.ID] {
				return fmt.Errorf("planning tool call id %q is duplicated", call.ID)
			}
			seenCalls[call.ID] = true
			var input any
			if err = json.Unmarshal([]byte(call.ArgumentsJSON), &input); err != nil {
				return fmt.Errorf("planning tool %s arguments: %w", call.Name, err)
			}
			assistantParts = append(assistantParts, map[string]any{
				"type": "tool-input-available", "toolCallId": call.ID,
				"toolName": call.Name, "input": input,
			})
		}
		if _, err = s.Store.AppendPlanningMessage(runCtx, core.PlanningMessage{
			SessionID: sessionID, Role: core.PlanningMessageAssistant,
			Content: next.ResponseText, Parts: core.JSONPayload(assistantParts),
		}); err != nil {
			return err
		}
		for _, call := range next.ToolCalls {
			pending[call.ID] = call
		}
		for _, part := range assistantParts {
			if err = emit(part); err != nil {
				return err
			}
		}

		if len(next.ToolCalls) == 0 {
			if err = emit(map[string]any{"type": "finish-step"}); err != nil {
				return err
			}
			return emit(map[string]any{"type": "finish", "finishReason": "stop"})
		}

		if containsFinalize(next.ToolCalls) {
			call := next.ToolCalls[0]
			var chunk map[string]any
			err = s.Store.WithPlanningSessionFinalization(runCtx, session.ID, func(lockedCtx context.Context) error {
				execution, executeErr := s.executeTool(lockedCtx, session, call, model)
				if executeErr != nil {
					return fmt.Errorf("planning tool %s: %w", call.Name, executeErr)
				}
				if execution.Produced == nil {
					return fmt.Errorf("planning tool %s did not produce final lineage", call.Name)
				}
				output, marshalErr := s.boundedOutput(execution.Output)
				if marshalErr != nil {
					return fmt.Errorf("planning tool %s: %w", call.Name, marshalErr)
				}
				chunk = map[string]any{
					"type": "tool-output-available", "toolCallId": call.ID, "toolName": call.Name, "output": output,
				}
				if _, appendErr := s.Store.AppendPlanningMessage(lockedCtx, core.PlanningMessage{
					SessionID: sessionID, Role: core.PlanningMessageTool,
					Parts: core.JSONPayload([]map[string]any{chunk}),
				}); appendErr != nil {
					return appendErr
				}
				delete(pending, call.ID)
				return s.archiveAndFinalize(lockedCtx, session, *execution.Produced)
			})
			if err != nil {
				return err
			}
			if err = emit(chunk); err != nil {
				return err
			}
			if err = emit(map[string]any{"type": "finish-step"}); err != nil {
				return err
			}
			return emit(map[string]any{"type": "finish", "finishReason": "tool-calls"})
		}

		executions := make([]toolExecution, len(next.ToolCalls))
		executionErrors := make([]error, len(next.ToolCalls))
		var executionGroup sync.WaitGroup
		// Parallel exploration calls intentionally share a best-effort session
		// budget snapshot. Each complete attempt is charged durably, but calls in
		// this one step may all observe the same pre-step low-budget threshold.
		for index, call := range next.ToolCalls {
			executionGroup.Add(1)
			go func() {
				defer executionGroup.Done()
				executions[index], executionErrors[index] = s.executeTool(runCtx, session, call, model)
			}()
		}
		executionGroup.Wait()
		for index, call := range next.ToolCalls {
			execution, executeErr := executions[index], executionErrors[index]
			if executeErr != nil {
				return fmt.Errorf("planning tool %s: %w", call.Name, executeErr)
			}
			if execution.Produced != nil {
				return fmt.Errorf("planning tool %s produced terminal lineage outside finalization", call.Name)
			}
			var output any
			if execution.Exploration {
				output = execution.Output
			} else {
				var marshalErr error
				output, marshalErr = s.boundedOutput(execution.Output)
				if marshalErr != nil {
					return fmt.Errorf("planning tool %s: %w", call.Name, marshalErr)
				}
			}
			chunk := map[string]any{
				"type": "tool-output-available", "toolCallId": call.ID, "toolName": call.Name, "output": output,
			}
			if _, err = s.Store.AppendPlanningMessage(runCtx, core.PlanningMessage{
				SessionID: sessionID, Role: core.PlanningMessageTool,
				Parts: core.JSONPayload([]map[string]any{chunk}),
			}); err != nil {
				return err
			}
			delete(pending, call.ID)
			if err = emit(chunk); err != nil {
				return err
			}
		}
		if err = emit(map[string]any{"type": "finish-step"}); err != nil {
			return err
		}
	}
	return fmt.Errorf("planning agent reached the bounded %d-step limit without a final response", maxSteps)
}

func (s *Service) appendSyntheticToolResults(
	ctx context.Context,
	sessionID string,
	pending map[string]toolCall,
	runErr error,
) error {
	status := "failed"
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) || ctx.Err() != nil {
		status = "cancelled"
	}
	callIDs := make([]string, 0, len(pending))
	for callID := range pending {
		callIDs = append(callIDs, callID)
	}
	sort.Strings(callIDs)
	parts := make([]map[string]any, 0, len(callIDs))
	for _, callID := range callIDs {
		parts = append(parts, map[string]any{
			"type": "tool-output-error", "toolCallId": callID, "toolName": pending[callID].Name,
			"output": map[string]any{
				"ok": false, "status": status, "tool": pending[callID].Name,
				"error": "planning tool execution did not complete; retry the request",
			},
		})
	}
	appendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.Store.AppendPlanningMessage(appendCtx, core.PlanningMessage{
		SessionID: sessionID, Role: core.PlanningMessageTool, Parts: core.JSONPayload(parts),
	})
	return err
}

func (s *Service) modelSettings(ctx context.Context, session core.PlanningSession) (string, string, time.Duration, error) {
	if strings.TrimSpace(s.Model) != "" {
		return s.Model, s.Effort, 0, nil
	}
	if s.ConfigProvider == nil {
		return "", "", 0, fmt.Errorf("planning model configuration is unavailable")
	}
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return "", "", 0, err
	}
	var settings config.PlanningSettings
	if cfg.ExecutionSettings != nil {
		settings = cfg.ExecutionSettings.ControlPlane.Planning
	}
	if session.Model != "" {
		settings.Model = session.Model
	}
	if session.Effort != "" {
		settings.Effort = session.Effort
	}
	if strings.TrimSpace(settings.Model) == "" {
		return "", "", 0, fmt.Errorf("planning model is not configured")
	}
	var timeout time.Duration
	if settings.TimeoutText != "" {
		timeout, err = time.ParseDuration(settings.TimeoutText)
		if err != nil {
			return "", "", 0, fmt.Errorf("planning model timeout: %w", err)
		}
	}
	return settings.Model, settings.Effort, timeout, nil
}

func (s *Service) prompt(ctx context.Context, session core.PlanningSession, messages []core.PlanningMessage, step, maxSteps int) (string, error) {
	liveMessages := append([]core.PlanningMessage(nil), messages...)
	contextValue := struct {
		Session  core.PlanningSession   `json:"session"`
		Messages []core.PlanningMessage `json:"messages"`
		Step     int                    `json:"step"`
		MaxSteps int                    `json:"max_steps"`
	}{Session: session, Messages: liveMessages, Step: step, MaxSteps: maxSteps}
	contextJSON, err := json.Marshal(contextValue)
	if err != nil {
		return "", err
	}
	maxBytes := s.MaxContextBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxContextBytes
	}
	for _, result := range explorationResultMessages(liveMessages) {
		if len(contextJSON) <= maxBytes {
			break
		}
		liveMessages[result.messageIndex] = elideExplorationResult(
			liveMessages[result.messageIndex], result.callIDs,
		)
		contextValue.Messages = liveMessages
		contextJSON, err = json.Marshal(contextValue)
		if err != nil {
			return "", err
		}
	}
	if len(contextJSON) > maxBytes {
		return "", fmt.Errorf("planning context exceeds the %d-byte limit", maxBytes)
	}
	repositories := []string{}
	if s.ConfigProvider != nil {
		cfg, configErr := s.ConfigProvider(ctx)
		if configErr != nil {
			return "", configErr
		}
		for _, repo := range cfg.Repos {
			repositories = append(repositories, repo.Name)
		}
	}
	pins := make([]string, 0, len(session.PinnedRevisions))
	for repo, revision := range session.PinnedRevisions {
		pins = append(pins, repo+"@"+revision)
	}
	sort.Strings(pins)
	snapshotStatement := "You are exploring read-only snapshots: " + strings.Join(pins, ", ") +
		". Content cannot change during this session; never re-read expecting different content; writes are impossible."
	explorationSchemas, err := json.Marshal(explorationToolSchemas())
	if err != nil {
		return "", err
	}
	role := strings.TrimSpace(s.Prompt)
	if role == "" {
		return "", fmt.Errorf("planning role prompt is unavailable or empty")
	}
	return role +
		"\n\nConfigured workspace repositories: " + strings.Join(repositories, ", ") + "." +
		"\n" + snapshotStatement +
		"\nStrict exploration tool schemas:\n" + string(explorationSchemas) +
		"\n\nDurable conversation context:\n" + string(contextJSON), nil
}

type explorationResultRef struct {
	messageIndex int
	callIDs      map[string]bool
}

func explorationResultMessages(messages []core.PlanningMessage) []explorationResultRef {
	explorationCalls := map[string]bool{}
	results := make([]explorationResultRef, 0)
	for index, message := range messages {
		var parts []map[string]any
		if json.Unmarshal(message.Parts, &parts) != nil {
			continue
		}
		if message.Role == core.PlanningMessageAssistant {
			for _, part := range parts {
				name, _ := part["toolName"].(string)
				callID, _ := part["toolCallId"].(string)
				if callID != "" && isExplorationTool(name) {
					explorationCalls[callID] = true
				}
			}
			continue
		}
		if message.Role != core.PlanningMessageTool {
			continue
		}
		callIDs := map[string]bool{}
		for _, part := range parts {
			callID, _ := part["toolCallId"].(string)
			if explorationCalls[callID] {
				callIDs[callID] = true
			}
		}
		if len(callIDs) != 0 {
			results = append(results, explorationResultRef{messageIndex: index, callIDs: callIDs})
		}
	}
	return results
}

func elideExplorationResult(message core.PlanningMessage, explorationCalls map[string]bool) core.PlanningMessage {
	var parts []map[string]any
	if json.Unmarshal(message.Parts, &parts) != nil {
		return message
	}
	elided := 0
	for _, part := range parts {
		callID, _ := part["toolCallId"].(string)
		if !explorationCalls[callID] {
			continue
		}
		part["output"] = map[string]any{
			"elided":         true,
			"message":        "Older exploration output was elided from the live prompt; the full result remains in the durable transcript.",
			"transcript_seq": message.Seq,
		}
		elided++
	}
	if elided == len(parts) {
		message.Content = ""
	}
	message.Parts = core.JSONPayload(parts)
	return message
}

func isExplorationTool(name string) bool {
	switch name {
	case "list_files", "read_file", "grep", "history":
		return true
	default:
		return false
	}
}

func parseDecision(output string) (decision, error) {
	var value decision
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("response must match the planning-step schema: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return value, fmt.Errorf("response contains more than one JSON value")
		}
		return value, fmt.Errorf("response has trailing data: %w", err)
	}
	value.ResponseText = strings.TrimSpace(value.ResponseText)
	return value, nil
}

func decisionSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"response_text": map[string]any{"type": "string"},
			"tool_calls": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"id":             map[string]any{"type": "string", "minLength": 1},
						"name":           map[string]any{"type": "string", "enum": toolNames()},
						"arguments_json": map[string]any{"type": "string", "minLength": 2},
					},
					"required": []string{"id", "name", "arguments_json"},
				},
			},
		},
		"required": []string{"response_text", "tool_calls"},
	}
}

func explorationToolSchemas() []map[string]any {
	parameter := func(valueType, description string) map[string]any {
		return map[string]any{"type": valueType, "description": description}
	}
	repo := parameter("string", "Select a configured workspace repository; omit it to use the session primary repository.")
	return []map[string]any{
		{
			"name":        "list_files",
			"description": "List bounded file paths and blob sizes from an immutable repository snapshot.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"repo":  repo,
					"path":  parameter("string", "Narrow the listing to this repository-relative directory prefix."),
					"glob":  parameter("string", "Filter paths with this optional Git-style glob."),
					"depth": parameter("integer", "Limit descendant path depth; omit or use zero for recursive listing."),
				},
			},
		},
		{
			"name":        "read_file",
			"description": "Read a deterministic line-numbered window from one text blob in an immutable repository snapshot.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"path"},
				"properties": map[string]any{
					"repo":   repo,
					"path":   parameter("string", "Read this required repository-relative file path."),
					"offset": parameter("integer", "Start at this one-based line number; default to 1."),
					"limit":  parameter("integer", "Return at most this many lines; default to 400 and never exceed 1000."),
				},
			},
		},
		{
			"name":        "grep",
			"description": "Search text blobs with a bounded regular expression query against an immutable repository snapshot.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"pattern"},
				"properties": map[string]any{
					"repo":           repo,
					"pattern":        parameter("string", "Search with this required Git regular expression."),
					"path":           parameter("string", "Narrow the search with this optional repository-relative pathspec or glob."),
					"context":        parameter("integer", "Include zero to five lines of surrounding context; default to zero."),
					"mode":           map[string]any{"type": "string", "enum": []string{"content", "files_with_matches"}, "description": "Return matching content by default or only matching file paths."},
					"case_sensitive": parameter("boolean", "Override smart-case matching when explicitly set."),
				},
			},
		},
		{
			"name":        "history",
			"description": "Inspect bounded commit history and latest stat context for one path at the pinned revision.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"path"},
				"properties": map[string]any{
					"repo": repo,
					"path": parameter("string", "Inspect this required repository-relative path."),
					"n":    parameter("integer", "Return at most this many commits; default to 20 and never exceed 50."),
				},
			},
		},
	}
}

func toolNames() []string {
	return []string{
		"list_files", "read_file", "grep", "history",
		"list_requirements", "read_requirement", "list_approved_specs",
		"read_approved_spec", "read_artifact", "read_task_lineage",
		"draft_requirement", "revise_requirement", "finalize_requirement",
		"draft_blueprint", "revise_blueprint", "finalize_blueprint",
	}
}

func containsFinalize(calls []toolCall) bool {
	for _, call := range calls {
		if strings.HasPrefix(call.Name, "finalize_") {
			return true
		}
	}
	return false
}

func (s *Service) executeTool(ctx context.Context, session core.PlanningSession, call toolCall, model string) (toolExecution, error) {
	switch call.Name {
	case "list_requirements":
		var args struct{}
		if err := decodeArgs(call.ArgumentsJSON, &args); err != nil {
			return toolExecution{}, err
		}
		items, err := s.Store.ListRequirements(ctx)
		if err != nil {
			return toolExecution{}, err
		}
		return toolExecution{Output: items}, nil
	case "read_requirement":
		var args struct {
			RequirementID string `json:"requirement_id"`
			Version       int    `json:"version"`
		}
		if err := decodeArgs(call.ArgumentsJSON, &args); err != nil {
			return toolExecution{}, err
		}
		requirement, err := s.Store.GetRequirement(ctx, args.RequirementID)
		if err != nil {
			return toolExecution{}, err
		}
		if args.Version > 0 {
			version, versionErr := s.Store.GetRequirementVersion(ctx, args.RequirementID, args.Version)
			return toolExecution{Output: map[string]any{"requirement": requirement, "version": version}}, versionErr
		}
		versions, err := s.Store.ListRequirementVersions(ctx, args.RequirementID)
		return toolExecution{Output: map[string]any{"requirement": requirement, "versions": versions}}, err
	case "list_approved_specs":
		var args struct{}
		if err := decodeArgs(call.ArgumentsJSON, &args); err != nil {
			return toolExecution{}, err
		}
		tasks, err := s.Store.ListTasks(ctx)
		if err != nil {
			return toolExecution{}, err
		}
		type approved struct {
			Task core.Task        `json:"task"`
			Spec core.SpecVersion `json:"spec"`
		}
		items := make([]approved, 0)
		for _, task := range tasks {
			spec, exists, specErr := s.Store.GetLatestSpecVersion(ctx, task.ID)
			if specErr != nil {
				return toolExecution{}, specErr
			}
			if exists && spec.Approved {
				items = append(items, approved{Task: task, Spec: spec})
				if len(items) == 50 {
					break
				}
			}
		}
		return toolExecution{Output: items}, nil
	case "read_approved_spec":
		var args struct {
			TaskID string `json:"task_id"`
		}
		if err := decodeArgs(call.ArgumentsJSON, &args); err != nil {
			return toolExecution{}, err
		}
		task, err := s.Store.GetTask(ctx, args.TaskID)
		if err != nil {
			return toolExecution{}, err
		}
		spec, exists, err := s.Store.GetLatestSpecVersion(ctx, args.TaskID)
		if err != nil {
			return toolExecution{}, err
		}
		if !exists || !spec.Approved {
			return toolExecution{}, fmt.Errorf("task %s has no approved spec", args.TaskID)
		}
		return toolExecution{Output: map[string]any{"task": task, "spec": spec}}, nil
	case "read_artifact":
		var args struct {
			ArtifactID string `json:"artifact_id"`
		}
		if err := decodeArgs(call.ArgumentsJSON, &args); err != nil {
			return toolExecution{}, err
		}
		artifact, content, err := s.Store.GetArtifact(ctx, args.ArtifactID)
		if err != nil {
			return toolExecution{}, err
		}
		maxBytes := s.MaxToolBytes
		if maxBytes <= 0 {
			maxBytes = DefaultMaxToolBytes
		}
		if len(content) > maxBytes {
			return toolExecution{}, fmt.Errorf("artifact %s exceeds the %d-byte planning read limit", args.ArtifactID, maxBytes)
		}
		output := map[string]any{"artifact": artifact}
		if textualContentType(artifact.ContentType) {
			output["content"] = string(content)
		} else {
			output["content_base64"] = base64.StdEncoding.EncodeToString(content)
		}
		return toolExecution{Output: output}, nil
	case "read_task_lineage":
		var args struct {
			TaskID string `json:"task_id"`
		}
		if err := decodeArgs(call.ArgumentsJSON, &args); err != nil {
			return toolExecution{}, err
		}
		task, err := s.Store.GetTask(ctx, args.TaskID)
		if err != nil {
			return toolExecution{}, err
		}
		spec, exists, err := s.Store.GetLatestSpecVersion(ctx, args.TaskID)
		if err != nil {
			return toolExecution{}, err
		}
		events, err := s.Store.ListEvents(ctx, args.TaskID)
		if err != nil {
			return toolExecution{}, err
		}
		if len(events) > 100 {
			events = events[len(events)-100:]
		}
		output := map[string]any{"task": task, "events": events}
		if exists {
			output["latest_spec"] = spec
		}
		return toolExecution{Output: output}, nil
	case "list_files", "read_file", "grep", "history":
		return s.explorationTool(ctx, session, call)
	case "draft_requirement", "revise_requirement", "finalize_requirement":
		return s.requirementTool(ctx, session, call)
	case "draft_blueprint", "revise_blueprint", "finalize_blueprint":
		return s.blueprintTool(ctx, session, call, model)
	default:
		return toolExecution{}, fmt.Errorf("unsupported planning tool %q", call.Name)
	}
}

type explorationContext struct {
	session   core.PlanningSession
	repo      config.Repo
	manager   *gitx.Manager
	snapshot  gitx.Snapshot
	capTokens int
	lowBudget bool
}

func (s *Service) explorationTool(ctx context.Context, original core.PlanningSession, call toolCall) (toolExecution, error) {
	exploration, err := s.resolveExploration(ctx, original, repoArgument(call.ArgumentsJSON))
	if err != nil {
		return toolExecution{}, s.recordExplorationAttempt(ctx, original.ID, err)
	}
	var output string
	var refine string
	switch call.Name {
	case "list_files":
		output, refine, err = s.listFiles(ctx, exploration, call.ArgumentsJSON)
	case "read_file":
		output, refine, err = s.readFile(ctx, exploration, call.ArgumentsJSON)
	case "grep":
		output, refine, err = s.grepFiles(ctx, exploration, call.ArgumentsJSON)
	case "history":
		output, refine, err = s.history(ctx, exploration, call.ArgumentsJSON)
	default:
		err = fmt.Errorf("unsupported planning exploration tool %q", call.Name)
	}
	if err != nil {
		return toolExecution{}, s.recordExplorationAttempt(ctx, original.ID, err)
	}
	if exploration.lowBudget {
		output = strings.TrimRight(output, "\n") +
			"\nsession exploration budget low; prefer targeted reads"
	}
	if strings.Contains(strings.ToLower(output), "truncated") {
		output = strings.TrimRight(output, "\n") + fmt.Sprintf("\napplied cap: %d tokens", exploration.capTokens)
	}
	output = truncateExploration(output, exploration.capTokens, refine, call.Name == "grep")
	used := approximateTokens(output)
	if _, err = s.Store.RecordPlanningExplorationTokens(ctx, original.ID, used); err != nil {
		return toolExecution{}, err
	}
	return toolExecution{Output: output, Exploration: true}, nil
}

func (s *Service) recordExplorationAttempt(ctx context.Context, sessionID string, attemptErr error) error {
	// Failed calls still consumed bounded repository work. Charge at least one
	// token so the shared soft budget does not treat repeated failures as free.
	used := max(1, approximateTokens(attemptErr.Error()))
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.Store.RecordPlanningExplorationTokens(recordCtx, sessionID, used); err != nil {
		return errors.Join(attemptErr, fmt.Errorf("record failed exploration usage: %w", err))
	}
	return attemptErr
}

func repoArgument(raw string) string {
	var envelope struct {
		Repo string `json:"repo"`
	}
	_ = json.Unmarshal([]byte(raw), &envelope)
	return strings.TrimSpace(envelope.Repo)
}

func (s *Service) resolveExploration(
	ctx context.Context,
	original core.PlanningSession,
	repoName string,
) (explorationContext, error) {
	session, err := s.Store.GetPlanningSession(ctx, original.ID)
	if err != nil {
		return explorationContext{}, err
	}
	if s.ConfigProvider == nil {
		return explorationContext{}, fmt.Errorf("planning repository configuration is unavailable")
	}
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return explorationContext{}, err
	}
	if repoName == "" {
		repoName = session.PrimaryRepo
	}
	names := make([]string, 0, len(cfg.Repos))
	var selected *config.Repo
	for i := range cfg.Repos {
		names = append(names, cfg.Repos[i].Name)
		if cfg.Repos[i].Name == repoName {
			selected = &cfg.Repos[i]
		}
	}
	if selected == nil {
		return explorationContext{}, fmt.Errorf(
			"unknown planning repository %q; configured repositories: %s",
			repoName, strings.Join(names, ", "),
		)
	}
	manager := s.Git
	if manager == nil {
		manager = gitx.NewManager(cfg.CacheDir, "")
	}
	revision := session.PinnedRevisions[selected.Name]
	var snapshot gitx.Snapshot
	if revision == "" {
		candidate, pinErr := manager.PinSnapshot(ctx, selected.URL, selected.Base)
		if pinErr != nil {
			return explorationContext{}, fmt.Errorf("pin planning repository %s: %w", selected.Name, pinErr)
		}
		session, err = s.Store.PinPlanningSessionRepo(ctx, session.ID, selected.Name, candidate.Revision)
		if err != nil {
			return explorationContext{}, err
		}
		revision = session.PinnedRevisions[selected.Name]
	}
	snapshot, err = manager.OpenSnapshot(ctx, selected.URL, revision)
	if err != nil {
		return explorationContext{}, fmt.Errorf("open planning repository %s@%s: %w", selected.Name, revision, err)
	}
	capTokens := 0
	if cfg.ExecutionSettings != nil {
		capTokens = cfg.ExecutionSettings.ControlPlane.Planning.ExplorationOutputTokens
	}
	if capTokens <= 0 {
		capTokens = config.DefaultPlanningExplorationOutputTokens
	}
	low := session.ExplorationTokensUsed >= 15*capTokens
	if low {
		capTokens = max(1, capTokens/2)
	}
	return explorationContext{
		session: session, repo: *selected, manager: manager, snapshot: snapshot,
		capTokens: capTokens, lowBudget: low,
	}, nil
}

func (s *Service) listFiles(ctx context.Context, exploration explorationContext, raw string) (string, string, error) {
	var args struct {
		Repo  string `json:"repo"`
		Path  string `json:"path"`
		Glob  string `json:"glob"`
		Depth int    `json:"depth"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return "", "", err
	}
	if args.Depth < 0 {
		return "", "", fmt.Errorf("depth must not be negative")
	}
	prefix := strings.Trim(strings.TrimSpace(args.Path), "/")
	entries, truncated, err := exploration.manager.ListSnapshotTree(
		ctx, exploration.snapshot, prefix, exploration.capTokens*4,
	)
	if err != nil {
		return "", "", err
	}
	filtered := make([]gitx.TreeEntry, 0)
	for _, entry := range entries {
		if prefix != "" && entry.Path != prefix && !strings.HasPrefix(entry.Path, prefix+"/") {
			continue
		}
		relative := strings.TrimPrefix(strings.TrimPrefix(entry.Path, prefix), "/")
		if args.Depth > 0 && strings.Count(relative, "/")+1 > args.Depth {
			continue
		}
		if args.Glob != "" {
			matches, matchErr := path.Match(args.Glob, entry.Path)
			if matchErr != nil {
				return "", "", fmt.Errorf("glob: %w", matchErr)
			}
			if !matches {
				matches, _ = path.Match(args.Glob, path.Base(entry.Path))
			}
			if !matches {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Path < filtered[j].Path })
	limit := 500
	if exploration.lowBudget {
		limit /= 2
	}
	var lines []string
	for _, entry := range filtered[:min(len(filtered), limit)] {
		lines = append(lines, fmt.Sprintf("%s  %d", entry.Path, entry.Size))
	}
	if len(filtered) > limit {
		lines = append(lines, fmt.Sprintf(
			"…%d more files not shown; narrow with path or glob", len(filtered)-limit))
	}
	if truncated {
		lines = append(lines, "… repository listing truncated at git boundary; narrow with path or glob")
	}
	return strings.Join(lines, "\n"), "narrow with path or glob", nil
}

func (s *Service) readFile(ctx context.Context, exploration explorationContext, raw string) (string, string, error) {
	var args struct {
		Repo   string `json:"repo"`
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return "", "", err
	}
	if args.Offset == 0 {
		args.Offset = 1
	}
	if args.Limit == 0 {
		args.Limit = 400
	}
	maxLimit := 1000
	if exploration.lowBudget {
		maxLimit /= 2
	}
	if args.Offset < 1 {
		return "", "", fmt.Errorf("offset must be at least 1")
	}
	if args.Limit < 1 || args.Limit > maxLimit {
		return "", "", fmt.Errorf("limit must be between 1 and %d", maxLimit)
	}
	content, err := exploration.manager.ReadSnapshotTextBlob(
		ctx, exploration.snapshot, args.Path, exploration.capTokens*4,
	)
	if err != nil {
		return "", "", err
	}
	if !utf8.Valid(content) || strings.IndexByte(string(content), 0) >= 0 {
		return "", "", fmt.Errorf("read_file supports text blobs only")
	}
	lines := strings.Split(string(content), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	if args.Offset > total {
		return fmt.Sprintf("%s (lines 0–0 of %d)", args.Path, total), "use an offset within the file", nil
	}
	end := min(total, args.Offset+args.Limit-1)
	selected := lines[args.Offset-1 : end]
	rendered := make([]string, 0, len(selected))
	for index, line := range selected {
		runes := []rune(line)
		if len(runes) > 1000 {
			line = string(runes[:1000]) + "… (line truncated)"
		}
		rendered = append(rendered, fmt.Sprintf("%6d\t%s", args.Offset+index, line))
	}
	maxBytes := exploration.capTokens * 4
	for len(rendered) > 1 && len(strings.Join(rendered, "\n"))+256 > maxBytes {
		rendered = rendered[:len(rendered)-1]
		end--
	}
	header := fmt.Sprintf("%s (lines %d–%d of %d)", args.Path, args.Offset, end, total)
	output := header + "\n" + strings.Join(rendered, "\n")
	if end < total {
		output += fmt.Sprintf("\nTotal file lines: %d; call again with offset=%d", total, end+1)
	}
	return output, fmt.Sprintf("call again with offset=%d", end+1), nil
}

func (s *Service) grepFiles(ctx context.Context, exploration explorationContext, raw string) (string, string, error) {
	var args struct {
		Repo          string `json:"repo"`
		Pattern       string `json:"pattern"`
		Path          string `json:"path"`
		Context       int    `json:"context"`
		Mode          string `json:"mode"`
		CaseSensitive *bool  `json:"case_sensitive"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return "", "", err
	}
	if args.Pattern == "" {
		return "", "", fmt.Errorf("pattern is required")
	}
	if args.Context < 0 || args.Context > 5 {
		return "", "", fmt.Errorf("context must be between 0 and 5")
	}
	if args.Mode == "" {
		args.Mode = "content"
	}
	if args.Mode != "content" && args.Mode != "files_with_matches" {
		return "", "", fmt.Errorf("mode must be content or files_with_matches")
	}
	caseInsensitive := !containsUpper(args.Pattern)
	if args.CaseSensitive != nil {
		caseInsensitive = !*args.CaseSensitive
	}
	limit := 200
	if args.Mode == "files_with_matches" {
		limit = 100
	}
	if exploration.lowBudget {
		limit /= 2
	}
	output, err := exploration.manager.GrepSnapshot(
		ctx, exploration.snapshot, args.Pattern, args.Path, args.Context,
		args.Mode == "files_with_matches", caseInsensitive,
		limit, exploration.capTokens*4,
	)
	if err != nil {
		return "", "", err
	}
	output = strings.ReplaceAll(output, exploration.snapshot.Revision+":", "")
	output = strings.ReplaceAll(output, exploration.snapshot.Revision+"-", "")
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	if len(lines) > limit {
		originalLines := len(lines)
		originalTokens := approximateTokens(strings.Join(lines, "\n"))
		prefix := fmt.Sprintf("Warning: truncated output (original token count: %d)\n", originalTokens)
		suffix := fmt.Sprintf(
			"\nTotal output lines: %d\nrefine with repo, path, pattern, or mode", originalLines)
		headCount := limit / 2
		kept := append([]string(nil), lines[:headCount]...)
		kept = append(kept, fmt.Sprintf("… %d middle lines omitted …", len(lines)-limit))
		kept = append(kept, lines[len(lines)-(limit-headCount):]...)
		return prefix + strings.Join(kept, "\n") + suffix,
			"refine with repo, path, pattern, or mode", nil
	}
	return strings.Join(lines, "\n"), "refine with repo, path, pattern, or mode", nil
}

func (s *Service) history(ctx context.Context, exploration explorationContext, raw string) (string, string, error) {
	var args struct {
		Repo string `json:"repo"`
		Path string `json:"path"`
		N    int    `json:"n"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return "", "", err
	}
	if args.N == 0 {
		args.N = 20
	}
	maxN := 50
	if exploration.lowBudget {
		maxN /= 2
	}
	if args.N < 1 || args.N > maxN {
		return "", "", fmt.Errorf("n must be between 1 and %d", maxN)
	}
	output, err := exploration.manager.SnapshotHistory(
		ctx, exploration.snapshot, args.Path, args.N, exploration.capTokens*4,
	)
	return output, "reduce n or narrow path", err
}

func truncateExploration(output string, capTokens int, refine string, middle bool) string {
	maxBytes := max(1, capTokens*4)
	if len(output) <= maxBytes {
		return output
	}
	originalTokens := approximateTokens(output)
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	totalLines := len(lines)
	prefix := fmt.Sprintf("Warning: truncated output (original token count: %d; applied cap: %d tokens)\n", originalTokens, capTokens)
	suffix := fmt.Sprintf("\nTotal output lines: %d\n%s", totalLines, refine)
	marker := "\n… middle omitted …\n"
	metadata := prefix + suffix
	if len(metadata) >= maxBytes {
		return fitUTF8(metadata, maxBytes)
	}
	remaining := maxBytes - len(prefix) - len(suffix)
	if !middle || remaining <= len(marker) {
		return prefix + fitUTF8(output, remaining) + suffix
	}
	payload := remaining - len(marker)
	headBytes := payload / 2
	tailBytes := payload - headBytes
	head := fitUTF8(output, headBytes)
	tail := fitUTF8FromEnd(output, tailBytes)
	return prefix + head + marker + tail + suffix
}

func fitUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func fitUTF8FromEnd(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[len(value)-maxBytes:]
	for !utf8.ValidString(value) {
		value = value[1:]
	}
	return value
}

func approximateTokens(value string) int {
	return (len(value) + 3) / 4
}

func containsUpper(value string) bool {
	for _, character := range value {
		if unicode.IsUpper(character) {
			return true
		}
	}
	return false
}

type requirementArgs struct {
	RequirementID string                      `json:"requirement_id"`
	Title         string                      `json:"title"`
	Prose         string                      `json:"prose"`
	Statements    []core.RequirementStatement `json:"statements"`
}

func (s *Service) requirementTool(ctx context.Context, session core.PlanningSession, call toolCall) (toolExecution, error) {
	var args requirementArgs
	if err := decodeArgs(call.ArgumentsJSON, &args); err != nil {
		return toolExecution{}, err
	}
	document, err := pipeline.RenderRequirementDocument(args.Prose, args.Statements)
	if err != nil {
		return toolExecution{}, err
	}
	if call.Name == "draft_requirement" {
		if strings.TrimSpace(args.Title) == "" {
			return toolExecution{}, fmt.Errorf("title is required")
		}
		return toolExecution{Output: map[string]any{"title": strings.TrimSpace(args.Title), "content": document.Markdown, "statements": document.Statements}}, nil
	}
	if call.Name == "revise_requirement" {
		if strings.TrimSpace(args.RequirementID) == "" {
			return toolExecution{}, fmt.Errorf("requirement_id is required")
		}
		requirement, err := s.Store.GetRequirement(ctx, args.RequirementID)
		if err != nil {
			return toolExecution{}, err
		}
		versions, err := s.Store.ListRequirementVersions(ctx, args.RequirementID)
		if err != nil {
			return toolExecution{}, err
		}
		issued := make([]string, 0)
		for _, version := range versions {
			for _, statement := range version.Statements {
				issued = append(issued, statement.ID)
			}
		}
		if err = core.ValidateRequirementRevision(requirement.StatementHighWaterMark, issued, document.Statements); err != nil {
			return toolExecution{}, err
		}
		return toolExecution{Output: map[string]any{"requirement": requirement, "content": document.Markdown, "statements": document.Statements}}, nil
	}

	requirementID := strings.TrimSpace(args.RequirementID)
	var requirement core.Requirement
	var version core.RequirementVersion
	if requirementID == "" {
		requirementID = "req-" + plannedID(session.ID)
		title := strings.TrimSpace(args.Title)
		if title == "" {
			return toolExecution{}, fmt.Errorf("title is required for a new requirement")
		}
		if existing, getErr := s.Store.GetRequirement(ctx, requirementID); getErr == nil {
			versions, listErr := s.Store.ListRequirementVersions(ctx, requirementID)
			if listErr != nil {
				return toolExecution{}, fmt.Errorf("resume requirement %s: %w", requirementID, listErr)
			}
			if len(versions) == 0 {
				return toolExecution{}, fmt.Errorf("resume requirement %s: no versions found", requirementID)
			}
			latest := versions[len(versions)-1]
			if existing.Title != title || latest.OriginSessionID != session.ID {
				return toolExecution{}, fmt.Errorf("planning requirement %s already exists with different input", requirementID)
			}
			requirement = existing
			if latest.Content == document.Markdown {
				version = latest
			} else {
				version, err = s.Store.ProposeRequirementVersion(ctx, core.RequirementVersion{
					RequirementID: requirementID, Content: document.Markdown,
					Statements: document.Statements, Origin: core.RequirementOriginChat,
					OriginSessionID: session.ID,
				})
				if err != nil {
					return toolExecution{}, err
				}
			}
		} else {
			requirement, version, err = s.createRequirementWithAvailableSlug(ctx,
				requirementID, title, core.RequirementVersion{
					Content: document.Markdown, Statements: document.Statements,
					Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
				})
			if err != nil {
				return toolExecution{}, err
			}
		}
	} else {
		requirement, err = s.Store.GetRequirement(ctx, requirementID)
		if err != nil {
			return toolExecution{}, err
		}
		if title := strings.TrimSpace(args.Title); title != "" && title != requirement.Title {
			return toolExecution{}, fmt.Errorf("requirement title is immutable; got %q, want %q", title, requirement.Title)
		}
		versions, listErr := s.Store.ListRequirementVersions(ctx, requirementID)
		if listErr != nil {
			return toolExecution{}, listErr
		}
		if len(versions) > 0 {
			latest := versions[len(versions)-1]
			if latest.Content == document.Markdown && latest.OriginSessionID == session.ID {
				version = latest
			}
		}
		if version.Version == 0 {
			version, err = s.Store.ProposeRequirementVersion(ctx, core.RequirementVersion{
				RequirementID: requirementID, Content: document.Markdown,
				Statements: document.Statements, Origin: core.RequirementOriginChat,
				OriginSessionID: session.ID,
			})
			if err != nil {
				return toolExecution{}, err
			}
		}
	}
	requirement, err = s.Store.GetRequirement(ctx, requirementID)
	if err != nil {
		return toolExecution{}, err
	}
	return toolExecution{
		Output:   map[string]any{"requirement": requirement, "version": version, "confirmation_required": true},
		Produced: &produced{RequirementID: requirementID},
	}, nil
}

func (s *Service) createRequirementWithAvailableSlug(
	ctx context.Context,
	id, title string,
	first core.RequirementVersion,
) (core.Requirement, core.RequirementVersion, error) {
	base := core.RequirementSlug(title)
	for ordinal := 1; ordinal > 0; ordinal++ {
		if err := ctx.Err(); err != nil {
			return core.Requirement{}, core.RequirementVersion{}, err
		}
		slug := core.RequirementSlugCandidate(base, ordinal)
		if slug == "" {
			return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf(
				"allocate requirement slug for %q: suffix space exhausted", title)
		}
		requirement, version, err := s.Store.CreateRequirement(ctx,
			core.Requirement{ID: id, Slug: slug, Title: title}, first)
		if !errors.Is(err, store.ErrRequirementSlugConflict) {
			return requirement, version, err
		}
	}
	return core.Requirement{}, core.RequirementVersion{}, fmt.Errorf(
		"allocate requirement slug for %q: suffix space exhausted", title)
}

type blueprintArgs struct {
	Title         string                            `json:"title"`
	Repo          string                            `json:"repo"`
	Markdown      string                            `json:"markdown"`
	Acceptance    []pipeline.AcceptanceCriterion    `json:"acceptance"`
	Decomposition []core.BlueprintDecompositionItem `json:"decomposition"`
}

func (args blueprintArgs) structured() pipeline.StructuredSpec {
	return pipeline.StructuredSpec{
		Markdown: args.Markdown, Acceptance: args.Acceptance, Decomposition: args.Decomposition,
	}
}

func (s *Service) blueprintTool(ctx context.Context, session core.PlanningSession, call toolCall, model string) (toolExecution, error) {
	var args blueprintArgs
	if err := decodeArgs(call.ArgumentsJSON, &args); err != nil {
		return toolExecution{}, err
	}
	raw, err := json.Marshal(args.structured())
	if err != nil {
		return toolExecution{}, err
	}
	parsed, err := pipeline.RenderStructuredSpec(string(raw))
	if err != nil {
		return toolExecution{}, err
	}
	if strings.TrimSpace(args.Title) == "" || strings.TrimSpace(args.Repo) == "" {
		return toolExecution{}, fmt.Errorf("blueprint title and repo are required")
	}
	if call.Name != "finalize_blueprint" {
		return toolExecution{Output: map[string]any{
			"title": strings.TrimSpace(args.Title), "repo": strings.TrimSpace(args.Repo),
			"content": parsed.Markdown, "acceptance_count": len(parsed.Acceptance),
			"decomposition_count": len(parsed.Decomposition),
		}}, nil
	}
	if s.FinalizeBlueprint == nil {
		return toolExecution{}, fmt.Errorf("blueprint finalization is unavailable")
	}
	taskID := plannedID(session.ID)
	task, version, err := s.FinalizeBlueprint(
		ctx, session.ID, taskID, args.Title, args.Repo, args.structured(), model,
	)
	if err != nil {
		return toolExecution{}, err
	}
	return toolExecution{
		Output: map[string]any{
			"task": task, "spec": version,
			"approval_required": task.State == core.TaskAwaiting,
		},
		Produced: &produced{TaskID: task.ID},
	}, nil
}

func (s *Service) archiveAndFinalize(ctx context.Context, session core.PlanningSession, value produced) error {
	messages, err := s.Store.ListPlanningMessages(ctx, session.ID)
	if err != nil {
		return err
	}
	transcript, err := json.Marshal(map[string]any{
		"session": session, "messages": messages,
	})
	if err != nil {
		return err
	}
	artifact := core.Artifact{
		Name: "planning-session-" + session.ID + ".json", ContentType: "application/json",
		Role: core.ArtifactRoleGeneratedAudit, TaskID: value.TaskID, RequirementID: value.RequirementID,
	}
	artifact, err = s.Store.CreateArtifact(ctx, artifact, transcript)
	if err != nil {
		return err
	}
	_, err = s.Store.FinalizePlanningSession(ctx, store.PlanningFinalizeRequest{
		SessionID: session.ID, RequirementID: value.RequirementID,
		TaskID: value.TaskID, TranscriptArtifactID: artifact.ID,
	})
	return err
}

func (s *Service) boundedOutput(value any) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	maxBytes := s.MaxToolBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxToolBytes
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("output exceeds the %d-byte limit", maxBytes)
	}
	var normalized any
	if err = json.Unmarshal(data, &normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func decodeArgs(raw string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("arguments contain more than one JSON value")
		}
		return err
	}
	return nil
}

func plannedID(sessionID string) string {
	value := strings.TrimSpace(strings.TrimPrefix(sessionID, "session-"))
	if value == "" {
		return core.NewTaskID()
	}
	return value
}

func textualContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return strings.HasPrefix(contentType, "text/") ||
		contentType == "application/json" ||
		contentType == "application/xml" ||
		contentType == "application/yaml" ||
		contentType == "application/x-yaml"
}
