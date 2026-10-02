package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const cliDocsDeclaration = `schema_version: 1
docs:
  - path: docs/knowledge-base/**
    description: shipped-system knowledge base
rule:
  none_statement: "docs: none"
  reason_required: true
  text: Durable docs describe merged behavior only.
`

func docsCLIRepo(t *testing.T, declaration string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.invalid")
	if err := os.MkdirAll(filepath.Join(root, ".conveyor"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".conveyor/docs.yaml"), []byte(declaration), 0600); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-qm", "fixture")
	return root
}

func runDocsCLI(t *testing.T, args ...string) (docsValidateReceipt, error) {
	t.Helper()
	cmd := docsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(append([]string{"validate"}, args...))
	err := cmd.Execute()
	var receipt docsValidateReceipt
	if decodeErr := json.Unmarshal(out.Bytes(), &receipt); decodeErr != nil {
		t.Fatalf("receipt: %s %v (command %v)", out.String(), decodeErr, err)
	}
	return receipt, err
}

func TestDocsValidateOffline(t *testing.T) {
	root := docsCLIRepo(t, cliDocsDeclaration, map[string]string{
		"docs/knowledge-base/page.md": "content",
	})
	receipt, err := runDocsCLI(t, root)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(receipt.Globs) != 1 || len(receipt.Globs[0].Matches) != 1 || receipt.Globs[0].Matches[0] != "docs/knowledge-base/page.md" {
		t.Fatalf("globs=%+v", receipt.Globs)
	}
	if receipt.Revision == "" {
		t.Fatal("receipt omitted revision")
	}
	if len(receipt.Diagnostics) != 0 {
		t.Fatalf("diagnostics=%+v", receipt.Diagnostics)
	}
}

func TestDocsValidateFailsOnEmptyMatch(t *testing.T) {
	declaration := strings.ReplaceAll(cliDocsDeclaration, "docs/knowledge-base/**", "docs/architecture/**")
	root := docsCLIRepo(t, declaration, map[string]string{"docs/knowledge-base/page.md": "content"})
	receipt, err := runDocsCLI(t, root)
	if err == nil {
		t.Fatal("empty-match glob accepted")
	}
	if len(receipt.Diagnostics) != 1 || !strings.Contains(receipt.Diagnostics[0].Message, "matches no tracked file") {
		t.Fatalf("diagnostics=%+v", receipt.Diagnostics)
	}
}

func TestDocsValidateFailsOnInvalidDeclaration(t *testing.T) {
	tests := []struct {
		name        string
		declaration string
		want        string
	}{
		{
			name:        "unsupported version",
			declaration: strings.Replace(cliDocsDeclaration, "schema_version: 1", "schema_version: 9", 1),
			want:        "unsupported schema_version",
		},
		{
			name: "empty docs",
			declaration: `schema_version: 1
docs: []
rule:
  none_statement: "docs: none"
  reason_required: true
  text: rule
`,
			want: "at least one path",
		},
		{
			name:        "malformed glob",
			declaration: strings.Replace(cliDocsDeclaration, "docs/knowledge-base/**", "docs/[a]/**", 1),
			want:        "character-class",
		},
		{
			name:        "oversized file",
			declaration: cliDocsDeclaration + "# " + strings.Repeat("x", 1<<20),
			want:        "limit",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := docsCLIRepo(t, test.declaration, map[string]string{"docs/knowledge-base/page.md": "content"})
			receipt, err := runDocsCLI(t, root)
			if err == nil {
				t.Fatalf("invalid declaration accepted: %+v", receipt)
			}
			if len(receipt.Diagnostics) == 0 || !strings.Contains(receipt.Diagnostics[0].Message, test.want) {
				t.Fatalf("diagnostics=%+v want %q", receipt.Diagnostics, test.want)
			}
		})
	}
}
