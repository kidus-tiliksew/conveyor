package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	"github.com/kidus-tiliksew/conveyor/internal/store/singlestore"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

type memoMeasurementStore interface {
	store.Store
	BootstrapWorkspaceConfig(context.Context, *config.Config) (bool, error)
	ReadStats(int) dbmetrics.ReadInterval
	Close()
}

func prepareMemoSingleStoreEvents(ctx context.Context, raw string) error {
	if os.Getenv("CONVEYOR_FIXTURE_PREPARED") != "1" {
		return fmt.Errorf("SingleStore event preparation requires an owned prepared fixture")
	}
	var cfg *mysql.Config
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "singlestore" && u.Scheme != "mysql") || u.Hostname() == "" {
			return fmt.Errorf("invalid SingleStore fixture URL")
		}
		cfg = mysql.NewConfig()
		cfg.Net = "tcp"
		port := u.Port()
		if port == "" {
			port = "3306"
		}
		cfg.Addr = net.JoinHostPort(u.Hostname(), port)
		cfg.DBName = strings.TrimPrefix(u.Path, "/")
		if u.User != nil {
			cfg.User = u.User.Username()
			cfg.Passwd, _ = u.User.Password()
		}
		cfg.TLSConfig = u.Query().Get("tls")
		if values, ok := u.Query()["compress"]; ok {
			if len(values) != 1 {
				return fmt.Errorf("invalid SingleStore compression option")
			}
			enabled, err := strconv.ParseBool(values[0])
			if err != nil || cfg.Apply(mysql.EnableCompression(enabled)) != nil {
				return fmt.Errorf("invalid SingleStore compression option")
			}
		}
	} else {
		var err error
		cfg, err = mysql.ParseDSN(raw)
		if err != nil {
			return fmt.Errorf("invalid SingleStore fixture DSN")
		}
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{2,62}_test$`).MatchString(cfg.DBName) {
		return fmt.Errorf("SingleStore fixture database name is not an owned test name")
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return fmt.Errorf("cannot configure SingleStore fixture connector")
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	safeError := func(stage string, err error) error {
		var driverErr *mysql.MySQLError
		if errors.As(err, &driverErr) {
			return fmt.Errorf("%s: MySQL error %d SQLSTATE %s", stage, driverErr.Number, string(driverErr.SQLState[:]))
		}
		return fmt.Errorf("%s failed", stage)
	}
	var current string
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current); err != nil {
		return safeError("select fixture database", err)
	}
	if current != cfg.DBName {
		return fmt.Errorf("SingleStore current database does not match the owned fixture")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "sql", "events-columnstore", "01-create.sql"))
	if err != nil {
		return fmt.Errorf("read event preparation script failed")
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		statement = strings.ReplaceAll(statement, "`your_database`", "`"+current+"`")
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return safeError("prepare events operator script", err)
		}
	}
	return nil
}

func TestHotReadEgressMeasurementIntegration(t *testing.T) {
	if os.Getenv("CONVEYOR_READ_EGRESS_MEASUREMENT") != "1" {
		t.Skip("set CONVEYOR_READ_EGRESS_MEASUREMENT=1 to measure real drivers")
	}
	if strings.TrimSpace(os.Getenv("CONVEYOR_READ_EGRESS_CANDIDATE_REVISION")) == "" {
		t.Fatal("CONVEYOR_READ_EGRESS_CANDIDATE_REVISION must identify the actual candidate patch")
	}
	selected := strings.TrimSpace(os.Getenv("CONVEYOR_READ_EGRESS_MEMO_BACKEND"))
	if selected != "postgres" && selected != "singlestore" {
		t.Fatal("CONVEYOR_READ_EGRESS_MEMO_BACKEND must be postgres or singlestore")
	}
	backends := []struct{ name, variable string }{
		{"postgres", "CONVEYOR_TEST_DATABASE_URL"},
		{"singlestore", "CONVEYOR_TEST_SINGLESTORE_URL"},
	}
	for _, backend := range backends {
		if backend.name != selected {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			url := strings.TrimSpace(os.Getenv(backend.variable))
			if url == "" {
				t.Fatalf("%s is required for the selected backend", backend.variable)
			}
			ctx := store.WithActor(t.Context(), store.SystemActor())
			var st memoMeasurementStore
			var err error
			if backend.name == "postgres" {
				st, err = postgres.Open(ctx, url)
			} else {
				if err := prepareMemoSingleStoreEvents(ctx, url); err != nil {
					t.Fatal(err)
				}
				st, err = singlestore.Open(ctx, url)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ws := "memo-egress-" + core.NewTaskID()
			ctx = store.WithWorkspace(ctx, ws)
			cfg := &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
			if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			taskID := "memo-" + core.NewTaskID()
			now := time.Now().UTC().Truncate(time.Minute).Add(time.Minute + 5*time.Second)
			task := core.Task{ID: taskID, Workspace: ws, Title: "Memo measurement task", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/" + taskID, State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, NextStage: core.StageImplement, CreatedAt: now.Add(-time.Hour)}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "task.created", At: task.CreatedAt}); err != nil {
				t.Fatal(err)
			}
			reqID := "memo-req-" + core.NewTaskID()
			if _, _, err := st.CreateRequirement(ctx, core.Requirement{ID: reqID, Title: "Memo measurement requirement"}, core.RequirementVersion{Content: "# Memo measurement requirement", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Measure the read."}}}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.ConfirmRequirementVersion(ctx, reqID, 1); err != nil {
				t.Fatal(err)
			}
			env, err := storetest.ReadEgressEnvFromProcess(backend.name, st.ReadStats)
			if err != nil {
				t.Fatal(err)
			}
			env.CacheMode = "warm-response-memo"
			previousClock := activityClock
			activityClock = func() time.Time { return now }
			defer func() { activityClock = previousClock }()
			newServer := func() *Server {
				s := NewServer(st)
				s.Workspace = ws
				membership := &membershipFixture{workspaces: []core.Workspace{{ID: ws, Name: ws}}, roles: map[string]map[string]core.WorkspaceRole{"reader": {ws: core.WorkspaceRoleViewer}}}
				s.Workspaces, s.Memberships = membership, membership
				s.Credentials = staticCredentialVerifier{"reader": {ID: "reader-pat", OwnerUserID: "reader", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}}
				s.ConfigProvider = func(context.Context) (*config.Config, error) { return cfg, nil }
				s.readMemo = &readMemo{clock: func() time.Time { return now }}
				s.nextActionClock = func() time.Time { return now }
				return s
			}
			for _, tc := range []struct {
				name, path, expected string
				mcp, memo            bool
			}{
				{"rest-tasks", "/v1/tasks?workspace_id=" + ws, taskID, false, true},
				{"rest-next-actions", "/v1/next-actions?workspace_id=" + ws, taskID, false, true},
				{"mcp-list-operator-inbox", "/mcp", taskID, true, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					baseline, candidate := newServer(), newServer()
					request := func(ctx context.Context, s *Server) (storetest.ReadEgressOutcome, error) {
						method, body := http.MethodGet, ""
						if tc.mcp {
							method = http.MethodPost
							wire, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "list_operator_inbox", "arguments": map[string]any{"workspace_id": ws}}})
							if err != nil {
								return storetest.ReadEgressOutcome{}, err
							}
							body = string(wire)
						}
						do := func(tag string) *httptest.ResponseRecorder {
							r := httptest.NewRequest(method, tc.path, strings.NewReader(body)).WithContext(ctx)
							r.Header.Set("Authorization", "Bearer reader")
							if tag != "" {
								r.Header.Set("If-None-Match", tag)
							}
							w := httptest.NewRecorder()
							s.Handler().ServeHTTP(w, r)
							return w
						}
						response := do("")
						if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), tc.expected) {
							return storetest.ReadEgressOutcome{}, fmt.Errorf("unexpected response: %d %s", response.Code, response.Body.String())
						}
						served := response.Body.String()
						if tc.mcp {
							var envelope struct {
								Error  json.RawMessage `json:"error"`
								Result struct {
									IsError bool `json:"isError"`
									Content []struct {
										Text string `json:"text"`
									} `json:"content"`
								} `json:"result"`
							}
							if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || len(envelope.Error) != 0 || envelope.Result.IsError || len(envelope.Result.Content) != 1 {
								return storetest.ReadEgressOutcome{}, fmt.Errorf("invalid MCP response: %s", response.Body.String())
							}
							normalized, err := normalizeMCPInboxText(envelope.Result.Content[0].Text)
							if err != nil {
								return storetest.ReadEgressOutcome{}, err
							}
							served = normalized
						}
						semanticHeaders := func(h http.Header) map[string]string {
							out := make(map[string]string)
							for _, name := range []string{"Content-Type", "ETag", "Cache-Control", "Vary", "X-Conveyor-Total", "X-Conveyor-Limit", "X-Conveyor-Offset", "X-Conveyor-Cursor"} {
								out[name] = h.Get(name)
							}
							return out
						}
						result := []any{response.Code, semanticHeaders(response.Header()), served}
						if tc.memo && !tc.mcp {
							tag := response.Header().Get("ETag")
							if tag == "" {
								return storetest.ReadEgressOutcome{}, fmt.Errorf("missing ETag")
							}
							conditional := do(tag)
							if conditional.Code != http.StatusNotModified || conditional.Body.Len() != 0 || conditional.Header().Get("ETag") != tag {
								return storetest.ReadEgressOutcome{}, fmt.Errorf("invalid conditional response: %d %v %s", conditional.Code, conditional.Header(), conditional.Body.String())
							}
							result = append(result, conditional.Code, semanticHeaders(conditional.Header()), conditional.Body.String())
						}
						digest, err := storetest.ReadEgressDigest(result)
						return storetest.ReadEgressOutcome{Digest: digest, Successes: 1}, err
					}
					if _, err := request(ctx, candidate); err != nil {
						t.Fatal(err)
					}
					c := storetest.ReadEgressCase{Name: tc.name, BaselineRevision: "current-handler-cold-memo", Fixture: map[string]int{"tasks": 1, "requirements": 1}, RequireZeroCandidateReads: tc.memo,
						Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
							baseline.readMemo = &readMemo{clock: func() time.Time { return now }}
							return request(ctx, baseline)
						},
						Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) { return request(ctx, candidate) },
					}
					report, err := storetest.MeasureReadEgress(ctx, env, c)
					if err != nil {
						t.Fatal(err)
					}
					line, err := json.Marshal(report)
					if err != nil {
						t.Fatal(err)
					}
					fmt.Printf("%s%s\n", storetest.ReadEgressMarker, line)
				})
			}
		})
	}
}

// normalizeMCPInboxText validates the list_operator_inbox mcpReadPage envelope
// and returns it with the per-read snapshot token and expiry replaced by
// placeholders. Those two are fresh per read; every other field is digested.
// The validation requires a non-empty snapshot, a non-zero expires_at, the
// mcpReadEvidence text, and an items array.
func normalizeMCPInboxText(text string) (string, error) {
	var page map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &page); err != nil {
		return "", fmt.Errorf("MCP inbox text is not an envelope object: %w", err)
	}
	var snapshot string
	if err := json.Unmarshal(page["snapshot"], &snapshot); err != nil || snapshot == "" {
		return "", fmt.Errorf("MCP inbox envelope has no snapshot: %s", text)
	}
	var expires time.Time
	if err := json.Unmarshal(page["expires_at"], &expires); err != nil || expires.IsZero() {
		return "", fmt.Errorf("MCP inbox envelope has no valid expires_at: %s", text)
	}
	var evidence string
	if err := json.Unmarshal(page["evidence"], &evidence); err != nil || evidence != mcpReadEvidence {
		return "", fmt.Errorf("MCP inbox envelope evidence differs: %s", text)
	}
	if _, ok := page["items"]; !ok {
		return "", fmt.Errorf("MCP inbox envelope has no items: %s", text)
	}
	page["snapshot"] = json.RawMessage(`"<snapshot>"`)
	page["expires_at"] = json.RawMessage(`"<expires_at>"`)
	normalized, err := json.Marshal(page)
	if err != nil {
		return "", err
	}
	return string(normalized), nil
}
