package nextaction

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestKindsIncludesEveryKindConstant(t *testing.T) {
	kinds := Kinds()
	if !slices.IsSorted(kinds) {
		t.Fatal("catalogue kinds are not sorted")
	}
	file, err := parser.ParseFile(token.NewFileSet(), "input.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			for i, name := range value.Names {
				if !strings.HasPrefix(name.Name, "Kind") {
					continue
				}
				if i >= len(value.Values) {
					t.Fatalf("%s has no explicit value", name.Name)
				}
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s is not a string literal", name.Name)
				}
				kind, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(kinds, kind) {
					t.Errorf("%s (%q) absent from catalogue", name.Name, kind)
				}
				count++
			}
		}
	}
	if count == 0 {
		t.Fatal("no kind constants found")
	}
}
