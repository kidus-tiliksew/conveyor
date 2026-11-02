package dbmetrics

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestStoreMethodsSetReadLabel enforces the store-boundary label rule on
// both durable drivers and both event-log drivers: every exported *Store
// method whose first parameter is a named context.Context begins with
// WithReadLabel on that parameter and its own name, and a method taking a
// callback that receives a context sets no label (component-persistence,
// Database read accounting).
func TestStoreMethodsSetReadLabel(t *testing.T) {
	packages := []struct{ dir, prefix string }{
		{"../postgres", ""},
		{"../singlestore", ""},
		{"../../eventlog/pglog", "eventlog."},
		{"../../eventlog/s2log", "eventlog."},
	}
	for _, pkg := range packages {
		files, err := filepath.Glob(filepath.Join(pkg.dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		labeled := 0
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil || !fn.Name.IsExported() || !isStoreReceiver(fn) {
					continue
				}
				ctxName, ok := contextParam(fn.Type)
				if !ok {
					continue
				}
				where := fset.Position(fn.Pos()).String() + " " + fn.Name.Name
				label, hasLabel := firstStatementLabel(fn.Body, ctxName)
				if takesContextCallback(fn.Type) {
					if hasLabel {
						t.Errorf("%s takes a context callback and must not set a read label", where)
					}
					continue
				}
				want := pkg.prefix + fn.Name.Name
				switch {
				case !hasLabel:
					t.Errorf("%s must begin with %s = dbmetrics.WithReadLabel(%s, %q)", where, ctxName, ctxName, want)
				case label != want:
					t.Errorf("%s sets read label %q, want %q", where, label, want)
				default:
					labeled++
				}
			}
		}
		if labeled == 0 {
			t.Errorf("%s: no labeled store method found", pkg.dir)
		}
	}
}

func isStoreReceiver(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == "Store"
}

func isContextType(expr ast.Expr) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "context" && selector.Sel.Name == "Context"
}

// contextParam returns the name of a named first context.Context parameter.
func contextParam(fn *ast.FuncType) (string, bool) {
	if fn.Params == nil || len(fn.Params.List) == 0 {
		return "", false
	}
	first := fn.Params.List[0]
	if !isContextType(first.Type) || len(first.Names) == 0 || first.Names[0].Name == "_" {
		return "", false
	}
	return first.Names[0].Name, true
}

func takesContextCallback(fn *ast.FuncType) bool {
	for _, field := range fn.Params.List {
		callback, ok := field.Type.(*ast.FuncType)
		if !ok || callback.Params == nil {
			continue
		}
		for _, param := range callback.Params.List {
			if isContextType(param.Type) {
				return true
			}
		}
	}
	return false
}

// firstStatementLabel reports the label of a leading
// `ctx = dbmetrics.WithReadLabel(ctx, "<label>")` statement.
func firstStatementLabel(body *ast.BlockStmt, ctxName string) (string, bool) {
	if len(body.List) == 0 {
		return "", false
	}
	assign, ok := body.List[0].(*ast.AssignStmt)
	if !ok || assign.Tok != token.ASSIGN || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return "", false
	}
	if lhs, ok := assign.Lhs[0].(*ast.Ident); !ok || lhs.Name != ctxName {
		return "", false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return "", false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "WithReadLabel" {
		return "", false
	}
	if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != "dbmetrics" {
		return "", false
	}
	if arg, ok := call.Args[0].(*ast.Ident); !ok || arg.Name != ctxName {
		return "", false
	}
	literal, ok := call.Args[1].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	label, err := strconv.Unquote(literal.Value)
	return label, err == nil
}
