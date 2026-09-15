package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// component-harness-execution v6, Native MCP registrations and stored credentials;
// req-cli-authentication REQ-4/AC-4.2 and AC-4.3.
type mcpIdentity struct {
	server, endpoint, name, tokenEnv, bridge string
	explicit                                 bool
}

var mcpNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var mcpSlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

func newMCPIdentity(server, name, binary, credentials string) (mcpIdentity, error) {
	canonical, err := normalizeServerURL(server)
	if err != nil {
		return mcpIdentity{}, err
	}
	u, _ := url.Parse(canonical)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))
	slug := strings.Trim(mcpSlugPattern.ReplaceAllString(u.Hostname(), "-"), "-")
	if len(slug) > 30 {
		slug = slug[:30]
	}
	if slug == "" {
		slug = "server"
	}
	explicit := name != ""
	if explicit {
		name = strings.TrimSpace(name)
		for _, c := range name {
			if c > 127 {
				return mcpIdentity{}, errors.New("name must contain only ASCII letters, digits, underscores or hyphens")
			}
		}
		name = strings.ToLower(name)
		if !mcpNamePattern.MatchString(name) {
			return mcpIdentity{}, errors.New("name must contain 1-63 ASCII letters, digits, underscores or hyphens, starting with a letter or digit")
		}
	} else {
		name = slug + "-" + hash[:12] + "-conveyor"
	}
	if !filepath.IsAbs(binary) || !filepath.IsAbs(credentials) {
		return mcpIdentity{}, errors.New("MCP credential bridge requires absolute executable and credential file paths")
	}
	bridge := shellQuoteMCP(binary) + " auth token --server " + shellQuoteMCP(canonical) + " --credentials-file " + shellQuoteMCP(credentials)
	return mcpIdentity{server: canonical, endpoint: canonical + "/mcp", name: name, tokenEnv: "CONVEYOR_MCP_TOKEN_" + strings.ToUpper(hash), bridge: bridge, explicit: explicit}, nil
}

