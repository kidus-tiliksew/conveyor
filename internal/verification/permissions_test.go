package verification

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKitPermissionIntersection(t *testing.T) {
	root := t.TempDir()
	a := VerificationPermission{Kind: "filesystem_write", Binding: "repo", Target: filepath.Join(root, "output")}
	grant := VerificationPermission{Kind: a.Kind, Binding: a.Binding, Target: root}
	for _, tc := range []struct {
		name              string
		authorized, local []VerificationPermission
		want              string
	}{
		{"allowed", []VerificationPermission{grant}, []VerificationPermission{a}, ""},
		{"server denied", []VerificationPermission{}, []VerificationPermission{grant}, "work-order authorization"},
		{"local denied", []VerificationPermission{grant}, []VerificationPermission{}, "local grant"},
		{"sibling denied", []VerificationPermission{{Kind: a.Kind, Binding: a.Binding, Target: root + "-other"}}, []VerificationPermission{grant}, "filesystem_write"},
		{"binding denied", []VerificationPermission{{Kind: a.Kind, Binding: "other", Target: root}}, []VerificationPermission{grant}, "binding repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RequireVerificationPermissions([]VerificationPermission{a}, tc.authorized, tc.local)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestKitPermissionCanonicalOriginsAndHandles(t *testing.T) {
	for _, target := range []string{"https://*.example.test", "https://user:secret@example.test", "https://example.test/path", "https://example.test?token=value"} {
		if _, err := NormalizeVerificationPermissions([]VerificationPermission{{Kind: "network", Binding: "api", Target: target}}); err == nil {
			t.Fatalf("accepted %s", target)
		}
	}
	p, err := NormalizeVerificationPermissions([]VerificationPermission{{Kind: "network", Binding: "api", Target: "https://EXAMPLE.test/"}, {Kind: "network", Binding: "api", Target: "https://example.test:443"}})
	if err != nil || len(p) != 1 || p[0].Target != "https://example.test:443" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err = NormalizeVerificationPermissions([]VerificationPermission{{Kind: "credential", Binding: "api", Target: "secret=value"}}); err == nil {
		t.Fatal("accepted credential value")
	}
}

func TestKitPermissionSymlinkEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside", "escape/new/file"} {
		if _, err := ResolvePermissionPath(root, path); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	got, err := ResolvePermissionPath(root, "new/file")
	if err != nil || got != filepath.Join(root, "new/file") {
		t.Fatalf("%s %v", got, err)
	}
}
