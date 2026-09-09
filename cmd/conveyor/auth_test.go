package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/spf13/cobra"
)

func isolateLocalAuthTest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	previousConfigDir := userConfigDir
	previousReader := readLoginToken
	previousServer, previousWorkspace := serverFlag, workspaceFlag
	previousServerExplicit, previousWorkspaceExplicit := serverFlagExplicit, workspaceFlagExplicit
	userConfigDir = func() (string, error) { return root, nil }
	serverFlag, workspaceFlag = "", ""
	serverFlagExplicit, workspaceFlagExplicit = false, false
	t.Setenv("CONVEYOR_ADDR", "")
	t.Setenv("CONVEYOR_API_TOKEN", "")
	t.Setenv("CONVEYOR_WORKSPACE", "")
	t.Cleanup(func() {
		userConfigDir = previousConfigDir
		readLoginToken = previousReader
		serverFlag, workspaceFlag = previousServer, previousWorkspace
		serverFlagExplicit, workspaceFlagExplicit = previousServerExplicit, previousWorkspaceExplicit
	})
	return root
}

func TestAuthLoginVerifiesStoresSecurelyAndSupportsMultipleServers(t *testing.T) {
	root := isolateLocalAuthTest(t)
	newIdentityServer := func(token, email string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/me" || r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(core.CallerIdentity{ID: "usr-1", Email: email, DisplayName: "Owner"})
		}))
	}
	first := newIdentityServer("cv_pat_pat_abcdefghijklmnop_first", "one@example.test")
	defer first.Close()
	second := newIdentityServer("cv_pat_pat_qrstuvwxyzABCDEF_second", "two@example.test")
	defer second.Close()

	login := func(server, token string) string {
		t.Helper()
		t.Setenv("CONVEYOR_ADDR", server+"/")
		readLoginToken = func(*cobra.Command) (string, error) { return token, nil }
		var output bytes.Buffer
		command := authCmd()
		command.SetArgs([]string{"login"})
		command.SetOut(&output)
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), token) {
			t.Fatal("login output disclosed the credential")
		}
		return output.String()
	}
	if output := login(first.URL, "cv_pat_pat_abcdefghijklmnop_first"); !strings.Contains(output, "one@example.test") {
		t.Fatalf("login output = %q", output)
	}
	login(second.URL, "cv_pat_pat_qrstuvwxyzABCDEF_second")

	config, err := loadLocalAuthConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Servers) != 2 || config.Servers[first.URL].Token == "" || config.Servers[second.URL].Token == "" {
		t.Fatalf("stored servers = %#v", config.Servers)
	}
	path := filepath.Join(root, "conveyor", "credentials.json")
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("permissions file=%o dir=%o", fileInfo.Mode().Perm(), dirInfo.Mode().Perm())
	}
}

func TestAuthLoginFailureStoresNothingAndNonTTYIsActionable(t *testing.T) {
	isolateLocalAuthTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "attempted-secret", http.StatusUnauthorized)
	}))
	defer server.Close()
	t.Setenv("CONVEYOR_ADDR", server.URL)
	readLoginToken = func(*cobra.Command) (string, error) { return "attempted-secret", nil }
	command := authCmd()
	command.SetArgs([]string{"login"})
	err := command.Execute()
	if err == nil || strings.Contains(err.Error(), "attempted-secret") {
		t.Fatalf("login error = %v", err)
	}
	config, loadErr := loadLocalAuthConfig()
	if loadErr != nil || len(config.Servers) != 0 {
		t.Fatalf("failed login stored state: %#v err=%v", config.Servers, loadErr)
	}

	readLoginToken = readTerminalLoginToken
	command = authCmd()
	command.SetArgs([]string{"login"})
	command.SetIn(strings.NewReader("not-a-terminal"))
	err = command.Execute()
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("non-TTY error = %v", err)
	}
}

