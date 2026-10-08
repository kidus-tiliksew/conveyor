package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds for the client-local verification_toolchains section
// (component-verification-runner).
const (
	MaxVerificationToolchains           = 32
	MaxVerificationToolchainSearchPaths = 32
	MaxVerificationToolchainPathBytes   = 4096
)

type toolchainSettingKind int

const (
	toolchainDirectory toolchainSettingKind = iota
	toolchainCacheDirectory
	toolchainPathList
	toolchainFileOrOff
)

// verificationToolchainSettings is the closed tool-setting schema. It has no
// generic passthrough, so factory, forge, session, provider credential,
// shell-startup and loader-injection variables cannot be configured.
var verificationToolchainSettings = map[string]toolchainSettingKind{
	"GOPATH":           toolchainPathList,
	"GOROOT":           toolchainDirectory,
	"GOENV":            toolchainFileOrOff,
	"GOCACHE":          toolchainCacheDirectory,
	"GOMODCACHE":       toolchainCacheDirectory,
	"XDG_CONFIG_HOME":  toolchainDirectory,
	"XDG_CACHE_HOME":   toolchainCacheDirectory,
	"npm_config_cache": toolchainCacheDirectory,
}

// VerificationToolchain is an operator-owned, client-local toolchain profile
// for verification children. Like KitPermissionGrant it never enters
// WorkspaceDocument, frozen task policy, harness routing or server
// configuration writes (req-verification-kits REQ-3/AC-3.1, REQ-7/AC-7.1).
type VerificationToolchain struct {
	Server      string            `yaml:"server" json:"server"`
	Workspace   string            `yaml:"workspace" json:"workspace"`
	Repository  string            `yaml:"repository" json:"repository"`
	SearchPaths []string          `yaml:"search_paths" json:"search_paths"`
	Home        string            `yaml:"home,omitempty" json:"home,omitempty"`
	Settings    map[string]string `yaml:"settings,omitempty" json:"settings,omitempty"`
}

// VerificationToolchainSettingKeys returns the closed setting keys in order.
func VerificationToolchainSettingKeys() []string {
	keys := make([]string, 0, len(verificationToolchainSettings))
	for key := range verificationToolchainSettings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// VerificationToolchainCacheSetting reports whether a setting names a cache
// location. Preflight checks caches only when they already exist.
func VerificationToolchainCacheSetting(key string) bool {
	kind, ok := verificationToolchainSettings[key]
	return ok && (kind == toolchainCacheDirectory || kind == toolchainPathList)
}

func (t VerificationToolchain) Validate() error {
	if err := validateLocalScope("verification_toolchains", t.Server, t.Workspace, t.Repository); err != nil {
		return err
	}
	if len(t.SearchPaths) == 0 || len(t.SearchPaths) > MaxVerificationToolchainSearchPaths {
		return fmt.Errorf("verification_toolchains: search_paths requires 1 to %d entries", MaxVerificationToolchainSearchPaths)
	}
	for i, path := range t.SearchPaths {
		if err := validateToolchainPath(path); err != nil {
			return fmt.Errorf("verification_toolchains: search_paths[%d]: %w", i, err)
		}
	}
	if t.Home != "" {
		if err := validateToolchainPath(t.Home); err != nil {
			return fmt.Errorf("verification_toolchains: home: %w", err)
		}
	}
	for key, value := range t.Settings {
		kind, ok := verificationToolchainSettings[key]
		if !ok {
			return fmt.Errorf("verification_toolchains: unknown setting %q (allowed: %s)", key, strings.Join(VerificationToolchainSettingKeys(), ", "))
		}
		var err error
		switch kind {
		case toolchainPathList:
			err = validateToolchainPathList(value)
		case toolchainFileOrOff:
			if value != "off" {
				err = validateToolchainPath(value)
			}
		default:
			err = validateToolchainPath(value)
		}
		if err != nil {
			return fmt.Errorf("verification_toolchains: settings.%s: %w", key, err)
		}
	}
	return nil
}

// validateLocalScope applies the kit_permissions scope rules: an explicit
// server URL, workspace and repository.
func validateLocalScope(section, server, workspace, repository string) error {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s: explicit server URL required", section)
	}
	if workspace == "" || repository == "" {
		return fmt.Errorf("%s: workspace and repository required", section)
	}
	return nil
}

func localScopeKey(server, workspace, repository string) string {
	server = strings.TrimRight(server, "/")
	if u, err := url.Parse(server); err == nil {
		u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
		server = u.String()
	}
	return server + "\x00" + workspace + "\x00" + repository
}

func validateToolchainPathList(value string) error {
	elements := strings.Split(value, string(os.PathListSeparator))
	if len(elements) > MaxVerificationToolchainSearchPaths {
		return fmt.Errorf("path list exceeds %d entries", MaxVerificationToolchainSearchPaths)
	}
	for _, element := range elements {
		if element == "" {
			return fmt.Errorf("empty path-list element")
		}
		if err := validateToolchainPath(element); err != nil {
			return err
		}
	}
	return nil
}

// validateToolchainPath accepts one literal absolute native path. Values are
// never expanded, so shell variables and tildes are refused rather than
// interpreted.
func validateToolchainPath(path string) error {
	if path == "" {
		return fmt.Errorf("empty path")
	}
	if len(path) > MaxVerificationToolchainPathBytes {
		return fmt.Errorf("path exceeds %d bytes", MaxVerificationToolchainPathBytes)
	}
	if !utf8.ValidString(path) {
		return fmt.Errorf("path must be valid UTF-8")
	}
	for _, r := range path {
		if r == 0 || unicode.IsControl(r) {
			return fmt.Errorf("path contains a control character")
		}
	}
	if strings.Contains(path, "$") {
		return fmt.Errorf("path contains an unexpanded variable")
	}
	if strings.HasPrefix(path, "~") {
		return fmt.Errorf("path contains an unexpanded tilde")
	}
	if strings.ContainsRune(path, os.PathListSeparator) {
		return fmt.Errorf("path contains the path-list separator %q", os.PathListSeparator)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path must be absolute")
	}
	return nil
}

func validateVerificationToolchains(records []VerificationToolchain) error {
	if len(records) > MaxVerificationToolchains {
		return fmt.Errorf("verification_toolchains: at most %d records", MaxVerificationToolchains)
	}
	seen := map[string]bool{}
	for _, record := range records {
		if err := record.Validate(); err != nil {
			return err
		}
		key := localScopeKey(record.Server, record.Workspace, record.Repository)
		if seen[key] {
			return fmt.Errorf("verification_toolchains: duplicate scope for server %s workspace %s repository %s", record.Server, record.Workspace, record.Repository)
		}
		seen[key] = true
	}
	return nil
}

// VerificationToolchainFor selects the one record scoped to server, workspace
// and repository. A nil result selects the minimal default environment.
func (c *Config) VerificationToolchainFor(server, workspace, repository string) (*VerificationToolchain, error) {
	if err := validateVerificationToolchains(c.VerificationToolchains); err != nil {
		return nil, err
	}
	want := localScopeKey(server, workspace, repository)
	var selected *VerificationToolchain
	for i := range c.VerificationToolchains {
		record := c.VerificationToolchains[i]
		if localScopeKey(record.Server, record.Workspace, record.Repository) != want {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("verification_toolchains: ambiguous scope for server %s workspace %s repository %s", server, workspace, repository)
		}
		copied := record
		copied.SearchPaths = append([]string(nil), record.SearchPaths...)
		copied.Settings = map[string]string{}
		for key, value := range record.Settings {
			copied.Settings[key] = value
		}
		selected = &copied
	}
	return selected, nil
}
