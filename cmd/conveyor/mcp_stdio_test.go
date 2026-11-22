package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Sentinel credentials for the stdio bridge tests. No output stream may carry
// any of them (component-cli-onboarding invariant 45).
const (
	mcpStdioFileToken    = "cv_pat_pat_stdiofiletoken_first"
	mcpStdioRotatedToken = "cv_pat_pat_stdiofiletoken_rotated"
	mcpStdioAmbientToken = "cv_pat_pat_stdioambienttoken_env"
	mcpStdioDefaultToken = "cv_pat_pat_stdiodefaulttoken_file"
)

var mcpStdioSentinels = []string{mcpStdioFileToken, mcpStdioRotatedToken, mcpStdioAmbientToken, mcpStdioDefaultToken}

// mcpStdioTestTimeout bounds every wait in these tests so that a broken
// bridge fails the test instead of hanging it. No ordering depends on it.
const mcpStdioTestTimeout = 20 * time.Second

func writeMCPStdioCredentials(t *testing.T, path string, servers map[string]string) {
	t.Helper()
	config := localAuthConfig{Comment: localAuthConfigComment, Servers: map[string]localServerConfig{}}
	for server, token := range servers {
		config.Servers[server] = localServerConfig{Token: token}
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
}

// isolateMCPStdioTest isolates globals and puts an ambient token and a
// default credential file in place for the server, so a test proves that
// neither one reaches HTTP.
func isolateMCPStdioTest(t *testing.T, server string) string {
	t.Helper()
	isolateLocalAuthTest(t)
	t.Setenv("CONVEYOR_ADDR", server)
	t.Setenv("CONVEYOR_API_TOKEN", mcpStdioAmbientToken)
	if err := updateLocalServerConfig(server, func(entry *localServerConfig) { entry.Token = mcpStdioDefaultToken }); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(t.TempDir(), "named-credentials.json")
}

type mcpStdioSession struct {
	t      *testing.T
	stdin  *io.PipeWriter
	lines  chan string
	stderr *lockedBuffer
	stdout *lockedBuffer
	done   chan error
	eof    chan struct{}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// eofSignalReader closes eof once the bridge has read end of input.
type eofSignalReader struct {
	reader io.Reader
	once   sync.Once
	eof    chan struct{}
}

func (r *eofSignalReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if errors.Is(err, io.EOF) {
		r.once.Do(func() { close(r.eof) })
	}
	return n, err
}

// startMCPStdio runs the registered `conveyor mcp stdio` command through the
// root command tree with piped standard streams.
func startMCPStdio(t *testing.T, args ...string) *mcpStdioSession {
	t.Helper()
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	session := &mcpStdioSession{
		t:      t,
		stdin:  stdinWriter,
		lines:  make(chan string, 64),
		stderr: &lockedBuffer{},
		stdout: &lockedBuffer{},
		done:   make(chan error, 1),
		eof:    make(chan struct{}),
	}
	root := newRootCmd()
	root.SetArgs(append([]string{"mcp", "stdio"}, args...))
	root.SetIn(&eofSignalReader{reader: stdinReader, eof: session.eof})
	root.SetOut(stdoutWriter)
	root.SetErr(session.stderr)
	go func() {
		err := root.Execute()
		_ = stdoutWriter.Close()
		session.done <- err
	}()
	go func() {
		reader := bufio.NewReader(stdoutReader)
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				_, _ = session.stdout.Write([]byte(line))
				session.lines <- line
			}
			if err != nil {
				close(session.lines)
				return
			}
		}
	}()
	t.Cleanup(func() { _ = stdinWriter.Close() })
	return session
}

func (s *mcpStdioSession) send(line string) {
	s.t.Helper()
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(s.stdin, line+"\n")
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			s.t.Fatalf("write standard input: %v", err)
		}
	case <-time.After(mcpStdioTestTimeout):
		s.t.Fatal("bridge did not read standard input")
	}
}

func (s *mcpStdioSession) next() string {
	s.t.Helper()
	select {
	case line, ok := <-s.lines:
		if !ok {
			s.t.Fatal("standard output closed before the expected line")
		}
		if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
			s.t.Fatalf("output is not one line: %q", line)
		}
		return strings.TrimSuffix(line, "\n")
	case <-time.After(mcpStdioTestTimeout):
		s.t.Fatal("no output line from the bridge")
	}
	return ""
}