func TestAuthStatusTokenAndLogoutRevoke(t *testing.T) {
	isolateLocalAuthTest(t)
	const value = "cv_pat_pat_abcdefghijklmnop_secret"
	var revoked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+value {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
			_ = json.NewEncoder(w).Encode(core.CallerIdentity{Email: "owner@example.test", DisplayName: "Owner"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tokens":
			_ = json.NewEncoder(w).Encode([]core.PersonalAccessToken{{ID: "pat_abcdefghijklmnop", Label: "laptop"}})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/tokens/pat_abcdefghijklmnop":
			revoked = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("CONVEYOR_ADDR", server.URL)
	if err := updateLocalServerConfig(server.URL, func(entry *localServerConfig) { entry.Token = value; entry.Workspace = "demo" }); err != nil {
		t.Fatal(err)
	}

	var status bytes.Buffer
	command := authCmd()
	command.SetArgs([]string{"status"})
	command.SetOut(&status)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(status.String(), value) || !strings.Contains(status.String(), "laptop") || !strings.Contains(status.String(), "owner@example.test") {
		t.Fatalf("status output = %q", status.String())
	}

	var tokenOutput bytes.Buffer
	command = authCmd()
	command.SetArgs([]string{"token"})
	command.SetOut(&tokenOutput)
	if err := command.Execute(); err != nil || tokenOutput.String() != value+"\n" {
		t.Fatalf("token output=%q err=%v", tokenOutput.String(), err)
	}

	command = authCmd()
	command.SetArgs([]string{"logout", "--revoke"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("logout did not revoke the matching server token")
	}
	config, err := loadLocalAuthConfig()
	if err != nil || config.Servers[server.URL].Token != "" || config.Servers[server.URL].Workspace != "demo" {
		t.Fatalf("logout state = %#v err=%v", config.Servers, err)
	}
}

func TestAuthLogoutRemovesLocallyWhenServerIsUnavailable(t *testing.T) {
	isolateLocalAuthTest(t)
	server := "http://127.0.0.1:1"
	t.Setenv("CONVEYOR_ADDR", server)
	if err := updateLocalServerConfig(server, func(entry *localServerConfig) { entry.Token = "cv_pat_pat_abcdefghijklmnop_secret" }); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	command := authCmd()
	command.SetArgs([]string{"logout", "--revoke"})
	command.SetErr(&warnings)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warnings.String(), "warning") {
		t.Fatalf("warnings = %q", warnings.String())
	}
	config, err := loadLocalAuthConfig()
	if err != nil || len(config.Servers) != 0 {
		t.Fatalf("local credential survived logout: %#v err=%v", config.Servers, err)
	}
}

func TestClientResolutionPrecedenceAndSingletonFallback(t *testing.T) {
	isolateLocalAuthTest(t)
	server := "https://factory.example.test/base"
	t.Setenv("CONVEYOR_ADDR", server)
	if err := updateLocalServerConfig(server, func(entry *localServerConfig) {
		entry.Token = "stored-token"
		entry.Workspace = "stored-workspace"
	}); err != nil {
		t.Fatal(err)
	}

	c := newClient()
	if c.token != "stored-token" || c.workspace != "stored-workspace" || c.resolved.Token.Source != "stored file" {
		t.Fatalf("stored resolution = %+v", c)
	}
	t.Setenv("CONVEYOR_API_TOKEN", "environment-token")
	t.Setenv("CONVEYOR_WORKSPACE", "environment-workspace")
	c = newClient()
	if c.token != "environment-token" || c.workspace != "environment-workspace" || c.resolved.Token.Source != "environment CONVEYOR_API_TOKEN" {
		t.Fatalf("environment resolution = %+v", c)
	}
	workspaceFlag, workspaceFlagExplicit = "flag-workspace", true
	c = newClient()
	if c.workspace != "flag-workspace" || c.resolved.Workspace.Source != "flag" {
		t.Fatalf("flag resolution = %+v", c)
	}

	workspaceFlag, workspaceFlagExplicit = "", false
	t.Setenv("CONVEYOR_ADDR", "https://empty.example.test")
	t.Setenv("CONVEYOR_API_TOKEN", "")
	t.Setenv("CONVEYOR_WORKSPACE", "")
	c = newClient()
	if c.workspace != "" || c.resolved.Workspace.Source != "singleton fallback" {
		t.Fatalf("singleton resolution = %+v", c)
	}
}

func TestClientTokenResolutionBindsEnvironmentTokenToEnvironmentServer(t *testing.T) {
	isolateLocalAuthTest(t)
	envServer := "https://env.example.test"
	other := "https://other.example.test"
	t.Setenv("CONVEYOR_ADDR", envServer)
	for server, token := range map[string]string{envServer: "stored-env-token", other: "tok-b"} {
		if err := updateLocalServerConfig(server, func(entry *localServerConfig) { entry.Token = token }); err != nil {
			t.Fatal(err)
		}
	}

	// The environment token is eligible for its own environment server and
	// wins over the stored credential (req-cli-authentication AC-1.4).
	t.Setenv("CONVEYOR_API_TOKEN", "environment-token")
	c := newClient()
	if c.token != "environment-token" || c.base != envServer || c.resolved.Token.Source != "environment CONVEYOR_API_TOKEN" || !c.resolved.StoredCredential {
		t.Fatalf("matching environment server = %+v", c)
	}

	// An explicit server equal to the environment server still uses the
	// environment token.
	serverFlag, serverFlagExplicit = envServer+"/", true
	c = newClient()
	if c.token != "environment-token" || c.base != envServer || c.resolved.Token.Source != "environment CONVEYOR_API_TOKEN" {
		t.Fatalf("explicit matching server = %+v", c)
	}

	// A different explicit server ignores the environment token and falls back
	// to that server's stored credential, with a redacted explanatory source.
	serverFlag, serverFlagExplicit = other, true
	c = newClient()
	if c.token != "tok-b" || c.base != other || !c.resolved.StoredCredential {
		t.Fatalf("mismatched explicit server with stored credential = %+v", c)
	}
	if c.resolved.Token.Source != "stored file (environment CONVEYOR_API_TOKEN ignored for https://env.example.test)" {
		t.Fatalf("mismatched source = %q", c.resolved.Token.Source)
	}

	// Without a stored credential the environment token is still ignored and
	// the source explains the skipped token without exposing any value.
	if err := updateLocalServerConfig(other, func(entry *localServerConfig) { entry.Token = "" }); err != nil {
		t.Fatal(err)
	}
	c = newClient()
	if c.token != "" || c.resolved.StoredCredential || c.resolved.Token.Source != "environment CONVEYOR_API_TOKEN ignored for https://env.example.test" {
		t.Fatalf("mismatched explicit server without stored credential = %+v", c)
	}

	// Localhost automation: with CONVEYOR_ADDR unset the environment token
	// still applies to the default localhost server.
	serverFlag, serverFlagExplicit = "", false
	t.Setenv("CONVEYOR_ADDR", "")
	c = newClient()
	if c.token != "environment-token" || c.base != "http://localhost:8080" || c.resolved.Token.Source != "environment CONVEYOR_API_TOKEN" {
		t.Fatalf("localhost automation = %+v", c)
	}
}

func TestAuthStatusReportsTokenSourceWithoutRevealingValues(t *testing.T) {
	isolateLocalAuthTest(t)
	mismatched := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer stored-secret-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(core.CallerIdentity{Email: "other@example.test", DisplayName: "Other"})
	}))
	defer mismatched.Close()
	t.Setenv("CONVEYOR_ADDR", "https://env.example.test")
	t.Setenv("CONVEYOR_API_TOKEN", "environment-secret-token")
	if err := updateLocalServerConfig(mismatched.URL, func(entry *localServerConfig) { entry.Token = "stored-secret-token" }); err != nil {
		t.Fatal(err)
	}
	serverFlag, serverFlagExplicit = mismatched.URL, true

	var status, stderr bytes.Buffer
	command := authCmd()
	command.SetArgs([]string{"status"})
	command.SetOut(&status)
	command.SetErr(&stderr)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	output := status.String() + stderr.String()
	if !strings.Contains(output, "Token source") ||
		!strings.Contains(output, "environment CONVEYOR_API_TOKEN ignored for https://env.example.test") {
		t.Fatalf("status output = %q", output)
	}
	for _, secret := range []string{"environment-secret-token", "stored-secret-token"} {
		if strings.Contains(output, secret) {
			t.Fatalf("status output disclosed a token value: %q", output)
		}
	}
}

