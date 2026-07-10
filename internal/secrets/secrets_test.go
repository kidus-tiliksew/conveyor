package secrets

import (
	"context"
	"os"
	"path/filepath"
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

	r := &LocalFileResolver{Root: root}
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
