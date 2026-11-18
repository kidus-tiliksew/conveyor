package httpapi

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// unnamedCrate matches a crate written without its name. Inside a JSON string
// a quote is always escaped, so the pattern matches only JSON structure.
var unnamedCrate = regexp.MustCompile(`"crate":\{"id":"[^"]*","name":""\}`)

// TestCrateTaskResponsesNameEveryCrate type-checks this package and fails on
// any JSON write of a value whose type can hold a task's crate, unless the
// write is writeTaskJSON or the same function names the written value with
// nameTaskCrates first (component-crates, Public surfaces). A new response
// that writes a task or a task row through a plain writer fails here.
func TestCrateTaskResponsesNameEveryCrate(t *testing.T) {
	pkg, info, fset, files := typeCheckHTTPAPI(t)
	writers := map[string][]int{"writeJSON": {2}, "writeJSONWithETag": {2}, "writeConditionalJSON": {2, 3}, "renderMCPReadItem": {0}}
	var violations []string
	checked := 0
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "writeTaskJSON" || fn.Name.Name == "MarshalJSON" {
				continue
			}
			named := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "nameTaskCrates" {
					for _, arg := range call.Args[1:] {
						named[rootIdent(arg)] = true
					}
					return true
				}
				var positions []int
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					positions = writers[fun.Name]
				case *ast.SelectorExpr:
					if ident, ok := fun.X.(*ast.Ident); ok && ident.Name == "json" && (fun.Sel.Name == "Marshal" || fun.Sel.Name == "MarshalIndent") {
						positions = []int{0}
					}
					if fun.Sel.Name == "Encode" {
						positions = []int{0}
					}
				}
				for _, position := range positions {
					if position >= len(call.Args) {
						continue
					}
					arg := call.Args[position]
					if !exprHoldsTaskCrate(info, arg) {
						continue
					}
					checked++
					if root := rootIdent(arg); root == "" || !named[root] {
						violations = append(violations, fmt.Sprintf("%s: %s writes %s without naming its crates", fset.Position(call.Pos()), fn.Name.Name, types.ExprString(arg)))
					}
				}
				return true
			})
		}
	}
	if pkg.Name() != "httpapi" || checked == 0 {
		t.Fatalf("checked %d task-bearing writes in package %q; the guard found nothing to check", checked, pkg.Name())
	}
	if len(violations) > 0 {
		t.Fatalf("task responses that skip crate names:\n%s", strings.Join(violations, "\n"))
	}
}

// exprHoldsTaskCrate reports whether an expression's type can hold a crate,
// or, for a literal such as map[string]any{...}, whether any element can.
func exprHoldsTaskCrate(info *types.Info, expr ast.Expr) bool {
	if holdsTaskCrate(info.TypeOf(expr), map[types.Type]bool{}) {
		return true
	}
	switch e := expr.(type) {
	case *ast.UnaryExpr:
		return exprHoldsTaskCrate(info, e.X)
	case *ast.ParenExpr:
		return exprHoldsTaskCrate(info, e.X)
	case *ast.KeyValueExpr:
		return exprHoldsTaskCrate(info, e.Value)
	case *ast.CompositeLit:
		for _, element := range e.Elts {
			if exprHoldsTaskCrate(info, element) {
				return true
			}
		}
	}
	return false
}

// rootIdent returns the variable an expression reads: x for x, &x, x.f,
// x[i], or x.f[i]. A literal or a call has no root.
func rootIdent(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.Ident:
			return e.Name
		case *ast.UnaryExpr:
			expr = e.X
		case *ast.SelectorExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		default:
			return ""
		}
	}
}

// holdsTaskCrate reports whether a JSON-encoded value of type t can carry a
// task's crate: core.Task or core.TaskCrate reached through exported or
// embedded fields, pointers, slices, arrays, or maps.
func holdsTaskCrate(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	switch u := t.(type) {
	case *types.Alias:
		return holdsTaskCrate(types.Unalias(u), seen)
	case *types.Named:
		if obj := u.Obj(); obj.Pkg() != nil && strings.HasSuffix(obj.Pkg().Path(), "/internal/core") && (obj.Name() == "Task" || obj.Name() == "TaskCrate") {
			return true
		}
		return holdsTaskCrate(u.Underlying(), seen)
	case *types.Pointer:
		return holdsTaskCrate(u.Elem(), seen)
	case *types.Slice:
		return holdsTaskCrate(u.Elem(), seen)
	case *types.Array:
		return holdsTaskCrate(u.Elem(), seen)
	case *types.Map:
		return holdsTaskCrate(u.Elem(), seen)
	case *types.Struct:
		for i := range u.NumFields() {
			field := u.Field(i)
			if strings.Contains(u.Tag(i), `json:"-"`) || (!field.Exported() && !field.Embedded()) {
				continue
			}
			if holdsTaskCrate(field.Type(), seen) {
				return true
			}
		}
	}
	return false
}

