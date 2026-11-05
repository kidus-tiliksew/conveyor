package singlestore

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// translatingPaths names the functions that may hold a raw database handle or
// start a transaction, because they translate every error they return
// (component-persistence; DEC-38). A transaction exists only inside one of
// them, so a tx.QueryContext in a helper is covered by the boundary that began
// it, as PostgreSQL's boundaryTx covers its transaction helpers.
var translatingPaths = map[string]bool{
	"backend_queries.go:backendRow":           true,
	"backend_queries.go:backendRows":          true,
	"backend_queries.go:backendExec":          true,
	"backend_queries.go:loadJob":              true,
	"store.go:withTx":                         true,
	"store.go:Open":                           true,
	"locks.go:sessionLock":                    true,
	"locks.go:WithTaskSideEffectLock":         true,
	"planning.go:WithPlanningSessionRun":      true,
	"verification.go:RunVerificationDelivery": true,
}

// startupOnlyFiles and startupOnlyFunctions run only under Open, whose single
// return translates their errors, so they keep raw statements. The guard
// proves that reachability: no other function may call them, and they declare
// no exported function.
var startupOnlyFiles = map[string]bool{
	"migrate.go":                    true,
	"migrate_document_dismissal.go": true,
	"events_prerequisite.go":        true,
}

var startupOnlyFunctions = map[string]bool{
	"verification_chunk_expiry.go:migrateVerificationChunkExpiryIndex": true,
}

// driverWrapperFiles implement the database/sql driver interfaces below every
// store path. They forward driver errors to database/sql by contract.
var driverWrapperFiles = map[string]bool{"db_timing.go": true}

var rawStatementMethods = map[string]bool{
	"Query": true, "QueryContext": true, "QueryRow": true, "QueryRowContext": true,
	"Exec": true, "ExecContext": true, "Prepare": true, "PrepareContext": true,
}

var rawHandleTypes = map[string]bool{"s2log.Executor": true, "*sql.DB": true, "*sql.Conn": true}

// queryBoundaryFindings reports source that reaches the database outside the
// translating paths. It reads syntax, so comments and SQL text never match.
func queryBoundaryFindings(sources map[string]string) ([]string, error) {
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	startupNames := map[string]bool{}
	for name, source := range sources {
		file, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			return nil, err
		}
		files[name] = file
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && isStartupOnly(name, fn.Name.Name) {
				startupNames[fn.Name.Name] = true
			}
		}
	}
	var findings []string
	for name, file := range files {
		if driverWrapperFiles[name] {
			continue
		}
		imports := importNames(file)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			report := func(pos token.Pos, format string, args ...any) {
				findings = append(findings, fmt.Sprintf("%s:%d: %s %s", name, fset.Position(pos).Line, fn.Name.Name, fmt.Sprintf(format, args...)))
			}
			if isStartupOnly(name, fn.Name.Name) {
				if fn.Name.IsExported() {
					report(fn.Pos(), "is exported in startup-only source")
				}
				continue
			}
			checkCallsToStartupOnly(fn, startupNames, report)
			if !translatingPaths[name+":"+fn.Name.Name] {
				checkFunctionStatements(fn, imports, report)
			}
		}
	}
	sort.Strings(findings)
	return findings, nil
}

func isStartupOnly(file, function string) bool {
	return startupOnlyFiles[file] || startupOnlyFunctions[file+":"+function]
}

func importNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, spec := range file.Imports {
		if spec.Name != nil {
			names[spec.Name.Name] = true
			continue
		}
		path, _ := strconv.Unquote(spec.Path.Value)
		names[path[strings.LastIndex(path, "/")+1:]] = true
	}
	return names
}

func checkCallsToStartupOnly(fn *ast.FuncDecl, startupNames map[string]bool, report func(token.Pos, string, ...any)) {
	if fn.Name.Name == "Open" {
		return
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		var callee string
		switch target := call.Fun.(type) {
		case *ast.Ident:
			callee = target.Name
		case *ast.SelectorExpr:
			callee = target.Sel.Name
		}
		if startupNames[callee] {
			report(call.Pos(), "calls startup-only %s outside Open", callee)
		}
		return true
	})
}

