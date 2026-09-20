package verification

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const validManifest = `schema_version: 1
kits:
  - id: sample
    name: Sample kit
    version: "1.0"
    path: kits/sample
    governing_pins:
      requirements: [{document_id: req-example, version: 3}]
      system_designs: [{document_id: component-example, version: 2}]
    exercises:
      - id: observe
        stages: [verify]
        kind: script
        argv: ["./exercise.sh"]
        cwd: "."
        timeout_seconds: 120
        prerequisites: [{id: api, kind: service, environment_binding: api}]
        permissions: [{kind: network, target_binding: api}]
        inputs: [{name: case_id, type: string, required: true, sensitive: false}]
        required_assertions: [readable]
        retry_policy: reconciliation_required
        operations:
          - id: create
            target_binding: api
            reconciliation: {argv: ["./reconcile.sh"], timeout_seconds: 60}
        evidence_outputs: [{type: api_exchange, schema_version: 1, minimum_items: 1}]
        supports: [{document_id: req-example, version: 3, acceptance_criterion_id: AC-1.1}]
    ui: {argv: ["./ui.sh"], port: 8080, assets: [index.html]}
`

func fixtureManifest(t *testing.T) *Manifest {
	t.Helper()
	m, err := Parse(strings.NewReader(validManifest), nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func TestManifestFields(t *testing.T) {
	m := fixtureManifest(t)
	e := m.Kits[0].Exercises[0]
	if e.Operations[0].Reconciliation.TimeoutSeconds != 60 || e.Inputs[0].Name != "case_id" || !e.Inputs[0].Required || e.Prerequisites[0].EnvironmentBinding != "api" || e.Permissions[0].TargetBinding != "api" || e.EvidenceOutputs[0].MinimumItems != 1 || e.Supports[0].AcceptanceCriterionID != "AC-1.1" || m.Kits[0].UI.Port != 8080 {
		t.Fatalf("lost contract fields: %+v", m)
	}
}
func TestManifestRefusals(t *testing.T) {
	tests := []struct{ name, old, new, want string }{
		{"unknown root", "schema_version: 1", "schema_version: 1\nsurprise: true", "manifest.surprise"},
		{"unknown nested", "timeout_seconds: 60", "timeout_seconds: 60, surprise: true", "reconciliation.surprise"},
		{"duplicate root", "schema_version: 1", "schema_version: 1\nschema_version: 1", "schema_version: duplicate"},
		{"duplicate nested", "timeout_seconds: 60", "timeout_seconds: 60, timeout_seconds: 70", "reconciliation.timeout_seconds: duplicate"},
		{"alias", "stages: [verify]", "stages: &stages [verify]", "stages: aliases"},
		{"alias reference", "stages: [verify]", "stages: &stages [verify]", "aliases"},
		{"absolute", "path: kits/sample", "path: /etc", ".path:"},
		{"traversal", "cwd: \".\"", "cwd: ../escape", ".cwd:"},
		{"windows absolute", "path: kits/sample", "path: 'C:\\escape'", ".path:"},
		{"argv traversal", "./exercise.sh", "../escape.sh", ".argv:"},
		{"ui traversal", "assets: [index.html]", "assets: [../secret]", "ui.assets[0]:"},
		{"missing retry", "        retry_policy: reconciliation_required\n", "", "retry_policy:"},
		{"unknown retry", "retry_policy: reconciliation_required", "retry_policy: always", "retry_policy:"},
		{"missing reconciler", "            reconciliation: {argv: [\"./reconcile.sh\"], timeout_seconds: 60}\n", "", "operations[0].reconciliation:"},
		{"unbounded reconciler", "timeout_seconds: 60", "timeout_seconds: 0", "reconciliation.timeout_seconds:"},
		{"missing safety basis", "retry_policy: reconciliation_required", "retry_policy: safe_to_replay", "safety_basis:"},
		{"unknown permission", "kind: network", "kind: superuser", "permissions[0].kind:"},
		{"unknown prerequisite", "kind: service", "kind: guessed", "prerequisites[0].kind:"},
		{"undeclared operation target", "target_binding: api\n", "target_binding: missing\n", "operations[0].target_binding:"},
		{"bad support", "acceptance_criterion_id: AC-1.1", "acceptance_criterion_id: REQ-1", "supports[0]:"},
		{"unpinned support", "document_id: req-example, version: 3, acceptance", "document_id: other, version: 3, acceptance", "supports[0]:"},
		{"missing assertion list", "        required_assertions: [readable]\n", "", "required_assertions:"},
		{"duplicate assertion", "required_assertions: [readable]", "required_assertions: [readable, readable]", "required_assertions:"},
		{"wrong scalar type", "version: \"1.0\"", "version: 1", ".version:"},
		{"unknown schema", "schema_version: 1", "schema_version: 2", "schema_version:"},
		{"multiple documents", "schema_version: 1", "schema_version: 1\n---\nschema_version: 1", "exactly one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Replace(validManifest, tt.old, tt.new, 1)
			_, err := Parse(strings.NewReader(input), nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %s", err, tt.want)
			}
		})
	}
	t.Run("size", func(t *testing.T) {
		_, err := Parse(strings.NewReader(strings.Repeat(" ", MaxManifestBytes+1)), nil)
		if err == nil || !strings.Contains(err.Error(), "manifest: exceeds 1 MiB") {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct {
		name, want string
		mutate     func(*Manifest)
	}{
		{"duplicate kit", "duplicate kit ID", func(m *Manifest) { m.Kits = append(m.Kits, m.Kits[0]) }},
		{"duplicate exercise", "duplicate exercise ID", func(m *Manifest) { m.Kits[0].Exercises = append(m.Kits[0].Exercises, m.Kits[0].Exercises[0]) }},
		{"kit count", "exceeds 100 kits", func(m *Manifest) {
			k := m.Kits[0]
			for i := 1; i <= 100; i++ {
				k.ID = fmt.Sprint(i)
				m.Kits = append(m.Kits, k)
			}
		}},
		{"exercise count", "exceeds 100 exercises", func(m *Manifest) {
			e := m.Kits[0].Exercises[0]
			for i := 1; i <= 100; i++ {
				e.ID = fmt.Sprint(i)
				m.Kits[0].Exercises = append(m.Kits[0].Exercises, e)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureManifest(t)
			tc.mutate(m)
			data, err := yaml.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Parse(strings.NewReader(string(data)), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
func TestManifestPaths(t *testing.T) {
	root := t.TempDir()
	kit := filepath.Join(root, "kits/sample")
	if err := os.MkdirAll(kit, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"exercise.sh", "reconcile.sh", "ui.sh", "index.html"} {
		if err := os.WriteFile(filepath.Join(kit, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	check, err := FilesystemPathCheck(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Parse(strings.NewReader(validManifest), check); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, target, want string }{{"outside repository", filepath.Join(t.TempDir(), "secret"), "symlink escapes"}, {"outside kit", filepath.Join(root, "secret"), "symlink escapes"}} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(tc.target, []byte("secret"), 0600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(kit, "escape")
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(link)
			input := strings.Replace(validManifest, "assets: [index.html]", "assets: [escape]", 1)
			_, err := Parse(strings.NewReader(input), check)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal(err)
			}
		})
	}
	if err := os.Remove(filepath.Join(kit, "exercise.sh")); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(strings.NewReader(validManifest), check); err == nil || !strings.Contains(err.Error(), "exercises[0].argv") {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kit, ".git"), []byte("gitdir: external"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := check(".", "kits/sample"); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Fatal(err)
	}
}

func TestManifestPermissionContainment(t *testing.T) {
	root := t.TempDir()
	kit := filepath.Join(root, "kits/sample")
	if err := os.MkdirAll(kit, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"exercise.sh", "reconcile.sh", "ui.sh", "index.html"} {
		if err := os.WriteFile(filepath.Join(kit, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(kit, "escape")); err != nil {
		t.Fatal(err)
	}
	check, err := FilesystemPathCheck(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "absent"), filepath.Join(kit, "escape-dangling")); err != nil {
		t.Fatal(err)
	}
	for _, dest := range []string{"escape", "escape/new-file", "escape-dangling", "escape-dangling/new-file", "new-output/file"} {
		input := strings.Replace(validManifest, "permissions: [{kind: network, target_binding: api}]", "permissions: [{kind: network, target_binding: api}, {kind: filesystem_write, path: "+dest+"}]", 1)
		_, err := Parse(strings.NewReader(input), check)
		if strings.HasPrefix(dest, "escape") {
			if err == nil || !strings.Contains(err.Error(), "permissions[1].path") {
				t.Fatalf("%s: %v", dest, err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
}
func TestManifestRetainsInvalidSiblings(t *testing.T) {
	input := validManifest + "  - id: invalid\n    unknown: true\n"
	m, err := Parse(strings.NewReader(input), nil)
	if err == nil || len(m.Kits) != 2 || m.Kits[1].ID != "invalid" || len(m.Kits[0].Diagnostics) != 0 || len(m.Kits[1].Diagnostics) == 0 {
		t.Fatalf("%+v %v", m, err)
	}
}
