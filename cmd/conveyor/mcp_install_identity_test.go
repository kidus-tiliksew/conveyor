package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func testMCPIdentity(t *testing.T, server, name string) mcpIdentity {
	t.Helper()
	id, err := newMCPIdentity(server, name, "/opt/Conveyor's tools/conveyor", "/config space/conveyor/credentials.json")
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func testMCPReconcile(tool string, prior []byte, id mcpIdentity, adopt bool) ([]byte, string, string, error) {
	if tool == "codex" {
		return reconcileNamedCodex(prior, id, adopt)
	}
	return reconcileNamedJSON(prior, tool, id, adopt)
}
func testMCPEntries(t *testing.T, tool string, data []byte) map[string]any {
	t.Helper()
	var root map[string]any
	if tool == "codex" {
		if err := toml.Unmarshal(data, &root); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := json.Unmarshal(data, &root); err != nil {
			t.Fatal(err)
		}
	}
	key := "mcpServers"
	if tool == "codex" {
		key = "mcp_servers"
	}
	if tool == "opencode" {
		key = "mcp"
	}
	return root[key].(map[string]any)
}
func TestMCPIdentityNaming(t *testing.T) {
	names := map[string]bool{}
	vars := map[string]bool{}
	for _, server := range []string{"https://one.example", "http://one.example", "https://one.example:443", "https://one.example/base", "https://one.example/other", "http://[::1]:8080/base"} {
		id := testMCPIdentity(t, server, "")
		if names[id.name] || vars[id.tokenEnv] {
			t.Fatal("server identities collided")
		}
		names[id.name] = true
		vars[id.tokenEnv] = true
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(server)))
		if !strings.Contains(id.name, hash[:12]) || id.tokenEnv != "CONVEYOR_MCP_TOKEN_"+strings.ToUpper(hash) {
			t.Fatal("incorrect identity hash")
		}
		again := testMCPIdentity(t, server+"/mcp/", "")
		if id.name != again.name {
			t.Fatal("normalization changed identity")
		}
	}
	if got := testMCPIdentity(t, "HTTPS://ONE.Example/mcp/", " Funnelflux-Conveyor "); got.name != "funnelflux-conveyor" || got.server != "https://one.example" {
		t.Fatal(got.name)
	}
	for _, name := range []string{" ", "a.b", "../x", "a b", "é", strings.Repeat("a", 64), "-x"} {
		if _, err := newMCPIdentity("https://one.example", name, "/bin/conveyor", "/config/credentials.json"); err == nil {
			t.Fatalf("accepted name %q", name)
		}
	}
}

func TestMCPNamedCoexistenceReinstallAndRename(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			for _, tool := range []string{"codex", "claude", "cursor", "opencode", "antigravity"} {
				t.Run(tool, func(t *testing.T) {
					first := testMCPModeIdentity(t, mode, "https://one.example/base:part", "first-conveyor")
					second := testMCPModeIdentity(t, mode, "https://two.example:443/other", "")
					data, status, name, err := testMCPReconcile(tool, nil, first, false)
					if err != nil || status != "created" || name != first.name {
						t.Fatalf("create %s %s %v", status, name, err)
					}
					data, _, _, err = testMCPReconcile(tool, data, second, false)
					if err != nil {
						t.Fatal(err)
					}
					for _, id := range []mcpIdentity{first, second, second, first} {
						next, status, _, err := testMCPReconcile(tool, data, id, false)
						if err != nil || status != "unchanged" || !bytes.Equal(data, next) {
							t.Fatalf("reinstall status=%s err=%v", status, err)
						}
					}
					entries := testMCPEntries(t, tool, data)
					if len(entries) != 2 {
						t.Fatal("lost/duplicated server")
					}
					other, _ := json.Marshal(entries[second.name])
					inferred := testMCPModeIdentity(t, mode, first.server, "")
					next, status, name, err := testMCPReconcile(tool, data, inferred, false)
					if err != nil || name != first.name || status != "unchanged" || !bytes.Equal(data, next) {
						t.Fatalf("custom name reuse %s %v", name, err)
					}
					renamed := testMCPModeIdentity(t, mode, first.server, "renamed-conveyor")
					next, _, name, err = testMCPReconcile(tool, data, renamed, false)
					if err != nil || name != renamed.name {
						t.Fatal(err)
					}
					entries = testMCPEntries(t, tool, next)
					if len(entries) != 2 || entries[first.name] != nil {
						t.Fatal("rename duplicated registration")
					}
					after, _ := json.Marshal(entries[second.name])
					if !bytes.Equal(other, after) {
						t.Fatal("rename changed other server")
					}
					conflict := testMCPModeIdentity(t, mode, second.server, renamed.name)
					if _, _, _, err = testMCPReconcile(tool, next, conflict, true); err == nil {
						t.Fatal("repointed named registration")
					}
					if bytes.Contains(data, []byte(mcpTokenEnv)) {
						t.Fatal("shared token in personal registration")
					}
				})
			}
		})
	}
}

func TestMCPMigrationPreservesNestedPolicies(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			fixtures := map[string]string{
				"codex":    "model = 'test'\n" + codexOwnerMarker + "\n[mcp_servers.conveyor]\nurl = 'https://one.example/mcp'\nbearer_token_env_var = 'CONVEYOR_API_TOKEN'\nenabled = false\nstartup_timeout_sec = 30\n[mcp_servers.other]\nurl='https://other.example/mcp'\n[mcp_servers.conveyor.tools.create_task]\napproval_mode='prompt'\noutput_token_limit=300\n",
				"claude":   `{"theme" : "dark","_conveyor_mcp_install":"owner=v1","mcpServers":{"conveyor":{"type":"http","url":"https://one.example/mcp","headers":{"Authorization":"Bearer ${CONVEYOR_API_TOKEN}","X-Other":"value"},"tools":{"create_task":{"approval_mode":"prompt"}},"timeout":30},"manual":{"url":"https://manual.example/mcp"}}}`,
				"cursor":   `{"_conveyor_mcp_install":"owner=v1","mcpServers":{"conveyor":{"url":"${env:CONVEYOR_ADDR}","headers":{"Authorization":"Bearer ${env:CONVEYOR_API_TOKEN}"},"tools":{"create_task":{"approval_mode":"prompt"}}}}}`,
				"opencode": `{"mcp":{"conveyor":{"_conveyor_mcp_install":"owner=v1","type":"remote","url":"{env:CONVEYOR_ADDR}","headers":{"Authorization":"Bearer {env:CONVEYOR_API_TOKEN}"},"enabled":false,"timeout":30,"tools":{"create_task":{"approval_mode":"prompt"}}}}}`,
			}
			for tool, fixture := range fixtures {
				t.Run(tool, func(t *testing.T) {
					id := testMCPModeIdentity(t, mode, "https://one.example", "")
					t.Setenv(mcpAddressEnv, "https://one.example/mcp")
					data, _, name, err := testMCPReconcile(tool, []byte(fixture), id, true)
					if err != nil {
						t.Fatal(err)
					}
					entries := testMCPEntries(t, tool, data)
					if name != id.name || entries["conveyor"] != nil {
						t.Fatal("migration did not rename")
					}
					entry := entries[name].(map[string]any)
					policy := entry["tools"].(map[string]any)["create_task"].(map[string]any)
					if policy["approval_mode"] != "prompt" {
						t.Fatal("lost nested policy")
					}
					again, status, _, err := testMCPReconcile(tool, data, id, false)
					if err != nil || status != "unchanged" || !bytes.Equal(again, data) {
						t.Fatalf("migration not idempotent %s %v", status, err)
					}
					if tool == "codex" && !bytes.Contains(data, []byte("[mcp_servers.other]\nurl='https://other.example/mcp'")) {
						t.Fatal("other TOML bytes changed")
					}
					if tool == "claude" && !bytes.Contains(data, []byte(`"theme" : "dark"`)) {
						t.Fatal("other JSON bytes changed")
					}
				})
			}
		})
	}
}

func TestMCPRefusesManualAuthAndAdoptionRepoint(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			for _, tool := range []string{"codex", "claude", "cursor", "opencode", "antigravity"} {
				t.Run(tool, func(t *testing.T) {
					id := testMCPModeIdentity(t, mode, "https://one.example", "manual")
					var prior []byte
					if tool == "codex" {
						prior = []byte("[mcp_servers.manual]\nurl='https://one.example/mcp'\nhttp_headers={Authorization='Bearer fixture-secret'}\n")
					} else {
						key := "mcpServers"
						if tool == "opencode" {
							key = "mcp"
						}
						prior = []byte(fmt.Sprintf(`{"%s":{"manual":{"url":"https://one.example/mcp","headers":{"Authorization":"Bearer fixture-secret"}}}}`, key))
					}
					for _, adopt := range []bool{false, true} {
						_, _, _, err := testMCPReconcile(tool, prior, id, adopt)
						if err == nil || strings.Contains(err.Error(), "fixture-secret") {
							t.Fatal("manual auth not refused safely")
						}
					}
					other := testMCPModeIdentity(t, mode, "https://two.example", "manual")
					if _, _, _, err := testMCPReconcile(tool, prior, other, true); err == nil {
						t.Fatal("adopt repointed manual server")
					}
				})
			}
		})
	}
}