func TestClient401DiagnosticReportsSourceWithoutTokenValues(t *testing.T) {
	isolateLocalAuthTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	t.Setenv("CONVEYOR_API_TOKEN", "environment-secret-token")
	if err := updateLocalServerConfig(server.URL, func(entry *localServerConfig) { entry.Token = "stored-secret-token" }); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONVEYOR_ADDR", server.URL)
	c := newClient()
	err := c.do(http.MethodGet, "/v1/me", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "token from environment CONVEYOR_API_TOKEN") ||
		!strings.Contains(err.Error(), "a stored credential exists for "+server.URL) {
		t.Fatalf("401 diagnostic = %v", err)
	}

	serverFlag, serverFlagExplicit = server.URL, true
	t.Setenv("CONVEYOR_ADDR", "https://env.example.test")
	c = newClient()
	err = c.do(http.MethodGet, "/v1/me", nil, nil)
	if err == nil || !strings.Contains(err.Error(),
		"stored file (environment CONVEYOR_API_TOKEN ignored for https://env.example.test)") ||
		!strings.Contains(err.Error(), "a stored credential exists for "+server.URL) {
		t.Fatalf("mismatched 401 diagnostic = %v", err)
	}
	message := err.Error()
	for _, secret := range []string{"environment-secret-token", "stored-secret-token"} {
		if strings.Contains(message, secret) {
			t.Fatalf("401 diagnostic disclosed a token value: %q", message)
		}
	}
}