// finish closes standard input and returns the command's error and every
// remaining output line.
func (s *mcpStdioSession) finish() (error, []string) {
	s.t.Helper()
	_ = s.stdin.Close()
	var err error
	select {
	case err = <-s.done:
	case <-time.After(mcpStdioTestTimeout):
		s.t.Fatal("bridge did not exit after standard input closed")
	}
	var rest []string
	for line := range s.lines {
		rest = append(rest, strings.TrimSuffix(line, "\n"))
	}
	return err, rest
}

func (s *mcpStdioSession) assertNoSecrets() {
	s.t.Helper()
	for _, stream := range []struct{ name, text string }{{"stdout", s.stdout.String()}, {"stderr", s.stderr.String()}} {
		for _, secret := range mcpStdioSentinels {
			if strings.Contains(stream.text, secret) {
				s.t.Fatalf("%s carries a credential: %q", stream.name, stream.text)
			}
		}
	}
}

// assertMCPLines checks that every standard output line is a JSON-RPC message.
func assertMCPLines(t *testing.T, text string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &object); err != nil || string(object["jsonrpc"]) != `"2.0"` {
			t.Fatalf("standard output line is not a JSON-RPC message: %q", line)
		}
	}
}

type rpcLine struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeRPCLine(t *testing.T, line string) rpcLine {
	t.Helper()
	var decoded rpcLine
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return decoded
}

type recordedMCPRequest struct {
	rpcMethod     string
	method        string
	path          string
	authorization string
	contentType   string
	body          string
}

func TestMCPStdioForwardsRequestsUnchangedWithFileCredential(t *testing.T) {
	var mu sync.Mutex
	var recorded []recordedMCPRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &request)
		mu.Lock()
		recorded = append(recorded, recordedMCPRequest{request.Method, r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(body)})
		mu.Unlock()
		switch request.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			// Pretty-printed with a newline inside the body: the bridge
			// must write it as one line.
			fmt.Fprintf(w, "{\n  \"jsonrpc\": \"2.0\",\n  \"id\": %s,\n  \"result\": {\"protocolVersion\": \"2025-03-26\", \"note\": \"a\\nb\"}\n}\n", request.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"get_task"}]}}`, request.ID)
		case "tools/call":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"{\"id\":\"task-1\"}"}]}}`, request.ID)
		default:
			t.Errorf("unexpected method %q", request.Method)
		}
	}))
	defer server.Close()
	credentials := isolateMCPStdioTest(t, server.URL)
	writeMCPStdioCredentials(t, credentials, map[string]string{server.URL: mcpStdioFileToken})

	// The server flag names the endpoint form with a trailing slash; the
	// bridge uses the canonical base for both the URL and the credential key.
	session := startMCPStdio(t, "--server", server.URL+"/mcp/", "--credentials-file", credentials)
	initialize := `{"jsonrpc":"2.0", "id":1.50, "method":"initialize","params":{"protocolVersion":"2025-03-26"}}`
	initialized := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	list := `{"jsonrpc":"2.0","id":"list-1","method":"tools/list"}`
	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_task","arguments":{"task_id":"task-1"}}}`

	session.send(initialize)
	if got := session.next(); got != `{"jsonrpc":"2.0","id":1.50,"result":{"protocolVersion":"2025-03-26","note":"a\nb"}}` {
		t.Fatalf("initialize answer = %q", got)
	}
	session.send(initialized)
	session.send(list)
	if got := session.next(); got != `{"jsonrpc":"2.0","id":"list-1","result":{"tools":[{"name":"get_task"}]}}` {
		t.Fatalf("tools/list answer = %q", got)
	}
	session.send(call)
	if got := session.next(); got != `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"{\"id\":\"task-1\"}"}]}}` {
		t.Fatalf("tools/call answer = %q", got)
	}
	err, rest := session.finish()
	if err != nil {
		t.Fatalf("bridge exit: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("the 202 notification wrote output: %q", rest)
	}
	// The bridge forwards each message on its own goroutine, so the server
	// can receive them in any order. Match each request by its JSON-RPC
	// method, not by arrival order.
	mu.Lock()
	defer mu.Unlock()
	wantBodies := map[string]string{
		"initialize":                initialize,
		"notifications/initialized": initialized,
		"tools/list":                list,
		"tools/call":                call,
	}
	if len(recorded) != len(wantBodies) {
		t.Fatalf("requests = %#v", recorded)
	}
	seen := map[string]bool{}
	for _, request := range recorded {
		want, ok := wantBodies[request.rpcMethod]
		if !ok || seen[request.rpcMethod] {
			t.Fatalf("unexpected or repeated JSON-RPC method %q", request.rpcMethod)
		}
		seen[request.rpcMethod] = true
		if request.method != http.MethodPost || request.path != "/mcp" {
			t.Fatalf("%s request = %s %s", request.rpcMethod, request.method, request.path)
		}
		if request.authorization != "Bearer "+mcpStdioFileToken {
			t.Fatalf("%s request used another credential", request.rpcMethod)
		}
		if request.contentType != "application/json" {
			t.Fatalf("%s request content type = %q", request.rpcMethod, request.contentType)
		}
		if request.body != want {
			t.Fatalf("%s request body = %q, want %q", request.rpcMethod, request.body, want)
		}
	}
	assertMCPLines(t, session.stdout.String())
	session.assertNoSecrets()
}

