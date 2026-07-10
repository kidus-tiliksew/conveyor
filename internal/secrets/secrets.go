// Package secrets implements the reference model from spec §10.1: the
// control plane stores only references (secretref://workspace/set/NAME);
// the runner resolves them at sandbox boot. Plaintext values never
// appear in the control-plane database, job payloads, or queue messages.
package secrets

import (
	"context"
	"fmt"
	"strings"
)

const refScheme = "secretref://"

// Ref is a parsed secret reference: secretref://<workspace>/<set>/<name>.
type Ref struct {
	Workspace string
	Set       string
	Name      string
}

func (r Ref) String() string {
	return refScheme + r.Workspace + "/" + r.Set + "/" + r.Name
}

func ParseRef(s string) (Ref, error) {
	raw, ok := strings.CutPrefix(s, refScheme)
	if !ok {
		return Ref{}, fmt.Errorf("secret ref %q: missing %s prefix", s, refScheme)
	}
	parts := strings.Split(raw, "/")
	if len(parts) != 3 {
		return Ref{}, fmt.Errorf("secret ref %q: want secretref://<workspace>/<set>/<name>", s)
	}
	for _, p := range parts {
		if !validSegment(p) {
			return Ref{}, fmt.Errorf("secret ref %q: invalid segment %q", s, p)
		}
	}
	return Ref{Workspace: parts[0], Set: parts[1], Name: parts[2]}, nil
}

// validSegment rejects anything that could act as a path component
// ("..", ".", separators) — refs are resolved against filesystem and
// backend paths, so segments must never traverse (spec §10.1).
func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// Resolver is the pluggable secret backend (Vault, GCP SM, AWS SM, SOPS
// files). Phase 1 ships the SOPS/local-file backend — the zero-infra
// default from spec §17.2.
type Resolver interface {
	Resolve(ctx context.Context, ref Ref) (string, error)
}

// SetPolicy carries per-set delivery policy; sets with LocalEligible ==
// false are delivered only to cloud runners regardless of which runner
// claims the job (spec §10.1, §8.5).
type SetPolicy struct {
	LocalEligible bool
}