func TestMCPSharedWorkerAttachmentNeedsExplicitAdoption(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			for _, tool := range []string{"cursor", "opencode"} {
				t.Run(tool, func(t *testing.T) {
					key, endpoint := "mcpServers", "${env:CONVEYOR_ADDR}"
					if tool == "opencode" {
						key, endpoint = "mcp", "{env:CONVEYOR_ADDR}"
					}
					fixture := []byte(fmt.Sprintf(`{"_conveyor_mcp_install":"owner=v1","%s":{"conveyor":{"_conveyor_mcp_install":"owner=v1","url":%q}}}`, key, endpoint))
					id := testMCPModeIdentity(t, mode, "https://one.example", "")
					t.Setenv(mcpAddressEnv, "https://one.example/mcp")
					next, _, _, err := testMCPReconcile(tool, fixture, id, false)
					if err != nil {
						t.Fatal(err)
					}
					entry := testMCPEntries(t, tool, next)["conveyor"].(map[string]any)
					if entry["url"] != endpoint {
						t.Fatal("changed worker attachment")
					}
					id.explicit = true
					id.name = "conveyor"
					t.Setenv(mcpAddressEnv, "https://other.example/mcp")
					if _, _, _, err := testMCPReconcile(tool, fixture, id, true); err == nil {
						t.Fatal("adopted ambiguous worker endpoint")
					}
				})
			}
		})
	}
}

// TestMCPCommandTwoCredentialsAndList runs in both modes for all five
// clients: two servers coexist, --list writes nothing, and no token reaches
// the file or the output.
func TestMCPCommandTwoCredentialsAndList(t *testing.T) {
	for _, tool := range []string{"codex", "claude", "cursor", "opencode", "antigravity"} {
		for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
			t.Run(tool+"/"+mode, func(t *testing.T) { testMCPCommandTwoCredentialsAndList(t, tool, mode) })
		}
	}
}

func testMCPCommandTwoCredentialsAndList(t *testing.T, tool, mode string) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv(mcpAddressEnv, "https://unrelated.example")
	t.Setenv(mcpTokenEnv, "ambient-secret")
	setMCPTestConfig(t, localAuthConfig{Servers: map[string]localServerConfig{"https://one.example": {Token: "first-secret"}, "https://two.example": {Token: "second-secret"}}})
	lookup := mcpModeLookup(t, mode, testMCPIdentity(t, "https://one.example", "").name, testMCPIdentity(t, "https://two.example", "").name)
	t.Cleanup(func() { serverFlag = ""; serverFlagExplicit = false })
	targets := mcpTargets(home, []skillTool{{name: tool}})
	path := targets[0].path
	for _, server := range []string{"https://one.example", "https://two.example", "https://one.example", "https://two.example"} {
		serverFlag, serverFlagExplicit = server, true
		cmd := mcpInstallCmdWithLookPath(lookup)
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs([]string{"--auth", mode, "--tool", tool, "--list"})
		before, _ := os.ReadFile(path)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("list wrote configuration")
		}
		cmd = mcpInstallCmdWithLookPath(lookup)
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs([]string{"--auth", mode, "--tool", tool})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		for _, secret := range []string{"ambient-secret", "first-secret", "second-secret"} {
			if bytes.Contains(data, []byte(secret)) || strings.Contains(output.String(), secret) {
				t.Fatal("secret leaked")
			}
		}
		if !strings.Contains(output.String(), "Native initialize/tools-list: not run") || !strings.Contains(output.String(), "Restart:") {
			t.Fatal("missing evidence boundary")
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatal("not owner-only")
		}
	}
	data, _ := os.ReadFile(path)
	if len(testMCPEntries(t, tool, data)) != 2 {
		t.Fatal("two saved servers did not coexist")
	}
	config, err := loadLocalAuthConfig()
	if err != nil || config.Servers["https://one.example"].Token != "first-secret" || config.Servers["https://two.example"].Token != "second-secret" {
		t.Fatal("installation altered stored credentials")
	}
}

func TestMCPReinstallPreservesOtherCodexOwnership(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			first := testMCPModeIdentity(t, mode, "https://one.example", "one")
			second := testMCPModeIdentity(t, mode, "https://two.example", "two")
			data, _, _, err := reconcileNamedCodex(nil, first, false)
			if err != nil {
				t.Fatal(err)
			}
			data, _, _, err = reconcileNamedCodex(data, second, false)
			if err != nil {
				t.Fatal(err)
			}
			// Rename the first block, whose next block has an ownership comment.
			first.name = "renamed"
			data, _, _, err = reconcileNamedCodex(data, first, false)
			if err != nil {
				t.Fatal(err)
			}
			second.explicit = false
			_, status, name, err := reconcileNamedCodex(data, second, false)
			if err != nil || status != "unchanged" || name != "two" {
				t.Fatalf("lost other owner: status=%s name=%s err=%v", status, name, err)
			}
		})
	}
}

func TestMCPRefusesAmbiguousOrUnsupportedConfigBeforeWrite(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			cases := []struct{ tool, prior string }{
				{"codex", "[mcp_servers.one]\nurl='https://one.example/mcp'\n[mcp_servers.one]\nurl='https://one.example/mcp'\n"},
				{"codex", "mcp_servers={one={url='https://one.example/mcp'}}\n"},
				{"claude", `{"mcpServers":{"one":{"url":"https://one.example/mcp","url":"https://two.example/mcp"}}}`},
				{"cursor", `{"mcpServers":{"one":{"url":"https://one.example/mcp","oauth":{"clientId":"custom"}}}}`},
				{"opencode", `{"mcp":{"one":{"url":"https://one.example/mcp","oauth":true}}}`},
				{"antigravity", `{"mcpServers":{"one":{"serverUrl":"https://one.example/mcp","serverUrl":"https://two.example/mcp"}}}`},
				{"antigravity", `{"mcpServers":{"one":{"serverUrl":"https://one.example/mcp","env":{"A":"b"}}}}`},
				{"antigravity", `{"mcpServers":{"one":{"command":"/usr/bin/other","args":["mcp","stdio","--server","https://one.example","--credentials-file","/config space/conveyor/credentials.json"]}},"_conveyor_mcp_install":{"one":{"owner":"v2","endpoint":"https://one.example/mcp"}}}`},
				{"antigravity", "{\"mcpServers\":{\n// keep my comment\n}}"},
			}
			for _, c := range cases {
				t.Run(c.tool+c.prior[:8], func(t *testing.T) {
					home := t.TempDir()
					path := filepath.Join(home, "config")
					prior := []byte(c.prior)
					if err := os.WriteFile(path, prior, 0o640); err != nil {
						t.Fatal(err)
					}
					id := testMCPModeIdentity(t, mode, "https://one.example", "one")
					if _, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: c.tool, path: path}, id, true, true, noOpenCodeOnPath); err == nil {
						t.Fatal("accepted ambiguous config")
					}
					after, _ := os.ReadFile(path)
					info, _ := os.Stat(path)
					if !bytes.Equal(prior, after) || info.Mode().Perm() != 0o640 {
						t.Fatal("failure changed bytes or permissions")
					}
				})
			}
		})
	}
}

func TestMCPListDoesNotExecuteParserAndInstallSecuresExistingFile(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "config.json")
			id := testMCPModeIdentity(t, mode, "https://one.example", "")
			data, _, _, err := reconcileNamedJSON(nil, "cursor", id, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o640); err != nil {
				t.Fatal(err)
			}
			lookup := func(string) (string, error) { t.Fatal("list executed native parser lookup"); return "", nil }
			if _, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "cursor", path: path}, id, false, false, lookup); err != nil {
				t.Fatal(err)
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0o640 {
				t.Fatal("list changed permissions")
			}
			result, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "cursor", path: path}, id, false, true, noOpenCodeOnPath)
			info, _ = os.Stat(path)
			if err != nil || result.status != "secured" || info.Mode().Perm() != 0o600 {
				t.Fatal("reinstall did not secure file")
			}
		})
	}
}

func TestMCPCodexMultilineTableTextCannotChangeUnrelatedValues(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			prior := []byte("note = '''\n" + codexOwnerMarker + "\n[mcp_servers.one]\ntext inside a multiline string\n'''\n[mcp_servers.one]\nurl='https://one.example/mcp'\n")
			id := testMCPModeIdentity(t, mode, "https://one.example", "one")
			if _, _, _, err := reconcileNamedCodex(prior, id, true); err == nil {
				t.Fatal("accepted unsafe multiline table layout")
			}
		})
	}
}

