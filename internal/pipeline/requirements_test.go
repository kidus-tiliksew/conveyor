package pipeline

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestHistoricalCLIAuthenticationProposal(t *testing.T) {
	_, err := ParseRequirementDocument(historicalCLIAuthenticationV2)
	want := `requirement content must begin with its "# <title>" heading; line 1 is "CLI authentication (proposed v2)"`
	if err == nil || err.Error() != want {
		t.Fatalf("historical proposal error = %v, want %q", err, want)
	}
	withoutPreamble := strings.TrimPrefix(historicalCLIAuthenticationV2, "CLI authentication (proposed v2)\n")
	_, err = ParseRequirementDocument(withoutPreamble)
	if err == nil || !strings.HasPrefix(err.Error(), "requirement content line 57 is ") || !strings.HasSuffix(err.Error(), ": statement identifiers belong inside the conveyor:requirements fence") {
		t.Fatalf("trailing statement error = %v", err)
	}
	corrected, _, found := strings.Cut(withoutPreamble, "\nREQ-1:")
	if !found {
		t.Fatal("historical duplicate statements missing")
	}
	baseline, err := ParseRequirementDocument(historicalCLIAuthenticationV1)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRequirementDocument(corrected)
	if err != nil {
		t.Fatal(err)
	}
	// The only intended edits are AC-1.4 and new AC-2.4. Compare every other
	// statement, criterion, user story, and prose byte against the baseline.
	baseline.Statements[0].AcceptanceCriteria[3].Statement = "When a command needs a credential, it shall resolve an explicit flag first, then the environment, then the stored credential; an environment token shall be the credential only for its environment server, the normalized `CONVEYOR_ADDR` or `http://localhost:8080` when unset, and any other resolved server shall fall back to that server's stored credential, so that environment-based automation is unchanged."
	baseline.Statements[1].AcceptanceCriteria = append(baseline.Statements[1].AcceptanceCriteria, core.AcceptanceCriterion{
		ID: "AC-2.4", Statement: "When a command resolves a credential or receives an authentication rejection, it shall report the credential source, and when it ignores an environment token for a mismatched server it shall say so with a redacted explanation that never exposes a credential value.",
	})
	if !reflect.DeepEqual(parsed.Statements, baseline.Statements) || requirementProse(parsed.Markdown) != requirementProse(baseline.Markdown) || parsed.Markdown != corrected {
		t.Fatalf("corrected proposal differs beyond the two intended edits: %+v", parsed)
	}
	for i, statement := range baseline.Statements {
		duplicate := fmt.Sprintf("%s: %s", statement.ID, statement.Statement)
		if !strings.Contains(historicalCLIAuthenticationV2, "\n"+duplicate) {
			t.Fatalf("historical duplicate %d missing", i)
		}
	}
}

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

// Exact demo req-cli-authentication versions retrieved 2026-09-10 and attached
// to task 260910-fa029e. Version 2 is historical regression input, not authority.