func checkFunctionStatements(fn *ast.FuncDecl, imports map[string]bool, report func(token.Pos, string, ...any)) {
	handles := rawHandleParameters(fn.Type)
	var stack []ast.Node
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		var parent ast.Node
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		stack = append(stack, node)
		switch current := node.(type) {
		case *ast.FuncLit:
			for handle := range rawHandleParameters(current.Type) {
				handles[handle] = true
			}
		case *ast.SelectorExpr:
			switch current.Sel.Name {
			case "db":
				checkHandleUse(current, parent, stack, imports, report)
			case "log":
				checkLogUse(current, parent, report)
			}
		case *ast.CallExpr:
			target, ok := current.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if target.Sel.Name == "BeginTx" {
				report(current.Pos(), "begins a transaction outside the translating paths")
			}
			if receiver, ok := target.X.(*ast.Ident); ok && handles[receiver.Name] && rawStatementMethods[target.Sel.Name] {
				report(current.Pos(), "runs %s on raw handle %s", target.Sel.Name, receiver.Name)
			}
		}
		return true
	})
}

// rawHandleParameters names the parameters that could carry the pool.
func rawHandleParameters(signature *ast.FuncType) map[string]bool {
	handles := map[string]bool{}
	if signature.Params == nil {
		return handles
	}
	for _, field := range signature.Params.List {
		if !rawHandleTypes[typeText(field.Type)] {
			continue
		}
		for _, name := range field.Names {
			handles[name.Name] = true
		}
	}
	return handles
}

func typeText(expression ast.Expr) string {
	switch current := expression.(type) {
	case *ast.StarExpr:
		return "*" + typeText(current.X)
	case *ast.SelectorExpr:
		return typeText(current.X) + "." + current.Sel.Name
	case *ast.Ident:
		return current.Name
	}
	return ""
}

// checkHandleUse allows the pool only as an argument to a package function,
// whose executor parameter the guard also checks, or for Close. Anything else,
// including an alias, could run a statement the guard cannot see.
func checkHandleUse(handle *ast.SelectorExpr, parent ast.Node, stack []ast.Node, imports map[string]bool, report func(token.Pos, string, ...any)) {
	switch use := parent.(type) {
	case *ast.CallExpr:
		for _, argument := range use.Args {
			if argument == ast.Expr(handle) {
				if target, ok := use.Fun.(*ast.SelectorExpr); ok {
					if receiver, ok := target.X.(*ast.Ident); ok && imports[receiver.Name] {
						report(handle.Pos(), "hands the pool to %s.%s, outside the translating paths", receiver.Name, target.Sel.Name)
					}
				}
				return
			}
		}
	case *ast.SelectorExpr:
		if use.Sel.Name == "Close" && len(stack) >= 3 {
			if _, called := stack[len(stack)-3].(*ast.CallExpr); called {
				return
			}
		}
		report(handle.Pos(), "calls %s on the pool outside the translating paths", use.Sel.Name)
		return
	}
	report(handle.Pos(), "uses the pool outside the translating paths")
}

// checkLogUse keeps event-log access inside a transaction, whose boundary
// translates it. The log driver itself returns driver errors unchanged.
func checkLogUse(handle *ast.SelectorExpr, parent ast.Node, report func(token.Pos, string, ...any)) {
	call, ok := parent.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return
	}
	if first, ok := call.Args[0].(*ast.CallExpr); ok {
		if name, ok := first.Fun.(*ast.Ident); ok && name.Name == "logTx" {
			return
		}
	}
	report(handle.Pos(), "reads the event log outside a transaction")
}

// TestPackageQueriesUseTranslatingPaths keeps every statement outside a
// transaction behind the translating helpers and every transaction behind a
// translating path (component-persistence). It needs no database, so go test
// ./... runs it with the SingleStore URL unset.
func TestPackageQueriesUseTranslatingPaths(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = string(body)
	}
	if len(sources) == 0 {
		t.Fatal("the guard read no package source")
	}
	findings, err := queryBoundaryFindings(sources)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		t.Error(finding)
	}
	for key := range translatingPaths {
		file, function, _ := strings.Cut(key, ":")
		if !strings.Contains(sources[file], function+"(") {
			t.Errorf("translating path %s no longer exists", key)
		}
	}
	for key := range startupOnlyFunctions {
		file, function, _ := strings.Cut(key, ":")
		if !strings.Contains(sources[file], function+"(") {
			t.Errorf("startup-only function %s no longer exists", key)
		}
	}
}

