package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// tickerAllowance is one permitted repeating timer: the file and enclosing
// function that start it, the source of its interval argument, and the
// requirement that permits it (component-verification-strategy, Idle-runtime
// checks; req-quiet-idle-runtime AC-1.1, AC-1.3).
type tickerAllowance struct {
	file, function, interval string
	timer, permittedBy       string
}

// idleTickerAllowList names every time.NewTicker and time.Tick under
// internal/ and cmd/conveyord/. A new ticker needs an entry naming the
// requirement that permits it, added in the same change.
var idleTickerAllowList = []tickerAllowance{
	{
		file: "cmd/conveyord/main.go", function: "main", interval: "poolStatsInterval",
		timer:       "pool and database read statistics snapshot every 30 seconds: the PostgreSQL pool line, then the read line on both durable backends",
		permittedBy: "reads pgxpool.Stat() and the store's in-process read ledger, and sends no statement (req-quiet-idle-runtime AC-1.1, AC-1.3; component-runtime, Observability)",
	},
	{
		file: "cmd/conveyord/main_github.go", function: "startIssueIntakePoll", interval: "d",
		timer:       "timed issue intake at the -poll-github interval",
		permittedBy: "req-quiet-idle-runtime AC-3.7: only when the deployment enables it",
	},
	{
		file: "internal/changes/peer_check.go", function: "PeerCheck.ticker", interval: "p.Interval",
		timer:       "daemon-wide peer check every 30 seconds: one PeerMarks statement for every workspace, open task stream, and pending await_review",
		permittedBy: "req-quiet-idle-runtime AC-1.3, AC-2.5: only with runtime.peers on a backend without change notification",
	},
	{
		file: "internal/httpapi/task_event_stream.go", function: "Server.openWake", interval: "streamKeepaliveInterval",
		timer:       "stream keep-alive comment every 25 seconds",
		permittedBy: "req-quiet-idle-runtime AC-1.2: writes to the client and reads nothing",
	},
	{
		file: "internal/workorder/service.go", function: "Service.contextForClaimedOrder", interval: "interval",
		timer:       "lease renewal while a get_work_order request assembles context",
		permittedBy: "req-quiet-idle-runtime AC-1.1: runs only while a request is served",
	},
}

// tickerUse is one reference to time.NewTicker or time.Tick.
type tickerUse struct {
	file, function, interval string
	line                     int
	call                     string
}

func (u tickerUse) String() string {
	return fmt.Sprintf("%s:%d %s in %s with interval %q", u.file, u.line, u.call, u.function, u.interval)
}

// TestIdleTickerGuard fails when a time.NewTicker or time.Tick is added
// under internal/ or cmd/conveyord/ outside idleTickerAllowList, and when an
// entry matches no ticker (component-verification-strategy, Idle-runtime
// checks; req-quiet-idle-runtime AC-1.1, AC-1.3).
func TestIdleTickerGuard(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	uses, err := scanTickers(root, "internal", "cmd/conveyord")
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) == 0 {
		t.Fatal("the guard found no ticker; the scan is broken")
	}
	for _, problem := range checkTickers(uses, idleTickerAllowList) {
		t.Error(problem)
	}
}