func TestMCPStdioUsesRotatedTokenAndNeverAmbientCredential(t *testing.T) {
	var mu sync.Mutex
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, request.ID)
	}))
	defer server.Close()
	credentials := isolateMCPStdioTest(t, server.URL)
	writeMCPStdioCredentials(t, credentials, map[string]string{server.URL: mcpStdioFileToken, "https://other.example.test": mcpStdioDefaultToken})

	session := startMCPStdio(t, "--server", server.URL, "--credentials-file", credentials)
	session.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	session.next()
	writeMCPStdioCredentials(t, credentials, map[string]string{server.URL: mcpStdioRotatedToken})
	session.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	session.next()
	if err, _ := session.finish(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer " + mcpStdioFileToken, "Bearer " + mcpStdioRotatedToken}
	if len(authorizations) != 2 || authorizations[0] != want[0] || authorizations[1] != want[1] {
		t.Fatalf("authorizations did not follow the named file: %d requests, first ok=%v, second ok=%v",
			len(authorizations), len(authorizations) > 0 && authorizations[0] == want[0], len(authorizations) > 1 && authorizations[1] == want[1])
	}
	session.assertNoSecrets()
}

func TestMCPStdioAnswersSecondRequestWhileFirstBlocksAndDrainsAtEOF(t *testing.T) {
	release := make(chan struct{})
	firstArrived := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if string(request.ID) == `"slow"` {
			close(firstArrived)
			<-release
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"method":%q}}`, request.ID, request.Method)
	}))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	credentials := isolateMCPStdioTest(t, server.URL)
	writeMCPStdioCredentials(t, credentials, map[string]string{server.URL: mcpStdioFileToken})

	session := startMCPStdio(t, "--server", server.URL, "--credentials-file", credentials)
	session.send(`{"jsonrpc":"2.0","id":"slow","method":"tools/call","params":{"name":"await_review","arguments":{"timeout_seconds":600}}}`)
	select {
	case <-firstArrived:
	case <-time.After(mcpStdioTestTimeout):
		t.Fatal("first request did not reach the server")
	}
	session.send(`{"jsonrpc":"2.0","id":"fast","method":"tools/list"}`)
	if got := decodeRPCLine(t, session.next()); string(got.ID) != `"fast"` {
		t.Fatalf("first answer while the slow request blocks has id %s", got.ID)
	}

	// Close standard input while the slow request is still in flight. The
	// bridge must not exit before it answers that request.
	_ = session.stdin.Close()
	select {
	case <-session.eof:
	case <-time.After(mcpStdioTestTimeout):
		t.Fatal("bridge did not read end of input")
	}
	select {
	case err := <-session.done:
		t.Fatalf("bridge exited with a request in flight: %v", err)
	default:
	}
	close(release)
	err, rest := session.finish()
	if err != nil {
		t.Fatalf("bridge exit: %v", err)
	}
	if len(rest) != 1 || string(decodeRPCLine(t, rest[0]).ID) != `"slow"` {
		t.Fatalf("drained output = %q", rest)
	}
	assertMCPLines(t, session.stdout.String())
	session.assertNoSecrets()
}

func TestMCPStdioSerializesConcurrentAnswers(t *testing.T) {
	const count = 40
	padding := strings.Repeat("x", 128<<10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"padding":%q}}`, request.ID, padding)
	}))
	defer server.Close()
	credentials := isolateMCPStdioTest(t, server.URL)
	writeMCPStdioCredentials(t, credentials, map[string]string{server.URL: mcpStdioFileToken})

	session := startMCPStdio(t, "--server", server.URL, "--credentials-file", credentials)
	// One request line larger than bufio.Scanner's 64 KiB default.
	session.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":0,"method":"tools/call","params":{"name":"get_task","arguments":{"note":%q}}}`, padding))
	for i := 1; i < count; i++ {
		session.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`, i))
	}
	seen := map[string]bool{}
	for i := 0; i < count; i++ {
		got := decodeRPCLine(t, session.next())
		seen[string(got.ID)] = true
	}
	if err, rest := session.finish(); err != nil || len(rest) != 0 {
		t.Fatalf("exit err=%v rest=%d", err, len(rest))
	}
	if len(seen) != count {
		t.Fatalf("distinct answers = %d", len(seen))
	}
	assertMCPLines(t, session.stdout.String())
}

// hijackServer answers each request by running fn on the raw connection.
func hijackServer(t *testing.T, fn func(net.Conn, *bufio.ReadWriter)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fn(conn, buffer)
	}))
}

func TestMCPStdioFailuresAnswerTheRequest(t *testing.T) {
	login := func(base string) string { return "conveyor auth login --server " + base }
	type failureCase struct {
		name string
		// server returns the test server, or nil for an unreachable server.
		server      func(t *testing.T) *httptest.Server
		credentials func(t *testing.T, path, base string)
		code        int
		contains    func(base string) []string
	}
	fileToken := func(t *testing.T, path, base string) {
		writeMCPStdioCredentials(t, path, map[string]string{base: mcpStdioFileToken})
	}
	echoHandler := func(status int, body string) func(t *testing.T) *httptest.Server {
		return func(t *testing.T) *httptest.Server {
			return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, strings.ReplaceAll(body, "TOKEN", r.Header.Get("Authorization")))
			}))
		}
	}
	cases := []failureCase{
		{
			name:        "missing credential file",
			server:      echoHandler(http.StatusOK, `{"jsonrpc":"2.0","id":7,"result":{}}`),
			credentials: func(*testing.T, string, string) {},
			code:        mcpStdioMissingCredential,
			contains:    func(base string) []string { return []string{"No stored credential for " + base, login(base)} },
		},
		{
			name:   "missing server entry",
			server: echoHandler(http.StatusOK, `{"jsonrpc":"2.0","id":7,"result":{}}`),
			credentials: func(t *testing.T, path, _ string) {
				writeMCPStdioCredentials(t, path, map[string]string{"https://other.example.test": mcpStdioFileToken})
			},
			code:     mcpStdioMissingCredential,
			contains: func(base string) []string { return []string{"No stored credential for " + base, login(base)} },
		},
		{
			name:   "empty token",
			server: echoHandler(http.StatusOK, `{"jsonrpc":"2.0","id":7,"result":{}}`),
			credentials: func(t *testing.T, path, base string) {
				writeMCPStdioCredentials(t, path, map[string]string{base: ""})
			},
			code:     mcpStdioMissingCredential,
			contains: func(base string) []string { return []string{"No stored credential for " + base, login(base)} },
		},
		{
			name:   "malformed credential file",
			server: echoHandler(http.StatusOK, `{"jsonrpc":"2.0","id":7,"result":{}}`),
			credentials: func(t *testing.T, path, _ string) {
				if err := os.WriteFile(path, []byte(`{"servers": {"`+mcpStdioFileToken), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			code:     mcpStdioUnreadableCredentials,
			contains: func(string) []string { return []string{"cannot read or parse the credentials file"} },
		},
		{
			name:        "401 with the token in the body",
			server:      echoHandler(http.StatusUnauthorized, `{"error":"bad TOKEN"}`),
			credentials: fileToken,
			code:        mcpStdioUnauthorized,
			contains:    func(base string) []string { return []string{"HTTP 401", login(base)} },
		},
		{
			name:        "403 with the token in the body",
			server:      echoHandler(http.StatusForbidden, `forbidden for TOKEN`),
			credentials: fileToken,
			code:        mcpStdioForbidden,
			contains:    func(string) []string { return []string{"HTTP 403"} },
		},
		{
			name:        "non-JSON response",
			server:      echoHandler(http.StatusOK, `<html>TOKEN</html>`),
			credentials: fileToken,
			code:        mcpStdioInvalidResponse,
			contains:    func(string) []string { return []string{"not JSON"} },
		},
		{
			name:        "malformed JSON-RPC response",
			server:      echoHandler(http.StatusOK, `{"id":7,"value":"TOKEN"}`),
			credentials: fileToken,
			code:        mcpStdioInvalidResponse,
			contains:    func(string) []string { return []string{"not a JSON-RPC message"} },
		},
		{
			name:        "unexpected status with the token in the body",
			server:      echoHandler(http.StatusBadGateway, `{"jsonrpc":"2.0","id":7,"result":{"echo":"TOKEN"}}`),
			credentials: fileToken,
			code:        mcpStdioInvalidResponse,
			contains:    func(string) []string { return []string{"HTTP 502"} },
		},
		{
			name:        "valid response that echoes the token",
			server:      echoHandler(http.StatusOK, `{"jsonrpc":"2.0","id":7,"result":{"echo":"TOKEN"}}`),
			credentials: fileToken,
			code:        mcpStdioInvalidResponse,
			contains:    func(string) []string { return []string{"withheld"} },
		},
		{
			name: "valid response that echoes the token with JSON escapes",
			server: func(t *testing.T) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.ReadAll(r.Body)
					var escaped strings.Builder
					for _, char := range strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
						fmt.Fprintf(&escaped, `\u%04x`, char)
					}
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":7,"result":{"echo":"%s"}}`, escaped.String())
				}))
			},
			credentials: fileToken,
			code:        mcpStdioInvalidResponse,
			contains:    func(string) []string { return []string{"withheld"} },
		},
		{
			name: "tool text result that holds the token in nested JSON",
			server: func(t *testing.T) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.ReadAll(r.Body)
					var escaped strings.Builder
					for _, char := range strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
						fmt.Fprintf(&escaped, `\\u%04x`, char)
					}
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"{\"token\":\"%s\"}"}]}}`, escaped.String())
				}))
			},
			credentials: fileToken,
			code:        mcpStdioInvalidResponse,
			contains:    func(string) []string { return []string{"withheld"} },
		},
		{
			name: "redirect is not followed",
			server: func(t *testing.T) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/mcp" {
						t.Errorf("bridge followed a redirect to %s", r.URL.Path)
					}
					http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
				}))
			},
			credentials: fileToken,
			code:        mcpStdioInvalidResponse,
			contains:    func(string) []string { return []string{"HTTP 307"} },
		},
		{
			name: "unreachable server",
			server: func(t *testing.T) *httptest.Server {
				return nil
			},
			credentials: fileToken,
			code:        mcpStdioUnreachable,
			contains:    func(base string) []string { return []string{"cannot connect to the server at " + base} },
		},
		{
			name: "connection closed before the response",
			server: func(t *testing.T) *httptest.Server {
				return hijackServer(t, func(net.Conn, *bufio.ReadWriter) {})
			},
			credentials: fileToken,
			code:        mcpStdioBrokenConnection,
			contains:    func(string) []string { return []string{"connection to the server broke"} },
		},
		{
			name: "connection closed inside the response body",
			server: func(t *testing.T) *httptest.Server {
				return hijackServer(t, func(_ net.Conn, buffer *bufio.ReadWriter) {
					_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n{\"jsonrpc\":\"2.0\",\"id\":7,")
					_ = buffer.Flush()
				})
			},
			credentials: fileToken,
			code:        mcpStdioBrokenConnection,
			contains:    func(string) []string { return []string{"connection to the server broke"} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := tc.server(t)
			var base string
			if server == nil {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				base = "http://" + listener.Addr().String()
				_ = listener.Close()
			} else {
				defer server.Close()
				base = server.URL
			}
			credentials := isolateMCPStdioTest(t, base)
			tc.credentials(t, credentials, base)
			session := startMCPStdio(t, "--server", base, "--credentials-file", credentials)

			// A failed notification writes nothing, then the request gets
			// its error, and a non-JSON line gets a parse error: the bridge
			// keeps serving after each failure.
			session.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			session.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"get_task"}}`)
			got := decodeRPCLine(t, session.next())
			if string(got.ID) != "7" || got.Error == nil || got.Error.Code != tc.code || got.Result != nil {
				t.Fatalf("answer = %+v", got)
			}
			for _, want := range tc.contains(base) {
				if !strings.Contains(got.Error.Message, want) {
					t.Fatalf("error %q does not contain %q", got.Error.Message, want)
				}
			}
			session.send(`not json`)
			parse := decodeRPCLine(t, session.next())
			if string(parse.ID) != "null" || parse.Error == nil || parse.Error.Code != mcpStdioParseError {
				t.Fatalf("parse answer = %+v", parse)
			}
			err, rest := session.finish()
			if err != nil {
				t.Fatalf("bridge exit: %v", err)
			}
			if len(rest) != 0 {
				t.Fatalf("the failed notification wrote output: %q", rest)
			}
			assertMCPLines(t, session.stdout.String())
			session.assertNoSecrets()
			if !strings.Contains(session.stderr.String(), got.Error.Message) {
				t.Fatalf("stderr = %q", session.stderr.String())
			}
		})
	}
}