func TestMCPParserRejectsMissingOrNullNativeHelper(t *testing.T) {
	for _, output := range []string{`{}`, `{"transport":{"http_headers_helper":null}}`} {
		home := t.TempDir()
		path := filepath.Join(home, "config.toml")
		id := testMCPIdentity(t, "https://one.example", "one")
		data, _, _, err := reconcileNamedCodex(nil, id, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		binary := openCodeValidationBinary(t, "echo "+shellQuoteMCP(output))
		if err := validateMCPParser(binary, "codex", path, id.name, id); err == nil {
			t.Fatal("accepted ignored helper field")
		}
	}
}

func TestMCPManualMatchingEndpointRequiresExplicitAdoption(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			for _, tool := range []string{"codex", "claude", "cursor", "opencode", "antigravity"} {
				t.Run(tool, func(t *testing.T) {
					id := testMCPModeIdentity(t, mode, "https://one.example", "")
					prior := []byte("[mcp_servers.manual]\nurl='https://one.example/mcp'\nstartup_timeout_sec=30\n")
					if tool != "codex" {
						key := "mcpServers"
						if tool == "opencode" {
							key = "mcp"
						}
						prior = []byte(fmt.Sprintf(`{"%s":{"manual":{"url":"https://one.example/mcp","timeout":30}}}`, key))
					}
					if _, _, _, err := testMCPReconcile(tool, prior, id, false); err == nil {
						t.Fatal("duplicated manual endpoint")
					}
					id = testMCPModeIdentity(t, mode, id.server, "manual")
					next, _, name, err := testMCPReconcile(tool, prior, id, true)
					if err != nil || name != "manual" {
						t.Fatalf("explicit adoption failed: %v", err)
					}
					entries := testMCPEntries(t, tool, next)
					if len(entries) != 1 {
						t.Fatal("adoption created duplicate")
					}
				})
			}
		})
	}
}

// The golden bytes below were produced by the base commit ff006278, before
// the Antigravity adapter existed. They hold the other four clients'
// registrations byte for byte.
func TestMCPExistingClientRegistrationsStayByteIdentical(t *testing.T) {
	golden := map[string][2]string{
		"codex": {
			"# conveyor:mcp-install owner=v1\n[mcp_servers.one-example-45c59beae3c6-conveyor]\nhttp_headers_helper = \"'/opt/Conveyor'\\\"'\\\"'s tools/conveyor' auth token --server 'https://one.example/base' --credentials-file '/config space/conveyor/credentials.json' --format http-headers\"\nurl = 'https://one.example/base/mcp'\n",
			"model = 'x'\n# conveyor:mcp-install owner=v1\n[mcp_servers.manual]\nhttp_headers_helper = \"'/opt/Conveyor'\\\"'\\\"'s tools/conveyor' auth token --server 'https://one.example' --credentials-file '/config space/conveyor/credentials.json' --format http-headers\"\nstartup_timeout_sec = 30\nurl = 'https://one.example/mcp'\n",
		},
		"claude": {
			"{\"mcpServers\":{\"one-example-45c59beae3c6-conveyor\":{\"headersHelper\":\"'/opt/Conveyor'\\\"'\\\"'s tools/conveyor' auth token --server 'https://one.example/base' --credentials-file '/config space/conveyor/credentials.json' --format http-headers\",\"type\":\"http\",\"url\":\"https://one.example/base/mcp\"}},\"_conveyor_mcp_install\":{\"one-example-45c59beae3c6-conveyor\":{\"owner\":\"v2\",\"endpoint\":\"https://one.example/base/mcp\"}}}\n",
			"{\"theme\":\"dark\",\"mcpServers\":{\"manual\":{\"headersHelper\":\"'/opt/Conveyor'\\\"'\\\"'s tools/conveyor' auth token --server 'https://one.example' --credentials-file '/config space/conveyor/credentials.json' --format http-headers\",\"timeout\":30,\"type\":\"http\",\"url\":\"https://one.example/mcp\"},\"other\":{\"command\":\"other\"}},\"_conveyor_mcp_install\":{\"manual\":{\"owner\":\"v2\",\"endpoint\":\"https://one.example/mcp\"}}}",
		},
		"cursor": {
			"{\"mcpServers\":{\"one-example-45c59beae3c6-conveyor\":{\"headers\":{\"Authorization\":\"Bearer ${env:CONVEYOR_MCP_TOKEN_45C59BEAE3C6D988B44ADC14FD86B4CC282662454C03131D4024601D9C45667D}\"},\"url\":\"https://one.example/base/mcp\"}},\"_conveyor_mcp_install\":{\"one-example-45c59beae3c6-conveyor\":{\"owner\":\"v2\",\"endpoint\":\"https://one.example/base/mcp\"}}}\n",
			"{\"mcpServers\":{\"manual\":{\"headers\":{\"Authorization\":\"Bearer ${env:CONVEYOR_MCP_TOKEN_EC97C070AB287A52432AE841FBDF423A4A7C52B7F0057A51F57A16E3D3827C87}\",\"X-Other\":\"v\"},\"url\":\"https://one.example/mcp\"},\"other\":{\"command\":\"other\"}},\"_conveyor_mcp_install\":{\"manual\":{\"owner\":\"v2\",\"endpoint\":\"https://one.example/mcp\"}}}",
		},
		"opencode": {
			"{\"mcp\":{\"one-example-45c59beae3c6-conveyor\":{\"_conveyor_mcp_install\":{\"owner\":\"v2\",\"endpoint\":\"https://one.example/base/mcp\"},\"headers\":{\"Authorization\":\"Bearer {env:CONVEYOR_MCP_TOKEN_45C59BEAE3C6D988B44ADC14FD86B4CC282662454C03131D4024601D9C45667D}\"},\"oauth\":false,\"type\":\"remote\",\"url\":\"https://one.example/base/mcp\"}}}\n",
			"{\"mcp\":{\"manual\":{\"_conveyor_mcp_install\":{\"owner\":\"v2\",\"endpoint\":\"https://one.example/mcp\"},\"enabled\":false,\"headers\":{\"Authorization\":\"Bearer {env:CONVEYOR_MCP_TOKEN_EC97C070AB287A52432AE841FBDF423A4A7C52B7F0057A51F57A16E3D3827C87}\"},\"oauth\":false,\"type\":\"remote\",\"url\":\"https://one.example/mcp\"},\"other\":{\"type\":\"local\",\"command\":[\"other\"]}}}",
		},
	}
	priors := map[string]string{
		"codex":    "model = 'x'\n[mcp_servers.manual]\nurl='https://one.example/mcp'\nstartup_timeout_sec=30\n",
		"claude":   `{"theme":"dark","mcpServers":{"manual":{"url":"https://one.example/mcp","timeout":30},"other":{"command":"other"}}}`,
		"cursor":   `{"mcpServers":{"manual":{"url":"https://one.example/mcp","headers":{"X-Other":"v"}},"other":{"command":"other"}}}`,
		"opencode": `{"mcp":{"manual":{"type":"remote","url":"https://one.example/mcp","enabled":false},"other":{"type":"local","command":["other"]}}}`,
	}
	for tool, want := range golden {
		t.Run(tool, func(t *testing.T) {
			created, status, _, err := testMCPReconcile(tool, nil, testMCPIdentity(t, "https://one.example/base", ""), false)
			if err != nil || status != "created" || string(created) != want[0] {
				t.Fatalf("create changed: status=%s err=%v\n%q", status, err, created)
			}
			adopted, status, _, err := testMCPReconcile(tool, []byte(priors[tool]), testMCPIdentity(t, "https://one.example", "manual"), true)
			if err != nil || status != "refreshed" || string(adopted) != want[1] {
				t.Fatalf("adoption changed: status=%s err=%v\n%q", status, err, adopted)
			}
		})
	}
}

func testAntigravityEntry(t *testing.T, data []byte, name string) map[string]any {
	t.Helper()
	entry, ok := testMCPEntries(t, "antigravity", data)[name].(map[string]any)
	if !ok {
		t.Fatalf("missing Antigravity entry %s:\n%s", name, data)
	}
	return entry
}

// checkAntigravityBridgeEntry holds an entry to invariant 6: command and args
// run the stdio bridge, and no env, url, serverUrl, or headers remain.
func checkAntigravityBridgeEntry(t *testing.T, entry map[string]any, id mcpIdentity) {
	t.Helper()
	want := []any{"mcp", "stdio", "--server", id.server, "--credentials-file", id.credentials}
	if entry["command"] != id.binary || !reflect.DeepEqual(entry["args"], want) {
		t.Fatalf("entry does not run the bridge: %v", entry)
	}
	for _, key := range []string{"env", "url", "serverUrl", "headers"} {
		if _, ok := entry[key]; ok {
			t.Fatalf("entry keeps %s: %v", key, entry)
		}
	}
}

// checkAntigravityModeEntry holds an entry to its mode: the bridge under
// --auth pat, or only serverUrl under --auth oauth, with no env, url,
// headers, command, or args (component-cli-onboarding invariant 6).
func checkAntigravityModeEntry(t *testing.T, entry map[string]any, id mcpIdentity) {
	t.Helper()
	if !id.oauth() {
		checkAntigravityBridgeEntry(t, entry, id)
		return
	}
	if entry["serverUrl"] != id.endpoint {
		t.Fatalf("entry does not name the endpoint: %v", entry)
	}
	for _, key := range []string{"env", "url", "headers", "command", "args"} {
		if _, ok := entry[key]; ok {
			t.Fatalf("OAuth entry keeps %s: %v", key, entry)
		}
	}
}