const historicalCLIAuthenticationV1 = "# CLI authentication\n" +
	"\n" +
	"A user who works from a terminal wants to log their machine into the factory once and then have every `conveyor` command, and every agent tool that talks to the factory, work without a token living in a shell profile. Automation that already passes a token through the environment should keep working unchanged.\n" +
	"\n" +
	"The CLI offers an explicit login that verifies a pasted personal access token against the server and stores it per server URL in a credential file only the owner can read. A status act reports who is logged in without revealing the token, and logout removes the stored entry, revoking the token on request. A token-print act emits the stored credential for command substitution, so agent and MCP configurations can reference it without a second copy.\n" +
	"\n" +
	"A user may also store a default workspace per server. Both the credential and the workspace resolve in the same order everywhere: an explicit flag first, then the environment, then the stored value. A connection act detects the agent tooling present on the machine and writes each tool's native MCP registration for the logged-in server, referencing the credential through the token-print bridge rather than inlining it.\n" +
	"\n" +
	"When verification fails at login, nothing is stored. When no credential resolves, a command reports that it is not logged in and names the login act.\n" +
	"\n" +
	"```conveyor:requirements\n" +
	"- id: REQ-1\n" +
	"  statement: The CLI shall authenticate through an explicit login act that verifies a pasted personal access token against the server and stores it per server URL in an owner-only credential file.\n" +
	"  user_story:\n" +
	"    as_a: user\n" +
	"    i_want: to log my machine into the factory once\n" +
	"    so_that: tokens stop living in my shell profile and every conveyor command just works\n" +
	"  acceptance_criteria:\n" +
	"    - id: AC-1.1\n" +
	"      statement: When a user logs in, the CLI shall verify the token against the server before storing it and shall report the authenticated identity on success.\n" +
	"    - id: AC-1.2\n" +
	"      statement: When verification fails, the CLI shall store nothing.\n" +
	"    - id: AC-1.3\n" +
	"      statement: When a token is stored, it shall be keyed by server URL in a file readable and writable only by its owner, and the CLI shall never write it to a shell profile.\n" +
	"    - id: AC-1.4\n" +
	"      statement: When a command needs a credential, it shall resolve an explicit flag first, then the environment, then the stored credential, so that environment-based automation is unchanged.\n" +
	"- id: REQ-2\n" +
	"  statement: The CLI shall provide status, logout, and token-print acts over the stored credential.\n" +
	"  acceptance_criteria:\n" +
	"    - id: AC-2.1\n" +
	"      statement: When a user runs the status act, it shall report the stored identity and server without revealing the credential.\n" +
	"    - id: AC-2.2\n" +
	"      statement: When a user logs out, the CLI shall remove the stored entry for that server, and when the user requests it shall also revoke the token server-side through the token self-service surface.\n" +
	"    - id: AC-2.3\n" +
	"      statement: When a user runs the token-print act, it shall emit the stored credential for command substitution so agent and MCP configurations can consume it without a second copy.\n" +
	"- id: REQ-3\n" +
	"  statement: The CLI shall store a default workspace per server and resolve workspace context identically across every command.\n" +
	"  acceptance_criteria:\n" +
	"    - id: AC-3.1\n" +
	"      statement: When a user runs the workspace configuration act, the CLI shall store the chosen default workspace keyed by server URL.\n" +
	"    - id: AC-3.2\n" +
	"      statement: When any command needs a workspace, it shall resolve an explicit flag first, then the environment, then the stored per-server default, then the singleton-workspace fallback.\n" +
	"- id: REQ-4\n" +
	"  statement: The CLI shall provide a connection act that writes each detected agent tool's native MCP registration for the logged-in server.\n" +
	"  acceptance_criteria:\n" +
	"    - id: AC-4.1\n" +
	"      statement: When a user runs the connection act, the CLI shall detect the agent tooling present on the machine and write each tool's registration in the shape that tool honors.\n" +
	"    - id: AC-4.2\n" +
	"      statement: When a registration is written, it shall reference the credential through the token-print bridge and shall never inline a token value.\n" +
	"    - id: AC-4.3\n" +
	"      statement: When the connection act runs again for the same server, it shall update only the entries it marked as its own, keyed per server, leaving other entries unchanged.\n" +
	"    - id: AC-4.4\n" +
	"      statement: When the user passes the tool-selection flag, the connection act shall write the registration for that one tool only.\n" +
	"```"

