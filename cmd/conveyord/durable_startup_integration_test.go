package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
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
			requireStartupWrite(t, cfgPath, []byte(durableStartupConfig(name, "startup")))
			daemon := startDurableDaemon(t, raw, cfgPath, 45*time.Second)
			if code, body := daemon.post(t, "/v1/workspaces", durableStartupToken, `{"id":"created","name":"Created","document":{"repos":[]}}`); code != http.StatusCreated {
				t.Fatalf("create workspace status=%d body=%s", code, body)
			}
			b := daemon.stop(t)
			label := "PostgreSQL"
			if name == "singlestore" {
				label = "SingleStore"
			}
			if !strings.Contains(string(b), "using durable "+label+" store with the event log") {
				t.Fatalf("startup log missing backend: %s", b)
			}
			deprecationWarning := config.DeprecatedGitHubAppKeyEncryptionKeyEnv + " is deprecated; rename it to " + config.GitHubAppKeyEncryptionKeyEnv
			// The routing-only fixture carries executor models; the deployment
			// loader ignores and names them (component-runtime).
			if !strings.Contains(string(b), "ignoring retired execution detail in deployment configuration (DEC-56): routing.stages.implement.execution, routing.stages.implement.model") {
				t.Fatalf("startup did not load the deployment file through the deployment loader: %s", b)
			}
			if strings.Count(string(b), deprecationWarning) != 1 || strings.Contains(string(b), "GitHub App key encryption unavailable") {
				t.Fatalf("startup log must warn once about the deprecated App key variable and install the key: %s", b)
			}
			st, err := backend.Open(t.Context(), config.Database{Backend: name, URL: raw})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if _, err = st.VerifyPersonalAccessToken(t.Context(), durableStartupToken); err != nil {
				t.Fatal(err)
			}
			if _, err = st.GetWorkspace(store.WithWorkspace(t.Context(), "startup"), "startup"); err != nil {
				t.Fatal(err)
			}
			if st.Log() == nil {
				t.Fatal("missing durable event log")
			}
			if _, err = st.GetWorkspace(store.WithWorkspace(t.Context(), "created"), "created"); err != nil {
				t.Fatal(err)
			}
		})
		// DEC-63(3), DEC-63(4): a deployment without a configured workspace
		// serves its owner, who creates the first workspace through the API.
		t.Run(name+"/NoConfiguredWorkspaceFirstCreation", func(t *testing.T) {
			raw := durableStartupDatabase(t, name)
			cfgPath := filepath.Join(t.TempDir(), "conveyor.yaml")
			requireStartupWrite(t, cfgPath, []byte(durableStartupConfig(name, "")))
			daemon := startDurableDaemon(t, raw, cfgPath, 90*time.Second)
			if code, body := daemon.get(t, "/v1/workspaces", durableStartupToken); code != http.StatusOK || strings.Contains(body, `"id"`) {
				t.Fatalf("owner workspaces before creation status=%d body=%s", code, body)
			}
			if code, body := daemon.post(t, "/v1/workspaces", durableStartupToken, `{"id":"first","name":"First","document":{"repos":[]}}`); code != http.StatusCreated {
				t.Fatalf("first workspace status=%d body=%s", code, body)
			}
			if code, body := daemon.get(t, "/v1/workspaces", durableStartupToken); code != http.StatusOK || !strings.Contains(body, `"id":"first"`) {
				t.Fatalf("owner workspaces after creation status=%d body=%s", code, body)
			}
			logs := daemon.stop(t)
			if !strings.Contains(string(logs), "no bootstrap workspace configured; first-run workspace creation is available") {
				t.Fatalf("startup did not report first-run creation: %s", logs)
			}
			st, err := backend.Open(t.Context(), config.Database{Backend: name, URL: raw})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			owner, err := st.VerifyPersonalAccessToken(t.Context(), durableStartupToken)
			if err != nil {
				t.Fatal(err)
			}
			if caller, err := st.GetCallerIdentity(t.Context(), owner.ID, "first"); err != nil || caller.Role != core.WorkspaceRoleOperator {
				t.Fatalf("creator binding=%+v err=%v", caller, err)
			}
			if org := durableScalar(t, name, raw, "SELECT org_id FROM workspaces WHERE id='first'"); org != "deployment" {
				t.Fatalf("first workspace organization=%q", org)
			}
			if orgs := durableScalar(t, name, raw, "SELECT COUNT(*) FROM orgs"); orgs != "1" {
				t.Fatalf("organizations=%s", orgs)
			}
		})
		// DEC-63(4): a demotion and an exclusion survive a real restart.
		t.Run(name+"/MembershipDecisionsSurviveRestart", func(t *testing.T) {
			raw := durableStartupDatabase(t, name)
			cfgPath := filepath.Join(t.TempDir(), "conveyor.yaml")
			requireStartupWrite(t, cfgPath, []byte(durableStartupConfig(name, "startup")))
			daemon := startDurableDaemon(t, raw, cfgPath, 90*time.Second)
			st, err := backend.Open(t.Context(), config.Database{Backend: name, URL: raw})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			owner, err := st.VerifyPersonalAccessToken(t.Context(), durableStartupToken)
			if err != nil {
				t.Fatal(err)
			}
			if code, body := daemon.post(t, "/v1/users", durableStartupToken, `{"email":"second@example.test","display_name":"Second"}`); code != http.StatusOK || body != "{\"accepted\":true}\n" {
				t.Fatalf("provision second status=%d body=%q", code, body)
			}
			if code, body := daemon.post(t, "/v1/workspaces", durableStartupToken, `{"id":"excluded","name":"Excluded","document":{"repos":[]}}`); code != http.StatusCreated {
				t.Fatalf("excluded workspace status=%d body=%s", code, body)
			}
			for _, workspace := range []string{"startup", "excluded"} {
				if code, body := daemon.post(t, "/v1/workspaces/"+workspace+"/members", durableStartupToken, `{"email":"second@example.test","role":"operator"}`); code != http.StatusCreated {
					t.Fatalf("grant second in %s status=%d body=%s", workspace, code, body)
				}
			}
			second, err := st.ProvisionIdentityUser(t.Context(), "second@example.test", "Second")
			if err != nil {
				t.Fatal(err)
			}
			secondPAT, err := st.IssueOwnPersonalAccessToken(t.Context(), second.ID, "second")
			if err != nil {
				t.Fatal(err)
			}
			if code, body := daemon.post(t, "/v1/workspaces/startup/members", secondPAT.Value, `{"email":"`+owner.Email+`","role":"viewer"}`); code != http.StatusCreated {
				t.Fatalf("demote owner status=%d body=%s", code, body)
			}
			if code, body := daemon.do(t, http.MethodDelete, "/v1/workspaces/excluded/members/"+owner.ID, secondPAT.Value, ""); code != http.StatusNoContent {
				t.Fatalf("exclude owner status=%d body=%s", code, body)
			}
			daemon.stop(t)
			restarted := startDurableDaemon(t, raw, cfgPath, 90*time.Second)
			code, body := restarted.get(t, "/v1/workspaces", durableStartupToken)
			if code != http.StatusOK || !strings.Contains(body, `"id":"startup"`) || strings.Contains(body, `"id":"excluded"`) {
				t.Fatalf("owner workspaces after restart status=%d body=%s", code, body)
			}
			if code, body := restarted.get(t, "/v1/workspaces/excluded", durableStartupToken); code != http.StatusNotFound {
				t.Fatalf("excluded workspace after restart status=%d body=%s", code, body)
			}
			if code, body := restarted.get(t, "/v1/me?workspace_id=startup", durableStartupToken); code != http.StatusOK || !strings.Contains(body, `"role":"viewer"`) {
				t.Fatalf("owner role after restart status=%d body=%s", code, body)
			}
			restarted.stop(t)
			for workspace, want := range map[string]core.WorkspaceRole{"startup": core.WorkspaceRoleViewer, "excluded": ""} {
				caller, err := st.GetCallerIdentity(t.Context(), owner.ID, workspace)
				if want == "" {
					if !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("owner restored in %s: %+v err=%v", workspace, caller, err)
					}
					continue
				}
				if err != nil || caller.Role != want {
					t.Fatalf("owner in %s=%+v err=%v want %s", workspace, caller, err, want)
				}
			}
			if caller, err := st.GetCallerIdentity(t.Context(), second.ID, "startup"); err != nil || caller.Role != core.WorkspaceRoleOperator {
				t.Fatalf("second operator binding=%+v err=%v", caller, err)
			}
		})
	}
}

