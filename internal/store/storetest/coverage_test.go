package storetest

import (
	"reflect"
	"slices"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func uncoveredMethods(contract reflect.Type, declarations map[string][]string) []string {
	covered := map[string]bool{"IsDurable": true, "Close": true}
	for _, methods := range declarations {
		for _, name := range methods {
			covered[name] = true
		}
	}
	var missing []string
	for i := 0; i < contract.NumMethod(); i++ {
		name := contract.Method(i).Name
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func TestBackendCoverage(t *testing.T) {
	contract := reflect.TypeFor[store.Backend]()
	if missing := uncoveredMethods(contract, suiteMethods); len(missing) > 0 {
		t.Fatalf("Backend methods without a conformance suite: %v", missing)
	}
	for suite, methods := range suiteMethods {
		if len(methods) == 0 {
			t.Errorf("suite %s declares no methods", suite)
		}
		for _, name := range methods {
			if _, ok := contract.MethodByName(name); !ok {
				t.Errorf("suite %s declares nonexistent Backend method %s", suite, name)
			}
		}
	}
}

func TestBackendCoverageRejectsUndeclaredMethod(t *testing.T) {
	type extended interface {
		store.Backend
		UndeclaredBackendMethod()
	}
	missing := uncoveredMethods(reflect.TypeFor[extended](), suiteMethods)
	if !slices.Contains(missing, "UndeclaredBackendMethod") {
		t.Fatal("coverage checker accepted an undeclared backend method")
	}
	// Removing a declaration also fails for an existing method, so the
	// negative probe cannot pass solely by recognizing the sentinel name.
	declarations := map[string][]string{}
	for suite, methods := range suiteMethods {
		for _, method := range methods {
			if method != "CreateTask" {
				declarations[suite] = append(declarations[suite], method)
			}
		}
	}
	if !slices.Contains(uncoveredMethods(reflect.TypeFor[store.Backend](), declarations), "CreateTask") {
		t.Fatal("coverage checker accepted a removed method declaration")
	}
}
