package config

import (
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPhase47Config(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	data := `workspace: demo
pack_dir: pack
max_bounces: 2
routing:
  stages:
    triage: {model: gpt-5.4, timeout: 20m, execution: in_process}
    spec: {model: gpt-5.4, timeout: 30m, execution: in_process}
    implement: {model: operator-owned, timeout: 4h, execution: mcp}
    review: {model: operator-owned, timeout: 1h, execution: mcp}
repos:
  - {name: conveyor, url: https://example.test/conveyor, base: main}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routing.Stages["implement"].Execution != ExecutionMCP || cfg.Repos[0].Name != "conveyor" {
		t.Fatalf("config=%+v", cfg)
	}
}

func TestLoadRejectsRemovedBudgetConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	data := `workspace: demo
pack_dir: pack
routing:
  stages:
    triage: {model: gpt, timeout: 20m, execution: in_process}
    spec: {model: gpt, timeout: 30m, execution: in_process}
    implement: {model: operator, budget_usd: 10, timeout: 4h, execution: mcp}
    review: {model: operator, timeout: 1h, execution: mcp}
repos:
  - {name: repo, url: https://example.test/repo, base: main}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "field budget_usd not found") {
		t.Fatalf("error=%v", err)
	}
}

func TestParseStoredWorkspaceDocumentRemovesLegacyBudget(t *testing.T) {
	data := `workspace: demo
max_bounces: 2
routing:
  stages:
    triage: {model: gpt, timeout: 20m, execution: in_process}
    spec: {model: gpt, timeout: 30m, execution: in_process}
    implement: {model: operator, budget_usd: 10, timeout: 4h, execution: mcp}
    review: {model: operator, timeout: 1h, execution: mcp}
repos:
  - {name: repo, url: https://example.test/repo, base: main}
`
	cfg, legacy, err := ParseStoredWorkspaceDocument([]byte(data), validConfig(), "stored")
	if err != nil || !legacy {
		t.Fatalf("legacy=%t err=%v", legacy, err)
	}
	canonical, err := MarshalWorkspaceDocument(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "budget") {
		t.Fatalf("canonical config retained budget: %s", canonical)
	}
}

func TestLoadRejectsRetiredConfigSurface(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := os.WriteFile(path, []byte("workspace: demo\ncredentials: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "field credentials not found") {
		t.Fatalf("error=%v", err)
	}
}

func TestReviewMayUseInProcessFallback(t *testing.T) {
	deployment := validConfig()
	document := deployment.WorkspaceDocument()
	route := document.Routing.Stages["review"]
	route.Execution = ExecutionInProcess
	document.Routing.Stages["review"] = route
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ParseWorkspaceDocument(raw, deployment, "test"); err != nil {
		t.Fatal(err)
	}
}

func validConfig() *Config {
	return &Config{Workspace: "demo", PackDir: ".", MaxBounces: 2, Database: Database{Backend: "memory"}, Routing: Routing{Stages: map[string]StageRoute{"triage": {Model: "gpt", TimeoutText: "20m", Execution: ExecutionInProcess}, "spec": {Model: "gpt", TimeoutText: "30m", Execution: ExecutionInProcess}, "implement": {Model: "operator", TimeoutText: "4h", Execution: ExecutionMCP}, "review": {Model: "operator", TimeoutText: "1h", Execution: ExecutionMCP}}}, Repos: []Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}
}
