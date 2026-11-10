package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

func riskTestRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	for _, p := range []string{".conveyor", "internal/store", "docs"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"internal/store/schema.sql", "docs/api.md"} {
		if err := os.WriteFile(filepath.Join(root, p), []byte("seed"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// An untracked file never satisfies a glob.
	if err := os.WriteFile(filepath.Join(root, "untracked.go"), []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "internal/store/schema.sql", "docs/api.md")
	return root
}

func executeRiskValidation(t *testing.T, path string) (riskValidateReceipt, error) {
	t.Helper()
	command := riskCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"validate", path})
	err := command.Execute()
	var receipt riskValidateReceipt
	if decodeErr := json.NewDecoder(&output).Decode(&receipt); decodeErr != nil {
		t.Fatalf("decoding CLI result: %v output=%s", decodeErr, output.String())
	}
	return receipt, err
}

// req-risk-based-review AC-3.6: the command checks the worktree declaration
// against tracked files, reports each glob that matches no tracked file, and
// exits non-zero for a malformed declaration or an unmatched glob. The
// built-in globs need not match any tracked file.
func TestRiskValidateCommand(t *testing.T) {
	root := riskTestRepository(t)
	declaration := filepath.Join(root, filepath.FromSlash(riskpolicy.DeclarationPath))
	valid := "schema_version: 1\nrules:\n  - paths: ['internal/**/schema.sql', 'docs/*.md']\n    min_tier: high\n    description: Schema and API docs\n    min_human_approvals: 1\n"
	for _, tc := range []struct {
		name, content string
		diagnostics   []riskValidateDiagnostic
	}{
		{"valid", valid, nil},
		{"no rules", "schema_version: 1\nrules: []\n", nil},
		{"unmatched glob", strings.Replace(valid, "docs/*.md", "untracked.go", 1), []riskValidateDiagnostic{{"rules[0].paths[1]", "untracked.go"}}},
		{"multiple unmatched globs", valid + "  - {paths: [web/**, 'cmd/*.go'], min_tier: medium, description: Web}\n", []riskValidateDiagnostic{{"rules[1].paths[0]", "web/**"}, {"rules[1].paths[1]", "cmd/*.go"}}},
		{"closed schema", valid + "owners: []\n", []riskValidateDiagnostic{{declaration, "owners"}}},
		{"rule closed schema", strings.Replace(valid, "    description:", "    severity: x\n    description:", 1), []riskValidateDiagnostic{{declaration, "severity"}}},
		{"multiple documents", valid + "---\n" + valid, []riskValidateDiagnostic{{declaration, "exactly one YAML document"}}},
		{"empty", "", []riskValidateDiagnostic{{declaration, "exactly one YAML document"}}},
		{"schema version", strings.Replace(valid, "schema_version: 1", "schema_version: 2", 1), []riskValidateDiagnostic{{declaration, "schema_version must be 1"}}},
		{"tier", strings.Replace(valid, "min_tier: high", "min_tier: critical", 1), []riskValidateDiagnostic{{declaration, "min_tier"}}},
		{"blank description", strings.Replace(valid, "Schema and API docs", "''", 1), []riskValidateDiagnostic{{declaration, "description"}}},
		{"empty paths", strings.Replace(valid, "['internal/**/schema.sql', 'docs/*.md']", "[]", 1), []riskValidateDiagnostic{{declaration, "paths must not be empty"}}},
		{"approvals", strings.Replace(valid, "min_human_approvals: 1", "min_human_approvals: -2", 1), []riskValidateDiagnostic{{declaration, "min_human_approvals"}}},
		{"oversize", valid + "# " + strings.Repeat("x", riskpolicy.MaxDeclarationBytes) + "\n", []riskValidateDiagnostic{{declaration, "exceeds"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(declaration, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			receipt, err := executeRiskValidation(t, root)
			if (err != nil) != (len(tc.diagnostics) > 0) || len(receipt.Diagnostics) != len(tc.diagnostics) || receipt.Path != declaration || receipt.Status != "" {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			for i, want := range tc.diagnostics {
				got := receipt.Diagnostics[i]
				if got.Path != want.Path || !strings.Contains(got.Message, want.Message) {
					t.Fatalf("diagnostic[%d]=%+v want path=%q message containing %q", i, got, want.Path, want.Message)
				}
			}
		})
	}
	// The declaration path itself may be passed.
	if err := os.WriteFile(declaration, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	if receipt, err := executeRiskValidation(t, declaration); err != nil || receipt.Path != declaration {
		t.Fatalf("file argument receipt=%+v err=%v", receipt, err)
	}
}

// AC-3.6: with no declaration the command succeeds and reports that only the
// built-in minimums apply, even when no workflow or declaration file exists.
func TestRiskValidateAbsentDeclaration(t *testing.T) {
	root := riskTestRepository(t)
	receipt, err := executeRiskValidation(t, root)
	if err != nil || receipt.Status != riskBuiltinOnlyStatus || receipt.Diagnostics == nil || len(receipt.Diagnostics) != 0 {
		t.Fatalf("absent declaration receipt=%+v err=%v", receipt, err)
	}
}

func TestRiskValidateInvalidInput(t *testing.T) {
	t.Run("nonexistent root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "does-not-exist")
		receipt, err := executeRiskValidation(t, root)
		if err == nil || receipt.Status != "" || len(receipt.Diagnostics) != 1 || receipt.Diagnostics[0].Path != root {
			t.Fatalf("nonexistent root receipt=%+v err=%v", receipt, err)
		}
	})
	t.Run("symlink declaration", func(t *testing.T) {
		root := riskTestRepository(t)
		target := filepath.Join(root, "declaration.yaml")
		if err := os.WriteFile(target, []byte("schema_version: 1\nrules: []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		declaration := filepath.Join(root, filepath.FromSlash(riskpolicy.DeclarationPath))
		if err := os.Symlink(target, declaration); err != nil {
			t.Fatal(err)
		}
		receipt, err := executeRiskValidation(t, root)
		if err == nil || len(receipt.Diagnostics) != 1 || !strings.Contains(receipt.Diagnostics[0].Message, "regular file") {
			t.Fatalf("symlink receipt=%+v err=%v", receipt, err)
		}
	})
	t.Run("outside a repository", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".conveyor"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(riskpolicy.DeclarationPath)), []byte("schema_version: 1\nrules: []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if receipt, err := executeRiskValidation(t, root); err == nil || len(receipt.Diagnostics) != 1 || !strings.Contains(receipt.Diagnostics[0].Message, "git rev-parse") {
			t.Fatalf("outside repository receipt=%+v err=%v", receipt, err)
		}
	})
}
