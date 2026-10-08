package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestInstalledClaudeCodeJSONMCPCompatibility runs the installed Claude Code
// executable with the built-in template argv, the generated credential-free
// registration, and a loopback synthetic MCP server. The synthetic token
// exists only in this process's memory and the child's environment; the
// child's HOME is an empty temporary directory, so no user configuration or
// account credential is read and no model turn is paid for: the child is
// stopped as soon as the receipt is decided. It proves that Claude expands
// ${CONVEYOR_API_TOKEN} from the child environment, that its stream-json
// initialization event satisfies the receipt, and that a rejected credential
// fails the receipt (component-harness-execution; req-security-boundaries
// AC-2.6, AC-2.7). It runs only when CONVEYOR_TEST_CLAUDE_COMPAT=1, and then a
// missing executable fails rather than skips.
func TestInstalledClaudeCodeJSONMCPCompatibility(t *testing.T) {
	if os.Getenv("CONVEYOR_TEST_CLAUDE_COMPAT") != "1" {
		t.Skip("set CONVEYOR_TEST_CLAUDE_COMPAT=1 to check the installed Claude Code executable")
	}
	executable, err := exec.LookPath("claude")
	if err != nil {
		t.Fatalf("missing evidence: claude is not installed: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(binary)
	version, err := exec.Command(executable, "--version").Output()
	if err != nil {
		t.Fatalf("claude --version: %v", err)
	}
	t.Logf("claude executable=%s resolved=%s sha256=%s version=%s", executable, resolved, hex.EncodeToString(sum[:]), strings.TrimSpace(string(version)))

	secret := make([]byte, 24)
	if _, err = rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	synthetic := "synthetic-" + hex.EncodeToString(secret)

	for _, test := range []struct {
		name       string
		accept     bool
		wantStatus string
	}{
		{name: "expanded reference connects", accept: true},
		{name: "rejected credential fails the receipt", accept: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			matched, mismatched := 0, 0
			methods := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				ok := r.Header.Get("Authorization") == "Bearer "+synthetic
				mu.Lock()
				if ok {
					matched++
				} else {
					mismatched++
				}
				mu.Unlock()
				if !ok || !test.accept {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
				var request struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params struct {
						ProtocolVersion string `json:"protocolVersion"`
					} `json:"params"`
				}
				if json.Unmarshal(body, &request) != nil {
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				mu.Lock()
				methods = append(methods, request.Method)
				mu.Unlock()
				if len(request.ID) == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				var result any = map[string]any{}
				switch request.Method {
				case "initialize":
					result = map[string]any{"protocolVersion": request.Params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "synthetic-conveyor", "version": "0"}}
				case "tools/list":
					tools := []map[string]any{}
					for _, stage := range []core.Stage{core.StageSpec, core.StageImplement, core.StageReview, core.StageVerify} {
						for _, name := range claudeStageTools(stage) {
							tools = append(tools, map[string]any{"name": name, "description": name, "inputSchema": map[string]any{"type": "object"}})
						}
					}
					result = map[string]any{"tools": tools}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
			}))
			defer server.Close()

			directory := t.TempDir()
			home := filepath.Join(directory, "home")
			work := filepath.Join(directory, "work")
			for _, path := range []string{home, work} {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			configPath, err := prepareMCPConfig(directory, server.URL, config.MCPTransportJSONFile)
			if err != nil {
				t.Fatal(err)
			}
			template := config.HarnessTemplates()[1].Harness
			if err = admitJSONMCPHarness(template); err != nil {
				t.Fatal(err)
			}
			argv, err := claudeJSONMCPLaunchArgv(template, expandHarness(template, "", "", "Reply with OK.", configPath), configPath)
			if err != nil {
				t.Fatal(err)
			}
			argv[0] = executable
			command := exec.Command(argv[0], argv[1:]...)
			command.Dir = work
			command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CONVEYOR_API_TOKEN=" + synthetic}
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			receipt := newClaudeMCPReceipt(core.StageImplement)
			var output strings.Builder
			var outputMu sync.Mutex
			// The real child's stdout passes the same ingress guard the
			// launcher installs ahead of its redactor.
			command.Stdout = newClaudeReceiptIngressGuard(io.MultiWriter(receipt, writerFunc(func(p []byte) (int, error) {
				outputMu.Lock()
				defer outputMu.Unlock()
				if output.Len() < 1<<20 {
					output.Write(p)
				}
				return len(p), nil
			})), receipt)
			command.Stderr = io.Discard
			if err = command.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- command.Wait() }()
			stop := func() {
				_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
				<-exited
			}
			var receiptErr error
			decided := false
			select {
			case receiptErr = <-receipt.Result():
				decided = true
				stop()
			case <-exited:
				select {
				case receiptErr = <-receipt.Result():
					decided = true
				default:
				}
			case <-time.After(jsonMCPReceiptTimeout(template)):
				stop()
			}
			outputMu.Lock()
			printed := output.String()
			outputMu.Unlock()
			configData, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			for name, text := range map[string]string{"registration": string(configData), "argv": strings.Join(argv, "\x00"), "stdout": printed} {
				if strings.Contains(text, synthetic) {
					t.Fatalf("%s carries the synthetic credential", name)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			t.Logf("receipt decided=%v err=%v matched_requests=%d mismatched_requests=%d methods=%v", decided, receiptErr, matched, mismatched, methods)
			if !decided {
				t.Fatal("missing evidence: Claude Code produced no initialization receipt within the probe timeout")
			}
			if test.accept {
				if receiptErr != nil || matched == 0 || mismatched != 0 {
					t.Fatalf("receipt err=%v matched=%d mismatched=%d", receiptErr, matched, mismatched)
				}
				return
			}
			if receiptErr == nil || !strings.Contains(receiptErr.Error(), "not connected") {
				t.Fatalf("rejected credential produced receipt err=%v", receiptErr)
			}
			if matched == 0 {
				t.Fatal("the rejected attempt never presented the expanded synthetic credential")
			}
		})
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