func TestMCPAntigravityCreateWritesOnlyTheBridgeEntry(t *testing.T) {
	id := testMCPIdentity(t, "https://one.example/base", "")
	data, status, name, err := reconcileNamedJSON(nil, "antigravity", id, false)
	if err != nil || status != "created" || name != id.name {
		t.Fatalf("create status=%s name=%s err=%v", status, name, err)
	}
	want := "{\"mcpServers\":{\"one-example-45c59beae3c6-conveyor\":{\"args\":[\"mcp\",\"stdio\",\"--server\",\"https://one.example/base\",\"--credentials-file\",\"/config space/conveyor/credentials.json\"],\"command\":\"/opt/Conveyor's tools/conveyor\"}},\"_conveyor_mcp_install\":{\"one-example-45c59beae3c6-conveyor\":{\"owner\":\"v2\",\"endpoint\":\"https://one.example/base/mcp\"}}}\n"
	if string(data) != want {
		t.Fatalf("created bytes:\n%q", data)
	}
	checkAntigravityBridgeEntry(t, testAntigravityEntry(t, data, name), id)
	for _, forbidden := range []string{mcpTokenEnv, id.tokenEnv, "auth token", "Bearer"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("registration carries %q", forbidden)
		}
	}
	again, status, _, err := reconcileNamedJSON(data, "antigravity", id, false)
	if err != nil || status != "unchanged" || !bytes.Equal(again, data) {
		t.Fatalf("rerun status=%s err=%v", status, err)
	}
	// The ownership marker survives the key order and indentation that agy
	// writes when it rewrites the file.
	pretty := []byte(`{
  "_conveyor_mcp_install": {
    "one-example-45c59beae3c6-conveyor": {"owner": "v2", "endpoint": "https://one.example/base/mcp"}
  },
  "mcpServers": {
    "one-example-45c59beae3c6-conveyor": {
      "command": "/opt/Conveyor's tools/conveyor",
      "args": ["mcp", "stdio", "--server", "https://one.example/base", "--credentials-file", "/config space/conveyor/credentials.json"]
    }
  }
}`)
	again, status, _, err = reconcileNamedJSON(pretty, "antigravity", id, false)
	if err != nil || status != "unchanged" || !bytes.Equal(again, pretty) {
		t.Fatalf("rerun on agy layout status=%s err=%v", status, err)
	}
}

func TestMCPAntigravityRenamePreservesPolicyAndOtherEntries(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			id := testMCPModeIdentity(t, mode, "https://one.example", "")
			prior := []byte(`{"theme" : "dark","mcpServers":{"other":{"serverUrl":"https://other.example/mcp"}}}`)
			data, _, _, err := reconcileNamedJSON(prior, "antigravity", id, false)
			if err != nil {
				t.Fatal(err)
			}
			// agy writes "disabled" on its own entries; the installer keeps it.
			data = bytes.Replace(data, []byte(`"`+id.name+`":{`), []byte(`"`+id.name+`":{"disabled":true,`), 1)
			renamed := testMCPModeIdentity(t, mode, id.server, "renamed-conveyor")
			next, status, name, err := reconcileNamedJSON(data, "antigravity", renamed, false)
			if err != nil || status != "refreshed" || name != "renamed-conveyor" {
				t.Fatalf("rename status=%s name=%s err=%v", status, name, err)
			}
			entries := testMCPEntries(t, "antigravity", next)
			if len(entries) != 2 || entries[id.name] != nil {
				t.Fatalf("rename duplicated or lost entries: %v", entries)
			}
			entry := testAntigravityEntry(t, next, name)
			checkAntigravityModeEntry(t, entry, renamed)
			if entry["disabled"] != true {
				t.Fatal("rename lost the disabled policy")
			}
			if !bytes.Contains(next, []byte(`"theme" : "dark"`)) || !bytes.Contains(next, []byte(`"other":{"serverUrl":"https://other.example/mcp"}`)) {
				t.Fatalf("rename changed unrelated bytes:\n%s", next)
			}
			var root struct {
				Owners map[string]mcpOwnedEndpoint `json:"_conveyor_mcp_install"`
			}
			if err := json.Unmarshal(next, &root); err != nil {
				t.Fatal(err)
			}
			if len(root.Owners) != 1 || root.Owners[name] != (mcpOwnedEndpoint{Owner: "v2", Endpoint: renamed.endpoint}) {
				t.Fatalf("rename did not move the ownership marker: %v", root.Owners)
			}
			again, status, _, err := reconcileNamedJSON(next, "antigravity", testMCPModeIdentity(t, mode, id.server, ""), false)
			if err != nil || status != "unchanged" || !bytes.Equal(again, next) {
				t.Fatalf("renamed entry is not reused: status=%s err=%v", status, err)
			}
		})
	}
}

func TestMCPAntigravityAdoptsAgyAddEntry(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			for _, field := range []string{"serverUrl", "url"} {
				t.Run(field, func(t *testing.T) {
					// `agy mcp add manual https://one.example/mcp` writes serverUrl
					// and disabled. A hand-written entry can use url.
					prior := []byte(fmt.Sprintf(`{"theme":"dark","mcpServers":{"manual":{"disabled":false,%q:"https://one.example/mcp","headers":{"X-Trace":"1"}},"other":{"serverUrl":"https://other.example/mcp"}}}`, field))
					for _, id := range []mcpIdentity{testMCPModeIdentity(t, mode, "https://one.example", ""), testMCPModeIdentity(t, mode, "https://one.example", "manual")} {
						_, _, _, err := reconcileNamedJSON(prior, "antigravity", id, false)
						if err == nil || !strings.Contains(err.Error(), "--name manual --adopt") {
							t.Fatalf("unowned entry accepted without adoption: %v", err)
						}
					}
					if _, _, _, err := reconcileNamedJSON(prior, "antigravity", testMCPModeIdentity(t, mode, "https://one.example", ""), true); err == nil {
						t.Fatal("--adopt without --name adopted an arbitrary name")
					}
					id := testMCPModeIdentity(t, mode, "https://one.example", "manual")
					next, status, name, err := reconcileNamedJSON(prior, "antigravity", id, true)
					if err != nil || status != "refreshed" || name != "manual" {
						t.Fatalf("adoption status=%s name=%s err=%v", status, name, err)
					}
					entry := testAntigravityEntry(t, next, "manual")
					checkAntigravityModeEntry(t, entry, id)
					// The bridge has command and args. OAuth has only serverUrl.
					if want := map[bool]int{false: 3, true: 2}[id.oauth()]; entry["disabled"] != false || len(entry) != want {
						t.Fatalf("adoption changed unrelated entry fields: %v", entry)
					}
					if !bytes.Contains(next, []byte(`"other":{"serverUrl":"https://other.example/mcp"}`)) || !bytes.Contains(next, []byte(`"theme":"dark"`)) {
						t.Fatalf("adoption changed other bytes:\n%s", next)
					}
					again, status, _, err := reconcileNamedJSON(next, "antigravity", testMCPModeIdentity(t, mode, id.server, ""), false)
					if err != nil || status != "unchanged" || !bytes.Equal(again, next) {
						t.Fatalf("adopted entry is not owned: status=%s err=%v", status, err)
					}
				})
			}
		})
	}
}

func TestMCPAntigravityAdoptionReplacesLegacyTokenReference(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			prior := []byte(`{"mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","headers":{"Authorization":"Bearer ${CONVEYOR_API_TOKEN}"}}}}`)
			id := testMCPModeIdentity(t, mode, "https://one.example", "manual")
			if _, _, _, err := reconcileNamedJSON(prior, "antigravity", id, false); err == nil {
				t.Fatal("legacy reference replaced without adoption")
			}
			next, _, _, err := reconcileNamedJSON(prior, "antigravity", id, true)
			if err != nil {
				t.Fatal(err)
			}
			checkAntigravityModeEntry(t, testAntigravityEntry(t, next, "manual"), id)
			if bytes.Contains(next, []byte(mcpTokenEnv)) {
				t.Fatal("legacy token reference remains")
			}
		})
	}
}

