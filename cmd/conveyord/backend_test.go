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
	"github.com/kidus-tiliksew/conveyor/internal/testsupport/connprobe"
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

func TestConveyordSelectsSingleStoreBackend(t *testing.T) {
	if configPath := os.Getenv("CONVEYOR_TEST_SINGLESTORE_CONFIG"); configPath != "" {
		flag.CommandLine = flag.NewFlagSet("conveyord", flag.ExitOnError)
		os.Args = []string{"conveyord", "-config", configPath}
		main()
		return
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "conveyor.yaml")
	envPath := filepath.Join(dir, "empty.env")
	configuration := `workspace: demo
database: {backend: singlestore, url: "invalid"}
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
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConveyordSelectsSingleStoreBackend$")
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "CONVEYOR_") {
			command.Env = append(command.Env, item)
		}
	}
	command.Env = append(command.Env,
		"CONVEYOR_TEST_SINGLESTORE_CONFIG="+configPath,
		"CONVEYOR_ENV_FILE="+envPath,
		"CONVEYOR_API_TOKEN=fixture-api-token",
		"CONVEYOR_LLM_API_KEY=fixture-llm-key")
	output, err := command.CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(output), "open store: invalid SingleStore DSN") {
		t.Fatalf("daemon did not reach SingleStore driver validation: %v\n%s", err, output)
	}
}

// The real daemon process, with the backend-only database node conveyor init
// writes and a padded CONVEYOR_DATABASE_URL, reaches connection setup with the
// exact user and database. Before the store boundary trimmed the value, this
// process exited at "open store: configure Postgres pool" with no connection
// attempt (req-deployment-and-releases REQ-2, AC-2.3).
func TestConveyordReachesConnectionSetupWithPaddedDatabaseURL(t *testing.T) {
	if configPath := os.Getenv("CONVEYOR_TEST_PADDED_URL_CONFIG"); configPath != "" {
		flag.CommandLine = flag.NewFlagSet("conveyord", flag.ExitOnError)
		os.Args = []string{"conveyord", "-config", configPath}
		main()
		return
	}
	const password = "fixture-daemon-pw-8d2"
	want := connprobe.Handshake{User: "fixture_user", Database: "fixture_db"}
	for _, tc := range []struct {
		name, backendName string
		probe             func(*testing.T) *connprobe.Probe
		connection        func(addr string) string
	}{
		{"postgres", "postgres", connprobe.Postgres, func(addr string) string {
			return "postgres://fixture_user:" + password + "@" + addr + "/fixture_db?sslmode=disable&connect_timeout=2"
		}},
		{"singlestore URL", "singlestore", connprobe.MySQL, func(addr string) string {
			return "singlestore://fixture_user:" + password + "@" + addr + "/fixture_db"
		}},
		{"singlestore DSN", "singlestore", connprobe.MySQL, func(addr string) string {
			return "fixture_user:" + password + "@tcp(" + addr + ")/fixture_db"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := tc.probe(t)
			dir := t.TempDir()
			configPath := filepath.Join(dir, "conveyor.yaml")
			envPath := filepath.Join(dir, "empty.env")
			if err := os.WriteFile(configPath, []byte(durableStartupConfig(tc.backendName, "demo")), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(envPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConveyordReachesConnectionSetupWithPaddedDatabaseURL$")
			for _, item := range os.Environ() {
				if !strings.HasPrefix(item, "CONVEYOR_") {
					command.Env = append(command.Env, item)
				}
			}
			command.Env = append(command.Env,
				"CONVEYOR_TEST_PADDED_URL_CONFIG="+configPath,
				"CONVEYOR_ENV_FILE="+envPath,
				"CONVEYOR_DATABASE_URL=  "+tc.connection(probe.Addr)+" \n",
				"CONVEYOR_API_TOKEN=fixture-api-token",
				"CONVEYOR_LLM_API_KEY=fixture-llm-key")
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("daemon did not exit")
			}
			// Failure text below reports facts about the output, never the output.
			if err == nil {
				t.Fatal("the probe accepted no session, so the daemon must exit nonzero")
			}
			if strings.Contains(string(output), password) {
				t.Fatal("daemon output holds the password")
			}
			if strings.Contains(string(output), "connection string") || strings.Contains(string(output), "invalid SingleStore") {
				t.Fatal("daemon stopped at the parser before connection setup")
			}
			if !strings.Contains(string(output), "open store:") {
				t.Fatalf("daemon exited before it opened the store: %v", err)
			}
			probe.Require(t, "daemon", want)
		})
	}
}