func TestClientPreconditionsNameGenericCredentialAndLoginAct(t *testing.T) {
	isolateLocalAuthTest(t)
	c := &client{base: "http://localhost:8080"}
	_, err := c.createTask("body", "repo", "main")
	if err == nil || !strings.Contains(err.Error(), "a credential is required") ||
		!strings.Contains(err.Error(), "conveyor auth login") || strings.Contains(err.Error(), "CONVEYOR_API_TOKEN") {
		t.Fatalf("precondition error = %v", err)
	}
}

func TestConfigSetWorkspaceAndListSources(t *testing.T) {
	isolateLocalAuthTest(t)
	t.Setenv("CONVEYOR_ADDR", "https://factory.example.test/")
	command := configCmd()
	command.SetArgs([]string{"set", "workspace", "engineering"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command = configCmd()
	command.SetArgs([]string{"list"})
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	executionPath, err := localAuthConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	executionPath = filepath.Join(filepath.Dir(executionPath), localExecutionConfigName)
	if !strings.Contains(output.String(), "workspace\tengineering\tstored file") ||
		!strings.Contains(output.String(), "execution_config\t"+executionPath+"\tuser default") || strings.Contains(output.String(), "token") {
		t.Fatalf("config list = %q", output.String())
	}
}

func TestNormalizeServerURLRejectsUnsafeComponents(t *testing.T) {
	for _, input := range []string{"factory.example.test", "ftp://factory.example.test", "https://user@factory.example.test", "https://factory.example.test?q=x", "https://factory.example.test#x"} {
		if _, err := normalizeServerURL(input); err == nil {
			t.Fatalf("normalizeServerURL(%q) succeeded", input)
		}
	}
	if got, err := normalizeServerURL("HTTPS://Factory.Example.Test/base///"); err != nil || got != "https://factory.example.test/base" {
		t.Fatalf("normalized=%q err=%v", got, err)
	}
	for input, want := range map[string]string{
		"https://factory.example.test/mcp":         "https://factory.example.test",
		"https://factory.example.test/base/mcp///": "https://factory.example.test/base",
		"https://factory.example.test/mcp/mcp":     "https://factory.example.test/mcp",
		"https://factory.example.test/base/mcpx":   "https://factory.example.test/base/mcpx",
	} {
		if got, err := normalizeServerURL(input); err != nil || got != want {
			t.Fatalf("normalizeServerURL(%q)=%q, want %q err=%v", input, got, want, err)
		}
	}
}

func TestLocalAuthConfigPathErrorIsPreserved(t *testing.T) {
	isolateLocalAuthTest(t)
	userConfigDir = func() (string, error) { return "", errors.New("unavailable") }
	if _, err := resolveClientConfig(); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error = %v", err)
	}
}
