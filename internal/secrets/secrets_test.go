package secrets

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	ref, err := ParseRef("secretref://acme/default/DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Workspace != "acme" || ref.Set != "default" || ref.Name != "DATABASE_URL" {
		t.Fatalf("got %+v", ref)
	}
	if got := ref.String(); got != "secretref://acme/default/DATABASE_URL" {
		t.Fatalf("round-trip: %s", got)
	}

	for _, bad := range []string{
		"acme/default/DATABASE_URL",     // no scheme
		"secretref://acme/DATABASE_URL", // missing set
		"secretref:///default/X",        // empty workspace
		"secretref://acme/default/",     // empty name
		"secretref://../default/X",      // traversal workspace
		"secretref://acme/../X",         // traversal set
		"secretref://acme/default/a b",  // separator-adjacent chars
		"secretref://acme/de\\fault/X",  // backslash
	} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q): want error", bad)
		}
	}
}

func TestLocalFileResolverSetPlain(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	resolver := &LocalFileResolver{Root: root, Backend: BackendPlain}
	ref := Ref{Workspace: "acme", Set: "default", Name: "API_KEY"}
	if err := resolver.Set(context.Background(), ref, "value=with-equals"); err != nil {
		t.Fatal(err)
	}
	got, err := resolver.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != "value=with-equals" {
		t.Fatalf("value = %q", got)
	}
	info, err := os.Stat(filepath.Join(root, "acme", "default.env"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestLocalFileResolverSOPSCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("shell-backed fake sops")
	}
	root := t.TempDir()
	fake := filepath.Join(t.TempDir(), "sops")
	script := `#!/bin/sh
if [ "$1" = "decrypt" ]; then
  for arg in "$@"; do last="$arg"; done
  sed 's/^FAKE-SOPS://' "$last"
  printf '%s\n' 'TOKEN=decrypt-stderr-corruption' >&2
else
  printf 'FAKE-SOPS:'
  cat
  printf '%s\n' 'ENCRYPT_WARNING=stderr-corruption' >&2
fi
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	resolver := &LocalFileResolver{Root: root, Backend: BackendSOPS, SOPSBinary: fake}
	ref := Ref{Workspace: "acme", Set: "default", Name: "TOKEN"}
	if err := resolver.Set(context.Background(), ref, "canary"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "acme", "default.sops.env")
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(stored), "FAKE-SOPS:") {
		t.Fatalf("stored data did not pass through sops: %q", stored)
	}
	if strings.Contains(string(stored), "stderr-corruption") {
		t.Fatalf("sops stderr corrupted encrypted payload: %q", stored)
	}
	got, err := resolver.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != "canary" {
		t.Fatalf("value = %q", got)
	}
}

func TestLocalFileResolverSOPSErrorUsesStderrDiagnostic(t *testing.T) {
	if testing.Short() {
		t.Skip("shell-backed fake sops")
	}
	fake := filepath.Join(t.TempDir(), "sops")
	script := "#!/bin/sh\nprintf 'partial-payload-should-not-be-reported'\nprintf 'safe sops diagnostic' >&2\nexit 7\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	resolver := &LocalFileResolver{Root: root, Backend: BackendSOPS, SOPSBinary: fake}
	err := resolver.Set(context.Background(), Ref{Workspace: "acme", Set: "default", Name: "TOKEN"}, "canary")
	if err == nil || !strings.Contains(err.Error(), "safe sops diagnostic") {
		t.Fatalf("Set error = %v, want stderr diagnostic", err)
	}
	if strings.Contains(err.Error(), "partial-payload") {
		t.Fatalf("Set error leaked stdout payload: %v", err)
	}
	dir := filepath.Join(root, "acme")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "default.sops.env"), []byte("encrypted fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve(context.Background(), Ref{Workspace: "acme", Set: "default", Name: "TOKEN"})
	if err == nil || !strings.Contains(err.Error(), "safe sops diagnostic") {
		t.Fatalf("Resolve error = %v, want stderr diagnostic", err)
	}
	if strings.Contains(err.Error(), "partial-payload") {
		t.Fatalf("Resolve error leaked stdout payload: %v", err)
	}
}

func TestLocalFileResolverRealSOPS(t *testing.T) {
	sopsBinary, sopsErr := exec.LookPath("sops")
	ageKeygen, ageErr := exec.LookPath("age-keygen")
	if sopsErr != nil || ageErr != nil {
		t.Skip("sops and age-keygen are required")
	}
	root := t.TempDir()
	identity := filepath.Join(root, "age-key.txt")
	if out, err := exec.Command(ageKeygen, "-o", identity).CombinedOutput(); err != nil {
		t.Fatalf("age-keygen: %v: %s", err, out)
	}
	recipientBytes, err := exec.Command(ageKeygen, "-y", identity).Output()
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, ".sops.yaml")
	config := "creation_rules:\n  - path_regex: .*\\.sops\\.env$\n    age: " + strings.TrimSpace(string(recipientBytes)) + "\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", identity)
	resolver := &LocalFileResolver{Root: root, Backend: BackendSOPS, SOPSBinary: sopsBinary, SOPSConfig: configPath}
	ref := Ref{Workspace: "acme", Set: "integration", Name: "CANARY"}
	value := " leading = real-sops-canary # value "
	if err := resolver.Set(context.Background(), ref, value); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(root, "acme", "integration.sops.env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), value) {
		t.Fatal("SOPS file contains plaintext")
	}
	got, err := resolver.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != value {
		t.Fatalf("decrypted value = %q", got)
	}
}

func TestValidEnvName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"A", "API_KEY", "_PRIVATE", "A1"} {
		if !ValidEnvName(name) {
			t.Errorf("ValidEnvName(%q) = false", name)
		}
	}
	for _, name := range []string{"", "1KEY", "API-KEY", "A.B"} {
		if ValidEnvName(name) {
			t.Errorf("ValidEnvName(%q) = true", name)
		}
	}
}

func TestLocalFileResolver(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "acme")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# comment\nDATABASE_URL=postgres://localhost/dev\nAPI_KEY=abc123\n"
	if err := os.WriteFile(filepath.Join(dir, "default.env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &LocalFileResolver{Root: root, Backend: BackendPlain}
	got, err := r.Resolve(context.Background(), Ref{Workspace: "acme", Set: "default", Name: "DATABASE_URL"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "postgres://localhost/dev" {
		t.Fatalf("got %q", got)
	}

	if _, err := r.Resolve(context.Background(), Ref{Workspace: "acme", Set: "default", Name: "MISSING"}); err == nil {
		t.Error("want error for missing secret")
	}
}