func TestMCPAntigravityEnvironmentAddressBindsOnlyUnderAdoption(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			prior := []byte(`{"mcpServers":{"conveyor":{"serverUrl":"${env:CONVEYOR_ADDR}"}}}`)
			id := testMCPModeIdentity(t, mode, "https://one.example", "")
			t.Setenv(mcpAddressEnv, "https://one.example/mcp")
			next, status, _, err := reconcileNamedJSON(prior, "antigravity", id, false)
			if err != nil || status != "created" {
				t.Fatalf("create beside environment entry: status=%s err=%v", status, err)
			}
			if testAntigravityEntry(t, next, "conveyor")["serverUrl"] != "${env:CONVEYOR_ADDR}" {
				t.Fatal("changed the environment entry without adoption")
			}
			adopted, _, name, err := reconcileNamedJSON(prior, "antigravity", id, true)
			if err != nil || name != id.name {
				t.Fatalf("adopt matching environment entry: name=%s err=%v", name, err)
			}
			checkAntigravityModeEntry(t, testAntigravityEntry(t, adopted, id.name), id)
			t.Setenv(mcpAddressEnv, "https://other.example/mcp")
			named := testMCPModeIdentity(t, mode, id.server, "conveyor")
			if _, _, _, err := reconcileNamedJSON(prior, "antigravity", named, true); err == nil {
				t.Fatal("adopted an environment entry for another server")
			}
		})
	}
}

func TestMCPAntigravityRefusesForeignFieldsBeforeWrite(t *testing.T) {
	bridge := `"command":"/opt/Conveyor's tools/conveyor","args":["mcp","stdio","--server","https://one.example","--credentials-file","/config space/conveyor/credentials.json"]`
	marker := `"_conveyor_mcp_install":{"manual":{"owner":"v2","endpoint":"https://one.example/mcp"}}`
	cases := map[string]string{
		"foreign Authorization":           `{"mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","headers":{"Authorization":"Bearer fixture-secret"}}}}`,
		"lowercase foreign Authorization": `{"mcpServers":{"manual":{"url":"https://one.example/mcp","headers":{"authorization":"Bearer ${env:FIXTURE}"}}}}`,
		"foreign Authorization on owned":  `{"mcpServers":{"manual":{` + bridge + `,"headers":{"Authorization":"Bearer fixture-secret"}}},` + marker + `}`,
		"non-bridge command":              `{"mcpServers":{"manual":{"command":"/usr/bin/other","args":["mcp","stdio","--server","https://one.example","--credentials-file","/config space/conveyor/credentials.json"]}},` + marker + `}`,
		// An owned bridge whose credential file moved is refreshed (see
		// TestMCPAuthModeMovedPathsAllClients). An unowned one stays refused.
		"unowned other credentials file": `{"mcpServers":{"manual":{"command":"/opt/Conveyor's tools/conveyor","args":["mcp","stdio","--server","https://one.example","--credentials-file","/elsewhere/credentials.json"]}}}`,
		"extra bridge argument":          `{"mcpServers":{"manual":{"command":"/opt/Conveyor's tools/conveyor","args":["mcp","stdio","--server","https://one.example","--credentials-file","/config space/conveyor/credentials.json","--verbose"]}}}`,
		"args without command":           `{"mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","args":["x"]}}}`,
		"command on remote entry":        `{"mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","command":"/usr/bin/other"}}}`,
		"env on owned bridge":            `{"mcpServers":{"manual":{` + bridge + `,"env":{"CONVEYOR_API_TOKEN":"fixture-secret"}}},` + marker + `}`,
		"empty env on owned bridge":      `{"mcpServers":{"manual":{` + bridge + `,"env":{}}},` + marker + `}`,
		"second endpoint":                `{"mcpServers":{"manual":{"url":"https://one.example/mcp","serverUrl":"https://two.example/mcp"}}}`,
		"bridge for another server":      `{"mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","command":"/opt/Conveyor's tools/conveyor","args":["mcp","stdio","--server","https://two.example","--credentials-file","/config space/conveyor/credentials.json"]}}}`,
		"name for another server":        `{"mcpServers":{"manual":{"serverUrl":"https://two.example/mcp"}}}`,
		"marker without an address":      `{"mcpServers":{"manual":{"disabled":false}},` + marker + `}`,
		"bearer token variable":          `{"mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","bearer_token_env_var":"OTHER"}}}`,
		"invalid headers":                `{"mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","headers":"Authorization: Bearer fixture-secret"}}}`,
	}
	for label, prior := range cases {
		t.Run(label, func(t *testing.T) {
			home := t.TempDir()
			path := mcpTargets(home, []skillTool{{name: "antigravity"}})[0].path
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(prior), 0o640); err != nil {
				t.Fatal(err)
			}
			parser := antigravityListBinary(t, "", "manual")
			lookup := func(string) (string, error) { return parser, nil }
			for _, adopt := range []bool{false, true} {
				id := testMCPIdentity(t, "https://one.example", "manual")
				_, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "antigravity", path: path}, id, adopt, true, lookup)
				if err == nil {
					t.Fatalf("adopt=%t accepted %s", adopt, prior)
				}
				if strings.Contains(err.Error(), "fixture-secret") {
					t.Fatalf("error leaks a credential: %v", err)
				}
				after, _ := os.ReadFile(path)
				info, _ := os.Stat(path)
				if string(after) != prior || info.Mode().Perm() != 0o640 {
					t.Fatalf("refusal changed bytes or mode: %o %s", info.Mode().Perm(), after)
				}
				if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".conveyor-*")); len(leftovers) != 0 {
					t.Fatalf("staging leftovers: %v", leftovers)
				}
			}
		})
	}
}

func TestMCPAntigravityMarkerAloneGrantsNoOwnership(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			// The root marker names another server, so the bridge entry for this
			// server is unowned. Only explicit adoption takes it over.
			prior := []byte(`{"mcpServers":{"manual":{"command":"/opt/Conveyor's tools/conveyor","args":["mcp","stdio","--server","https://one.example","--credentials-file","/config space/conveyor/credentials.json"]}},"_conveyor_mcp_install":{"manual":{"owner":"v2","endpoint":"https://two.example/mcp"}}}`)
			for _, id := range []mcpIdentity{testMCPModeIdentity(t, mode, "https://one.example", ""), testMCPModeIdentity(t, mode, "https://one.example", "manual")} {
				if _, _, _, err := reconcileNamedJSON(prior, "antigravity", id, false); err == nil {
					t.Fatal("a marker for another server granted ownership")
				}
			}
			if _, _, _, err := reconcileNamedJSON(prior, "antigravity", testMCPModeIdentity(t, mode, "https://two.example", "manual"), true); err == nil {
				t.Fatal("the marked server repointed the bridge entry")
			}
			id := testMCPModeIdentity(t, mode, "https://one.example", "manual")
			next, status, _, err := reconcileNamedJSON(prior, "antigravity", id, true)
			if id.oauth() {
				// An unowned entry needs the exact current bridge for adoption.
				// OAuth does not resolve the current executable, so it refuses,
				// and --auth pat adopts it (component-cli-onboarding invariant 9).
				if err == nil {
					t.Fatal("OAuth adopted an unowned bridge entry")
				}
				return
			}
			if err != nil || status != "refreshed" {
				t.Fatalf("adoption status=%s err=%v", status, err)
			}
			checkAntigravityModeEntry(t, testAntigravityEntry(t, next, "manual"), id)
			if !bytes.Contains(next, []byte(`"manual":{"owner":"v2","endpoint":"https://one.example/mcp"}`)) {
				t.Fatalf("adoption did not bind the marker to this endpoint:\n%s", next)
			}
		})
	}
}

