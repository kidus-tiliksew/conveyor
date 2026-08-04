package pipeline

import (
	"reflect"
	"strings"
	"testing"
)

const requirementProseFixture = "# Planning sessions\n\nOperators state intent in their own language; only the machine block is validated."

func requirementBlock(body string) string {
	return "```conveyor:requirements\n" + body + "\n```"
}

func TestParseRequirementDocumentAcceptsProseWithOneBlock(t *testing.T) {
	t.Parallel()
	document := requirementProseFixture + "\n\n" + requirementBlock("- id: REQ-1\n  statement: Every requirement version is confirmed by an operator.")
	parsed, err := ParseRequirementDocument(document)
	if err != nil {
		t.Fatalf("valid requirement document rejected: %v", err)
	}
	if len(parsed.Statements) != 1 || parsed.Statements[0].ID != "REQ-1" {
		t.Fatalf("statements = %+v", parsed.Statements)
	}
	// The markdown is the stored version body, so the block travels with the
	// prose rather than being stripped out (spec §4.2 item 1).
	if parsed.Markdown != document {
		t.Fatalf("markdown = %q, want %q", parsed.Markdown, document)
	}
}

func TestParseRequirementDocumentRejectsInvalidBodies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		document string
		want     string
	}{
		{
			// Prose alone is intent without anything citable.
			name:     "no block",
			document: requirementProseFixture,
			want:     "requires one conveyor:requirements block",
		},
		{
			// Two blocks leave no single answer to "what are the statements".
			name: "two blocks",
			document: requirementProseFixture + "\n\n" +
				requirementBlock("- id: REQ-1\n  statement: First.") + "\n\n" +
				requirementBlock("- id: REQ-2\n  statement: Second."),
			want: "exactly one conveyor:requirements block; found 2",
		},
		{
			name:     "empty block",
			document: requirementProseFixture + "\n\n" + requirementBlock("[]"),
			want:     "must be a non-empty list",
		},
		{
			// A document that is only its machine block is a checklist, not a
			// living intent document.
			name:     "no prose outside the block",
			document: requirementBlock("- id: REQ-1\n  statement: Ship it."),
			want:     "requires prose alongside its requirements block",
		},
		{
			name:     "malformed yaml",
			document: requirementProseFixture + "\n\n" + requirementBlock(`- id: REQ-1`+"\n  statement: \"unterminated"),
			want:     "requirements block:",
		},
		{
			// Core statement rules apply verbatim; the parser adds no leniency.
			name:     "statement fails core validation",
			document: requirementProseFixture + "\n\n" + requirementBlock("- id: REQ-0\n  statement: Ship it."),
			want:     `invalid id "REQ-0"`,
		},
		{
			name:     "empty document",
			document: "   \n\t",
			want:     "requires prose",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseRequirementDocument(test.document)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRenderRequirementDocumentRoundTripsThroughTheParser(t *testing.T) {
	t.Parallel()
	statements := []RequirementStatement{
		{ID: "REQ-1", Statement: "Requirement versions are confirmed, never gated."},
		{ID: "REQ-2", Statement: "Statement IDs are never recycled."},
	}
	rendered, err := RenderRequirementDocument(requirementProseFixture, statements)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	// Conveyor owns the fence, so exactly one canonical block is emitted.
	if strings.Count(rendered.Markdown, "```conveyor:requirements\n") != 1 {
		t.Fatalf("non-canonical render:\n%s", rendered.Markdown)
	}
	roundTrip, err := ParseRequirementDocument(rendered.Markdown)
	if err != nil {
		t.Fatalf("rendered document failed re-parse: %v", err)
	}
	if len(roundTrip.Statements) != len(statements) {
		t.Fatalf("roundTrip statements = %+v", roundTrip.Statements)
	}
	for index, statement := range statements {
		if !reflect.DeepEqual(roundTrip.Statements[index], statement) {
			t.Fatalf("statement %d = %+v, want %+v", index, roundTrip.Statements[index], statement)
		}
	}
}

func TestRenderRequirementDocumentRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		prose      string
		statements []RequirementStatement
		want       string
	}{
		{
			name:       "empty prose",
			prose:      "   \n\t",
			statements: []RequirementStatement{{ID: "REQ-1", Statement: "Ship it."}},
			want:       "requirement prose is required",
		},
		{
			name:       "invalid statement id",
			prose:      requirementProseFixture,
			statements: []RequirementStatement{{ID: "REQ-01", Statement: "Ship it."}},
			want:       `invalid id "REQ-01"`,
		},
		{
			name:       "empty statement text",
			prose:      requirementProseFixture,
			statements: []RequirementStatement{{ID: "REQ-1", Statement: " "}},
			want:       "REQ-1 is empty",
		},
		{
			name:  "no statements",
			prose: requirementProseFixture,
			want:  "must be a non-empty list",
		},
		{
			// A model-authored fence would compete with the one Conveyor
			// serializes, so it is rejected rather than nested.
			name:       "prose carries a machine fence",
			prose:      requirementProseFixture + "\n\n" + requirementBlock("- id: REQ-1\n  statement: Ship it."),
			statements: []RequirementStatement{{ID: "REQ-1", Statement: "Ship it."}},
			want:       "must not contain a conveyor:requirements fence",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := RenderRequirementDocument(test.prose, test.statements)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
