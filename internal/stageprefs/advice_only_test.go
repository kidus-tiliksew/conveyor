package stageprefs

// TestStagePreferencesAdviceOnly is the component's guard that no factory path
// reads stage preferences (component-stage-preferences invariant 5,
// Verification; req-stage-preferences REQ-3; DEC-131(3)). It works per source
// file, not per package, because internal/httpapi and the store packages
// hold both the component's governed files and unrelated factory code.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	servicePackage       = "github.com/kidus-tiliksew/conveyor/internal/stageprefs"
	stagePreferenceTable = "user_stage_preferences"
)

// governedPaths are the component's governed source globs, copied from the
// conveyor:governs block of component-stage-preferences v2.
var governedPaths = []string{
	"cmd/conveyor/preferences*",
	"internal/httpapi/mcp_stage_preferences*",
	"internal/httpapi/stage_preferences*",
	"internal/stageprefs/**",
	"internal/store/postgres/stage_preferences*",
	"internal/store/singlestore/stage_preferences*",
	"internal/store/stage_preferences*",
	"internal/store/storetest/stage_preferences*",
	"internal/store/volatile_stage_preferences*",
	"web/src/components/stage-preferences/**",
	"web/src/lib/stage-preferences*",
	"web/tests/stage-preferences*",
}

// schemaDeclarations are the only files outside the governed globs that may
// name the table: the migrations that create it and add its risk column
// (component-persistence owns numbered migrations).
var schemaDeclarations = map[string]bool{
	"internal/store/postgres/migrations/166_user_stage_preferences.sql":          true,
	"internal/store/singlestore/migrations/0039_user_stage_preferences.sql":      true,
	"internal/store/postgres/migrations/168_user_stage_preferences_risk.sql":     true,
	"internal/store/singlestore/migrations/0041_user_stage_preferences_risk.sql": true,
}

// storeMethods are the persistence calls behind the service. A call outside
// the governed files would read the set without importing the service.
var storeMethods = map[string]bool{"GetStagePreferences": true, "UpdateStagePreferences": true}

func governed(file string) bool {
	for _, pattern := range governedPaths {
		if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
			if strings.HasPrefix(file, prefix+"/") {
				return true
			}
			continue
		}
		if matched, _ := path.Match(pattern, file); matched {
			return true
		}
	}
	return false
}

// adviceOnlyViolations reports why one repository file breaks the guard, or
// nothing. file is slash-separated and relative to the repository root.
func adviceOnlyViolations(file string, source []byte) []string {
	if governed(file) {
		return nil
	}
	if strings.HasSuffix(file, ".sql") {
		if strings.Contains(string(source), stagePreferenceTable) && !schemaDeclarations[file] {
			return []string{file + ": names " + stagePreferenceTable + " outside the component's governed paths and its migrations"}
		}
		return nil
	}
	if !strings.HasSuffix(file, ".go") {
		return nil
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), file, source, parser.SkipObjectResolution)
	if err != nil {
		return []string{file + ": " + err.Error()}
	}
	var violations []string
	for _, spec := range parsed.Imports {
		if importPath, _ := strconv.Unquote(spec.Path.Value); importPath == servicePackage {
			violations = append(violations, file+": imports "+servicePackage)
		}
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.BasicLit:
			if typed.Kind == token.STRING && strings.Contains(typed.Value, stagePreferenceTable) {
				violations = append(violations, file+": queries "+stagePreferenceTable)
			}
		case *ast.SelectorExpr:
			if storeMethods[typed.Sel.Name] {
				violations = append(violations, file+": calls "+typed.Sel.Name)
			}
		case *ast.FuncDecl:
			if storeMethods[typed.Name.Name] {
				violations = append(violations, file+": implements "+typed.Name.Name)
			}
		}
		return true
	})
	return violations
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

func TestStagePreferencesAdviceOnly(t *testing.T) {
	root := repositoryRoot(t)
	var violations []string
	scanned := 0
	err := filepath.WalkDir(root, func(full string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(full, ".go") && !strings.HasSuffix(full, ".sql") {
			return nil
		}
		relative, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		source, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		scanned++
		violations = append(violations, adviceOnlyViolations(filepath.ToSlash(relative), source)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned %d source files; the walk did not reach the repository", scanned)
	}
	sort.Strings(violations)
	for _, violation := range violations {
		t.Error(violation)
	}
}

// TestStagePreferencesAdviceOnlyClassifier proves the guard refuses factory
// paths and admits only the governed files and the two migrations.
func TestStagePreferencesAdviceOnlyClassifier(t *testing.T) {
	importService := []byte("package x\n\nimport _ \"" + servicePackage + "\"\n")
	queryTable := []byte("package x\n\nconst q = `SELECT version FROM " + stagePreferenceTable + " WHERE user_id=$1`\n")
	callStore := []byte("package x\n\nfunc f(s interface{ GetStagePreferences() }) { s.GetStagePreferences() }\n")
	implementStore := []byte("package x\n\ntype s struct{}\n\nfunc (s) UpdateStagePreferences() {}\n")
	tableSQL := []byte("CREATE TABLE " + stagePreferenceTable + " (user_id text);\n")
	forbidden := []struct {
		file   string
		source []byte
	}{
		{"internal/httpapi/mcp.go", importService},
		{"internal/httpapi/server.go", queryTable},
		{"internal/httpapi/stage_preference_helpers.go", importService},
		{"internal/workorder/claim.go", importService},
		{"internal/dispatch/dispatch.go", callStore},
		{"internal/store/lifecycle_store.go", implementStore},
		{"internal/store/postgres/lifecycle_store.go", queryTable},
		{"internal/review/verdict.go", queryTable},
		{"cmd/conveyord/main.go", importService},
		{"internal/store/postgres/migrations/167_policy.sql", tableSQL},
		{"internal/store/singlestore/migrate.go", queryTable},
		{"internal/store/postgres/migrations/169_policy.sql", tableSQL},
	}
	for _, tc := range forbidden {
		if violations := adviceOnlyViolations(tc.file, tc.source); len(violations) == 0 {
			t.Errorf("%s was admitted", tc.file)
		}
	}
	allowed := []struct {
		file   string
		source []byte
	}{
		{"internal/httpapi/mcp_stage_preferences.go", importService},
		{"internal/httpapi/stage_preferences.go", callStore},
		{"internal/store/postgres/stage_preferences.go", queryTable},
		{"internal/store/singlestore/stage_preferences.go", implementStore},
		{"internal/store/volatile_stage_preferences.go", implementStore},
		{"internal/store/storetest/stage_preferences.go", callStore},
		{"cmd/conveyor/preferences.go", importService},
		{"cmd/conveyor/preferences_test.go", callStore},
		{"internal/stageprefs/advice_only_test.go", queryTable},
		{"internal/store/postgres/migrations/166_user_stage_preferences.sql", tableSQL},
		{"internal/store/singlestore/migrations/0039_user_stage_preferences.sql", tableSQL},
		{"internal/store/postgres/migrations/168_user_stage_preferences_risk.sql", tableSQL},
		{"internal/store/singlestore/migrations/0041_user_stage_preferences_risk.sql", tableSQL},
		{"internal/store/singlestore/stage_preferences_migration_integration_test.go", queryTable},
		{"internal/httpapi/server.go", []byte("package x\n\nimport \"example/store\"\n\nvar _ store.StagePreferenceStore\n")},
	}
	for _, tc := range allowed {
		if violations := adviceOnlyViolations(tc.file, tc.source); len(violations) != 0 {
			t.Errorf("%s was refused: %v", tc.file, violations)
		}
	}
}