// A manual entry can hold one literal address and one CONVEYOR_ADDR
// reference. Its literal address makes it a manual registration for the
// server, so an install without --adopt must refuse instead of adding a
// duplicate (component-cli-onboarding invariants 8 and 9; AC-4.3).
func TestMCPAntigravityMixedLiteralAndEnvironmentAddress(t *testing.T) {
	// Both auth modes (req-mcp-oauth AC-5.1 to AC-5.3).
	for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
		t.Run(mode, func(t *testing.T) {
			orders := map[string]string{
				"url literal, serverUrl reference": `{"theme" : "dark","mcpServers":{"manual":{"url":"https://one.example/mcp","serverUrl":"${env:CONVEYOR_ADDR}","disabled":false}}}`,
				"serverUrl literal, url reference": `{"theme" : "dark","mcpServers":{"manual":{"url":"${env:CONVEYOR_ADDR}","serverUrl":"https://one.example/mcp","disabled":false}}}`,
			}
			for label, prior := range orders {
				t.Run(label, func(t *testing.T) {
					setup := func(t *testing.T) (string, string) {
						t.Helper()
						home := t.TempDir()
						path := mcpTargets(home, []skillTool{{name: "antigravity"}})[0].path
						if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte(prior), 0o640); err != nil {
							t.Fatal(err)
						}
						return home, path
					}
					unchanged := func(t *testing.T, path string) {
						t.Helper()
						after, _ := os.ReadFile(path)
						info, _ := os.Stat(path)
						if string(after) != prior || info.Mode().Perm() != 0o640 {
							t.Fatalf("refusal changed bytes or mode: %o %s", info.Mode().Perm(), after)
						}
						if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".conveyor-*")); len(leftovers) != 0 {
							t.Fatalf("staging leftovers: %v", leftovers)
						}
					}
					unnamed := testMCPModeIdentity(t, mode, "https://one.example", "")
					named := testMCPModeIdentity(t, mode, "https://one.example", "manual")
					agy := antigravityListBinary(t, "", unnamed.name, named.name)
					// Without --adopt the install refuses, whatever CONVEYOR_ADDR names.
					for _, address := range []string{"", "https://one.example/mcp", "https://other.example/mcp"} {
						t.Setenv(mcpAddressEnv, address)
						for _, id := range []mcpIdentity{unnamed, named} {
							home, path := setup(t)
							_, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "antigravity", path: path}, id, false, true, onlyAgyOnPath(agy))
							if err == nil {
								t.Fatalf("CONVEYOR_ADDR=%q name=%s: install without --adopt accepted the entry", address, id.name)
							}
							if !id.explicit && !strings.Contains(err.Error(), "server already has manual registration manual; use --name manual --adopt") {
								t.Fatalf("unnamed install did not name the manual entry: %v", err)
							}
							unchanged(t, path)
						}
					}
					// --name manual --adopt adopts the entry when CONVEYOR_ADDR names
					// the selected server, as invariant 8 binds the reference.
					t.Setenv(mcpAddressEnv, "https://one.example/mcp")
					home, path := setup(t)
					result, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "antigravity", path: path}, named, true, true, onlyAgyOnPath(agy))
					if err != nil || result.status != "refreshed" || result.name != "manual" {
						t.Fatalf("adoption result=%+v err=%v", result, err)
					}
					data, _ := os.ReadFile(path)
					entry := testAntigravityEntry(t, data, "manual")
					checkAntigravityModeEntry(t, entry, named)
					if entry["disabled"] != false || len(testMCPEntries(t, "antigravity", data)) != 1 || !bytes.Contains(data, []byte(`"theme" : "dark"`)) {
						t.Fatalf("adoption changed other fields or added an entry:\n%s", data)
					}
					if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
						t.Fatalf("adopted file mode %o", info.Mode().Perm())
					}
					t.Setenv(mcpAddressEnv, "")
					again, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "antigravity", path: path}, unnamed, false, true, onlyAgyOnPath(agy))
					if after, _ := os.ReadFile(path); err != nil || again.status != "unchanged" || !bytes.Equal(after, data) {
						t.Fatalf("adopted entry is not reused: result=%+v err=%v", again, err)
					}
					// The reference must name the selected server. Adoption refuses
					// when it names another server or nothing.
					for _, address := range []string{"https://other.example/mcp", ""} {
						t.Setenv(mcpAddressEnv, address)
						home, path := setup(t)
						if _, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: "antigravity", path: path}, named, true, true, onlyAgyOnPath(agy)); err == nil {
							t.Fatalf("CONVEYOR_ADDR=%q: adoption repointed the reference", address)
						}
						unchanged(t, path)
					}
				})
			}
		})
	}
}

// mcpAuthTools lists the five clients that every auth-mode table covers.
var mcpAuthTools = []string{"codex", "claude", "cursor", "opencode", "antigravity"}

func testMCPOAuthIdentity(t *testing.T, server, name string) mcpIdentity {
	t.Helper()
	id, err := newMCPOAuthIdentity(server, name)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// testMCPModeIdentity returns the identity of one auth mode. The PAT identity
// uses the paths of testMCPIdentity. The OAuth identity names no path.
func testMCPModeIdentity(t *testing.T, mode, server, name string) mcpIdentity {
	t.Helper()
	if mode == mcpAuthOAuth {
		return testMCPOAuthIdentity(t, server, name)
	}
	return testMCPIdentity(t, server, name)
}

// mcpModeFields is the exact set of transport and credential fields that a
// mode writes for one client (component-cli-onboarding invariant 6;
// req-mcp-oauth AC-5.1, AC-5.2). OpenCode also holds its ownership marker.
func mcpModeFields(tool string, id mcpIdentity) map[string]any {
	marker := map[string]any{"owner": "v2", "endpoint": id.endpoint}
	if id.oauth() {
		switch tool {
		case "codex", "cursor":
			return map[string]any{"url": id.endpoint}
		case "claude":
			return map[string]any{"type": "http", "url": id.endpoint}
		case "opencode":
			return map[string]any{"type": "remote", "url": id.endpoint, claudeOwnerKey: marker}
		case "antigravity":
			return map[string]any{"serverUrl": id.endpoint}
		}
	}
	switch tool {
	case "codex":
		return map[string]any{"url": id.endpoint, "http_headers_helper": id.helper()}
	case "claude":
		return map[string]any{"type": "http", "url": id.endpoint, "headersHelper": id.helper()}
	case "cursor":
		return map[string]any{"url": id.endpoint, "headers": map[string]any{"Authorization": "Bearer ${env:" + id.tokenEnv + "}"}}
	case "opencode":
		return map[string]any{"type": "remote", "url": id.endpoint, "oauth": false, "headers": map[string]any{"Authorization": "Bearer {env:" + id.tokenEnv + "}"}, claudeOwnerKey: marker}
	case "antigravity":
		return map[string]any{"command": id.binary, "args": []any{"mcp", "stdio", "--server", id.server, "--credentials-file", id.credentials}}
	}
	return nil
}

// mcpNormalized round-trips a value through JSON so that TOML and JSON
// numbers and maps compare equal.
func mcpNormalized(t *testing.T, value any) any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// checkMCPModeEntry holds an entry to exactly the mode's fields plus the
// unrelated fields given. The OAuth entry names no credential artifact.
func checkMCPModeEntry(t *testing.T, tool string, data []byte, name string, id mcpIdentity, unrelated map[string]any) {
	t.Helper()
	entry, ok := testMCPEntries(t, tool, data)[name].(map[string]any)
	if !ok {
		t.Fatalf("%s: missing entry %s:\n%s", tool, name, data)
	}
	want := mcpModeFields(tool, id)
	for key, value := range unrelated {
		if key == "headers" {
			headers, _ := want["headers"].(map[string]any)
			merged := map[string]any{}
			for h, v := range value.(map[string]any) {
				merged[h] = v
			}
			for h, v := range headers {
				merged[h] = v
			}
			want["headers"] = merged
			continue
		}
		want[key] = value
	}
	if got, expected := mcpNormalized(t, entry), mcpNormalized(t, want); !reflect.DeepEqual(got, expected) {
		t.Fatalf("%s %s entry:\n got %v\nwant %v", tool, id.auth, got, expected)
	}
	if id.oauth() {
		entryJSON, _ := json.Marshal(entry)
		for _, forbidden := range []string{"Bearer", "CONVEYOR_MCP_TOKEN_", mcpTokenEnv, "auth token", "Helper", "helper", `"oauth"`, `"command"`, `"args"`, `"env"`, "mcp stdio"} {
			if bytes.Contains(entryJSON, []byte(forbidden)) {
				t.Fatalf("%s OAuth entry carries %q: %s", tool, forbidden, entryJSON)
			}
		}
	}
	if tool != "codex" && tool != "opencode" {
		var root struct {
			Owners map[string]mcpOwnedEndpoint `json:"_conveyor_mcp_install"`
		}
		if err := json.Unmarshal(data, &root); err != nil {
			t.Fatal(err)
		}
		if root.Owners[name] != (mcpOwnedEndpoint{Owner: "v2", Endpoint: id.endpoint}) {
			t.Fatalf("%s ownership marker: %v", tool, root.Owners)
		}
	}
	if tool == "codex" && !bytes.Contains(data, []byte(codexOwnerMarker+"\n[mcp_servers."+name+"]")) {
		t.Fatalf("codex ownership marker missing:\n%s", data)
	}
}

// TestMCPAuthModeCreateShapesAllClients covers req-mcp-oauth AC-5.1 and
// AC-5.2 for all five clients in both modes: the exact created entry, and a
// byte-identical no-op rerun.
func TestMCPAuthModeCreateShapesAllClients(t *testing.T) {
	for _, tool := range mcpAuthTools {
		for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
			t.Run(tool+"/"+mode, func(t *testing.T) {
				id := testMCPModeIdentity(t, mode, "https://one.example/base", "")
				data, status, name, err := testMCPReconcile(tool, nil, id, false)
				if err != nil || status != "created" || name != id.name {
					t.Fatalf("create status=%s name=%s err=%v", status, name, err)
				}
				checkMCPModeEntry(t, tool, data, name, id, nil)
				if len(testMCPEntries(t, tool, data)) != 1 {
					t.Fatalf("created more than one entry:\n%s", data)
				}
				again, status, _, err := testMCPReconcile(tool, data, id, false)
				if err != nil || status != "unchanged" || !bytes.Equal(again, data) {
					t.Fatalf("rerun status=%s err=%v", status, err)
				}
				if mode == mcpAuthOAuth && bytes.Contains(data, []byte(id.tokenEnv)) {
					t.Fatal("OAuth registration names the token variable")
				}
			})
		}
	}
}

