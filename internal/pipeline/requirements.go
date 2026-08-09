package pipeline

import (
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"gopkg.in/yaml.v3"
)

// RequirementStatement is the core statement type; requirement documents are
// validated here so the block contract lives beside the legacy spec parser
// without sharing its rules.
type RequirementStatement = core.RequirementStatement

// RequirementDocument is a validated requirement version body: free prose plus
// exactly one conveyor:requirements block.
type RequirementDocument struct {
	Markdown   string
	Statements []RequirementStatement
}

// ParseRequirementDocument validates a requirement document. Unlike ParseSpec
// it mandates no prose sections: a requirement states intent in the operator's
// own language, so only the machine block is constrained (design-document-corpus).
//
// The block is required and must appear exactly once. Its statements may be
// empty only for a migration seed, which never travels through this parser —
// see core.ConfirmableRequirementVersion for the confirmation-time rule.
func ParseRequirementDocument(output string) (RequirementDocument, error) {
	markdown := strings.TrimSpace(output)
	if markdown == "" {
		return RequirementDocument{}, fmt.Errorf("requirement document requires prose")
	}
	blocks := fences(markdown, "requirements")
	if len(blocks) == 0 {
		if opening := machineFenceNearMiss(markdown, "requirements"); opening != "" {
			return RequirementDocument{}, fmt.Errorf("requirement document requires exact ```conveyor:requirements fence; rejected near-miss opening %q", opening)
		}
		return RequirementDocument{}, fmt.Errorf("requirement document requires one conveyor:requirements block")
	}
	if len(blocks) != 1 {
		return RequirementDocument{}, fmt.Errorf("requirement document requires exactly one conveyor:requirements block; found %d", len(blocks))
	}
	var statements []RequirementStatement
	if err := decodeYAMLList(blocks[0], &statements); err != nil {
		return RequirementDocument{}, fmt.Errorf("requirements block: %w", err)
	}
	if len(statements) == 0 {
		return RequirementDocument{}, fmt.Errorf("requirements block must be a non-empty list")
	}
	if err := core.ValidateRequirementStatements(statements); err != nil {
		return RequirementDocument{}, err
	}
	// Prose must not be only the machine block: a requirement without intent
	// text is a checklist, not a living document.
	if strings.TrimSpace(requirementProse(markdown)) == "" {
		return RequirementDocument{}, fmt.Errorf("requirement document requires prose alongside its requirements block")
	}
	return RequirementDocument{Markdown: markdown, Statements: statements}, nil
}

// RenderRequirementDocument serializes prose plus the canonical machine block.
// Conveyor owns the fence exactly as it does for legacy specs, so callers supply
// only prose and statements. The result is re-parsed as the final invariant.
func RenderRequirementDocument(prose string, statements []RequirementStatement) (RequirementDocument, error) {
	trimmed := strings.TrimSpace(prose)
	if trimmed == "" {
		return RequirementDocument{}, fmt.Errorf("requirement prose is required")
	}
	if machineFenceNearMiss(trimmed, "requirements") != "" {
		return RequirementDocument{}, fmt.Errorf("requirement prose must not contain a conveyor:requirements fence; Conveyor serializes it")
	}
	if err := core.ValidateRequirementStatements(statements); err != nil {
		return RequirementDocument{}, err
	}
	if len(statements) == 0 {
		return RequirementDocument{}, fmt.Errorf("requirements block must be a non-empty list")
	}
	block, err := yaml.Marshal(statements)
	if err != nil {
		return RequirementDocument{}, fmt.Errorf("render requirements block: %w", err)
	}
	var rendered strings.Builder
	rendered.WriteString(trimmed)
	rendered.WriteString("\n\n```conveyor:requirements\n")
	rendered.Write(block)
	rendered.WriteString("```")
	document, err := ParseRequirementDocument(rendered.String())
	if err != nil {
		return RequirementDocument{}, fmt.Errorf("Conveyor-rendered requirement failed canonical validation: %w", err)
	}
	return document, nil
}

// requirementProse returns the document with its machine block removed.
func requirementProse(markdown string) string {
	marker := "```conveyor:requirements"
	start := strings.Index(markdown, marker)
	if start < 0 {
		return markdown
	}
	end := strings.Index(markdown[start+len(marker):], "```")
	if end < 0 {
		return markdown[:start]
	}
	return markdown[:start] + markdown[start+len(marker)+end+len("```"):]
}
