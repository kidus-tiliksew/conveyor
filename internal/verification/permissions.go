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
