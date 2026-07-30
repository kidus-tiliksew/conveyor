// Package planning runs the bounded, in-process planning conversation over
// Conveyor's durable requirement, spec, artifact, and session contracts.
package planning

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const (
	DefaultMaxSteps        = 8
	DefaultMaxCallsPerStep = 4
	DefaultMaxContextBytes = 512 << 10
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
	FinalizeBlueprint BlueprintFinalizer
	Model             string
	Effort            string
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
	Output   any
	Produced *produced
}

func (s *Service) Run(ctx context.Context, sessionID string, user UserMessage, emit Emitter) error {
	if s == nil || s.Store == nil || s.Agent == nil {
		return fmt.Errorf("planning service is unavailable")
	}
	if emit == nil {
		return fmt.Errorf("planning stream emitter is required")
	}
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

	model, effort, routeTimeout, err := s.modelSettings(ctx)
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
		messages, listErr := s.Store.ListPlanningMessages(runCtx, sessionID)
		if listErr != nil {
			return listErr
		}
		prompt, promptErr := s.prompt(session, messages, step, maxSteps)
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

		var terminal *produced
		for _, call := range next.ToolCalls {
			execution, executeErr := s.executeTool(runCtx, session, call, model)
			if executeErr != nil {
				return fmt.Errorf("planning tool %s: %w", call.Name, executeErr)
			}
			output, marshalErr := s.boundedOutput(execution.Output)
			if marshalErr != nil {
				return fmt.Errorf("planning tool %s: %w", call.Name, marshalErr)
			}
			chunk := map[string]any{
				"type": "tool-output-available", "toolCallId": call.ID, "output": output,
			}
			if _, err = s.Store.AppendPlanningMessage(runCtx, core.PlanningMessage{
				SessionID: sessionID, Role: core.PlanningMessageTool,
				Content: string(mustJSON(output)), Parts: core.JSONPayload([]map[string]any{chunk}),
			}); err != nil {
				return err
			}
			if err = emit(chunk); err != nil {
				return err
			}
			if execution.Produced != nil {
				terminal = execution.Produced
			}
		}
		if terminal != nil {
			if err = s.archiveAndFinalize(runCtx, session, *terminal); err != nil {
				return err
			}
			if err = emit(map[string]any{"type": "finish-step"}); err != nil {
				return err
			}
			return emit(map[string]any{"type": "finish", "finishReason": "tool-calls"})
		}
		if err = emit(map[string]any{"type": "finish-step"}); err != nil {
			return err
		}
	}
	return fmt.Errorf("planning agent reached the bounded %d-step limit without a final response", maxSteps)
}

func (s *Service) modelSettings(ctx context.Context) (string, string, time.Duration, error) {
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
	var settings config.ModelTimeoutSettings
	if cfg.ExecutionSettings != nil {
		settings = cfg.ExecutionSettings.ControlPlane.Triage
	}
	if settings.Model == "" {
		route := cfg.Routing.Stages[string(core.StageTriage)]
		settings.Model, settings.Effort, settings.TimeoutText = route.Model, route.Effort, route.TimeoutText
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

func (s *Service) prompt(session core.PlanningSession, messages []core.PlanningMessage, step, maxSteps int) (string, error) {
	contextValue := struct {
		Session  core.PlanningSession   `json:"session"`
		Messages []core.PlanningMessage `json:"messages"`
		Step     int                    `json:"step"`
		MaxSteps int                    `json:"max_steps"`
	}{Session: session, Messages: messages, Step: step, MaxSteps: maxSteps}
	contextJSON, err := json.Marshal(contextValue)
	if err != nil {
		return "", err
	}
	maxBytes := s.MaxContextBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxContextBytes
	}
	if len(contextJSON) > maxBytes {
		return "", fmt.Errorf("planning context exceeds the %d-byte limit", maxBytes)
	}
	return planningPrompt + "\n\nDurable conversation context:\n" + string(contextJSON), nil
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

func toolNames() []string {
	return []string{
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
	case "draft_requirement", "revise_requirement", "finalize_requirement":
		return s.requirementTool(ctx, session, call)
	case "draft_blueprint", "revise_blueprint", "finalize_blueprint":
		return s.blueprintTool(ctx, session, call, model)
	default:
		return toolExecution{}, fmt.Errorf("unsupported planning tool %q", call.Name)
	}
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
			if existing.Title != title || latest.Content != document.Markdown || latest.OriginSessionID != session.ID {
				return toolExecution{}, fmt.Errorf("planning requirement %s already exists with different input", requirementID)
			}
			requirement, version = existing, latest
		} else {
			requirement, version, err = s.Store.CreateRequirement(ctx,
				core.Requirement{ID: requirementID, Title: title},
				core.RequirementVersion{
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
		"session_id": session.ID, "workspace": session.Workspace, "messages": messages,
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

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

var planningPrompt = strings.TrimSpace(`
You are Conveyor's in-product planning agent. Help the operator turn intent
into either a versioned requirement proposal or a blueprint at the normal spec
gate. You never confirm a requirement, approve a spec, merge work, or bypass a
gate. Use tools for durable reads and validated drafts; do not claim you read or
wrote anything unless the corresponding tool succeeded.

Return one planning_step JSON object. response_text is operator-facing prose.
tool_calls contains zero or more calls, each with a unique id, an exact tool
name, and arguments_json containing one JSON object. A finalize tool must be
the only call in its step.

Tools:
- list_requirements {}
- read_requirement {"requirement_id":"req-...","version":0}
- list_approved_specs {}
- read_approved_spec {"task_id":"..."}
- read_artifact {"artifact_id":"..."}
- read_task_lineage {"task_id":"..."}
- draft_requirement {"requirement_id":"","title":"...","prose":"...","statements":[{"id":"REQ-1","statement":"..."}]}
- revise_requirement {"requirement_id":"req-...","title":"","prose":"...","statements":[...]}
- finalize_requirement {"requirement_id":"","title":"...","prose":"...","statements":[...]}
- draft_blueprint {"title":"...","repo":"...","markdown":"## Intent\n...\n## Non-goals\n...","acceptance":[{"id":"AC-1","criterion":"...","verify":"test","ref":null}],"decomposition":[]}
- revise_blueprint uses the same arguments as draft_blueprint
- finalize_blueprint uses the same arguments as draft_blueprint

Finalize a requirement only when the operator's stated intent is sufficiently
specific. It creates an unconfirmed version. Finalize a blueprint only when its
Intent, Non-goals, acceptance criteria, repository, and optional decomposition
are coherent. It creates a parent task and spec version at the unchanged
approval gate. Ask a concise question in response_text when required facts are
missing; do not finalize by guessing.
`)
