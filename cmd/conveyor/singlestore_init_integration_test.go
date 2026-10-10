package main

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/config"
)

func TestSingleStoreInitAndUserIntegration(t *testing.T) {
	raw := os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL")
	if raw == "" {
		t.Skip("CONVEYOR_TEST_SINGLESTORE_URL is unset")
	}
	cfg, err := mysql.ParseDSN(raw)
	if err != nil || !strings.HasSuffix(cfg.DBName, "_test") {
		t.Fatal("CLI fixture requires a SingleStore _test DSN")
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	admin := sql.OpenDB(connector)
	t.Cleanup(func() { admin.Close() })
	database := fmt.Sprintf("cli_init_%d_test", time.Now().UnixNano())
	if _, err = admin.ExecContext(t.Context(), "CREATE DATABASE `"+database+"` PARTITIONS 2"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(store.WithActor(context.Background(), store.SystemActor()), 30*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE `"+database+"`"); err != nil {
			t.Error(err)
		}
	})
	cfg.DBName = database
	t.Setenv("CONVEYOR_DATABASE_URL", cfg.FormatDSN())
	t.Setenv("CONVEYOR_API_TOKEN", "singlestore-init-fixture-token")
	t.Setenv(config.LLMAPIKeyEnv, "unused-fixture-key")
	t.Setenv(config.PublicURLEnv, "https://conveyor.example/")
	answers := initAnswers{Organization: "SingleStore fixture", OperatorName: "Owner", OperatorEmail: "owner@example.test", WorkspaceID: "fresh", WorkspaceName: "Fresh", RepositoryName: "app", RepositoryURL: "https://github.com/example/app", BaseBranch: "main"}
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	var output strings.Builder
	if err = initializeDeployment(store.WithActor(t.Context(), store.SystemActor()), &output, path, answers); err != nil {
		t.Fatal(err)
	}
	first := signInTokenFromOutput(t, output.String())
	if first == "" {
		t.Fatal("init did not issue a sign-in link")
	}
	output.Reset()
	if err = initializeDeployment(store.WithActor(t.Context(), store.SystemActor()), &output, path, answers); err != nil {
		t.Fatal(err)
	}
	if next := signInTokenFromOutput(t, output.String()); next == "" || next == first {
		t.Fatal("init retry did not rotate sign-in link")
	}
	loaded, err := config.LoadDeployment(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Database.Backend != "singlestore" {
		t.Fatal("init persisted the wrong backend")
	}
	// A file in the pre-DEC-56 init shape is accepted on rerun and left
	// unchanged (component-identity-membership).
	legacyPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	legacy := legacyExecutorInitConfig(t, cfg.FormatDSN(), answers)
	if err = os.WriteFile(legacyPath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err = initializeDeployment(store.WithActor(t.Context(), store.SystemActor()), &output, legacyPath, answers); err != nil {
		t.Fatalf("rerun over a legacy executor config: %v", err)
	}
	if after, readErr := os.ReadFile(legacyPath); readErr != nil || string(after) != string(legacy) {
		t.Fatalf("legacy config was rewritten: %v", readErr)
	}
	command := userCmd()
	output.Reset()
	command.SetOut(&output)
	command.SetArgs([]string{"issue-link", answers.OperatorEmail})
	if err = command.Execute(); err != nil {
		t.Fatal(err)
	}
	if signInTokenFromOutput(t, output.String()) == "" {
		t.Fatal("user issue-link did not open the SingleStore deployment")
	}
}
