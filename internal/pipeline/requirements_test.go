package pipeline

import (
	"reflect"
	"strings"
	"testing"
)

const requirementProseFixture = "# Planning sessions\n\nOperators state intent in their own language; statement definitions belong in the machine block."

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
	// prose rather than being stripped out.
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
			want:     `must begin with its "# <title>" heading`,
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

func TestRequirementProseContract(t *testing.T) {
	block := requirementBlock("- id: REQ-1\n  statement: Authenticate safely.\n  acceptance_criteria:\n    - id: AC-1.1\n      statement: Reject invalid credentials.")
	valid := "# CLI authentication\n\n" + block
	tests := []struct{ name, content, want string }{
		{"preamble", "CLI authentication (proposed v2)\n" + valid, `requirement content must begin with its "# <title>" heading; line 1 is "CLI authentication (proposed v2)"`},
		{"blank lines before invalid heading", "\r\n \r\n## Title\r\n" + block, `line 3 is "## Title"`},
		{"empty heading", "# \n" + block, `line 1 is "# "`},
		{"missing space", "#Title\n" + block, `line 1 is "#Title"`},
		{"indented heading", " # Title\n" + block, `line 1 is " # Title"`},
		{"leading blank lines", "\n \n" + valid, ""},
		{"CRLF", strings.ReplaceAll(valid, "\n", "\r\n"), ""},
		{"inline citations", "# Title\n\nContinue as REQ-3 requires and cite AC-3.1: here.\n" + block, ""},
		{"ordinary trailing prose", valid + "\nThis follows REQ-1.", ""},
		{"identifier without colon", valid + "\nREQ-1 describes authentication.", ""},
		{"requirement before fence", "# Title\nREQ-3: Duplicate.\n" + block, `line 2 is "REQ-3: Duplicate.": statement identifiers belong inside the conveyor:requirements fence`},
		{"criterion after fence", valid + "\n\tAC-1.1 : Duplicate.", `statement identifiers belong inside the conveyor:requirements fence`},
		{"yaml requirement after fence", valid + "\n - id: REQ-1", `statement identifiers belong inside the conveyor:requirements fence`},
		{"yaml criterion before fence", "# Title\n-  id: AC-1.1\n" + block, `line 2 is "-  id: AC-1.1": statement identifiers belong inside the conveyor:requirements fence`},
		{"other code fence is prose", valid + "\n```yaml\n- id: REQ-2\n```", `statement identifiers belong inside the conveyor:requirements fence`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := ParseRequirementDocument(tt.content)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want %q", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if doc.Markdown != strings.TrimSpace(tt.content) || len(doc.Statements) != 1 || len(doc.Statements[0].AcceptanceCriteria) != 1 {
				t.Fatalf("document changed: %+v", doc)
			}
		})
	}
	for _, prefix := range []string{"", "# Title\nREQ-1: "} {
		_, err := ParseRequirementDocument(prefix + strings.Repeat("界", 1000) + "\n" + block)
		if err == nil || !strings.Contains(err.Error(), "…") || strings.Contains(err.Error(), strings.Repeat("界", 161)) {
			t.Fatalf("unbounded error: %v", err)
		}
	}
}

func TestRenderRequirementDocumentEnforcesProseContract(t *testing.T) {
	statements := []RequirementStatement{{ID: "REQ-1", Statement: "Authenticate safely."}}
	for _, prose := range []string{"Preamble\n# Title", "# Title\nREQ-1: Duplicate.", "# Title\n- id: AC-1.1"} {
		_, err := RenderRequirementDocument(prose, statements)
		if err == nil || !strings.Contains(err.Error(), "Conveyor-rendered requirement failed canonical validation:") {
			t.Fatalf("prose %q: %v", prose, err)
		}
	}
}
