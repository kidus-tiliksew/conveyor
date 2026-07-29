package taskops_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestProductionLifecycleWritersEnterTaskOps prevents the command-plane
// bypasses that §21.38 removed from the production Store surface. Test-only
// fixture adapters may enter taskops without exposing a production facade.
func TestProductionLifecycleWritersEnterTaskOps(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	legacy := map[string]bool{
		"CancelTask": true, "AcceptReviewDecision": true, "ChangeTaskSetup": true,
		"CreateWorkOrder": true, "CreateStageWorkOrder": true, "CreateReviewRound": true,
		"RetryReviewRound": true, "RecoverInterruptedReviewRound": true,
		"ClaimWorkOrder": true, "RedispatchWorkOrder": true, "RecoverWorkOrder": true, "UpdateWorkOrder": true,
		"RenewWorkerClaim": true, "ReleaseWorkerClaim": true,
	}
	guarded := map[string]bool{
		"CancelTaskCommand": true, "AcceptReviewDecisionCommand": true, "ChangeTaskSetupCommand": true,
		"CreateWorkOrderCommand": true, "CreateStageWorkOrderCommand": true, "CreateReviewRoundCommand": true,
		"RetryReviewRoundCommand": true, "RecoverInterruptedReviewRoundCommand": true,
		"ClaimWorkOrderCommand": true, "RedispatchWorkOrderCommand": true, "RecoverWorkOrderCommand": true, "UpdateWorkOrderCommand": true,
		"RenewWorkerClaimCommand": true, "ReleaseWorkerClaimCommand": true,
	}
	fset := token.NewFileSet()
	for _, rel := range []string{
		filepath.Join("internal", "store", "store.go"),
		filepath.Join("internal", "store", "setup_change.go"),
		filepath.Join("internal", "store", "postgres", "store.go"),
		filepath.Join("internal", "store", "postgres", "setup_change.go"),
		filepath.Join("internal", "store", "postgres", "worker.go"),
	} {
		path := filepath.Join(root, rel)
		parsed, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch item := node.(type) {
			case *ast.FuncDecl:
				if legacy[item.Name.Name] {
					t.Errorf("%s exports bypassable lifecycle method %s", rel, item.Name.Name)
				}
			case *ast.TypeSpec:
				if item.Name.Name != "Store" {
					return true
				}
				iface, ok := item.Type.(*ast.InterfaceType)
				if !ok {
					return true
				}
				for _, field := range iface.Methods.List {
					for _, name := range field.Names {
						if legacy[name.Name] {
							t.Errorf("%s Store interface exposes bypassable lifecycle method %s", rel, name.Name)
						}
					}
				}
			}
			return true
		})
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "dashboard" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(rel, filepath.Join("internal", "store")+string(filepath.Separator)) ||
			rel == filepath.Join("internal", "store", "store.go") {
			return nil
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		parsed, parseErr := parser.ParseFile(fset, path, source, 0)
		if parseErr != nil {
			return parseErr
		}
		hasAdmission := strings.HasPrefix(rel, filepath.Join("internal", "taskops")+string(filepath.Separator)) ||
			strings.Contains(string(source), "taskops.ExecuteWorkOrder") ||
			strings.Contains(string(source), "taskops.ExecuteSetupChange")
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			selector, isSelector := call.Fun.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			if legacy[selector.Sel.Name] {
				if owner, ok := selector.X.(*ast.SelectorExpr); ok && owner.Sel.Name == "Store" {
					t.Errorf("%s calls bypassable Store.%s", rel, selector.Sel.Name)
				}
			}
			if guarded[selector.Sel.Name] && !hasAdmission {
				t.Errorf("%s calls %s without taskops admission", rel, selector.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