// typeCheckHTTPAPI type-checks this package's non-test files against the
// export data of its dependencies that `go list -export` reports.
func typeCheckHTTPAPI(t *testing.T) (*types.Package, *types.Info, *token.FileSet, []*ast.File) {
	t.Helper()
	cmd := exec.Command("go", "list", "-export", "-deps", "-json", ".")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -export: %v", err)
	}
	exports := map[string]string{}
	var self struct {
		ImportPath string
		Dir        string
		GoFiles    []string
	}
	decoder := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var listed struct {
			ImportPath string
			Dir        string
			Export     string
			GoFiles    []string
		}
		if err := decoder.Decode(&listed); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[listed.ImportPath] = listed.Export
		self.ImportPath, self.Dir, self.GoFiles = listed.ImportPath, listed.Dir, listed.GoFiles
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range self.GoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(self.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	lookup := func(path string) (io.ReadCloser, error) {
		export := exports[path]
		if export == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(export)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	config := types.Config{Importer: importer.ForCompiler(fset, "gc", lookup)}
	pkg, err := config.Check(self.ImportPath, fset, files, info)
	if err != nil {
		t.Fatalf("type-check %s: %v", self.ImportPath, err)
	}
	return pkg, info, fset, files
}

// TestCrateNameOnEveryTaskResponse writes a member task through each read and
// act that returns a task or a task row, and requires the crate's current
// name in every response (component-crates, Public surfaces).
func TestCrateNameOnEveryTaskResponse(t *testing.T) {
	f := newCrateFixture(t)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Named crate"})
	member := f.task(t, crate.ID)
	dependency := f.task(t, crate.ID)
	want := `"crate":{"id":"` + crate.ID + `","name":"Named crate"}`
	operator := humanIntakeToken(core.WorkspaceRoleOperator)
	check := func(label string, status int, raw []byte, wantStatus int) {
		t.Helper()
		if status != wantStatus {
			t.Fatalf("%s status=%d body=%s", label, status, raw)
		}
		if !strings.Contains(string(raw), want) || unnamedCrate.Match(raw) {
			t.Fatalf("%s body lacks %s or holds an unnamed crate: %s", label, want, raw)
		}
	}
	for _, path := range []string{
		"/v1/tasks", "/v1/tasks/" + member.ID, "/v1/activity", "/v1/task-operations", "/v1/task-map",
		"/v1/board", "/v1/tasks/" + member.ID + "/activity",
	} {
		status, raw := f.do(t, f.viewer, http.MethodGet, path, nil)
		check("GET "+path, status, raw, http.StatusOK)
	}
	status, raw := f.do(t, operator, http.MethodPost, "/v1/tasks/"+member.ID+"/dependencies", map[string]any{"depends_on_task_id": dependency.ID, "reason": "Order the crate work.", "request_id": "crate-dependency"})
	check("add dependency", status, raw, http.StatusOK)
	status, raw = f.do(t, operator, http.MethodDelete, "/v1/tasks/"+member.ID+"/dependencies/"+dependency.ID, map[string]any{"reason": "Unorder the crate work.", "request_id": "crate-dependency-removal"})
	check("remove dependency", status, raw, http.StatusOK)
	status, raw = f.do(t, operator, http.MethodPost, "/v1/tasks/"+member.ID+"/branch", map[string]any{"branch": "crate/named-branch"})
	check("attach branch", status, raw, http.StatusOK)
	status, raw = f.do(t, operator, http.MethodPost, "/v1/tasks/"+member.ID+"/close", map[string]any{"reason": "The crate work moved."})
	check("close", status, raw, http.StatusOK)

	// A task in no crate carries no crate on the same responses.
	plain := f.task(t, "")
	for _, path := range []string{"/v1/tasks/" + plain.ID, "/v1/tasks/" + plain.ID + "/activity"} {
		status, raw := f.do(t, f.viewer, http.MethodGet, path, nil)
		if status != http.StatusOK || strings.Contains(string(raw), `"crate":`) {
			t.Fatalf("GET %s status=%d body=%s, want no crate", path, status, raw)
		}
	}
}

// TestCrateNamesReachNestedRows names crates in every shape a response uses:
// a task by value, an embedded task, a task pointer, slices, a map of task
// pointers, and an interface. It keeps a shared crate reference unchanged
// and skips a field JSON does not encode.
func TestCrateNamesReachNestedRows(t *testing.T) {
	f := newCrateFixture(t)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Nested"})
	shared := &core.TaskCrate{ID: crate.ID}
	type embedded struct{ core.Task }
	type holder struct {
		Task     core.Task             `json:"task"`
		Embedded embedded              `json:"embedded"`
		Pointer  *core.Task            `json:"pointer"`
		Rows     []activityItem        `json:"rows"`
		ByID     map[string]*core.Task `json:"by_id"`
		Any      any                   `json:"any"`
		Hidden   core.Task             `json:"-"`
	}
	value := holder{
		Task:     core.Task{ID: "a", Crate: shared},
		Embedded: embedded{core.Task{ID: "b", Crate: shared}},
		Pointer:  &core.Task{ID: "c", Crate: shared},
		Rows:     []activityItem{{Task: activityTask{ID: "d", Crate: shared}}},
		ByID:     map[string]*core.Task{"e": {ID: "e", Crate: shared}},
		Any:      &core.Task{ID: "f", Crate: shared},
		Hidden:   core.Task{ID: "g", Crate: shared},
	}
	if err := f.server.nameTaskCrates(store.WithWorkspace(t.Context(), "alpha"), &value); err != nil {
		t.Fatal(err)
	}
	for label, got := range map[string]*core.TaskCrate{
		"value": value.Task.Crate, "embedded": value.Embedded.Crate, "pointer": value.Pointer.Crate,
		"slice": value.Rows[0].Task.Crate, "map": value.ByID["e"].Crate, "interface": value.Any.(*core.Task).Crate,
	} {
		if got == nil || got.ID != crate.ID || got.Name != "Nested" {
			t.Fatalf("%s crate = %+v, want {%s Nested}", label, got, crate.ID)
		}
	}
	if shared.Name != "" || value.Hidden.Crate != shared {
		t.Fatalf("naming changed a shared reference: shared=%+v hidden=%p", shared, value.Hidden.Crate)
	}
}