func shellQuoteMCP(s string) string   { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func (id mcpIdentity) helper() string { return id.bridge + " --format http-headers" }

type mcpOwnedEndpoint struct {
	Owner    string `json:"owner"`
	Endpoint string `json:"endpoint"`
}
type mcpEntry struct {
	fields           map[string]any
	owned, legacyEnv bool
}

// A shared environment attachment may belong to a worker. Only explicit
// adoption and a matching bound environment endpoint permit its migration.
func mcpEntryEndpoint(entry mcpEntry, adopt bool) string {
	endpoint, _ := entry.fields["url"].(string)
	if entry.legacyEnv {
		if !adopt {
			return ""
		}
		endpoint = os.Getenv(mcpAddressEnv)
	}
	canonical, err := normalizeServerURL(endpoint)
	if err != nil {
		return ""
	}
	return canonical + "/mcp"
}

func selectMCPEntry(entries map[string]mcpEntry, id mcpIdentity, adopt bool) (name, source string, err error) {
	matches := []string{}
	for key, entry := range entries {
		if entry.owned && mcpEntryEndpoint(entry, adopt) == id.endpoint {
			matches = append(matches, key)
		}
	}
	sort.Strings(matches)
	name = id.name
	if !id.explicit && len(matches) == 1 && matches[0] != "conveyor" {
		name = matches[0]
	}
	if len(matches) > 1 && !id.explicit {
		return "", "", errors.New("multiple owned registrations match this server; remove the duplicate explicitly before reinstalling")
	}
	if entry, ok := entries[name]; ok {
		if mcpEntryEndpoint(entry, adopt) != id.endpoint {
			return "", "", fmt.Errorf("registration %s has a different or unresolved endpoint; refusing to repoint it", name)
		}
		if !entry.owned && !adopt {
			return "", "", fmt.Errorf("registration %s is not installer-owned; use --name %s --adopt to adopt the matching endpoint", name, name)
		}
		if len(matches) == 1 && matches[0] != name {
			return "", "", errors.New("destination conflicts with an existing registration for this server")
		}
		return name, name, nil
	}
	if len(matches) == 1 {
		return name, matches[0], nil
	}
	if len(matches) > 1 {
		return "", "", errors.New("multiple owned matches; --name must address an existing registration")
	}
	// --adopt addresses an explicit name, or the legacy conveyor entry only.
	if adopt && !id.explicit {
		if entry, ok := entries["conveyor"]; ok && mcpEntryEndpoint(entry, true) == id.endpoint {
			return name, "conveyor", nil
		}
	}
	for key, entry := range entries {
		if !entry.owned && !entry.legacyEnv && mcpEntryEndpoint(entry, false) == id.endpoint {
			return "", "", fmt.Errorf("server already has manual registration %s; use --name %s --adopt instead of creating a duplicate", key, key)
		}
	}
	return name, "", nil
}

func updateMCPFields(entry mcpEntry, tool string, id mcpIdentity, adopt bool) (map[string]any, error) {
	f := entry.fields
	if f == nil {
		f = map[string]any{}
	}
	legacy := entry.owned || adopt
	for _, key := range []string{"command", "args", "env", "env_vars", "oauth", "auth"} {
		if value, ok := f[key]; ok && !(tool == "opencode" && key == "oauth" && value == false) {
			return nil, fmt.Errorf("conflicting %s configuration; remove it explicitly before installing", key)
		}
	}
	for _, key := range []string{"headers", "http_headers", "env_http_headers"} {
		if value, ok := f[key]; ok {
			headers, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid %s object", key)
			}
			for h, v := range headers {
				if !strings.EqualFold(h, "Authorization") {
					continue
				}
				expected := "Bearer ${env:" + id.tokenEnv + "}"
				if tool == "opencode" {
					expected = "Bearer {env:" + id.tokenEnv + "}"
				}
				old := legacy && (v == "Bearer ${CONVEYOR_API_TOKEN}" || v == "Bearer ${env:CONVEYOR_API_TOKEN}" || v == "Bearer {env:CONVEYOR_API_TOKEN}")
				if !old && !((tool == "cursor" || tool == "opencode") && key == "headers" && v == expected) {
					return nil, errors.New("conflicting Authorization source; remove it explicitly before installing")
				}
				delete(headers, h)
			}
			if len(headers) == 0 {
				delete(f, key)
			}
		}
	}
	if v, ok := f["bearer_token_env_var"]; ok {
		if !(legacy && v == mcpTokenEnv) {
			return nil, errors.New("conflicting bearer token source; remove it explicitly before installing")
		}
		delete(f, "bearer_token_env_var")
	}
	for _, key := range []string{"headersHelper", "http_headers_helper"} {
		if v, ok := f[key]; ok && v != id.helper() {
			return nil, errors.New("conflicting credential helper; remove it explicitly before installing")
		}
	}
	f["url"] = id.endpoint
	switch tool {
	case "codex":
		f["http_headers_helper"] = id.helper()
	case "claude":
		f["type"] = "http"
		f["headersHelper"] = id.helper()
	case "cursor", "opencode":
		headers, _ := f["headers"].(map[string]any)
		if headers == nil {
			headers = map[string]any{}
		}
		headers["Authorization"] = "Bearer ${env:" + id.tokenEnv + "}"
		if tool == "opencode" {
			headers["Authorization"] = "Bearer {env:" + id.tokenEnv + "}"
			f["type"] = "remote"
			f["oauth"] = false
		}
		f["headers"] = headers
	}
	return f, nil
}