func TestMCPStdioKeepsServingAfterAFailedRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if string(request.ID) == "1" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`, request.ID)
	}))
	defer server.Close()
	credentials := isolateMCPStdioTest(t, server.URL)
	writeMCPStdioCredentials(t, credentials, map[string]string{server.URL: mcpStdioFileToken})
	session := startMCPStdio(t, "--server", server.URL, "--credentials-file", credentials)
	session.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if got := decodeRPCLine(t, session.next()); got.Error == nil || got.Error.Code != mcpStdioUnauthorized {
		t.Fatalf("first answer = %+v", got)
	}
	session.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if got := session.next(); got != `{"jsonrpc":"2.0","id":2,"result":{"ok":true}}` {
		t.Fatalf("second answer = %q", got)
	}
	if err, rest := session.finish(); err != nil || len(rest) != 0 {
		t.Fatalf("exit err=%v rest=%q", err, rest)
	}
	session.assertNoSecrets()
}

func TestMCPStdioServerErrorForNotificationWritesNothing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		// The /mcp route answers an unknown method with an error that has
		// no id, also for a notification.
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","error":{"code":-32601,"message":"method not found"}}`)
	}))
	defer server.Close()
	credentials := isolateMCPStdioTest(t, server.URL)
	writeMCPStdioCredentials(t, credentials, map[string]string{server.URL: mcpStdioFileToken})
	session := startMCPStdio(t, "--server", server.URL, "--credentials-file", credentials)
	session.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	if err, rest := session.finish(); err != nil || len(rest) != 0 {
		t.Fatalf("exit err=%v rest=%q", err, rest)
	}
	session.assertNoSecrets()
}