// mcpSwitchFixture is a manual entry for https://one.example/mcp named
// manual, with unrelated settings in the entry, another entry, and another
// root key. unrelated lists the entry settings that every mode keeps.
func mcpSwitchFixture(tool string) (prior string, unrelated map[string]any, others []string) {
	switch tool {
	case "codex":
		return "model = 'x'\n[mcp_servers.manual]\nurl = 'https://one.example/mcp'\nstartup_timeout_sec = 30\nhttp_headers = { X-Other = 'v' }\n[mcp_servers.manual.tools.create_task]\napproval_mode = 'prompt'\n[mcp_servers.other]\nurl = 'https://other.example/mcp'\n",
			map[string]any{"startup_timeout_sec": 30, "http_headers": map[string]any{"X-Other": "v"}, "tools": map[string]any{"create_task": map[string]any{"approval_mode": "prompt"}}},
			[]string{"model = 'x'\n", "[mcp_servers.other]\nurl = 'https://other.example/mcp'\n"}
	case "claude", "cursor":
		return `{"theme" : "dark","mcpServers":{"manual":{"url":"https://one.example/mcp","timeout":30,"headers":{"X-Other":"v"}},"other":{"url":"https://other.example/mcp"}}}`,
			map[string]any{"timeout": 30, "headers": map[string]any{"X-Other": "v"}},
			[]string{`"theme" : "dark"`, `"other":{"url":"https://other.example/mcp"}`}
	case "opencode":
		return `{"model" : "x","mcp":{"manual":{"type":"remote","url":"https://one.example/mcp","enabled":false,"timeout":30,"headers":{"X-Other":"v"}},"other":{"type":"local","command":["other"]}}}`,
			map[string]any{"enabled": false, "timeout": 30, "headers": map[string]any{"X-Other": "v"}},
			[]string{`"model" : "x"`, `"other":{"type":"local","command":["other"]}`}
	case "antigravity":
		return `{"theme" : "dark","mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","disabled":true},"other":{"serverUrl":"https://other.example/mcp"}}}`,
			map[string]any{"disabled": true},
			[]string{`"theme" : "dark"`, `"other":{"serverUrl":"https://other.example/mcp"}`}
	}
	return "", nil, nil
}

// TestMCPAuthModeSwitchAllClients covers req-mcp-oauth AC-5.3 for all five
// clients in both directions. Explicit adoption of a manual entry makes it
// owned. Each later switch needs no --adopt, keeps the name, the marker, the
// unrelated entry settings, the other entry, and the other root key, and
// removes the prior mode's owned fields. Each mode's rerun is a no-op.
func TestMCPAuthModeSwitchAllClients(t *testing.T) {
	for _, tool := range mcpAuthTools {
		for _, first := range []string{mcpAuthOAuth, mcpAuthPAT} {
			t.Run(tool+"/adopt-"+first, func(t *testing.T) {
				prior, unrelated, others := mcpSwitchFixture(tool)
				if tool == "antigravity" {
					unrelated = map[string]any{"disabled": true}
				}
				second := mcpAuthPAT
				if first == mcpAuthPAT {
					second = mcpAuthOAuth
				}
				data := []byte(prior)
				states := map[string][]byte{}
				for step, mode := range []string{first, second, first, second} {
					id := testMCPModeIdentity(t, mode, "https://one.example", "")
					if step == 0 {
						id = testMCPModeIdentity(t, mode, "https://one.example", "manual")
					}
					next, status, name, err := testMCPReconcile(tool, data, id, step == 0)
					if err != nil || status != "refreshed" || name != "manual" {
						t.Fatalf("step %d %s: status=%s name=%s err=%v", step, mode, status, name, err)
					}
					checkMCPModeEntry(t, tool, next, name, id, unrelated)
					if len(testMCPEntries(t, tool, next)) != 2 {
						t.Fatalf("step %d: lost or added an entry:\n%s", step, next)
					}
					for _, other := range others {
						if !bytes.Contains(next, []byte(other)) {
							t.Fatalf("step %d: changed unrelated bytes %q:\n%s", step, other, next)
						}
					}
					again, status, _, err := testMCPReconcile(tool, next, id, false)
					if err != nil || status != "unchanged" || !bytes.Equal(again, next) {
						t.Fatalf("step %d %s rerun status=%s err=%v", step, mode, status, err)
					}
					if previous, ok := states[mode]; ok && tool != "codex" && !bytes.Equal(previous, next) {
						t.Fatalf("step %d: %s state is not stable:\n%s\n%s", step, mode, previous, next)
					}
					states[mode] = next
					data = next
				}
			})
		}
	}
}

// mcpOwnedEntryWithPaths returns a PAT registration that an earlier install
// wrote with the given executable and credential file paths.
func mcpOwnedEntryWithPaths(t *testing.T, tool, binary, credentials string) ([]byte, mcpIdentity) {
	t.Helper()
	old, err := newMCPIdentity("https://one.example", "manual", binary, credentials)
	if err != nil {
		t.Fatal(err)
	}
	data, _, _, err := testMCPReconcile(tool, nil, old, false)
	if err != nil {
		t.Fatal(err)
	}
	return data, old
}

// TestMCPAuthModeMovedPathsAllClients covers an owned PAT registration whose
// executable or credential file moved, in both modes and for all five
// clients. The OAuth switch removes the old helper or bridge without
// resolving any path. The PAT rerun writes the current paths. Cursor and
// OpenCode name no path, so their row checks that the rerun keeps the entry.
// An unowned entry with the old paths stays refused under --adopt, and a
// marker never makes another executable a Conveyor executable.
func TestMCPAuthModeMovedPathsAllClients(t *testing.T) {
	moves := map[string][2]string{
		"executable moved":      {"/old/bin/conveyor", "/config space/conveyor/credentials.json"},
		"credential file moved": {"/opt/Conveyor's tools/conveyor", "/old config/conveyor/credentials.json"},
		"both moved":            {"/old/bin/conveyor", "/old config/conveyor/credentials.json"},
	}
	for _, tool := range mcpAuthTools {
		for label, paths := range moves {
			for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
				t.Run(tool+"/"+label+"/"+mode, func(t *testing.T) {
					data, _ := mcpOwnedEntryWithPaths(t, tool, paths[0], paths[1])
					id := testMCPModeIdentity(t, mode, "https://one.example", "")
					next, status, name, err := testMCPReconcile(tool, data, id, false)
					if err != nil || name != "manual" {
						t.Fatalf("owned moved entry: status=%s name=%s err=%v", status, name, err)
					}
					wantStatus := "refreshed"
					if mode == mcpAuthPAT && (tool == "cursor" || tool == "opencode") {
						wantStatus = "unchanged"
					}
					if status != wantStatus {
						t.Fatalf("status=%s, want %s", status, wantStatus)
					}
					checkMCPModeEntry(t, tool, next, name, id, nil)
					for _, old := range []string{"/old/bin", "/old config"} {
						if bytes.Contains(next, []byte(old)) {
							t.Fatalf("old path %q remains:\n%s", old, next)
						}
					}
				})
			}
		}
		t.Run(tool+"/unowned moved entry", func(t *testing.T) {
			if tool == "cursor" || tool == "opencode" {
				t.Skip("the registration names no executable or credential path")
			}
			data, _ := mcpOwnedEntryWithPaths(t, tool, "/old/bin/conveyor", "/old config/conveyor/credentials.json")
			// Remove the ownership marker, so the entry is a manual entry.
			if tool == "codex" {
				data = bytes.Replace(data, []byte(codexOwnerMarker+"\n"), nil, 1)
			} else {
				var root map[string]json.RawMessage
				_ = json.Unmarshal(data, &root)
				delete(root, claudeOwnerKey)
				data, _ = json.Marshal(root)
			}
			for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
				for _, adopt := range []bool{false, true} {
					id := testMCPModeIdentity(t, mode, "https://one.example", "manual")
					if _, _, _, err := testMCPReconcile(tool, data, id, adopt); err == nil {
						t.Fatalf("%s adopt=%t accepted an unowned entry with other paths", mode, adopt)
					}
				}
			}
		})
		t.Run(tool+"/marker with another executable", func(t *testing.T) {
			if tool == "cursor" || tool == "opencode" {
				t.Skip("the registration names no executable")
			}
			data, _ := mcpOwnedEntryWithPaths(t, tool, "/usr/bin/other", "/config space/conveyor/credentials.json")
			for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
				for _, adopt := range []bool{false, true} {
					id := testMCPModeIdentity(t, mode, "https://one.example", "manual")
					if _, _, _, err := testMCPReconcile(tool, data, id, adopt); err == nil {
						t.Fatalf("%s adopt=%t accepted an owned entry that runs another executable", mode, adopt)
					}
				}
			}
		})
	}
}