func validateJSONObjects(data []byte) error {
	if !json.Valid(data) {
		return errors.New("invalid JSON (comments cannot be preserved)")
	}
	var walk func(json.RawMessage) error
	walk = func(b json.RawMessage) error {
		b = bytes.TrimSpace(b)
		if len(b) == 0 {
			return errors.New("empty JSON")
		}
		if b[0] == '{' {
			obj, err := scanJSONObject(b)
			if err != nil {
				return err
			}
			for _, m := range obj.members {
				if err := walk(b[m.valueStart:m.valueEnd]); err != nil {
					return err
				}
			}
		} else if b[0] == '[' {
			var a []json.RawMessage
			_ = json.Unmarshal(b, &a)
			for _, v := range a {
				if err := walk(v); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(data)
}

func reconcileNamedJSON(prior []byte, tool string, id mcpIdentity, adopt bool) ([]byte, string, string, error) {
	if len(bytes.TrimSpace(prior)) == 0 {
		prior = []byte("{}\n")
	}
	if err := validateJSONObjects(prior); err != nil {
		return nil, "", "", err
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(prior, &root)
	key := "mcpServers"
	if tool == "opencode" {
		key = "mcp"
	}
	rawServers := root[key]
	if rawServers == nil {
		rawServers = []byte("{}")
	}
	var rawEntries map[string]json.RawMessage
	if err := json.Unmarshal(rawServers, &rawEntries); err != nil || rawEntries == nil {
		return nil, "", "", errors.New("MCP servers must be an object")
	}
	var owners map[string]mcpOwnedEndpoint
	_ = json.Unmarshal(root[claudeOwnerKey], &owners)
	if owners == nil {
		owners = map[string]mcpOwnedEndpoint{}
	}
	var legacyOwner string
	_ = json.Unmarshal(root[claudeOwnerKey], &legacyOwner)
	entries := map[string]mcpEntry{}
	for name, raw := range rawEntries {
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return nil, "", "", errors.New("MCP entry must be an object")
		}
		endpoint, _ := fields["url"].(string)
		owner := owners[name]
		owned := owner.Owner == "v2" && owner.Endpoint == endpoint
		if tool == "opencode" {
			var marker mcpOwnedEndpoint
			b, _ := json.Marshal(fields[claudeOwnerKey])
			_ = json.Unmarshal(b, &marker)
			owned = (marker.Owner == "v2" && marker.Endpoint == endpoint) || fields[claudeOwnerKey] == claudeOwnerValue
		} else if name == "conveyor" && legacyOwner == claudeOwnerValue {
			owned = true
		}
		entries[name] = mcpEntry{fields: fields, owned: owned, legacyEnv: endpoint == "${env:CONVEYOR_ADDR}" || endpoint == "{env:CONVEYOR_ADDR}"}
	}
	name, source, err := selectMCPEntry(entries, id, adopt)
	if err != nil {
		return nil, "", "", err
	}
	fields, err := updateMCPFields(entries[source], tool, id, adopt)
	if err != nil {
		return nil, "", "", err
	}
	marker := mcpOwnedEndpoint{Owner: "v2", Endpoint: id.endpoint}
	if tool == "opencode" {
		fields[claudeOwnerKey] = marker
	} else {
		owners[name] = marker
		if source != name {
			delete(owners, source)
		}
	}
	encoded, _ := json.Marshal(fields)
	nextServers := rawServers
	if source != "" && source != name {
		nextServers, err = renameJSONObjectMember(nextServers, source, name)
		if err != nil {
			return nil, "", "", err
		}
	}
	nextServers, err = setJSONObjectMember(nextServers, name, encoded)
	if err != nil {
		return nil, "", "", err
	}
	next, err := setJSONObjectMember(prior, key, nextServers)
	if err != nil {
		return nil, "", "", err
	}
	if tool != "opencode" {
		// Preserve ownership of the remaining legacy entry when adding another server.
		if legacyOwner == claudeOwnerValue && source != "conveyor" {
			if entry, ok := entries["conveyor"]; ok {
				endpoint, _ := entry.fields["url"].(string)
				owners["conveyor"] = mcpOwnedEndpoint{Owner: "v2", Endpoint: endpoint}
			}
		}
		b, _ := json.Marshal(owners)
		next, err = setJSONObjectMember(next, claudeOwnerKey, b)
	}
	status := "created"
	if source != "" {
		status = "refreshed"
	}
	if jsonEquivalent(prior, next) {
		return prior, "unchanged", name, nil
	}
	return next, status, name, err
}

func renameJSONObjectMember(data []byte, old, new string) ([]byte, error) {
	object, err := scanJSONObject(data)
	if err != nil {
		return nil, err
	}
	member, ok := object.member(old)
	if !ok {
		return nil, errors.New("missing migration source")
	}
	// Locate the key immediately before this value, keeping other members verbatim.
	for i := 0; i < member.valueStart; i++ {
		if data[i] != '"' {
			continue
		}
		end, e := scanJSONString(data, i)
		if e != nil {
			return nil, e
		}
		var key string
		_ = json.Unmarshal(data[i:end], &key)
		colon := skipJSONSpace(data, end)
		if key == old && colon < len(data) && data[colon] == ':' && skipJSONSpace(data, colon+1) == member.valueStart {
			b, _ := json.Marshal(new)
			return replaceBytes(data, i, end, b), nil
		}
		i = end - 1
	}
	return nil, errors.New("unsupported migration syntax")
}

// Only conventional server tables are edited. TOML decoding validates all
// syntax and duplicates first; inline/dotted alternatives fail closed.
var codexTablePattern = regexp.MustCompile(`^\[mcp_servers\.([a-zA-Z0-9_-]+)(\.[^\]]+)?\]\s*(?:#.*)?$`)

type codexBlock struct {
	start, end  int
	name        string
	base, owned bool
}

func codexBlocks(prior []byte) []codexBlock {
	var blocks []codexBlock
	offset := 0
	previous := ""
	previousStart := 0
	for _, line := range bytes.SplitAfter(prior, []byte("\n")) {
		trimmed := strings.TrimSpace(string(line))
		if strings.HasPrefix(trimmed, "[") {
			if len(blocks) > 0 && blocks[len(blocks)-1].end == 0 {
				blocks[len(blocks)-1].end = offset
				if previous == codexOwnerMarker {
					blocks[len(blocks)-1].end = previousStart
				}
			}
			m := codexTablePattern.FindStringSubmatch(trimmed)
			if m != nil {
				start := offset
				owned := previous == codexOwnerMarker
				if owned {
					start = previousStart
				}
				blocks = append(blocks, codexBlock{start: start, name: m[1], base: m[2] == "", owned: owned})
			}
		}
		previous = trimmed
		previousStart = offset
		offset += len(line)
	}
	if len(blocks) > 0 && blocks[len(blocks)-1].end == 0 {
		blocks[len(blocks)-1].end = len(prior)
	}
	return blocks
}

func reconcileNamedCodex(prior []byte, id mcpIdentity, adopt bool) ([]byte, string, string, error) {
	root := map[string]any{}
	if err := toml.Unmarshal(prior, &root); err != nil {
		return nil, "", "", errors.New("invalid Codex TOML configuration")
	}
	entries := map[string]mcpEntry{}
	servers, _ := root["mcp_servers"].(map[string]any)
	blocks := codexBlocks(prior)
	for name, value := range servers {
		f, ok := value.(map[string]any)
		if !ok {
			return nil, "", "", errors.New("invalid Codex server entry")
		}
		entry := mcpEntry{fields: f}
		for _, b := range blocks {
			if b.name == name && b.base {
				entry.owned = b.owned
			}
		}
		entries[name] = entry
	}
	name, source, err := selectMCPEntry(entries, id, adopt)
	if err != nil {
		return nil, "", "", err
	}
	fields, err := updateMCPFields(entries[source], "codex", id, adopt)
	if err != nil {
		return nil, "", "", err
	}
	if source != "" {
		found := false
		for _, b := range blocks {
			if b.name == source && b.base {
				found = true
			}
		}
		if !found {
			return nil, "", "", errors.New("unsupported Codex inline or quoted server syntax; use a conventional [mcp_servers.name] table before adoption")
		}
	}
	encoded, err := toml.Marshal(map[string]any{"mcp_servers": map[string]any{name: fields}})
	if err != nil {
		return nil, "", "", err
	}
	// Marshal emits a parent table which would collide with preexisting tables.
	lines := strings.Split(string(encoded), "\n")
	kept := []string{}
	for _, line := range lines {
		if strings.TrimSpace(line) == "[mcp_servers]" {
			continue
		}
		if strings.TrimSpace(line) == "[mcp_servers."+name+"]" {
			kept = append(kept, codexOwnerMarker)
		}
		kept = append(kept, line)
	}
	replacement := []byte(strings.Join(kept, "\n"))
	next := append([]byte(nil), prior...)
	for i := len(blocks) - 1; i >= 0; i-- {
		b := blocks[i]
		if b.name == source {
			next = replaceBytes(next, b.start, b.end, nil)
		}
	}
	if len(next) > 0 && next[len(next)-1] != '\n' {
		next = append(next, '\n')
	}
	next = append(next, replacement...)
	var check map[string]any
	if err := toml.Unmarshal(next, &check); err != nil {
		return nil, "", "", errors.New("unsupported Codex table layout; original configuration preserved")
	}
	// Verify the entire semantic result. Text that resembles a table inside a
	// multiline value or another unsupported layout must never lose settings.
	if servers == nil {
		servers = map[string]any{}
	}
	if source != "" {
		delete(servers, source)
	}
	servers[name] = fields
	root["mcp_servers"] = servers
	if !reflect.DeepEqual(root, check) {
		return nil, "", "", errors.New("unsupported Codex table layout would change unrelated settings; original configuration preserved")
	}
	// Preserve the original bytes on an idempotent reinstall.
	if source == name {
		var original map[string]any
		_ = toml.Unmarshal(prior, &original)
		a, _ := json.Marshal(original)
		b, _ := json.Marshal(check)
		if bytes.Equal(a, b) && entries[source].owned {
			return prior, "unchanged", name, nil
		}
	}
	status := "created"
	if source != "" {
		status = "refreshed"
	}
	return next, status, name, nil
}
