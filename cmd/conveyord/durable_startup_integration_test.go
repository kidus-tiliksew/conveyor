package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/backend"
)

// DEC-39; component-runtime. Both durable selections execute real migrations,
// identity/workspace bootstrap, startup reconciliation and ordered shutdown.
func TestConveyordDurableStartupIntegration(t *testing.T) {
	if path := os.Getenv("CONVEYOR_DURABLE_STARTUP_CONFIG"); path != "" {
		flag.CommandLine = flag.NewFlagSet("conveyord", flag.ExitOnError)
		os.Args = []string{"conveyord", "-config", path, "-addr", os.Getenv("CONVEYOR_DURABLE_STARTUP_ADDR"), "-poll-github", "0s"}
		main()
		return
	}
	for _, name := range []string{"postgres", "singlestore"} {
		t.Run(name, func(t *testing.T) {
			raw := durableStartupDatabase(t, name)
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "conveyor.yaml")
			envPath := filepath.Join(dir, "empty.env")
			requireStartupWrite(t, envPath, nil)
			requireStartupWrite(t, cfgPath, []byte(fmt.Sprintf(`workspace: startup
database: {backend: %s}
routing:
  stages:
    triage: {model: fixture, timeout: 20m, execution: in_process}
    spec: {model: fixture, timeout: 30m, execution: mcp}
    implement: {model: fixture, timeout: 4h, execution: mcp}
    review: {model: fixture, timeout: 1h, execution: mcp}
repos:
  - {name: repo, url: https://example.test/repo, base: main}
`, name)))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := listener.Addr().String()
			listener.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConveyordDurableStartupIntegration$")
			for _, e := range os.Environ() {
				if !strings.HasPrefix(e, "CONVEYOR_") {
					cmd.Env = append(cmd.Env, e)
				}
			}
			cmd.Env = append(cmd.Env, "CONVEYOR_DURABLE_STARTUP_CONFIG="+cfgPath, "CONVEYOR_DURABLE_STARTUP_ADDR="+addr, "CONVEYOR_ENV_FILE="+envPath, "CONVEYOR_DATABASE_URL="+raw, "CONVEYOR_API_TOKEN=startup-fixture-token", "CONVEYOR_LLM_API_KEY=unused-fixture-key")
			logPath := filepath.Join(dir, "daemon.log")
			output, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			cmd.Stdout = output
			cmd.Stderr = output
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
				}
			})
			client := http.Client{Timeout: time.Second}
			ready := false
			for !ready && ctx.Err() == nil {
				select {
				case err := <-done:
					b, _ := os.ReadFile(logPath)
					t.Fatalf("startup exited: %v\n%s", err, b)
				default:
				}
				response, err := client.Get("http://" + addr + "/healthz")
				if err == nil {
					response.Body.Close()
					ready = response.StatusCode == http.StatusOK
				}
				if !ready {
					time.Sleep(50 * time.Millisecond)
				}
			}
			if !ready {
				t.Fatal("daemon did not become healthy")
			}
			if err = cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
				done <- nil
			case <-ctx.Done():
				t.Fatal("shutdown timed out")
			}
			b, _ := os.ReadFile(logPath)
			label := "PostgreSQL"
			if name == "singlestore" {
				label = "SingleStore"
			}
			if !strings.Contains(string(b), "using durable "+label+" store with the event log") {
				t.Fatalf("startup log missing backend: %s", b)
			}
			st, err := backend.Open(t.Context(), config.Database{Backend: name, URL: raw})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if _, err = st.VerifyPersonalAccessToken(t.Context(), "startup-fixture-token"); err != nil {
				t.Fatal(err)
			}
			if _, err = st.GetWorkspace(store.WithWorkspace(t.Context(), "startup"), "startup"); err != nil {
				t.Fatal(err)
			}
			if st.Log() == nil {
				t.Fatal("missing durable event log")
			}
		})
	}
}
func requireStartupWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func durableStartupDatabase(t *testing.T, name string) string {
	t.Helper()
	variable := "CONVEYOR_TEST_DATABASE_URL"
	if name == "singlestore" {
		variable = "CONVEYOR_TEST_SINGLESTORE_URL"
	}
	raw := os.Getenv(variable)
	if raw == "" {
		t.Skip(variable + " is unset")
	}
	if name == "postgres" {
		u, err := url.Parse(raw)
		if err != nil || !strings.HasSuffix(u.Path, "_test") {
			t.Fatal("startup fixture requires a PostgreSQL _test database")
		}
		admin, err := pgxpool.New(t.Context(), raw)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(admin.Close)
		schema := "startup_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
		if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, err := admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			if err != nil {
				t.Error(err)
			}
		})
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	cfg, err := mysql.ParseDSN(raw)
	if err != nil || !strings.HasSuffix(cfg.DBName, "_test") {
		t.Fatal("startup fixture requires a SingleStore _test DSN")
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	admin := sql.OpenDB(connector)
	t.Cleanup(func() { admin.Close() })
	database := fmt.Sprintf("startup_%d_test", time.Now().UnixNano())
	if _, err = admin.ExecContext(t.Context(), "CREATE DATABASE `"+database+"` PARTITIONS 2"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE `"+database+"`"); err != nil {
			t.Error(err)
		}
	})
	cfg.DBName = database
	return cfg.FormatDSN()
}
