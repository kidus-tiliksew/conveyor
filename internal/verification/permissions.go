package verification

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
)

// VerificationPermission names an action in an operator-owned environment.
// Grants are admission authority, not a sandbox (VK-4; REQ-7/AC-7.3).
type VerificationPermission struct {
	Kind    string `json:"kind" yaml:"kind"`
	Binding string `json:"binding" yaml:"binding"`
	Target  string `json:"target,omitempty" yaml:"target,omitempty"`
}

func NormalizeVerificationPermissions(input []VerificationPermission) ([]VerificationPermission, error) {
	if input == nil || len(input) > 400 {
		return nil, fmt.Errorf("permissions: explicit bounded list required")
	}
	out := make([]VerificationPermission, 0, len(input))
	seen := map[VerificationPermission]bool{}
	for _, p := range input {
		if !VerificationBindingName(p.Binding) {
			return nil, fmt.Errorf("permission %s: invalid binding", p.Kind)
		}
		switch p.Kind {
		case "filesystem_read", "filesystem_write":
			if !filepath.IsAbs(p.Target) || strings.ContainsAny(p.Target, "\x00\r\n") {
				return nil, fmt.Errorf("permission %s: absolute filesystem root required", p.Kind)
			}
			p.Target = filepath.Clean(p.Target)
		case "network":
			u, err := url.Parse(p.Target)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(u.Host, "*%\\") {
				return nil, fmt.Errorf("permission network: explicit HTTP origin required")
			}
			host := strings.ToLower(u.Hostname())
			if strings.HasSuffix(host, ".") {
				host = strings.TrimSuffix(host, ".")
			}
			port := u.Port()
			if port == "" {
				if u.Scheme == "https" {
					port = "443"
				} else {
					port = "80"
				}
			}
			if _, err := net.LookupPort("tcp", port); err != nil {
				return nil, fmt.Errorf("permission network: invalid port")
			}
			p.Target = u.Scheme + "://" + net.JoinHostPort(host, port)
		case "credential":
			if !VerificationBindingName(p.Target) {
				return nil, fmt.Errorf("permission credential: handle required")
			}
		case "operator_interaction":
			if p.Target != "" {
				return nil, fmt.Errorf("permission operator_interaction: target forbidden")
			}
		default:
			return nil, fmt.Errorf("permission %s: unknown action", p.Kind)
		}
		if !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Binding != b.Binding {
			return a.Binding < b.Binding
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Target < b.Target
	})
	return out, nil
}

func VerificationBindingName(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return s != "." && s != ".."
}

func VerificationPermissionContains(grant, action VerificationPermission) bool {
	if grant.Kind != action.Kind || grant.Binding != action.Binding {
		return false
	}
	if grant.Kind == "filesystem_read" || grant.Kind == "filesystem_write" {
		rel, err := filepath.Rel(grant.Target, action.Target)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
	}
	return grant.Target == action.Target
}

// RequireVerificationPermissions returns the canonical requested subset only
// when both independently supplied authorities cover every action.
func RequireVerificationPermissions(requested, authorized, local []VerificationPermission) ([]VerificationPermission, error) {
	r, err := NormalizeVerificationPermissions(requested)
	if err != nil {
		return nil, err
	}
	a, err := NormalizeVerificationPermissions(authorized)
	if err != nil {
		return nil, err
	}
	l, err := NormalizeVerificationPermissions(local)
	if err != nil {
		return nil, err
	}
	for _, p := range r {
		for i, grants := range [][]VerificationPermission{a, l} {
			allowed := false
			for _, g := range grants {
				if VerificationPermissionContains(g, p) {
					allowed = true
					break
				}
			}
			if !allowed {
				source := "work-order authorization"
				if i == 1 {
					source = "local grant"
				}
				return nil, fmt.Errorf("missing %s for %s binding %s target %s", source, p.Kind, p.Binding, p.Target)
			}
		}
	}
	return r, nil
}

// ValidateRequestedActions checks the closed action kinds and bindings declared
// by the frozen contract. The host resolves paths and bindings; the server
// independently checks that those actions are covered by the exact grant.
func ValidateRequestedActions(e Exercise, actions []VerificationPermission) error {
	type identity struct{ kind, binding, path string }
	required := map[identity]bool{}
	for _, p := range e.Permissions {
		required[identity{p.Kind, p.TargetBinding, p.Path}] = true
	}
	for _, p := range e.Prerequisites {
		if p.Kind == "credential" {
			required[identity{"credential", p.EnvironmentBinding, ""}] = true
		}
		if p.Kind == "operator_interaction" {
			required[identity{"operator_interaction", "", ""}] = true
		}
	}
	if e.Kind == "interactive" || e.Kind == "hybrid" {
		required[identity{"operator_interaction", "", ""}] = true
	}
	for _, input := range e.Inputs {
		if input.Sensitive {
			required[identity{"credential", input.Name, ""}] = input.Required
		}
	}
	matches := func(key identity, a VerificationPermission) bool {
		if a.Kind != key.kind || (key.binding != "" && a.Binding != key.binding) {
			return false
		}
		if key.kind == "filesystem_read" || key.kind == "filesystem_write" {
			path := filepath.Clean(key.path)
			if path != "." && !strings.HasSuffix(a.Target, string(filepath.Separator)+path) {
				return false
			}
		}
		return true
	}
	for key, needed := range required {
		if !needed {
			continue
		}
		found := false
		for _, a := range actions {
			if matches(key, a) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("missing requested action %s binding %s", key.kind, key.binding)
		}
	}
	for _, a := range actions {
		// Optional UI launch adds interactive admission to a script contract.
		found := a.Kind == "operator_interaction"
		for key := range required {
			if matches(key, a) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("undeclared requested action %s binding %s", a.Kind, a.Binding)
		}
	}
	return nil
}
