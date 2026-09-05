package store_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The deployment contract must not depend on a driver outside its backend.
// This check runs in the no-database tier (component-persistence; DEC-36).
func TestBackendImportBoundary(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate source root")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	for _, tree := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			for _, spec := range file.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if (importPath == "github.com/jackc/pgx" || strings.HasPrefix(importPath, "github.com/jackc/pgx/")) &&
					!strings.HasPrefix(rel, "internal/store/postgres/") && !strings.HasPrefix(rel, "internal/eventlog/pglog/") {
					t.Errorf("%s imports driver %s outside the backend", rel, importPath)
				}
				if importPath == "github.com/go-sql-driver/mysql" && !strings.HasPrefix(rel, "internal/store/singlestore/") && !strings.HasPrefix(rel, "internal/eventlog/s2log/") {
					t.Errorf("%s imports MySQL outside the backend", rel)
				}
				const singleStore = "github.com/kidus-tiliksew/conveyor/internal/store/singlestore"
				if (importPath == singleStore || strings.HasPrefix(importPath, singleStore+"/")) && !strings.HasPrefix(rel, "internal/store/singlestore/") && filepath.ToSlash(filepath.Dir(rel)) != "internal/store/backend" {
					t.Errorf("%s imports SingleStore outside the backend factory", rel)
				}
				const postgres = "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
				if (importPath == postgres || strings.HasPrefix(importPath, postgres+"/")) &&
					!strings.HasPrefix(rel, "internal/store/postgres/") && filepath.ToSlash(filepath.Dir(rel)) != "internal/store/backend" {
					t.Errorf("%s imports PostgreSQL outside the backend factory", rel)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
