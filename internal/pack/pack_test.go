package pack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestLoadValidatesAndCachesWholePack(t *testing.T) {
	dir := t.TempDir()
	for _, subdir := range []string{"roles"} {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, stage := range pipelineStages {
		if err := os.WriteFile(filepath.Join(dir, "roles", string(stage)+".md"), []byte("role "+string(stage)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bundle, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "roles", "spec.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	role, err := bundle.Role(core.StageSpec)
	if err != nil || role != "role spec" {
		t.Fatalf("cached role = %q, err=%v", role, err)
	}
}
