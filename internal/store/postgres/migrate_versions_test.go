package postgres

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

// Parallel tasks each mint the next migration number against the main they
// branched from, so two branches can claim the same version and the ledger
// only rejects the duplicate at apply time — after merge, on every daemon
// start (the 066 collision merged as PRs #274/#275). This runs without a
// database so the collision fails the offending task's own unit gate.
func TestEmbeddedMigrationVersionsAreUnique(t *testing.T) {
	files, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no embedded migrations found")
	}
	seen := map[int]string{}
	for _, name := range files {
		version, err := migrationVersion(name)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if prior, ok := seen[version]; ok {
			t.Fatalf("migration version %d is claimed by both %s and %s; renumber the newer file", version, prior, name)
		}
		seen[version] = name
	}
}

func TestMigration096RemainsBurnedAndDocumented(t *testing.T) {
	files, err := fs.Glob(migrationFiles, "migrations/096_*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("burned migration 096 was reused: %v", files)
	}
	note, err := os.ReadFile("migrations/README.md")
	if err != nil {
		t.Fatal(err)
	}
	if text := string(note); !strings.Contains(text, "`096` is permanently burned") || !strings.Contains(text, "out of order") {
		t.Fatalf("migration 096 note is missing its permanent ordering rationale: %s", text)
	}
}
