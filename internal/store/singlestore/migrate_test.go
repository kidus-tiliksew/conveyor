package singlestore

import (
	"strings"
	"testing"
)

func TestMigrationSQLStatementsSurviveSemicolonSplit(t *testing.T) {
	files, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no migrations")
	}
	for _, file := range files {
		for i, raw := range strings.Split(file.sql, ";") {
			body := strings.TrimSpace(raw)
			if body == "" {
				continue
			}
			var kept []string
			for _, line := range strings.Split(body, "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "--") {
					continue
				}
				kept = append(kept, line)
			}
			if len(kept) == 0 {
				continue
			}
			verb := strings.ToUpper(strings.Fields(kept[0])[0])
			switch verb {
			case "CREATE", "ALTER", "SELECT", "INSERT", "DROP", "UPDATE", "DELETE":
			default:
				t.Errorf("%s statement %d starts with %q after semicolon split", file.name, i, kept[0])
			}
		}
	}
}