// TestQueryBoundaryGuardRejectsRawSource proves that the guard fails on each
// deliberately raw form and stays quiet on the translating forms.
func TestQueryBoundaryGuardRejectsRawSource(t *testing.T) {
	const header = "package singlestore\nimport (\n\"context\"\n\"database/sql\"\n\"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log\"\n\"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue\"\n)\n"
	raw := map[string]struct{ body, want string }{
		"pool query": {
			`func (s *Store) ListThings(ctx context.Context) error { _, err := s.db.QueryContext(ctx, "SELECT 1"); return err }`,
			"calls QueryContext on the pool",
		},
		"pool single-row query": {
			`func (s *Store) GetThing(ctx context.Context) error { return s.db.QueryRowContext(ctx, "SELECT 1").Scan() }`,
			"calls QueryRowContext on the pool",
		},
		"pool exec": {
			`func (s *Store) SetThing(ctx context.Context) error { _, err := s.db.ExecContext(ctx, "SELECT 1"); return err }`,
			"calls ExecContext on the pool",
		},
		"pool reached through another receiver": {
			`func helper(ctx context.Context, st *Store) error { _, err := st.db.QueryContext(ctx, "SELECT 1"); return err }`,
			"calls QueryContext on the pool",
		},
		"pool alias": {
			`func (s *Store) AliasThing(ctx context.Context) error { db := s.db; _, err := db.QueryContext(ctx, "SELECT 1"); return err }`,
			"uses the pool outside",
		},
		"pool handed to another package": {
			`func (s *Store) Hand(ctx context.Context) error { return s2log.EnsureSchema(ctx, s.db) }`,
			"hands the pool to s2log.EnsureSchema",
		},
		"pool transaction": {
			`func (s *Store) TxThing(ctx context.Context) error { tx, err := s.db.BeginTx(ctx, nil); if err != nil { return err }; _, err = tx.ExecContext(ctx, "SELECT 1"); return err }`,
			"calls BeginTx on the pool",
		},
		"pinned connection": {
			`func (s *Store) ConnThing(ctx context.Context) error { conn, err := s.db.Conn(ctx); if err != nil { return err }; return conn.Close() }`,
			"calls Conn on the pool",
		},
		"pinned connection transaction": {
			`func helper(ctx context.Context, conn *sql.Conn) error { _, err := conn.BeginTx(ctx, nil); return err }`,
			"begins a transaction outside",
		},
		"pinned connection statement": {
			`func helper(ctx context.Context, conn *sql.Conn) error { _, err := conn.ExecContext(ctx, "SELECT 1"); return err }`,
			"runs ExecContext on raw handle conn",
		},
		"executor parameter": {
			`func helper(ctx context.Context, db s2log.Executor) error { _, err := db.ExecContext(ctx, "SELECT 1"); return err }`,
			"runs ExecContext on raw handle db",
		},
		"event log outside a transaction": {
			`func (s *Store) LoadThing(ctx context.Context) error { _, err := logqueue.Load(ctx, s.log, "ws", "stream"); return err }`,
			"reads the event log outside a transaction",
		},
	}
	for name, test := range raw {
		t.Run(name, func(t *testing.T) {
			findings, err := queryBoundaryFindings(map[string]string{"fixture.go": header + test.body + "\n"})
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) == 0 || !strings.Contains(strings.Join(findings, "\n"), test.want) {
				t.Fatalf("findings = %q, want one containing %q", findings, test.want)
			}
		})
	}
	const translated = header + `
func (s *Store) Quiet(ctx context.Context) error {
	// s.db.QueryContext(ctx, "in a comment")
	const text = "s.db.ExecContext(ctx)"
	if err := backendRow(ctx, s.db, "SELECT 1").Scan(); err != nil {
		return err
	}
	if _, err := getThing(ctx, s.db); err != nil {
		return err
	}
	_, err := logqueue.Enqueue(logTx(ctx, nil), s.log, "ws", "kind", "key", nil, 1, nowForFixture())
	if err != nil {
		return err
	}
	s.db.Close()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, text)
		return err
	})
}
func getThing(ctx context.Context, db s2log.Executor) (string, error) {
	var value string
	return value, backendRow(ctx, db, "SELECT 1").Scan(&value)
}
`
	findings, err := queryBoundaryFindings(map[string]string{"fixture.go": translated})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("translating source drew findings: %q", findings)
	}

	const startup = "package singlestore\nimport \"context\"\nfunc (s *Store) migrateThing(ctx context.Context) error { _, err := s.db.ExecContext(ctx, \"SELECT 1\"); return err }\nfunc (s *Store) MigrateThing(ctx context.Context) error { return s.migrateThing(ctx) }\n"
	const caller = "package singlestore\nimport \"context\"\nfunc (s *Store) Reach(ctx context.Context) error { return s.migrateThing(ctx) }\n"
	findings, err = queryBoundaryFindings(map[string]string{"migrate.go": startup, "fixture.go": caller})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(findings, "\n")
	for _, want := range []string{"Reach calls startup-only migrateThing outside Open", "MigrateThing is exported in startup-only source"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("startup findings = %q, want %q", findings, want)
		}
	}
	if strings.Contains(joined, "calls ExecContext") {
		t.Fatalf("a startup-only statement drew a finding: %q", findings)
	}
}
