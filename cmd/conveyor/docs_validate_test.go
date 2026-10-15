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

// REQ-9/AC-9.2: exercise the actual CLI against the git index, not working-tree presence.
func TestDocsValidateCommand(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	git("init", "-q")
	for _, p := range []string{".conveyor", "docs", "internal"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"docs/api.md", "internal/server.go"} {
		if err := os.WriteFile(filepath.Join(root, p), []byte("seed"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "docs/api.md", "internal/server.go")
	declaration := filepath.Join(root, ".conveyor/docs.yaml")
	valid := "schema_version: 1\ndocs:\n  - path: docs/api.md\n    governs: [internal/**]\nrule:\n  reason_required: true\n"
	for _, tc := range []struct {
		name, content string
		diagnostics   []docsValidateDiagnostic
	}{
		{"valid", valid, nil},
		{"missing-doc", strings.Replace(valid, "docs/api.md", "docs/absent.md", 1), []docsValidateDiagnostic{{"docs[0].path", "docs/absent.md"}}},
		{"missing-source", strings.Replace(valid, "internal/**", "untracked/**", 1), []docsValidateDiagnostic{{"docs[0].governs[0]", "untracked/**"}}},
		{"multiple-missing-globs", strings.Replace(valid, "internal/**", "untracked/**, other/**", 1), []docsValidateDiagnostic{{"docs[0].governs[0]", "untracked/**"}, {"docs[0].governs[1]", "other/**"}}},
		{"closed-schema", valid + "unexpected: true\n", []docsValidateDiagnostic{{declaration, "unexpected"}}},
		{"multiple-documents", valid + "---\n" + valid, []docsValidateDiagnostic{{declaration, "document"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(declaration, []byte(tc.content), 0644); err != nil {
				t.Fatal(err)
			}
			receipt, err := executeDocsValidation(t, root)
			if (err != nil) != (len(tc.diagnostics) > 0) || len(receipt.Diagnostics) != len(tc.diagnostics) || receipt.Path != declaration {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			for i, want := range tc.diagnostics {
				got := receipt.Diagnostics[i]
				if got.Path != want.Path || !strings.Contains(got.Message, want.Message) {
					t.Fatalf("diagnostic[%d]=%+v want key=%q and glob/error=%q", i, got, want.Path, want.Message)
				}
			}
		})
	}
}

// REQ-9/AC-9.2, AC-9.6: absence is an explicit gate-off result, not invalid YAML.
func TestDocsValidateAbsentDeclaration(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	receipt, err := executeDocsValidation(t, root)
	if err != nil || receipt.Status != "gate off: declaration absent" || receipt.Diagnostics == nil || len(receipt.Diagnostics) != 0 {
		t.Fatalf("gate-off receipt=%+v err=%v", receipt, err)
	}
}

func TestDocsValidateInvalidInput(t *testing.T) {
	t.Run("nonexistent-root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "does-not-exist")
		receipt, err := executeDocsValidation(t, root)
		if err == nil || receipt.Status != "" || len(receipt.Diagnostics) != 1 || receipt.Diagnostics[0].Path != root {
			t.Fatalf("nonexistent root receipt=%+v err=%v", receipt, err)
		}
	})
	t.Run("symlink-declaration", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, ".conveyor"), 0755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "declaration.yaml")
		if err := os.WriteFile(target, []byte("schema_version: 1\ndocs:\n- path: docs/api.md\n  governs: [internal/**]\n"), 0644); err != nil {
			t.Fatal(err)
		}
		declaration := filepath.Join(root, ".conveyor", "docs.yaml")
		if err := os.Symlink(target, declaration); err != nil {
			t.Fatal(err)
		}
		receipt, err := executeDocsValidation(t, root)
		if err == nil || len(receipt.Diagnostics) != 1 || receipt.Diagnostics[0].Path != declaration || !strings.Contains(receipt.Diagnostics[0].Message, "symlink") {
			t.Fatalf("symlink receipt=%+v err=%v", receipt, err)
		}
	})
}

func executeDocsValidation(t *testing.T, path string) (docsValidateReceipt, error) {
	t.Helper()
	command := docsCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"validate", path})
	err := command.Execute()
	var receipt docsValidateReceipt
	if decodeErr := json.NewDecoder(&output).Decode(&receipt); decodeErr != nil {
		t.Fatalf("decoding CLI result: %v output=%s", decodeErr, output.String())
	}
	return receipt, err
}
