package docsconfig

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const validDeclaration = `schema_version: 1
docs:
  - path: docs/api.md
    governs: ["internal/**"]
    description: API
rule:
  reason_required: true
  text: Keep API current
`

// REQ-9/AC-9.1, AC-9.3, AC-9.4.
func TestDeclarationClosure(t *testing.T) {
	c, err := Parse([]byte(validDeclaration))
	if err != nil {
		t.Fatal(err)
	}
	if c.Rule.NoneStatement != "docs: none" {
		t.Fatal("default missing")
	}
	for _, tc := range []struct {
		paths    []string
		affected []string
		matched  []string
	}{
		{[]string{"internal/server.go"}, []string{"docs/api.md"}, []string{}},
		{[]string{"internal/server.go", "docs/api.md"}, []string{}, []string{"docs/api.md"}},
		{[]string{"README.md"}, []string{}, []string{}},
	} {
		evaluation, err := c.Evaluate(t.Context(), tc.paths)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(evaluation.AffectedDocuments, tc.affected) {
			t.Fatalf("affected %v=%v", tc.paths, evaluation.AffectedDocuments)
		}
		if !reflect.DeepEqual(evaluation.MatchedPaths, tc.matched) {
			t.Fatalf("matched %v=%v", tc.paths, evaluation.MatchedPaths)
		}
	}
	for _, bad := range []string{
		strings.Replace(validDeclaration, "schema_version: 1", "schema_version: 2", 1),
		validDeclaration + "unknown: true\n", validDeclaration + "---\n", validDeclaration + "---\n{}\n",
		strings.Replace(validDeclaration, "governs: [\"internal/**\"]", "governs: []", 1),
		strings.Replace(validDeclaration, "internal/**", "../internal/**", 1),
		strings.Replace(validDeclaration, "docs/api.md", "/etc/passwd", 1),
		strings.Replace(validDeclaration, "internal/**", "internal/[ab]*", 1),
		strings.Repeat("x", MaxFileBytes+1),
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatalf("invalid declaration accepted: %.80s", bad)
		}
	}
}
func TestNoneLineQualifier(t *testing.T) {
	for _, tc := range []struct {
		body     string
		required bool
		ok       bool
		reason   string
	}{
		{"docs: none", false, true, ""}, {"docs: none", true, false, ""},
		{"docs: none reason\r\n", true, true, "reason"}, {"docs: none\twhy", true, true, "why"},
		{" docs: none reason", true, false, ""}, {"quote docs: none reason", true, false, ""},
		{"docs: nonetheless", false, false, ""}, {"docs: none: why", true, false, ""},
		{"docs: none\nreason", true, false, ""},
	} {
		reason, ok := MatchNoneStatement(tc.body, "docs: none", tc.required)
		if ok != tc.ok || reason != tc.reason {
			t.Fatalf("body %q got %q,%t", tc.body, reason, ok)
		}
	}
	c, err := Parse([]byte(strings.Replace(validDeclaration, "docs/api.md", "docs/**/*.md", 1)))
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := c.Evaluate(t.Context(), []string{"docs/api.md", "docs/deep/guide.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evaluation.MatchedPaths, []string{"docs/api.md", "docs/deep/guide.md"}) {
		t.Fatalf("shared doublestar dialect=%v", evaluation.MatchedPaths)
	}
}

func TestDocumentationEvaluationCancellation(t *testing.T) {
	declaration, err := Parse([]byte(validDeclaration))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := declaration.Evaluate(ctx, []string{"internal/server.go"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled evaluation error=%v", err)
	}
}
