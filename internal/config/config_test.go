package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/secrets"
)

func TestLoadSecretAndToolPolicy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	data := `
workspace: demo
secrets:
  root: ~/phase1-secrets
  backend: plain
  sets:
    integration-tests:
      local_eligible: true
repos:
  - name: api
    url: file:///tmp/api
    secret_refs:
      - secretref://demo/integration-tests/CANARY
    tool_policy:
      allowed_commands:
        - [git]
        - [go, test]
      denied_commands:
        - [printenv]
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Secrets.Backend != secrets.BackendPlain {
		t.Fatalf("backend = %q", cfg.Secrets.Backend)
	}
	if !strings.HasSuffix(cfg.Secrets.Root, "phase1-secrets") || strings.HasPrefix(cfg.Secrets.Root, "~") {
		t.Fatalf("root was not expanded: %q", cfg.Secrets.Root)
	}
	if len(cfg.Repos[0].ToolPolicy.AllowedCommands) != 2 || cfg.Repos[0].ToolPolicy.AllowedCommands[1][1] != "test" {
		t.Fatalf("tool policy = %+v", cfg.Repos[0].ToolPolicy)
	}
	if !cfg.SecretPolicies()["demo/integration-tests"].LocalEligible {
		t.Fatal("local_eligible policy was not propagated")
	}
}

func TestExampleConfigLoadsWithStrictFields(t *testing.T) {
	t.Parallel()
	if _, err := Load(filepath.Join("..", "..", "conveyor.example.yaml")); err != nil {
		t.Fatalf("example config: %v", err)
	}
}

func TestLoadRejectsSecretWorkspaceOrMissingPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ref  string
		sets string
		want string
	}{
		{name: "workspace", ref: "secretref://other/default/X", sets: "    default: {local_eligible: true}\n", want: "does not match"},
		{name: "missing set", ref: "secretref://demo/default/X", want: "no delivery policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "conveyor.yaml")
			data := "workspace: demo\nsecrets:\n  backend: plain\n  sets:\n" + tc.sets + "repos:\n  - name: api\n    url: file:///tmp/api\n    secret_refs: [" + tc.ref + "]\n"
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadRejectsRepoNameThatEscapesJobsRoot(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	data := "workspace: acme\nsecrets:\n  backend: plain\nrepos:\n  - name: ../outside\n    url: file:///tmp/repo\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "path-safe segment") {
		t.Fatalf("Load error = %v, want path-safe segment rejection", err)
	}
}

func TestLoadRejectsUnknownOrUnenforceablePolicyFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		policy string
		want   string
	}{
		{name: "unknown field", policy: "      denied_command: [[env]]\n", want: "field denied_command not found"},
		{name: "network allow", policy: "      network_allow: [api.example.com]\n", want: "not enforceable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "conveyor.yaml")
			data := "workspace: acme\nsecrets:\n  backend: plain\nrepos:\n  - name: api\n    url: file:///tmp/repo\n    tool_policy:\n" + tc.policy
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadCredentialRouting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	data := `
workspace: demo
database:
  backend: postgres
  url: postgres://fixture
secrets:
  backend: plain
  sets:
    harness-auth: {local_eligible: true}
credentials:
  - id: claude-alice
    owner_id: alice
    kind: personal_sub
    vendor: anthropic
    harness: claude-code
    ref: secretref://demo/harness-auth/CLAUDE_CODE_OAUTH_TOKEN
vendor_policies:
  - vendor: anthropic
    harness: claude-code
    auth_mode: personal_sub
    subscription_headless: allowed
    reviewed_at: 2026-07-10
    source_url: https://example.com/terms
routing:
  owner_id: alice
  stages:
    implement:
      harnesses: [claude-code, codex]
      model_tier: mid
      budget_usd: 2.50
repos:
  - name: api
    url: file:///tmp/api
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Credentials[0].OwnerKind != "user" || cfg.Routing.Stages["implement"].Harnesses[0] != "claude-code" {
		t.Fatalf("config = %+v", cfg)
	}
}
