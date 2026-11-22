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
	for _, tool := range []string{"codex", "claude", "cursor", "opencode", "antigravity"} {
		t.Run(tool, func(t *testing.T) {
			first := testMCPIdentity(t, "https://one.example/base:part", "first-conveyor")
			second := testMCPIdentity(t, "https://two.example:443/other", "")
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
			inferred := testMCPIdentity(t, first.server, "")
			next, status, name, err := testMCPReconcile(tool, data, inferred, false)
			if err != nil || name != first.name || status != "unchanged" || !bytes.Equal(data, next) {
				t.Fatalf("custom name reuse %s %v", name, err)
			}
			renamed := testMCPIdentity(t, first.server, "renamed-conveyor")
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
			conflict := testMCPIdentity(t, second.server, renamed.name)
			if _, _, _, err = testMCPReconcile(tool, next, conflict, true); err == nil {
				t.Fatal("repointed named registration")
			}
			if bytes.Contains(data, []byte(mcpTokenEnv)) {
				t.Fatal("shared token in personal registration")
			}
		})
	}
}

func TestMCPMigrationPreservesNestedPolicies(t *testing.T) {
	fixtures := map[string]string{
		"codex":    "model = 'test'\n" + codexOwnerMarker + "\n[mcp_servers.conveyor]\nurl = 'https://one.example/mcp'\nbearer_token_env_var = 'CONVEYOR_API_TOKEN'\nenabled = false\nstartup_timeout_sec = 30\n[mcp_servers.other]\nurl='https://other.example/mcp'\n[mcp_servers.conveyor.tools.create_task]\napproval_mode='prompt'\noutput_token_limit=300\n",
		"claude":   `{"theme" : "dark","_conveyor_mcp_install":"owner=v1","mcpServers":{"conveyor":{"type":"http","url":"https://one.example/mcp","headers":{"Authorization":"Bearer ${CONVEYOR_API_TOKEN}","X-Other":"value"},"tools":{"create_task":{"approval_mode":"prompt"}},"timeout":30},"manual":{"url":"https://manual.example/mcp"}}}`,
		"cursor":   `{"_conveyor_mcp_install":"owner=v1","mcpServers":{"conveyor":{"url":"${env:CONVEYOR_ADDR}","headers":{"Authorization":"Bearer ${env:CONVEYOR_API_TOKEN}"},"tools":{"create_task":{"approval_mode":"prompt"}}}}}`,
		"opencode": `{"mcp":{"conveyor":{"_conveyor_mcp_install":"owner=v1","type":"remote","url":"{env:CONVEYOR_ADDR}","headers":{"Authorization":"Bearer {env:CONVEYOR_API_TOKEN}"},"enabled":false,"timeout":30,"tools":{"create_task":{"approval_mode":"prompt"}}}}}`,
	}
	for tool, fixture := range fixtures {
		t.Run(tool, func(t *testing.T) {
			id := testMCPIdentity(t, "https://one.example", "")
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
}

func TestMCPRefusesManualAuthAndAdoptionRepoint(t *testing.T) {
	for _, tool := range []string{"codex", "claude", "cursor", "opencode", "antigravity"} {
		t.Run(tool, func(t *testing.T) {
			id := testMCPIdentity(t, "https://one.example", "manual")
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
			other := testMCPIdentity(t, "https://two.example", "manual")
			if _, _, _, err := testMCPReconcile(tool, prior, other, true); err == nil {
				t.Fatal("adopt repointed manual server")
			}
		})
	}
}

func TestMCPSharedWorkerAttachmentNeedsExplicitAdoption(t *testing.T) {
	for _, tool := range []string{"cursor", "opencode"} {
		t.Run(tool, func(t *testing.T) {
			key, endpoint := "mcpServers", "${env:CONVEYOR_ADDR}"
			if tool == "opencode" {
				key, endpoint = "mcp", "{env:CONVEYOR_ADDR}"
			}
			fixture := []byte(fmt.Sprintf(`{"_conveyor_mcp_install":"owner=v1","%s":{"conveyor":{"_conveyor_mcp_install":"owner=v1","url":%q}}}`, key, endpoint))
			id := testMCPIdentity(t, "https://one.example", "")
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
}

func TestMCPCommandTwoCredentialsAndList(t *testing.T) {
	for _, tool := range []string{"codex", "claude", "cursor", "opencode", "antigravity"} {
		t.Run(tool, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
			t.Setenv(mcpAddressEnv, "https://unrelated.example")
			t.Setenv(mcpTokenEnv, "ambient-secret")
			setMCPTestConfig(t, localAuthConfig{Servers: map[string]localServerConfig{"https://one.example": {Token: "first-secret"}, "https://two.example": {Token: "second-secret"}}})
			parser := openCodeValidationBinary(t, `echo '{"http_headers_helper":"fixture"}'`)
			if tool == "antigravity" {
				parser = antigravityListBinary(t, "", testMCPIdentity(t, "https://one.example", "").name, testMCPIdentity(t, "https://two.example", "").name)
			}
			lookup := func(string) (string, error) { return parser, nil }
			t.Cleanup(func() { serverFlag = ""; serverFlagExplicit = false })
			targets := mcpTargets(home, []skillTool{{name: tool}})
			path := targets[0].path
			for _, server := range []string{"https://one.example", "https://two.example", "https://one.example", "https://two.example"} {
				serverFlag, serverFlagExplicit = server, true
				cmd := mcpInstallCmdWithLookPath(lookup)
				var output bytes.Buffer
				cmd.SetOut(&output)
				cmd.SetErr(&output)
				cmd.SetArgs([]string{"--tool", tool, "--list"})
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
				cmd.SetArgs([]string{"--tool", tool})
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
		})
	}
}

func TestMCPReinstallPreservesOtherCodexOwnership(t *testing.T) {
	first := testMCPIdentity(t, "https://one.example", "one")
	second := testMCPIdentity(t, "https://two.example", "two")
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
}

func TestMCPRefusesAmbiguousOrUnsupportedConfigBeforeWrite(t *testing.T) {
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
			id := testMCPIdentity(t, "https://one.example", "one")
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
}

func TestMCPListDoesNotExecuteParserAndInstallSecuresExistingFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	id := testMCPIdentity(t, "https://one.example", "")
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
}

func TestMCPCodexMultilineTableTextCannotChangeUnrelatedValues(t *testing.T) {
	prior := []byte("note = '''\n" + codexOwnerMarker + "\n[mcp_servers.one]\ntext inside a multiline string\n'''\n[mcp_servers.one]\nurl='https://one.example/mcp'\n")
	id := testMCPIdentity(t, "https://one.example", "one")
	if _, _, _, err := reconcileNamedCodex(prior, id, true); err == nil {
		t.Fatal("accepted unsafe multiline table layout")
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
		if err := validateMCPParser(binary, "codex", path, id.name); err == nil {
			t.Fatal("accepted ignored helper field")
		}
	}
}

func TestMCPManualMatchingEndpointRequiresExplicitAdoption(t *testing.T) {
	for _, tool := range []string{"codex", "claude", "cursor", "opencode", "antigravity"} {
		t.Run(tool, func(t *testing.T) {
			id := testMCPIdentity(t, "https://one.example", "")
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
			id = testMCPIdentity(t, id.server, "manual")
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
	id := testMCPIdentity(t, "https://one.example", "")
	prior := []byte(`{"theme" : "dark","mcpServers":{"other":{"serverUrl":"https://other.example/mcp"}}}`)
	data, _, _, err := reconcileNamedJSON(prior, "antigravity", id, false)
	if err != nil {
		t.Fatal(err)
	}
	// agy writes "disabled" on its own entries; the installer keeps it.
	data = bytes.Replace(data, []byte(`{"args"`), []byte(`{"disabled":true,"args"`), 1)
	renamed := testMCPIdentity(t, id.server, "renamed-conveyor")
	next, status, name, err := reconcileNamedJSON(data, "antigravity", renamed, false)
	if err != nil || status != "refreshed" || name != "renamed-conveyor" {
		t.Fatalf("rename status=%s name=%s err=%v", status, name, err)
	}
	entries := testMCPEntries(t, "antigravity", next)
	if len(entries) != 2 || entries[id.name] != nil {
		t.Fatalf("rename duplicated or lost entries: %v", entries)
	}
	entry := testAntigravityEntry(t, next, name)
	checkAntigravityBridgeEntry(t, entry, renamed)
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
	again, status, _, err := reconcileNamedJSON(next, "antigravity", testMCPIdentity(t, id.server, ""), false)
	if err != nil || status != "unchanged" || !bytes.Equal(again, next) {
		t.Fatalf("renamed entry is not reused: status=%s err=%v", status, err)
	}
}

func TestMCPAntigravityAdoptsAgyAddEntry(t *testing.T) {
	for _, field := range []string{"serverUrl", "url"} {
		t.Run(field, func(t *testing.T) {
			// `agy mcp add manual https://one.example/mcp` writes serverUrl
			// and disabled. A hand-written entry can use url.
			prior := []byte(fmt.Sprintf(`{"theme":"dark","mcpServers":{"manual":{"disabled":false,%q:"https://one.example/mcp","headers":{"X-Trace":"1"}},"other":{"serverUrl":"https://other.example/mcp"}}}`, field))
			for _, id := range []mcpIdentity{testMCPIdentity(t, "https://one.example", ""), testMCPIdentity(t, "https://one.example", "manual")} {
				_, _, _, err := reconcileNamedJSON(prior, "antigravity", id, false)
				if err == nil || !strings.Contains(err.Error(), "--name manual --adopt") {
					t.Fatalf("unowned entry accepted without adoption: %v", err)
				}
			}
			if _, _, _, err := reconcileNamedJSON(prior, "antigravity", testMCPIdentity(t, "https://one.example", ""), true); err == nil {
				t.Fatal("--adopt without --name adopted an arbitrary name")
			}
			id := testMCPIdentity(t, "https://one.example", "manual")
			next, status, name, err := reconcileNamedJSON(prior, "antigravity", id, true)
			if err != nil || status != "refreshed" || name != "manual" {
				t.Fatalf("adoption status=%s name=%s err=%v", status, name, err)
			}
			entry := testAntigravityEntry(t, next, "manual")
			checkAntigravityBridgeEntry(t, entry, id)
			if entry["disabled"] != false || len(entry) != 3 {
				t.Fatalf("adoption changed unrelated entry fields: %v", entry)
			}
			if !bytes.Contains(next, []byte(`"other":{"serverUrl":"https://other.example/mcp"}`)) || !bytes.Contains(next, []byte(`"theme":"dark"`)) {
				t.Fatalf("adoption changed other bytes:\n%s", next)
			}
			again, status, _, err := reconcileNamedJSON(next, "antigravity", testMCPIdentity(t, id.server, ""), false)
			if err != nil || status != "unchanged" || !bytes.Equal(again, next) {
				t.Fatalf("adopted entry is not owned: status=%s err=%v", status, err)
			}
		})
	}
}

func TestMCPAntigravityAdoptionReplacesLegacyTokenReference(t *testing.T) {
	prior := []byte(`{"mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","headers":{"Authorization":"Bearer ${CONVEYOR_API_TOKEN}"}}}}`)
	id := testMCPIdentity(t, "https://one.example", "manual")
	if _, _, _, err := reconcileNamedJSON(prior, "antigravity", id, false); err == nil {
		t.Fatal("legacy reference replaced without adoption")
	}
	next, _, _, err := reconcileNamedJSON(prior, "antigravity", id, true)
	if err != nil {
		t.Fatal(err)
	}
	checkAntigravityBridgeEntry(t, testAntigravityEntry(t, next, "manual"), id)
	if bytes.Contains(next, []byte(mcpTokenEnv)) {
		t.Fatal("legacy token reference remains")
	}
}

func TestMCPAntigravityEnvironmentAddressBindsOnlyUnderAdoption(t *testing.T) {
	prior := []byte(`{"mcpServers":{"conveyor":{"serverUrl":"${env:CONVEYOR_ADDR}"}}}`)
	id := testMCPIdentity(t, "https://one.example", "")
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
	checkAntigravityBridgeEntry(t, testAntigravityEntry(t, adopted, id.name), id)
	t.Setenv(mcpAddressEnv, "https://other.example/mcp")
	named := testMCPIdentity(t, id.server, "conveyor")
	if _, _, _, err := reconcileNamedJSON(prior, "antigravity", named, true); err == nil {
		t.Fatal("adopted an environment entry for another server")
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
		"other credentials file":          `{"mcpServers":{"manual":{"command":"/opt/Conveyor's tools/conveyor","args":["mcp","stdio","--server","https://one.example","--credentials-file","/elsewhere/credentials.json"]}},` + marker + `}`,
		"extra bridge argument":           `{"mcpServers":{"manual":{"command":"/opt/Conveyor's tools/conveyor","args":["mcp","stdio","--server","https://one.example","--credentials-file","/config space/conveyor/credentials.json","--verbose"]}}}`,
		"args without command":            `{"mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","args":["x"]}}}`,
		"command on remote entry":         `{"mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","command":"/usr/bin/other"}}}`,
		"env on owned bridge":             `{"mcpServers":{"manual":{` + bridge + `,"env":{"CONVEYOR_API_TOKEN":"fixture-secret"}}},` + marker + `}`,
		"empty env on owned bridge":       `{"mcpServers":{"manual":{` + bridge + `,"env":{}}},` + marker + `}`,
		"second endpoint":                 `{"mcpServers":{"manual":{"url":"https://one.example/mcp","serverUrl":"https://two.example/mcp"}}}`,
		"bridge for another server":       `{"mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","command":"/opt/Conveyor's tools/conveyor","args":["mcp","stdio","--server","https://two.example","--credentials-file","/config space/conveyor/credentials.json"]}}}`,
		"name for another server":         `{"mcpServers":{"manual":{"serverUrl":"https://two.example/mcp"}}}`,
		"marker without an address":       `{"mcpServers":{"manual":{"disabled":false}},` + marker + `}`,
		"bearer token variable":           `{"mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","bearer_token_env_var":"OTHER"}}}`,
		"invalid headers":                 `{"mcpServers":{"manual":{"serverUrl":"https://one.example/mcp","headers":"Authorization: Bearer fixture-secret"}}}`,
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
	// The root marker names another server, so the bridge entry for this
	// server is unowned. Only explicit adoption takes it over.
	prior := []byte(`{"mcpServers":{"manual":{"command":"/opt/Conveyor's tools/conveyor","args":["mcp","stdio","--server","https://one.example","--credentials-file","/config space/conveyor/credentials.json"]}},"_conveyor_mcp_install":{"manual":{"owner":"v2","endpoint":"https://two.example/mcp"}}}`)
	for _, id := range []mcpIdentity{testMCPIdentity(t, "https://one.example", ""), testMCPIdentity(t, "https://one.example", "manual")} {
		if _, _, _, err := reconcileNamedJSON(prior, "antigravity", id, false); err == nil {
			t.Fatal("a marker for another server granted ownership")
		}
	}
	if _, _, _, err := reconcileNamedJSON(prior, "antigravity", testMCPIdentity(t, "https://two.example", "manual"), true); err == nil {
		t.Fatal("the marked server repointed the bridge entry")
	}
	id := testMCPIdentity(t, "https://one.example", "manual")
	next, status, _, err := reconcileNamedJSON(prior, "antigravity", id, true)
	if err != nil || status != "refreshed" {
		t.Fatalf("adoption status=%s err=%v", status, err)
	}
	checkAntigravityBridgeEntry(t, testAntigravityEntry(t, next, "manual"), id)
	if !bytes.Contains(next, []byte(`"manual":{"owner":"v2","endpoint":"https://one.example/mcp"}`)) {
		t.Fatalf("adoption did not bind the marker to this endpoint:\n%s", next)
	}
}
