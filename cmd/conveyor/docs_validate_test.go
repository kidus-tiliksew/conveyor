package main

import (
	"bytes"
	"context"
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
		diagnostics   int
	}{
		{"valid", valid, 0},
		{"missing-doc", strings.Replace(valid, "docs/api.md", "docs/absent.md", 1), 1},
		{"missing-source", strings.Replace(valid, "internal/**", "untracked/**", 1), 1},
		{"closed-schema", valid + "unexpected: true\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(declaration, []byte(tc.content), 0644); err != nil {
				t.Fatal(err)
			}
			receipt, err := validateLocalDocs(context.Background(), root)
			if (err != nil) != (tc.diagnostics > 0) || len(receipt.Diagnostics) != tc.diagnostics {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			command := docsCmd()
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			command.SetArgs([]string{"validate", root})
			err = command.Execute()
			if (err != nil) != (tc.diagnostics > 0) || !strings.Contains(output.String(), "diagnostics") {
				t.Fatalf("CLI err=%v output=%s", err, output.String())
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
	command := docsCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"validate", root})
	if err := command.Execute(); err != nil {
		t.Fatalf("absent declaration refused: %v %s", err, output.String())
	}
	if !strings.Contains(output.String(), "gate off: declaration absent") || !strings.Contains(output.String(), `"diagnostics":[]`) {
		t.Fatalf("gate-off result=%s", output.String())
	}
}
