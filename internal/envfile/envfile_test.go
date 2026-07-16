package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	data := []byte("# local values\nCONVEYOR_ENVFILE_PLAIN=value\nexport CONVEYOR_ENVFILE_SINGLE='literal value'\nCONVEYOR_ENVFILE_DOUBLE=\"line\\nvalue\"\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CONVEYOR_ENVFILE_PLAIN", "CONVEYOR_ENVFILE_SINGLE", "CONVEYOR_ENVFILE_DOUBLE"} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Unsetenv(name) })
	}
	if err := Load(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("CONVEYOR_ENVFILE_PLAIN"); got != "value" {
		t.Fatalf("plain value = %q", got)
	}
	if got := os.Getenv("CONVEYOR_ENVFILE_SINGLE"); got != "literal value" {
		t.Fatalf("single-quoted value = %q", got)
	}
	if got := os.Getenv("CONVEYOR_ENVFILE_DOUBLE"); got != "line\nvalue" {
		t.Fatalf("double-quoted value = %q", got)
	}
}

func TestLoadPreservesExistingEnvironment(t *testing.T) {
	t.Setenv("CONVEYOR_ENVFILE_PRECEDENCE", "shell")
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("CONVEYOR_ENVFILE_PRECEDENCE=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Load(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("CONVEYOR_ENVFILE_PRECEDENCE"); got != "shell" {
		t.Fatalf("precedence value = %q", got)
	}
}

func TestLoadRejectsMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("not an assignment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Load(path); err == nil {
		t.Fatal("expected malformed line error")
	}
}
