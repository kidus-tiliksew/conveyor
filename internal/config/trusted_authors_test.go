package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repos[].trusted_authors loads, copies without aliasing, round-trips through
// the stored policy document, and refuses blank, spaced, and duplicate user
// IDs (req-risk-based-review REQ-9, AC-9.1, AC-9.3; feature-risk-based-review
// RR-1).
func TestRepoTrustedAuthors(t *testing.T) {
	load := func(t *testing.T, trusted string) (*Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "conveyor.yaml")
		data := `workspace: demo
routing:
  stages:
    triage: {model: gpt, timeout: 20m, execution: in_process}
    spec: {model: gpt, timeout: 30m, execution: in_process}
    implement: {model: operator, timeout: 4h, execution: mcp}
    review: {model: reviewer, timeout: 1h, execution: mcp}
repos:
  - {name: repo, url: https://example.test/repo, base: main` + trusted + `}
`
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return loadDeploymentForTest(path)
	}
	cfg, err := load(t, `, trusted_authors: [usr_a, usr_b]`)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := cfg.Repo("repo")
	if strings.Join(repo.TrustedAuthors, ",") != "usr_a,usr_b" {
		t.Fatalf("trusted_authors=%v", repo.TrustedAuthors)
	}
	clone := cloneConfig(cfg)
	clone.Repos[0].TrustedAuthors[0] = "changed"
	if cfg.Repos[0].TrustedAuthors[0] != "usr_a" {
		t.Fatal("a clone aliases trusted_authors")
	}
	stored, err := MarshalPolicyDocument(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored), "trusted_authors:") || !strings.Contains(string(stored), "usr_b") {
		t.Fatalf("stored document drops trusted_authors:\n%s", stored)
	}
	empty, err := load(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if repo, _ := empty.Repo("repo"); repo.TrustedAuthors != nil {
		t.Fatalf("omitted trusted_authors=%v, want empty", repo.TrustedAuthors)
	}
	for name, trusted := range map[string]string{
		"Blank":     `, trusted_authors: [""]`,
		"Spaced":    `, trusted_authors: ["usr a"]`,
		"Duplicate": `, trusted_authors: [usr_a, usr_a]`,
	} {
		if _, err := load(t, trusted); err == nil || !strings.Contains(err.Error(), "trusted_authors") {
			t.Fatalf("%s: err=%v, want a trusted_authors refusal", name, err)
		}
	}
}
