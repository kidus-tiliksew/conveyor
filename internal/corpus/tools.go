// Package corpus exposes the small, read-only confirmed-document tool surface
// shared by planning and triage. It deliberately has no repository or mutation
// dependencies: callers can list summaries, then explicitly read current
// confirmed requirement and System Design bodies.
package corpus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

const (
	ListRequirements  = "list_requirements"
	ReadRequirement   = "read_requirement"
	ListSystemDesigns = "list_system_designs"
	ReadSystemDesign  = "read_system_design"
	ListDecisions     = "list_decisions"
	maxSummaryRunes   = 240
)

// Store is intentionally narrower than store.Store so this package cannot
// acquire write capabilities as its consumers evolve.
type Store interface {
	ListRequirements(context.Context, bool) ([]core.Requirement, error)
	GetRequirement(context.Context, string) (core.Requirement, error)
	GetRequirementVersion(context.Context, string, int) (core.RequirementVersion, error)
	ListSystemDesigns(context.Context, bool) ([]core.SystemDesign, error)
	GetSystemDesign(context.Context, string) (core.SystemDesign, error)
	GetSystemDesignVersion(context.Context, string, int) (core.SystemDesignVersion, error)
	ListDecisions(context.Context) ([]core.Decision, error)
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// FunctionTool exposes Tool through the strict native Responses API function
// shape without duplicating the corpus definitions.
type FunctionTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

type RequirementSummary struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Slug    string `json:"slug"`
	Summary string `json:"summary"`
}

type SystemDesignSummary struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Slug     string `json:"slug"`
	Summary  string `json:"summary"`
}

type DecisionSummary struct {
	ID      string              `json:"id"`
	Status  core.DecisionStatus `json:"status"`
	Summary string              `json:"summary"`
}

func Tools() []Tool {
	empty := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}
	read := func(property, description string) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "required": []string{property}, "properties": map[string]any{
			property: map[string]any{"type": "string", "minLength": 1, "description": description},
		}}
	}
	return []Tool{
		{Name: ListRequirements, Description: "List confirmed requirements by identity and short summary; bodies are omitted.", Parameters: empty},
		{Name: ReadRequirement, Description: "Read the body of a requirement's current confirmed version.", Parameters: read("requirement_id", "Confirmed requirement identity to read.")},
		{Name: ListSystemDesigns, Description: "List confirmed System Designs by identity, category, and short summary; bodies are omitted.", Parameters: empty},
		{Name: ReadSystemDesign, Description: "Read the body of a System Design's current confirmed version.", Parameters: read("document_id", "Confirmed System Design identity to read.")},
		{Name: ListDecisions, Description: "List confirmed or superseded decisions by identity and short summary.", Parameters: empty},
	}
}

func FunctionTools() []FunctionTool {
	definitions := Tools()
	tools := make([]FunctionTool, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, FunctionTool{
			Type:        "function",
			Name:        definition.Name,
			Description: definition.Description,
			Parameters:  definition.Parameters,
			Strict:      true,
		})
	}
	return tools
}

func Names() []string {
	return []string{ListRequirements, ReadRequirement, ListSystemDesigns, ReadSystemDesign, ListDecisions}
}

func IsTool(name string) bool {
	for _, candidate := range Names() {
		if name == candidate {
			return true
		}
	}
	return false
}

type Executor struct{ Store Store }

