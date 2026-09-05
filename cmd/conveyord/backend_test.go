package main

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store/backend"
)

func TestConveyordRejectsVolatileBackend(t *testing.T) {
	if configPath := os.Getenv("CONVEYOR_TEST_VOLATILE_CONFIG"); configPath != "" {
		flag.CommandLine = flag.NewFlagSet("conveyord", flag.ExitOnError)
		os.Args = []string{"conveyord", "-config", configPath}
		main()
		return
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "conveyor.yaml")
	envPath := filepath.Join(dir, "empty.env")
	configuration := `workspace: demo
database: {backend: memory}
routing:
  stages:
    triage: {model: fixture, timeout: 20m, execution: in_process}
    spec: {model: fixture, timeout: 30m, execution: in_process}
    implement: {model: fixture, timeout: 4h, execution: mcp}
    review: {model: fixture, timeout: 1h, execution: mcp}
repos:
  - {name: repo, url: https://example.test/repo, base: main}
`
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConveyordRejectsVolatileBackend$")
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "CONVEYOR_") {
			command.Env = append(command.Env, item)
		}
	}
	command.Env = append(command.Env,
		"CONVEYOR_TEST_VOLATILE_CONFIG="+configPath,
		"CONVEYOR_ENV_FILE="+envPath,
		"CONVEYOR_API_TOKEN=fixture-api-token",
		"CONVEYOR_LLM_API_KEY=fixture-llm-key")
	output, err := command.CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(output), backend.ErrVolatileBackend.Error()) {
		t.Fatalf("daemon did not reject memory at startup: %v\n%s", err, output)
	}
}
