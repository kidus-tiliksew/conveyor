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
var ledgerMutationSQL = regexp.MustCompile(`(?i)\b(?:UPDATE\s+|DELETE\s+FROM\s+)(?:[a-z_][a-z_0-9]*\s*\.\s*)?(?:events|interventions)\b`)

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
		"delete\nfrom `interventions` WHERE id=?",
		"UPDATE `conveyor_test`.`events` SET kind=?",
		"DELETE FROM conveyor_test.interventions WHERE id=?",
	} {
		if !mutatesLedgerSQL(query) {
			t.Errorf("missed forbidden statement %q", query)
		}
	}
	for _, query := range []string{
		"INSERT INTO events (kind) VALUES (?)",
		"SELECT * FROM interventions",
		"UPDATE tasks SET state=?",
		"DELETE FROM events_archive WHERE id=?",
	} {
		if mutatesLedgerSQL(query) {
			t.Errorf("rejected unrelated statement %q", query)
		}
	}
}
