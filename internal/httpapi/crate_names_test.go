package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// unnamedCrate matches a crate written without its name. Inside a JSON string
// a quote is always escaped, so the pattern matches only JSON structure.
var unnamedCrate = regexp.MustCompile(`"crate":\{"id":"[^"]*","name":""\}`)

// TestCrateTaskResponsesNameEveryCrate type-checks this package and fails on
// any JSON write of a value that can hold a task's crate, unless the
// write is writeTaskJSON or the same function names the written value with
// nameTaskCrates earlier in its body (component-crates, Public surfaces). A
// new response that writes a task or a task row through a plain writer fails
// here. Both accepted forms name the whole value: the walk reaches every
// shape a response holds, and it returns an error, never an empty name, for
// a reference it cannot name (TestCrateNamesReachNestedRows,
// TestCrateNamesRefuseWhatTheyCannotName). A value can hold a crate by its
// type, or because the function stored a task-bearing value in it, as in
// body := map[string]any{"task": task} or body["task"] = task. A
// nameTaskCrates call whose error is dropped, by a bare call or by an
// assignment to the blank identifier, fails here too. MarshalJSON methods are
// not writes: they encode parts of a value its writer already named.
func TestCrateTaskResponsesNameEveryCrate(t *testing.T) {
	pkg, info, fset, files := typeCheckHTTPAPI(t)
	violations, checked := crateWriteViolations(info, fset, files)
	if pkg.Name() != "httpapi" || checked == 0 {
		t.Fatalf("checked %d task-bearing writes in package %q; the guard found nothing to check", checked, pkg.Name())
	}
	if len(violations) > 0 {
		t.Fatalf("task responses that skip crate names:\n%s", strings.Join(violations, "\n"))
	}
}