const durableStartupToken = "startup-fixture-token"

func durableStartupConfig(name, workspace string) string {
	head := ""
	if workspace != "" {
		head = "workspace: " + workspace + "\n"
	}
	body := fmt.Sprintf(`database: {backend: %s}
routing:
  stages:
    triage: {model: fixture, timeout: 20m, execution: in_process}
    spec: {model: fixture, timeout: 30m, execution: mcp}
    implement: {model: fixture, timeout: 4h, execution: mcp}
    review: {model: fixture, timeout: 1h, execution: mcp}
`, name)
	if workspace != "" {
		body += `repos:
  - {name: repo, url: https://example.test/repo, base: main}
`
	}
	return head + body
}

type durableDaemon struct {
	addr    string
	logPath string
	ctx     context.Context
	cancel  context.CancelFunc
	cmd     *exec.Cmd
	done    chan error
	client  http.Client
}

// startDurableDaemon runs the real daemon in a child process against raw and
// waits for /healthz. Readiness polling observes process health only; no
// membership ordering depends on it.
func startDurableDaemon(t *testing.T, raw, cfgPath string, timeout time.Duration) *durableDaemon {
	t.Helper()
	dir := filepath.Dir(cfgPath)
	envPath := filepath.Join(dir, "empty.env")
	requireStartupWrite(t, envPath, nil)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConveyordDurableStartupIntegration$")
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CONVEYOR_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "CONVEYOR_DURABLE_STARTUP_CONFIG="+cfgPath, "CONVEYOR_DURABLE_STARTUP_ADDR="+addr, "CONVEYOR_ENV_FILE="+envPath, "CONVEYOR_DATABASE_URL="+raw, "CONVEYOR_API_TOKEN="+durableStartupToken, "CONVEYOR_LLM_API_KEY=unused-fixture-key",
		// The deprecated alias alone supplies the App key encryption key and
		// warns once (DEC-59 clause 2; req-delivery-and-forge AC-1.11).
		config.DeprecatedGitHubAppKeyEncryptionKeyEnv+"="+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	logPath := filepath.Join(dir, fmt.Sprintf("daemon-%d.log", time.Now().UnixNano()))
	output, err := os.Create(logPath)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { output.Close() })
	cmd.Stdout = output
	cmd.Stderr = output
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	d := &durableDaemon{addr: addr, logPath: logPath, ctx: ctx, cancel: cancel, cmd: cmd, done: make(chan error, 1), client: http.Client{Timeout: time.Second}}
	go func() { d.done <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-d.done:
		case <-time.After(5 * time.Second):
		}
	})
	ready := false
	for !ready && ctx.Err() == nil {
		select {
		case err := <-d.done:
			b, _ := os.ReadFile(logPath)
			t.Fatalf("startup exited: %v\n%s", err, b)
		default:
		}
		response, err := d.client.Get("http://" + addr + "/healthz")
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
	d.client.Timeout = 10 * time.Second
	return d
}