// TestIdleTickerGuardReportsUnlistedAndStale proves the guard itself: an
// unlisted time.NewTicker, a time.Tick through a renamed import, and a
// method value of a dot-imported NewTicker are each reported, a listed
// ticker passes, an entry matching nothing is reported, and test files are
// not scanned.
func TestIdleTickerGuardReportsUnlistedAndStale(t *testing.T) {
	root := t.TempDir()
	write := func(path, src string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/a/a.go", `package a

import "time"

type Loop struct{ every time.Duration }

func (l *Loop) Run() {
	t := time.NewTicker(l.every)
	defer t.Stop()
}

func Sneak() {
	go func() {
		t := time.NewTicker(time.Second)
		t.Stop()
	}()
}
`)
	write("internal/b/b.go", `package b

import clock "time"

func Poll() { _ = clock.Tick(clock.Minute) }
`)
	write("cmd/conveyord/c.go", `package main

import . "time"

var start = NewTicker
`)
	write("internal/a/a_test.go", `package a

import "time"

func helper() { _ = time.Tick(time.Second) }
`)
	write("internal/other/other.go", `package other

import "time"

func Wait() { <-time.After(time.Second) }
`)
	uses, err := scanTickers(root, "internal", "cmd/conveyord")
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 4 {
		t.Fatalf("uses=%v, want 4", uses)
	}
	allow := []tickerAllowance{
		{file: "internal/a/a.go", function: "Loop.Run", interval: "l.every", timer: "loop", permittedBy: "fixture"},
		{file: "internal/gone/gone.go", function: "Gone", interval: "x", timer: "removed", permittedBy: "fixture"},
	}
	problems := checkTickers(uses, allow)
	joined := strings.Join(problems, "\n")
	for _, want := range []string{
		`internal/a/a.go:14 time.NewTicker in Sneak with interval "time.Second"`,
		`internal/b/b.go:5 time.Tick in Poll with interval "clock.Minute"`,
		`cmd/conveyord/c.go:5 time.NewTicker in (package scope) with interval ""`,
		`allow-list entry internal/gone/gone.go Gone interval "x" (removed) matches no ticker`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems missing %q:\n%s", want, joined)
		}
	}
	if len(problems) != 4 || strings.Contains(joined, "Loop.Run") {
		t.Fatalf("problems=%d, want 4 without the listed ticker:\n%s", len(problems), joined)
	}
}

// checkTickers returns one problem per unlisted ticker and per allow-list
// entry that matches no ticker.
func checkTickers(uses []tickerUse, allow []tickerAllowance) []string {
	matched := make([]bool, len(allow))
	var problems []string
	for _, use := range uses {
		ok := false
		for i, entry := range allow {
			if entry.file == use.file && entry.function == use.function && entry.interval == use.interval {
				matched[i], ok = true, true
			}
		}
		if !ok {
			problems = append(problems, use.String()+" is not on idleTickerAllowList; an idle daemon must send no statement (req-quiet-idle-runtime AC-1.1, AC-1.3), so add an entry naming the requirement that permits this timer or remove it")
		}
	}
	for i, entry := range allow {
		if !matched[i] {
			problems = append(problems, fmt.Sprintf("allow-list entry %s %s interval %q (%s) matches no ticker; remove it", entry.file, entry.function, entry.interval, entry.timer))
		}
	}
	return problems
}

// scanTickers parses every non-test Go file under the directories below root
// and returns each reference to time.NewTicker or time.Tick, through any
// import name of package time.
func scanTickers(root string, dirs ...string) ([]tickerUse, error) {
	var uses []tickerUse
	fset := token.NewFileSet()
	for _, dir := range dirs {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(dir)), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if name := entry.Name(); name == "testdata" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			uses = append(uses, fileTickers(fset, filepath.ToSlash(rel), file)...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(uses, func(i, j int) bool {
		if uses[i].file != uses[j].file {
			return uses[i].file < uses[j].file
		}
		return uses[i].line < uses[j].line
	})
	return uses, nil
}

func fileTickers(fset *token.FileSet, rel string, file *ast.File) []tickerUse {
	names := map[string]bool{}
	dot := false
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != "time" {
			continue
		}
		switch {
		case spec.Name == nil:
			names["time"] = true
		case spec.Name.Name == ".":
			dot = true
		case spec.Name.Name != "_":
			names[spec.Name.Name] = true
		}
	}
	if len(names) == 0 && !dot {
		return nil
	}
	isTicker := func(name string) bool { return name == "NewTicker" || name == "Tick" }
	var uses []tickerUse
	record := func(node ast.Node, function, name string, stack []ast.Node) {
		use := tickerUse{file: rel, function: function, line: fset.Position(node.Pos()).Line, call: "time." + name}
		if len(stack) > 0 {
			if call, ok := stack[len(stack)-1].(*ast.CallExpr); ok && call.Fun == node && len(call.Args) == 1 {
				var b bytes.Buffer
				_ = printer.Fprint(&b, fset, call.Args[0])
				use.interval = b.String()
			}
		}
		uses = append(uses, use)
	}
	visit := func(root ast.Node, function string) {
		var stack []ast.Node
		ast.Inspect(root, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			switch n := node.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && names[x.Name] && isTicker(n.Sel.Name) {
					record(n, function, n.Sel.Name, stack)
				}
			case *ast.Ident:
				if dot && isTicker(n.Name) {
					if parent, ok := lastNode(stack).(*ast.SelectorExpr); !ok || parent.X == n {
						record(n, function, n.Name, stack)
					}
				}
			}
			stack = append(stack, node)
			return true
		})
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			visit(decl, "(package scope)")
			continue
		}
		name := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) == 1 {
			name = receiverName(fn.Recv.List[0].Type) + "." + name
		}
		if fn.Body != nil {
			visit(fn.Body, name)
		}
	}
	return uses
}

func lastNode(stack []ast.Node) ast.Node {
	if len(stack) == 0 {
		return nil
	}
	return stack[len(stack)-1]
}

func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}