// TestCrateGuardFlagsUnnamedWrites type-checks a sample file in this package
// and requires the guard to flag each write that can lose a crate's name,
// and to pass each named write.
func TestCrateGuardFlagsUnnamedWrites(t *testing.T) {
	const sample = `package httpapi

import (
	"context"
	"net/http"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func guardSampleBareLiteral(w http.ResponseWriter, task core.Task) {
	body := map[string]any{"task": task}
	writeJSON(w, http.StatusOK, body)
}

func guardSampleIndexed(w http.ResponseWriter, task core.Task) {
	body := map[string]any{}
	body["task"] = task
	copied := body
	writeJSON(w, http.StatusOK, copied)
}

func guardSampleVar(w http.ResponseWriter, task core.Task) {
	var body any = task
	writeJSON(w, http.StatusOK, body)
}

func guardSampleBlank(ctx context.Context, s *Server, w http.ResponseWriter, task core.Task) {
	body := map[string]any{"task": task}
	_ = s.nameTaskCrates(ctx, &body)
	writeJSON(w, http.StatusOK, body)
}

func guardSampleBare(ctx context.Context, s *Server, w http.ResponseWriter, task core.Task) {
	s.nameTaskCrates(ctx, &task)
	writeJSON(w, http.StatusOK, task)
}

func guardSampleNamed(ctx context.Context, s *Server, w http.ResponseWriter, task core.Task) {
	body := map[string]any{"task": task}
	if err := s.nameTaskCrates(ctx, &body); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func guardSampleTaskJSON(s *Server, w http.ResponseWriter, r *http.Request, task core.Task) {
	body := map[string]any{"task": task}
	s.writeTaskJSON(w, r, http.StatusOK, body)
}
`
	_, info, fset, files := typeCheckHTTPAPI(t, sample)
	violations, checked := crateWriteViolations(info, fset, files[len(files)-1:])
	joined := strings.Join(violations, "\n")
	for _, want := range []string{
		"guardSampleBareLiteral writes body without naming",
		"guardSampleIndexed writes copied without naming",
		"guardSampleVar writes body without naming",
		"guardSampleBlank drops the error",
		"guardSampleBare drops the error",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("guard missed %q; it reported:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "guardSampleNamed") || strings.Contains(joined, "guardSampleTaskJSON") {
		t.Errorf("guard flagged a named write:\n%s", joined)
	}
	if checked != 6 {
		t.Errorf("guard checked %d sample writes, want 6", checked)
	}
}

// crateWriteViolations applies the crate-name guard to the functions of
// files and returns its findings and the number of task-bearing writes it
// checked.
func crateWriteViolations(info *types.Info, fset *token.FileSet, files []*ast.File) ([]string, int) {
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
			holders := taskCrateHolders(info, fn.Body)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call := droppedNamingCall(n); call != nil {
					violations = append(violations, fmt.Sprintf("%s: %s drops the error of nameTaskCrates", fset.Position(call.Pos()), fn.Name.Name))
				}
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
					if !exprHoldsTaskCrate(info, arg, holders) {
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
	return violations, checked
}

// droppedNamingCall returns a nameTaskCrates call whose error the statement
// drops: a bare call statement, or an assignment of the call to _.
func droppedNamingCall(n ast.Node) *ast.CallExpr {
	var expr ast.Expr
	switch statement := n.(type) {
	case *ast.ExprStmt:
		expr = statement.X
	case *ast.AssignStmt:
		if len(statement.Rhs) != 1 {
			return nil
		}
		for _, lhs := range statement.Lhs {
			if ident, ok := lhs.(*ast.Ident); !ok || ident.Name != "_" {
				return nil
			}
		}
		expr = statement.Rhs[0]
	default:
		return nil
	}
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok {
		return nil
	}
	if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "nameTaskCrates" {
		return call
	}
	return nil
}

// taskCrateHolders returns the local variables of a function body that hold
// a task-bearing value by assignment, though their own type may not show it:
// body := map[string]any{"task": task}, var body any = task, body["task"] =
// task, or a copy of such a variable. It repeats until no new variable joins.
func taskCrateHolders(info *types.Info, body *ast.BlockStmt) map[string]bool {
	holders := map[string]bool{}
	for changed := true; changed; {
		changed = false
		mark := func(lhs, rhs ast.Expr) {
			root := rootIdent(lhs)
			if root == "" || root == "_" || holders[root] || !exprHoldsTaskCrate(info, rhs, holders) {
				return
			}
			holders[root] = true
			changed = true
		}
		ast.Inspect(body, func(n ast.Node) bool {
			switch statement := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range statement.Lhs {
					if len(statement.Rhs) == len(statement.Lhs) {
						mark(lhs, statement.Rhs[i])
					} else if len(statement.Rhs) == 1 {
						mark(lhs, statement.Rhs[0])
					}
				}
			case *ast.ValueSpec:
				for i, name := range statement.Names {
					if len(statement.Values) == len(statement.Names) {
						mark(name, statement.Values[i])
					}
				}
			}
			return true
		})
	}
	return holders
}

// exprHoldsTaskCrate reports whether an expression's type can hold a crate,
// whether it reads a variable in holders, or, for a literal such as
// map[string]any{...}, whether any element can.
func exprHoldsTaskCrate(info *types.Info, expr ast.Expr, holders map[string]bool) bool {
	if holdsTaskCrate(info.TypeOf(expr), map[types.Type]bool{}) {
		return true
	}
	if root := rootIdent(expr); root != "" && holders[root] {
		return true
	}
	switch e := expr.(type) {
	case *ast.UnaryExpr:
		return exprHoldsTaskCrate(info, e.X, holders)
	case *ast.ParenExpr:
		return exprHoldsTaskCrate(info, e.X, holders)
	case *ast.KeyValueExpr:
		return exprHoldsTaskCrate(info, e.Value, holders)
	case *ast.CompositeLit:
		for _, element := range e.Elts {
			if exprHoldsTaskCrate(info, element, holders) {
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

// typeCheckHTTPAPI type-checks this package's non-test files, plus any extra
// sources given, against the export data of its dependencies that
// `go list -export` reports. The extra files come last in the result.
func typeCheckHTTPAPI(t *testing.T, extra ...string) (*types.Package, *types.Info, *token.FileSet, []*ast.File) {
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
	for i, source := range extra {
		file, err := parser.ParseFile(fset, fmt.Sprintf("guard_sample_%d.go", i), source, parser.SkipObjectResolution)
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

// TestCrateNamesReachNestedRows names crates in every shape a response can
// hold: a task by value, an embedded task, a task pointer, slices, arrays,
// maps of task values and of task pointers, a map[string]any holding a task
// by value or by pointer, an interface holding a task by value, and nested
// maps. It keeps a shared crate reference unchanged and skips a field JSON
// does not encode (component-crates, Public surfaces).
func TestCrateNamesReachNestedRows(t *testing.T) {
	f := newCrateFixture(t)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Nested"})
	shared := &core.TaskCrate{ID: crate.ID}
	member := func(id string) core.Task { return core.Task{ID: id, Crate: shared} }
	type embedded struct{ core.Task }
	type holder struct {
		Task      core.Task             `json:"task"`
		Embedded  embedded              `json:"embedded"`
		Pointer   *core.Task            `json:"pointer"`
		Rows      []activityItem        `json:"rows"`
		Array     [2]core.Task          `json:"array"`
		ByID      map[string]*core.Task `json:"by_id"`
		Values    map[string]core.Task  `json:"values"`
		Any       any                   `json:"any"`
		AnyValue  any                   `json:"any_value"`
		Hidden    core.Task             `json:"-"`
		unexposed core.Task
	}
	value := holder{
		Task:      member("a"),
		Embedded:  embedded{member("b")},
		Pointer:   &core.Task{ID: "c", Crate: shared},
		Rows:      []activityItem{{Task: activityTask{ID: "d", Crate: shared}}},
		Array:     [2]core.Task{member("e"), member("f")},
		ByID:      map[string]*core.Task{"g": {ID: "g", Crate: shared}},
		Values:    map[string]core.Task{"h": member("h")},
		Any:       &core.Task{ID: "i", Crate: shared},
		AnyValue:  member("j"),
		Hidden:    member("hidden"),
		unexposed: member("unexposed"),
	}
	byValue := map[string]any{"task": member("k"), "created": true}
	byPointer := map[string]any{"task": &core.Task{ID: "l", Crate: shared}}
	nested := map[string]any{"items": []any{member("m")}, "inner": map[string]any{"task": member("n")}}
	if err := f.server.nameTaskCrates(store.WithWorkspace(t.Context(), "alpha"), &value, &byValue, &byPointer, &nested); err != nil {
		t.Fatal(err)
	}
	want := `"crate":{"id":"` + crate.ID + `","name":"Nested"}`
	for label, target := range map[string]any{"holder": value, "map by value": byValue, "map by pointer": byPointer, "nested maps": nested} {
		raw, err := json.Marshal(target)
		if err != nil {
			t.Fatal(err)
		}
		if unnamedCrate.Match(raw) || !strings.Contains(string(raw), want) {
			t.Fatalf("%s holds an unnamed crate: %s", label, raw)
		}
	}
	if got := strings.Count(crateJSON(t, value), want); got != 10 {
		t.Fatalf("holder names %d crates, want 10 (every encoded task): %s", got, crateJSON(t, value))
	}
	if shared.Name != "" || value.Hidden.Crate != shared || value.unexposed.Crate != shared {
		t.Fatalf("naming changed a shared reference or a field JSON skips: shared=%+v", shared)
	}
}

func crateJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestCrateNamesRefuseWhatTheyCannotName fails the naming, never sends an
// empty name, when a crate reference sits where it cannot be stored back or
// its crate has no name in the workspace.
func TestCrateNamesRefuseWhatTheyCannotName(t *testing.T) {
	namer := &crateNamer{reaches: map[reflect.Type]bool{}, names: map[string]string{"crate-1": "Named"}}
	unaddressable := reflect.ValueOf(core.Task{ID: "a", Crate: &core.TaskCrate{ID: "crate-1"}})
	if err := namer.name(unaddressable); err == nil {
		t.Fatal("naming a task that cannot be stored back succeeded")
	}
	f := newCrateFixture(t)
	if err := f.server.nameTaskCrates(t.Context(), core.Task{}); err == nil {
		t.Fatal("naming a target that is not a pointer succeeded")
	}
	missing := core.Task{ID: "b", Crate: &core.TaskCrate{ID: "crate-000000-000000"}}
	if err := f.server.nameTaskCrates(store.WithWorkspace(t.Context(), "alpha"), &missing); err == nil || missing.Crate.Name != "" {
		t.Fatalf("naming a crate with no name returned err=%v crate=%+v", err, missing.Crate)
	}
}

// TestCrateNamesConcurrentColdCalls names different task-bearing wrappers at
// the same time, each call with its own cold memo. Run it with -race: no
// state is shared between calls, so every call names every crate.
func TestCrateNamesConcurrentColdCalls(t *testing.T) {
	f := newCrateFixture(t)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Concurrent"})
	want := `"crate":{"id":"` + crate.ID + `","name":"Concurrent"}`
	type wrapper struct {
		Rows []struct{ core.Task } `json:"rows"`
	}
	build := []func() any{
		func() any { return map[string]any{"task": core.Task{ID: "a", Crate: &core.TaskCrate{ID: crate.ID}}} },
		func() any { return []activityItem{{Task: activityTask{ID: "b", Crate: &core.TaskCrate{ID: crate.ID}}}} },
		func() any {
			return wrapper{Rows: []struct{ core.Task }{{core.Task{ID: "c", Crate: &core.TaskCrate{ID: crate.ID}}}}}
		},
		func() any {
			return store.Board{Items: []store.BoardItem{{ID: "d", Crate: &core.TaskCrate{ID: crate.ID}}}}
		},
		func() any { return []inboxItem{{Task: &core.Task{ID: "e", Crate: &core.TaskCrate{ID: crate.ID}}}} },
	}
	ctx := store.WithWorkspace(t.Context(), "alpha")
	errs := make(chan error, 200)
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func(value any) {
			defer wg.Done()
			target := reflect.New(reflect.TypeOf(value))
			target.Elem().Set(reflect.ValueOf(value))
			if err := f.server.nameTaskCrates(ctx, target.Interface()); err != nil {
				errs <- err
				return
			}
			raw, err := json.Marshal(target.Elem().Interface())
			if err != nil || !strings.Contains(string(raw), want) || unnamedCrate.Match(raw) {
				errs <- fmt.Errorf("%T: %s %v", value, raw, err)
			}
		}(build[i%len(build)]())
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestCrateNamedOnReviewRequestChangesAndFollowUp calls the three acts that
// answer with a task inside a map for a member task through their real
// routes, and requires the crate's current name in each answer
// (component-crates, Public surfaces).
func TestCrateNamedOnReviewRequestChangesAndFollowUp(t *testing.T) {
	want := func(t *testing.T, label string, status, wantStatus int, raw []byte, crateID, name string) {
		t.Helper()
		if status != wantStatus {
			t.Fatalf("%s status=%d body=%s", label, status, raw)
		}
		if !strings.Contains(string(raw), `"crate":{"id":"`+crateID+`","name":"`+name+`"}`) || unnamedCrate.Match(raw) {
			t.Fatalf("%s body lacks the named crate: %s", label, raw)
		}
	}
	t.Run("review", func(t *testing.T) {
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
		st := store.NewMemory()
		crate, err := st.(store.CrateStore).CreateCrate(ctx, store.CrateInput{Name: "Reviewed"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.CreateTask(ctx, core.Task{ID: "crate-review", Workspace: "demo", State: core.TaskAwaiting, CreatedAt: time.Now(), Crate: &core.TaskCrate{ID: crate.ID}}); err != nil {
			t.Fatal(err)
		}
		server := NewServer(st)
		server.BearerToken, server.Workspace = "token", "demo"
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/crate-review/review", strings.NewReader(`{"action":"pull_to_local","reason_code":"needs-human"}`))
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		want(t, "review", response.Code, http.StatusAccepted, response.Body.Bytes(), crate.ID, "Reviewed")
	})
	t.Run("request changes", func(t *testing.T) {
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
		st := store.NewMemory()
		crate, err := st.(store.CrateStore).CreateCrate(ctx, store.CrateInput{Name: "Bounced"})
		if err != nil {
			t.Fatal(err)
		}
		task := core.Task{
			ID: "crate-request-changes", Workspace: "demo", Repo: "conveyor", BaseBranch: "main",
			Branch: "conveyor/task-crate-request-changes", State: core.TaskRunning,
			NextStage: core.StageReview, RecoveryStage: core.StageImplement, CreatedAt: time.Now().UTC(),
			Crate: &core.TaskCrate{ID: crate.ID},
		}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, StartedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true}); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{
			Workspace: "demo", MaxBounces: 3, WorkOrderQueueTimeout: time.Hour,
			Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}}},
			Repos:   []config.Repo{{Name: "conveyor", Base: "main"}},
		}
		server := NewServer(st)
		server.BearerToken, server.Workspace = "user-token", "demo"
		server.InvitationSessions = &invitationSessionFixture{credential: core.AuthenticatedCredential{
			ID: "session_local", OwnerUserID: "local-operator", Kind: core.CredentialUser,
			Scope: core.CredentialScopeOperator, Method: core.CredentialMethodSession,
		}}
		server.ConfigProvider = func(context.Context) (*config.Config, error) { return cfg, nil }
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/request-changes", strings.NewReader(`{"feedback":"Name the crate."}`))
		request.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: "session-secret"})
		request.Header.Set("X-Conveyor-CSRF", "1")
		request.Header.Set("Origin", "http://example.com")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		want(t, "request changes", response.Code, http.StatusAccepted, response.Body.Bytes(), crate.ID, "Bounced")
	})
	t.Run("staleness follow-up", func(t *testing.T) {
		ctx := store.WithActor(store.WithWorkspace(t.Context(), "demo"), store.Actor{ID: "alice", Role: core.ActorHuman})
		st := store.NewMemory()
		requirement, v1, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-crate-staleness", Title: "Crate staleness"}, core.RequirementVersion{
			Content: "# First intent.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Delivery stays aligned."}}, Origin: core.RequirementOriginOperator,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, v1.Version); err != nil {
			t.Fatal(err)
		}
		delivery := core.Task{ID: "crate-stale-delivery", Workspace: "demo", Title: "Stale delivery", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-crate-stale-delivery", State: core.TaskMerged, CreatedAt: time.Now().UTC()}
		if err = st.CreateTask(ctx, delivery); err != nil {
			t.Fatal(err)
		}
		if err = st.AppendEvent(ctx, core.Event{TaskID: delivery.ID, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": requirement.ID, "version": v1.Version})}); err != nil {
			t.Fatal(err)
		}
		v2, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirement.ID, Content: "# Second intent.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Delivery stays aligned with revisions."}}, Origin: core.RequirementOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, v2.Version); err != nil {
			t.Fatal(err)
		}
		if err = st.AppendEvent(ctx, core.Event{TaskID: delivery.ID, Kind: "merge.confirmed", At: time.Now().UTC().Add(time.Minute), Payload: core.JSONPayload(map[string]any{"url": "https://example.test/pull/1", "task_title": delivery.Title})}); err != nil {
			t.Fatal(err)
		}
		server := NewServer(st)
		server.Workspace, server.BearerToken = "demo", "token"
		server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Investigate stale delivery", nil }
		handler := server.Handler()
		read := httptest.NewRecorder()
		handler.ServeHTTP(read, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/requirements/"+requirement.ID, nil)))
		var view requirementView
		if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &view) != nil || len(view.Staleness.Deliveries) != 1 {
			t.Fatalf("requirement status=%d body=%s", read.Code, read.Body.String())
		}
		path := fmt.Sprintf("/v1/requirements/%s/staleness/%s/follow-up", requirement.ID, view.Staleness.Deliveries[0].SignalID)
		post := func() *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodPost, path, nil)
			request.Header.Set("Authorization", "Bearer token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			return response
		}
		created := post()
		var first struct {
			Task core.Task `json:"task"`
		}
		if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &first) != nil || first.Task.ID == "" {
			t.Fatalf("follow-up status=%d body=%s", created.Code, created.Body.String())
		}
		crate, err := st.(store.CrateStore).CreateCrate(ctx, store.CrateInput{Name: "Followed up"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.(store.CrateStore).SetTaskCrate(ctx, first.Task.ID, crate.ID); err != nil {
			t.Fatal(err)
		}
		retried := post()
		want(t, "staleness follow-up", retried.Code, http.StatusOK, retried.Body.Bytes(), crate.ID, "Followed up")
	})
}
