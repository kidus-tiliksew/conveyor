package singlestore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// DEC-38, component-persistence: ledger writers must remain append-only even
// when a new query bypasses writeRow. Runtime checks cover generated SQL.
var ledgerMutationSQL = regexp.MustCompile(`(?i)\b(?:UPDATE\s+|DELETE\s+FROM\s+)(?:[a-z_][a-z_0-9]*\s*\.\s*)?(?:events|events_columnstore|interventions)\b`)

func mutatesLedgerSQL(query string) bool {
	return ledgerMutationSQL.MatchString(strings.ReplaceAll(query, "`", ""))
}

func TestPackageSQLPreservesAppendOnlyLedgers(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		switch filepath.Ext(path) {
		case ".go":
			positions := token.NewFileSet()
			file, err := parser.ParseFile(positions, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				query, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Error(err)
				} else if mutatesLedgerSQL(query) {
					t.Errorf("%s: SQL mutates an append-only ledger", positions.Position(literal.Pos()))
				}
				return true
			})
		case ".sql":
			query, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if mutatesLedgerSQL(string(query)) {
				t.Errorf("%s: SQL mutates an append-only ledger", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLedgerMutationSQLDetection(t *testing.T) {
	for _, query := range []string{
		"UPDATE events SET actor_id=?",
		"UPDATE events_columnstore SET actor_id=?",
		"DELETE FROM `conveyor_test`.`events_columnstore` WHERE id=?",
		"delete\nfrom `interventions` WHERE id=?",
		"UPDATE `conveyor_test`.`events` SET kind=?",
		"DELETE FROM conveyor_test.interventions WHERE id=?",
	} {
		if !mutatesLedgerSQL(query) {
			t.Errorf("missed forbidden statement %q", query)
		}
	}
	for _, query := range []string{
		"INSERT INTO events_columnstore (kind) VALUES (?)",
		"SELECT * FROM interventions",
		"UPDATE tasks SET state=?",
		"DELETE FROM events_archive WHERE id=?",
	} {
		if mutatesLedgerSQL(query) {
			t.Errorf("rejected unrelated statement %q", query)
		}
	}
}

// All live queries use the approved stable table. Migration 16's legacy
// writer is deliberately separate; it predates the columnstore prerequisite.
func TestApplicationSQLDoesNotReadLegacyEvents(t *testing.T) {
	legacySQL := regexp.MustCompile("(?i)\\b(?:FROM|JOIN|INTO|UPDATE)\\s+events\\b")
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		positions := token.NewFileSet()
		file, err := parser.ParseFile(positions, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			query, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			if legacySQL.MatchString(strings.ReplaceAll(query, "`", "")) {
				t.Errorf("%s: application SQL targets legacy events", positions.Position(lit.Pos()))
			}
			return true
		})
	}
	if claimedEventsTable != "events_columnstore" {
		t.Fatalf("claimed event lookups target %q", claimedEventsTable)
	}
}