// mcpRefusalCases lists, for one client, entries that every mode refuses
// before any write. Each names https://one.example/mcp as manual, with the
// installer's ownership marker when owned is true.
func mcpRefusalCases(tool string) map[string]struct {
	entry string
	owned bool
} {
	type c = struct {
		entry string
		owned bool
	}
	helper := func(binary, server string) string {
		return shellQuoteMCP(binary) + " auth token --server " + shellQuoteMCP(server) + " --credentials-file " + shellQuoteMCP("/config space/conveyor/credentials.json") + " --format http-headers"
	}
	otherToken := "CONVEYOR_MCP_TOKEN_" + strings.ToUpper(fmt.Sprintf("%x", sha256.Sum256([]byte("https://two.example"))))
	if tool == "codex" {
		return map[string]c{
			"foreign Authorization":         {"url = 'https://one.example/mcp'\nhttp_headers = { Authorization = 'Bearer fixture-secret' }\n", true},
			"foreign env Authorization":     {"url = 'https://one.example/mcp'\nenv_http_headers = { Authorization = 'OTHER' }\n", true},
			"foreign bearer variable":       {"url = 'https://one.example/mcp'\nbearer_token_env_var = 'OTHER'\n", true},
			"foreign helper":                {"url = 'https://one.example/mcp'\nhttp_headers_helper = 'print-token fixture-secret'\n", true},
			"helper of another executable":  {"url = 'https://one.example/mcp'\nhttp_headers_helper = " + strconvQuote(helper("/usr/bin/other", "https://one.example")) + "\n", true},
			"helper of another server":      {"url = 'https://one.example/mcp'\nhttp_headers_helper = " + strconvQuote(helper("/opt/bin/conveyor", "https://two.example")) + "\n", true},
			"relative helper executable":    {"url = 'https://one.example/mcp'\nhttp_headers_helper = " + strconvQuote(helper("conveyor", "https://one.example")) + "\n", true},
			"helper with extra shell text":  {"url = 'https://one.example/mcp'\nhttp_headers_helper = " + strconvQuote(helper("/opt/bin/conveyor", "https://one.example")+"; true") + "\n", true},
			"stdio command":                 {"url = 'https://one.example/mcp'\ncommand = 'other'\n", true},
			"environment":                   {"url = 'https://one.example/mcp'\nenv = { A = 'b' }\n", true},
			"name of another endpoint":      {"url = 'https://two.example/mcp'\n", true},
			"unowned foreign Authorization": {"url = 'https://one.example/mcp'\nhttp_headers = { Authorization = 'Bearer fixture-secret' }\n", false},
		}
	}
	key := "mcpServers"
	if tool == "opencode" {
		key = "mcp"
	}
	_ = key
	address := `"url":"https://one.example/mcp"`
	if tool == "antigravity" {
		address = `"serverUrl":"https://one.example/mcp"`
	}
	cases := map[string]c{
		"foreign Authorization":            {address + `,"headers":{"Authorization":"Bearer fixture-secret"}`, true},
		"lowercase foreign Authorization":  {address + `,"headers":{"authorization":"Bearer ${env:FIXTURE}"}`, true},
		"token variable of another server": {address + `,"headers":{"Authorization":"Bearer ${env:` + otherToken + `}"}`, true},
		"foreign bearer variable":          {address + `,"bearer_token_env_var":"OTHER"`, true},
		"foreign helper":                   {address + `,"headersHelper":"print-token fixture-secret"`, true},
		"helper of another executable":     {address + `,"headersHelper":` + strconvQuote(helper("/usr/bin/other", "https://one.example")), true},
		"helper of another server":         {address + `,"headersHelper":` + strconvQuote(helper("/opt/bin/conveyor", "https://two.example")), true},
		"environment":                      {address + `,"env":{"A":"b"}`, true},
		"foreign OAuth configuration":      {address + `,"oauth":{"clientId":"custom"}`, true},
		"name of another endpoint":         {strings.Replace(address, "one.example", "two.example", 1), true},
		"unowned foreign Authorization":    {address + `,"headers":{"Authorization":"Bearer fixture-secret"}`, false},
	}
	if tool == "antigravity" {
		cases["non-bridge command"] = c{`"command":"/usr/bin/other","args":["mcp","stdio","--server","https://one.example","--credentials-file","/config space/conveyor/credentials.json"]`, true}
		cases["bridge for another server"] = c{address + `,"command":"/opt/bin/conveyor","args":["mcp","stdio","--server","https://two.example","--credentials-file","/config space/conveyor/credentials.json"]`, true}
		cases["bridge with a relative credential file"] = c{`"command":"/opt/bin/conveyor","args":["mcp","stdio","--server","https://one.example","--credentials-file","credentials.json"]`, true}
		cases["bridge with an extra argument"] = c{`"command":"/opt/bin/conveyor","args":["mcp","stdio","--server","https://one.example","--credentials-file","/config space/conveyor/credentials.json","--verbose"]`, true}
		cases["unowned moved bridge"] = c{`"command":"/old/bin/conveyor","args":["mcp","stdio","--server","https://one.example","--credentials-file","/config space/conveyor/credentials.json"]`, false}
	} else {
		cases["stdio command"] = c{address + `,"command":"other"`, true}
	}
	return cases
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestMCPAuthModeRefusalsAllClients covers the refusals of
// component-cli-onboarding invariant 9 for all five clients, in both modes,
// with and without --adopt. Each refusal keeps the file's bytes and mode,
// leaves no staged file, and never prints a credential.
func TestMCPAuthModeRefusalsAllClients(t *testing.T) {
	for _, tool := range mcpAuthTools {
		for label, c := range mcpRefusalCases(tool) {
			t.Run(tool+"/"+label, func(t *testing.T) {
				var prior string
				endpoint := "https://one.example/mcp"
				if strings.Contains(c.entry, "two.example/mcp") {
					endpoint = "https://two.example/mcp"
				}
				switch {
				case tool == "codex" && c.owned:
					prior = codexOwnerMarker + "\n[mcp_servers.manual]\n" + c.entry
				case tool == "codex":
					prior = "[mcp_servers.manual]\n" + c.entry
				case tool == "opencode" && c.owned:
					prior = `{"mcp":{"manual":{` + c.entry + `,"_conveyor_mcp_install":{"owner":"v2","endpoint":"` + endpoint + `"}}}}`
				case tool == "opencode":
					prior = `{"mcp":{"manual":{` + c.entry + `}}}`
				case c.owned:
					prior = `{"mcpServers":{"manual":{` + c.entry + `}},"_conveyor_mcp_install":{"manual":{"owner":"v2","endpoint":"` + endpoint + `"}}}`
				default:
					prior = `{"mcpServers":{"manual":{` + c.entry + `}}}`
				}
				for _, mode := range []string{mcpAuthOAuth, mcpAuthPAT} {
					for _, adopt := range []bool{false, true} {
						home := t.TempDir()
						path := mcpTargets(home, []skillTool{{name: tool}})[0].path
						if tool == "opencode" {
							path = filepath.Join(home, "opencode", "opencode.json")
						}
						if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte(prior), 0o640); err != nil {
							t.Fatal(err)
						}
						id := testMCPModeIdentity(t, mode, "https://one.example", "manual")
						lookup := func(name string) (string, error) {
							t.Fatalf("refusal looked up parser %s", name)
							return "", nil
						}
						_, err := reconcileNamedMCPRegistration(home, mcpInstallTarget{tool: tool, path: path}, id, adopt, true, lookup)
						if err == nil {
							t.Fatalf("%s adopt=%t accepted:\n%s", mode, adopt, prior)
						}
						if strings.Contains(err.Error(), "fixture-secret") {
							t.Fatalf("error leaks a credential: %v", err)
						}
						after, _ := os.ReadFile(path)
						info, _ := os.Stat(path)
						if string(after) != prior || info.Mode().Perm() != 0o640 {
							t.Fatalf("%s adopt=%t refusal changed bytes or mode: %o %s", mode, adopt, info.Mode().Perm(), after)
						}
						if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".conveyor-*")); len(leftovers) != 0 {
							t.Fatalf("staging leftovers: %v", leftovers)
						}
					}
				}
			})
		}
	}
}

// TestMCPHelperParserAcceptsOnlyTheInstallerForm checks the helper reader
// that recognizes a moved Conveyor helper on an owned entry.
func TestMCPHelperParserAcceptsOnlyTheInstallerForm(t *testing.T) {
	id := testMCPIdentity(t, "https://one.example/base", "")
	binary, server, credentials, ok := parseMCPHelper(id.helper())
	if !ok || binary != id.binary || server != id.server || credentials != id.credentials {
		t.Fatalf("parse %q: %q %q %q %t", id.helper(), binary, server, credentials, ok)
	}
	for _, value := range []string{
		"",
		"conveyor auth token --server https://one.example --credentials-file /c --format http-headers",
		id.helper() + " ",
		id.helper() + "; rm -rf /",
		strings.Replace(id.helper(), " --format http-headers", " --format raw", 1),
		strings.Replace(id.helper(), "auth token", "auth  token", 1),
		"'/bin/conveyor' auth token --server 'https://one.example' --credentials-file '/c'\"x\" --format http-headers",
		"'/bin/conveyor auth token --server 'https://one.example' --credentials-file '/c' --format http-headers",
	} {
		if _, _, _, ok := parseMCPHelper(value); ok {
			t.Fatalf("accepted %q", value)
		}
	}
}
