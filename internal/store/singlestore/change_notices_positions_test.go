package singlestore

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The task-event commit guard (component-persistence invariant 42). Every
// transaction that can write a task event registers notice state and commits
// only through commitTx, whose preparation advances task_event_positions and
// records the events' witnesses. writeRow refuses a task event in a
// transaction without notice state; this source guard refuses a commit that
// skips the preparation.

// commitSiteFindings reports every Commit call outside commitTx, and every
// function that begins a transaction and commits it without registering its
// notice state first. finishNotices commits a notice batch, not a
// transaction, and db_timing.go wraps the driver below database/sql.
func commitSiteFindings(sources map[string]string) ([]string, error) {
	fset := token.NewFileSet()
	var findings []string
	for name, source := range sources {
		if driverWrapperFiles[name] {
			continue
		}
		file, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			begins, registers, commits := false, false, false
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				var receiver ast.Expr
				switch target := call.Fun.(type) {
				case *ast.Ident:
					callee = target.Name
				case *ast.SelectorExpr:
					callee, receiver = target.Sel.Name, target.X
				}
				switch callee {
				case "BeginTx":
					begins = true
				case "beginNotices":
					registers = true
				case "commitTx":
					commits = true
				case "Commit":
					if ident, ok := receiver.(*ast.Ident); ok && ident.Name == "batch" && fn.Name.Name == "finishNotices" {
						return true
					}
					if fn.Name.Name == "commitTx" {
						return true
					}
					findings = append(findings, fmt.Sprintf("%s:%d: %s commits a transaction outside commitTx", name, fset.Position(call.Pos()).Line, fn.Name.Name))
				}
				return true
			})
			if begins && commits && !registers {
				findings = append(findings, fmt.Sprintf("%s:%d: %s commits a transaction it began without registering notice state", name, fset.Position(fn.Pos()).Line, fn.Name.Name))
			}
		}
	}
	sort.Strings(findings)
	return findings, nil
}

// TestTaskEventCommitSites: the package's transactions commit only through
// commitTx, after registering notice state. Today these are withTx,
// RunVerificationDelivery, the per-key transactions of a verification
// delivery pass (withKeyTx), and the dismissal archive repair.
func TestTaskEventCommitSites(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		sources[file] = string(body)
	}
	findings, err := commitSiteFindings(sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("task-event commit guard:\n%s", strings.Join(findings, "\n"))
	}
	// The guard reaches every commit site it names.
	for _, site := range []string{"store.go:withTx", "verification.go:RunVerificationDelivery", "locks.go:withKeyTx", "migrate_document_dismissal.go:migrateDocumentDismissalArchival"} {
		file, function, _ := strings.Cut(site, ":")
		if !strings.Contains(sources[file], "func ") || !strings.Contains(sources[file], function) || !strings.Contains(sources[file], "commitTx(") {
			t.Errorf("%s no longer commits through commitTx", site)
		}
	}
}

// TestTaskEventCommitGuardRejectsBypass: a commit that skips the
// preparation, or a committed transaction without notice state, fails the
// guard.
func TestTaskEventCommitGuardRejectsBypass(t *testing.T) {
	const header = "package singlestore\nimport (\n\"context\"\n\"database/sql\"\n)\n"
	for name, tc := range map[string]struct{ body, want string }{
		"raw commit after an event": {
			`func (s *Store) Deliver(ctx context.Context) error { tx, err := s.db.BeginTx(ctx, nil); if err != nil { return err }; s.beginNotices(tx); return tx.Commit() }`,
			"Deliver commits a transaction outside commitTx",
		},
		"commit without notice state": {
			`func (s *Store) Repair(ctx context.Context) error { tx, err := s.db.BeginTx(ctx, nil); if err != nil { return err }; return commitTx(ctx, tx) }`,
			"Repair commits a transaction it began without registering notice state",
		},
		"commit in a helper": {
			`func finish(tx *sql.Tx) error { return tx.Commit() }`,
			"finish commits a transaction outside commitTx",
		},
	} {
		t.Run(name, func(t *testing.T) {
			findings, err := commitSiteFindings(map[string]string{"bypass.go": header + tc.body})
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != 1 || !strings.Contains(findings[0], tc.want) {
				t.Fatalf("findings=%q, want one naming %q", findings, tc.want)
			}
		})
	}
	ok := `func (s *Store) Fine(ctx context.Context) error { tx, err := s.db.BeginTx(ctx, nil); if err != nil { return err }; s.beginNotices(tx); defer finishNotices(tx, false); return commitTx(ctx, tx) }
func (s *Store) Lock(ctx context.Context) error { tx, err := s.db.BeginTx(ctx, nil); if err != nil { return err }; return tx.Rollback() }`
	if findings, err := commitSiteFindings(map[string]string{"fine.go": header + ok}); err != nil || len(findings) != 0 {
		t.Fatalf("a registered commit through commitTx and a rollback-only lock: findings=%q err=%v", findings, err)
	}
}

