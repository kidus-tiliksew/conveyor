package docsconfig

import (
	"strings"
	"testing"
)

const validDoc = `schema_version: 1
docs:
  - path: docs/knowledge-base/**
    description: shipped-system knowledge base
  - path: docs/architecture/**
    description: architecture pages
rule:
  none_statement: "docs: none"
  reason_required: true
  text: >-
    Durable docs describe merged behavior only.
    A task that changes behavior updates the affected docs in the same pull
    request, or states "docs: none" with a reason.
`

func TestParseAcceptsExampleSchema(t *testing.T) {
	cfg, err := Parse([]byte(validDoc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.SchemaVersion != 1 {
		t.Fatalf("schema_version=%d", cfg.SchemaVersion)
	}
	if got := cfg.Paths(); len(got) != 2 || got[0] != "docs/knowledge-base/**" || got[1] != "docs/architecture/**" {
		t.Fatalf("paths=%v", got)
	}
	if cfg.Rule.NoneStatement != "docs: none" || !cfg.Rule.ReasonRequired {
		t.Fatalf("rule=%+v", cfg.Rule)
	}
}

func TestParseRejectsInvalidSchemas(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "empty docs",
			doc:  "schema_version: 1\ndocs: []\nrule:\n  none_statement: \"docs: none\"\n  reason_required: true\n  text: rule\n",
			want: "at least one path",
		},
		{
			name: "missing docs",
			doc:  "schema_version: 1\nrule:\n  none_statement: \"docs: none\"\n  reason_required: true\n  text: rule\n",
			want: "at least one path",
		},
		{
			name: "unsupported schema_version",
			doc:  "schema_version: 2\ndocs:\n  - path: docs/**\n    description: docs\nrule:\n  none_statement: \"docs: none\"\n  reason_required: true\n  text: rule\n",
			want: "unsupported schema_version",
		},
		{
			name: "malformed glob character class",
			doc:  "schema_version: 1\ndocs:\n  - path: docs/[a]/*\n    description: docs\nrule:\n  none_statement: \"docs: none\"\n  reason_required: true\n  text: rule\n",
			want: "character-class",
		},
		{
			name: "absolute glob",
			doc:  "schema_version: 1\ndocs:\n  - path: /docs/**\n    description: docs\nrule:\n  none_statement: \"docs: none\"\n  reason_required: true\n  text: rule\n",
			want: "repository-relative",
		},
		{
			name: "traversing glob",
			doc:  "schema_version: 1\ndocs:\n  - path: ../docs/**\n    description: docs\nrule:\n  none_statement: \"docs: none\"\n  reason_required: true\n  text: rule\n",
			want: "repository-relative",
		},
		{
			name: "missing description",
			doc:  "schema_version: 1\ndocs:\n  - path: docs/**\nrule:\n  none_statement: \"docs: none\"\n  reason_required: true\n  text: rule\n",
			want: "description is required",
		},
		{
			name: "missing none_statement",
			doc:  "schema_version: 1\ndocs:\n  - path: docs/**\n    description: docs\nrule:\n  reason_required: true\n  text: rule\n",
			want: "none_statement is required",
		},
		{
			name: "oversized rule text",
			doc:  "schema_version: 1\ndocs:\n  - path: docs/**\n    description: docs\nrule:\n  none_statement: \"docs: none\"\n  reason_required: false\n  text: " + strings.Repeat("x", MaxRuleTextBytes+1) + "\n",
			want: "rule.text exceeds",
		},
		{
			name: "unknown field",
			doc:  "schema_version: 1\ndocs:\n  - path: docs/**\n    description: docs\nrule:\n  none_statement: \"docs: none\"\n  reason_required: true\n  text: rule\nextra: true\n",
			want: "field extra not found",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.doc))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v want substring %q", err, test.want)
			}
		})
	}
}

func TestParseRejectsOversizedFile(t *testing.T) {
	blob := validDoc + "# " + strings.Repeat("x", MaxFileBytes)
	if _, err := Parse([]byte(blob)); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err=%v", err)
	}
}

func TestMatchPath(t *testing.T) {
	cfg, err := Parse([]byte(validDoc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, test := range []struct {
		path string
		want bool
	}{
		{"docs/knowledge-base/page.md", true},
		{"docs/knowledge-base/nested/deep/page.md", true},
		{"docs/architecture/page.md", true},
		{"docs/other/page.md", false},
		{"docs/knowledge-base", false},
		{"other/docs/knowledge-base/page.md", false},
	} {
		if got := cfg.MatchPath(test.path); got != test.want {
			t.Errorf("MatchPath(%q)=%t want %t", test.path, got, test.want)
		}
	}
}

func TestMatchNoneStatement(t *testing.T) {
	const body = "## Summary\n\nThe behavior is covered.\n\ndocs: none — no durable docs describe this internal refactor yet.\n"
	reason, ok := MatchNoneStatement(body, "docs: none", true)
	if !ok || !strings.Contains(reason, "no durable docs") {
		t.Fatalf("ok=%t reason=%q", ok, reason)
	}
	if _, ok := MatchNoneStatement(body, "docs: none", false); !ok {
		t.Fatal("reason_required=false rejected a valid statement")
	}
	for _, test := range []struct {
		name           string
		body           string
		literal        string
		reasonRequired bool
		wantOK         bool
	}{
		{name: "case variant", body: "Docs: None because\n", literal: "docs: none", reasonRequired: true, wantOK: false},
		{name: "mid-line occurrence", body: "see docs: none in the template\n", literal: "docs: none", reasonRequired: true, wantOK: false},
		{name: "indented literal", body: "  docs: none because\n", literal: "docs: none", reasonRequired: true, wantOK: false},
		{name: "empty reason required", body: "docs: none\n", literal: "docs: none", reasonRequired: true, wantOK: false},
		{name: "empty reason optional", body: "docs: none\n", literal: "docs: none", reasonRequired: false, wantOK: true},
		{name: "crlf line", body: "intro\r\ndocs: none because\r\n", literal: "docs: none", reasonRequired: true, wantOK: true},
		{name: "literal continued by word", body: "docs: noneXYZ\n", literal: "docs: none", reasonRequired: false, wantOK: false},
		{name: "literal continued by word with reason required", body: "docs: noneXYZ\n", literal: "docs: none", reasonRequired: true, wantOK: false},
		{name: "literal prefix of another word", body: "docs: nonsense\n", literal: "docs: none", reasonRequired: false, wantOK: false},
		{name: "literal with space text", body: "docs: none because internal\n", literal: "docs: none", reasonRequired: true, wantOK: true},
		{name: "literal tab separated reason", body: "docs: none\treason\n", literal: "docs: none", reasonRequired: true, wantOK: true},
		{name: "literal exact no newline", body: "docs: none", literal: "docs: none", reasonRequired: false, wantOK: true},
		{name: "literal trailing cr", body: "docs: none\r", literal: "docs: none", reasonRequired: false, wantOK: true},
		{name: "trailing whitespace exact", body: "docs: none   \n", literal: "docs: none", reasonRequired: false, wantOK: true},
		{name: "no statement", body: "nothing here\n", literal: "docs: none", reasonRequired: false, wantOK: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, ok := MatchNoneStatement(test.body, test.literal, test.reasonRequired)
			if ok != test.wantOK {
				t.Fatalf("ok=%t want %t", ok, test.wantOK)
			}
		})
	}
}