func TestMCPStdioRequiresExplicitServerAndAbsoluteCredentialsFile(t *testing.T) {
	isolateMCPStdioTest(t, "http://127.0.0.1:1")
	absolute := filepath.Join(t.TempDir(), "credentials.json")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing server", []string{"--credentials-file", absolute}, "requires --server"},
		{"empty server", []string{"--server", "", "--credentials-file", absolute}, "server URL is required"},
		{"malformed server", []string{"--server", "ftp://example.test", "--credentials-file", absolute}, "http or https"},
		{"server with credentials", []string{"--server", "https://user:" + mcpStdioFileToken + "@example.test", "--credentials-file", absolute}, "must not contain credentials"},
		{"missing credentials file", []string{"--server", "https://example.test"}, "requires --credentials-file"},
		{"empty credentials file", []string{"--server", "https://example.test", "--credentials-file", ""}, "requires --credentials-file"},
		{"relative credentials file", []string{"--server", "https://example.test", "--credentials-file", "credentials.json"}, "credentials-file must be absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read := false
			var stdout, stderr bytes.Buffer
			root := newRootCmd()
			root.SetArgs(append([]string{"mcp", "stdio"}, tc.args...))
			root.SetIn(readerFunc(func([]byte) (int, error) { read = true; return 0, io.EOF }))
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if read {
				t.Fatal("bridge read standard input before it checked its flags")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q", stdout.String())
			}
			if strings.Contains(err.Error(), mcpStdioFileToken) || strings.Contains(stderr.String(), mcpStdioFileToken) {
				t.Fatal("startup error carries a credential")
			}
		})
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestMCPStdioIsRegisteredBesideInstall(t *testing.T) {
	command, _, err := newRootCmd().Find([]string{"mcp", "stdio"})
	if err != nil || command.Name() != "stdio" || command.Parent().Name() != "mcp" {
		t.Fatalf("command = %v err = %v", command, err)
	}
	if command.Flags().Lookup("credentials-file") == nil {
		t.Fatal("stdio has no --credentials-file flag")
	}
	install, _, err := newRootCmd().Find([]string{"mcp", "install"})
	if err != nil || install.Name() != "install" {
		t.Fatalf("install = %v err = %v", install, err)
	}
}