func (e Executor) Execute(ctx context.Context, name, argumentsJSON string) (any, error) {
	if e.Store == nil {
		return nil, fmt.Errorf("corpus store is not configured")
	}
	switch name {
	case ListRequirements:
		if err := decodeNoArgs(argumentsJSON); err != nil {
			return nil, err
		}
		documents, err := e.Store.ListRequirements(ctx, false)
		if err != nil {
			return nil, err
		}
		items := make([]RequirementSummary, 0, len(documents))
		for _, document := range documents {
			if document.CurrentVersion <= 0 {
				continue
			}
			version, getErr := e.Store.GetRequirementVersion(ctx, document.ID, document.CurrentVersion)
			if getErr != nil {
				return nil, getErr
			}
			if !version.Confirmed {
				continue
			}
			items = append(items, RequirementSummary{ID: document.ID, Title: document.Title, Slug: document.Slug, Summary: summarize(version.Content)})
		}
		return items, nil
	case ReadRequirement:
		var args struct {
			RequirementID string `json:"requirement_id"`
		}
		if err := decode(argumentsJSON, &args); err != nil {
			return nil, err
		}
		args.RequirementID = strings.TrimSpace(args.RequirementID)
		if args.RequirementID == "" {
			return nil, fmt.Errorf("requirement_id is required")
		}
		document, err := e.Store.GetRequirement(ctx, args.RequirementID)
		if err != nil {
			return nil, err
		}
		if document.CurrentVersion <= 0 {
			return nil, fmt.Errorf("requirement %s has no confirmed version", document.ID)
		}
		version, err := e.Store.GetRequirementVersion(ctx, document.ID, document.CurrentVersion)
		if err != nil {
			return nil, err
		}
		if !version.Confirmed {
			return nil, fmt.Errorf("requirement %s current version is not confirmed", document.ID)
		}
		return map[string]any{"requirement": RequirementSummary{ID: document.ID, Title: document.Title, Slug: document.Slug, Summary: summarize(version.Content)}, "version": version.Version, "content": version.Content, "statements": version.Statements}, nil
	case ListSystemDesigns:
		if err := decodeNoArgs(argumentsJSON); err != nil {
			return nil, err
		}
		documents, err := e.Store.ListSystemDesigns(ctx, false)
		if err != nil {
			return nil, err
		}
		items := make([]SystemDesignSummary, 0, len(documents))
		for _, document := range documents {
			if document.CurrentVersion <= 0 {
				continue
			}
			version, getErr := e.Store.GetSystemDesignVersion(ctx, document.ID, document.CurrentVersion)
			if getErr != nil {
				return nil, getErr
			}
			if !version.Confirmed {
				continue
			}
			items = append(items, SystemDesignSummary{ID: document.ID, Title: document.Title, Category: document.Category, Slug: document.Slug, Summary: summarize(version.Content)})
		}
		return items, nil
	case ReadSystemDesign:
		var args struct {
			DocumentID string `json:"document_id"`
		}
		if err := decode(argumentsJSON, &args); err != nil {
			return nil, err
		}
		args.DocumentID = strings.TrimSpace(args.DocumentID)
		if args.DocumentID == "" {
			return nil, fmt.Errorf("document_id is required")
		}
		document, err := e.Store.GetSystemDesign(ctx, args.DocumentID)
		if err != nil {
			return nil, err
		}
		if document.CurrentVersion <= 0 {
			return nil, fmt.Errorf("system design %s has no confirmed version", document.ID)
		}
		version, err := e.Store.GetSystemDesignVersion(ctx, document.ID, document.CurrentVersion)
		if err != nil {
			return nil, err
		}
		if !version.Confirmed {
			return nil, fmt.Errorf("system design %s current version is not confirmed", document.ID)
		}
		return map[string]any{"document": SystemDesignSummary{ID: document.ID, Title: document.Title, Category: document.Category, Slug: document.Slug, Summary: summarize(version.Content)}, "version": version.Version, "content": version.Content, "governs": version.Governs}, nil
	case ListDecisions:
		if err := decodeNoArgs(argumentsJSON); err != nil {
			return nil, err
		}
		decisions, err := e.Store.ListDecisions(ctx)
		if err != nil {
			return nil, err
		}
		items := make([]DecisionSummary, 0, len(decisions))
		for _, decision := range decisions {
			if decision.Status != core.DecisionConfirmed && decision.Status != core.DecisionSuperseded {
				continue
			}
			items = append(items, DecisionSummary{ID: decision.ID, Status: decision.Status, Summary: summarize(decision.Statement)})
		}
		return items, nil
	default:
		return nil, fmt.Errorf("unsupported corpus tool %q", name)
	}
}

func decodeNoArgs(raw string) error {
	var args struct{}
	return decode(raw, &args)
}

func decode(raw string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode tool arguments: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return fmt.Errorf("tool arguments contain more than one JSON value")
	} else if err != io.EOF {
		return fmt.Errorf("tool arguments contain trailing data: %w", err)
	}
	return nil
}

func summarize(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#>-*"))
		if line == "" || strings.HasPrefix(line, "```") {
			continue
		}
		if utf8.RuneCountInString(line) <= maxSummaryRunes {
			return line
		}
		runes := []rune(line)
		return strings.TrimSpace(string(runes[:maxSummaryRunes-1])) + "…"
	}
	return ""
}