// TestAcceptTaskEventPosition: a peer accepts the highest witnessed position
// up to the counter before the first invisible event, in position (commit)
// order. Event IDs play no part, so a lower-ID event committed later blocks
// its position like any other.
func TestAcceptTaskEventPosition(t *testing.T) {
	for _, tc := range []struct {
		name           string
		floor, counter int64
		witnessed      map[int64]bool
		want           int64
	}{
		{"idle", 7, 7, nil, 7},
		{"one visible position", 7, 8, map[int64]bool{8: true}, 8},
		{"counter visible before its event", 7, 8, map[int64]bool{8: false}, 7},
		{"later visible events do not hide an earlier invisible one", 7, 10, map[int64]bool{8: false, 9: true, 10: true}, 7},
		{"earlier visible, later invisible", 7, 10, map[int64]bool{8: true, 9: true, 10: false}, 9},
		{"pruned witnesses below a visible one", 3, 10, map[int64]bool{10: true}, 10},
		{"counter without witnesses is not accepted", 0, 12, nil, 0},
		{"fresh peer with the counter's witness visible", 0, 12, map[int64]bool{12: true}, 12},
		{"fresh peer with the counter's witness invisible", 0, 12, map[int64]bool{12: false}, 0},
		{"witness above the counter read is ignored", 7, 8, map[int64]bool{8: true, 9: true}, 8},
		{"counter moved back", 20, 5, map[int64]bool{5: true}, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceptTaskEventPosition(tc.floor, tc.counter, tc.witnessed); got != tc.want {
				t.Fatalf("acceptTaskEventPosition(%d, %d, %v) = %d, want %d", tc.floor, tc.counter, tc.witnessed, got, tc.want)
			}
		})
	}
}

// TestPeerMarksStatementBoundsWitnesses: the one statement bounds each
// accepted workspace's witnesses by its accepted position in both witness
// branches, and reaches events_columnstore only through an inner join on the
// witness rows.
func TestPeerMarksStatementBoundsWitnesses(t *testing.T) {
	query, args := peerMarksStatement(nil)
	if len(args) != 0 || strings.Count(query, "WHERE w.position > 0") != 2 || strings.Count(query, "SELECT") != 4 {
		t.Fatalf("fresh statement=%q args=%v", query, args)
	}
	query, args = peerMarksStatement(map[string]int64{"b": 4, "a": 9})
	if strings.Count(query, "WHERE w.position > CASE w.workspace_id WHEN ? THEN ? WHEN ? THEN ? ELSE 0 END") != 2 || fmt.Sprint(args) != "[a 9 b 4 a 9 b 4]" {
		t.Fatalf("accepted statement=%q args=%v", query, args)
	}
	if strings.Contains(query, ";") || strings.Contains(strings.ToUpper(query), "LEFT JOIN") || strings.Count(query, "events_columnstore") != 1 {
		t.Fatalf("the peer read must be one statement that reaches events through one inner join: %q", query)
	}
}