const historicalCLIAuthenticationV2 = "CLI authentication (proposed v2)\n" +
	"# CLI authentication\n" +
	"\n" +
	"A user who works from a terminal wants to log their machine into the factory once and then have every `conveyor` command, and every agent tool that talks to the factory, work without a token living in a shell profile. Automation that already passes a token through the environment should keep working unchanged.\n" +
	"\n" +
	"The CLI offers an explicit login that verifies a pasted personal access token against the server and stores it per server URL in a credential file only the owner can read. A status act reports who is logged in without revealing the token, and logout removes the stored entry, revoking the token on request. A token-print act emits the stored credential for command substitution, so agent and MCP configurations can reference it without a second copy.\n" +
	"\n" +
	"A user may also store a default workspace per server. Both the credential and the workspace resolve in the same order everywhere: an explicit flag first, then the environment, then the stored value. A connection act detects the agent tooling present on the machine and writes each tool's native MCP registration for the logged-in server, referencing the credential through the token-print bridge rather than inlining it.\n" +
	"\n" +
	"When verification fails at login, nothing is stored. When no credential resolves, a command reports that it is not logged in and names the login act.\n" +
	"\n" +
	"```conveyor:requirements\n" +
	"- id: REQ-1\n" +
	"  statement: The CLI shall authenticate through an explicit login act that verifies a pasted personal access token against the server and stores it per server URL in an owner-only credential file.\n" +
	"  user_story:\n" +
	"    as_a: user\n" +
	"    i_want: to log my machine into the factory once\n" +
	"    so_that: tokens stop living in my shell profile and every conveyor command just works\n" +
	"  acceptance_criteria:\n" +
	"    - id: AC-1.1\n" +
	"      statement: When a user logs in, the CLI shall verify the token against the server before storing it and shall report the authenticated identity on success.\n" +
	"    - id: AC-1.2\n" +
	"      statement: When verification fails, the CLI shall store nothing.\n" +
	"    - id: AC-1.3\n" +
	"      statement: When a token is stored, it shall be keyed by server URL in a file readable and writable only by its owner, and the CLI shall never write it to a shell profile.\n" +
	"    - id: AC-1.4\n" +
	"      statement: When a command needs a credential, it shall resolve an explicit flag first, then the environment, then the stored credential; an environment token shall be the credential only for its environment server, the normalized `CONVEYOR_ADDR` or `http://localhost:8080` when unset, and any other resolved server shall fall back to that server's stored credential, so that environment-based automation is unchanged.\n" +
	"- id: REQ-2\n" +
	"  statement: The CLI shall provide status, logout, and token-print acts over the stored credential.\n" +
	"  acceptance_criteria:\n" +
	"    - id: AC-2.1\n" +
	"      statement: When a user runs the status act, it shall report the stored identity and server without revealing the credential.\n" +
	"    - id: AC-2.2\n" +
	"      statement: When a user logs out, the CLI shall remove the stored entry for that server, and when the user requests it shall also revoke the token server-side through the token self-service surface.\n" +
	"    - id: AC-2.3\n" +
	"      statement: When a user runs the token-print act, it shall emit the stored credential for command substitution so agent and MCP configurations can consume it without a second copy.\n" +
	"    - id: AC-2.4\n" +
	"      statement: When a command resolves a credential or receives an authentication rejection, it shall report the credential source, and when it ignores an environment token for a mismatched server it shall say so with a redacted explanation that never exposes a credential value.\n" +
	"- id: REQ-3\n" +
	"  statement: The CLI shall store a default workspace per server and resolve workspace context identically across every command.\n" +
	"  acceptance_criteria:\n" +
	"    - id: AC-3.1\n" +
	"      statement: When a user runs the workspace configuration act, the CLI shall store the chosen default workspace keyed by server URL.\n" +
	"    - id: AC-3.2\n" +
	"      statement: When any command needs a workspace, it shall resolve an explicit flag first, then the environment, then the stored per-server default, then the singleton-workspace fallback.\n" +
	"- id: REQ-4\n" +
	"  statement: The CLI shall provide a connection act that writes each detected agent tool's native MCP registration for the logged-in server.\n" +
	"  acceptance_criteria:\n" +
	"    - id: AC-4.1\n" +
	"      statement: When a user runs the connection act, the CLI shall detect the agent tooling present on the machine and write each tool's registration in the shape that tool honors.\n" +
	"    - id: AC-4.2\n" +
	"      statement: When a registration is written, it shall reference the credential through the token-print bridge and shall never inline a token value.\n" +
	"    - id: AC-4.3\n" +
	"      statement: When the connection act runs again for the same server, it shall update only the entries it marked as its own, keyed per server, leaving other entries unchanged.\n" +
	"    - id: AC-4.4\n" +
	"      statement: When the user passes the tool-selection flag, the connection act shall write the registration for that one tool only.\n" +
	"```\n" +
	"REQ-1: The CLI shall authenticate through an explicit login act that verifies a pasted personal access token against the server and stores it per server URL in an owner-only credential file.\n" +
	"REQ-2: The CLI shall provide status, logout, and token-print acts over the stored credential.\n" +
	"REQ-3: The CLI shall store a default workspace per server and resolve workspace context identically across every command.\n" +
	"REQ-4: The CLI shall provide a connection act that writes each detected agent tool's native MCP registration for the logged-in server."