func (d *durableDaemon) do(t *testing.T, method, path, token, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(d.ctx, method, "http://"+d.addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(payload)
}

func (d *durableDaemon) get(t *testing.T, path, token string) (int, string) {
	t.Helper()
	return d.do(t, http.MethodGet, path, token, "")
}

func (d *durableDaemon) post(t *testing.T, path, token, body string) (int, string) {
	t.Helper()
	return d.do(t, http.MethodPost, path, token, body)
}

// stop interrupts the daemon, requires an orderly exit, and returns its log.
func (d *durableDaemon) stop(t *testing.T) []byte {
	t.Helper()
	if err := d.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-d.done:
		if err != nil {
			t.Fatal(err)
		}
		d.done <- nil
	case <-d.ctx.Done():
		t.Fatal("shutdown timed out")
	}
	b, _ := os.ReadFile(d.logPath)
	return b
}

// durableScalar reads one value through the backend's native driver.
func durableScalar(t *testing.T, name, raw, query string) string {
	t.Helper()
	var value any
	if name == "postgres" {
		pool, err := pgxpool.New(t.Context(), raw)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		if err := pool.QueryRow(t.Context(), query).Scan(&value); err != nil {
			t.Fatal(err)
		}
	} else {
		cfg, err := mysql.ParseDSN(raw)
		if err != nil {
			t.Fatal(err)
		}
		connector, err := mysql.NewConnector(cfg)
		if err != nil {
			t.Fatal(err)
		}
		db := sql.OpenDB(connector)
		defer db.Close()
		if err := db.QueryRowContext(t.Context(), query).Scan(&value); err != nil {
			t.Fatal(err)
		}
	}
	if b, ok := value.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(value)
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
