package batch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The interactive reserve of DESIGN §11.1 protects nothing unless batch work is
// marked as batch. That marking is one bool, set in one place, and every other
// test in this package would still pass if a future edit added a second
// construction site that forgot it — the fakes would happily admit the work and
// only a production model axis would notice.
//
// So this test reads the package's own source: exactly one CapacityRequest
// literal exists in non-test code, it sets Batch: true, and nothing anywhere
// assigns the field afterwards.
func TestCapacityRequestIsAlwaysMarkedBatch(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	literals := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				id, ok := node.Type.(*ast.Ident)
				if !ok || id.Name != "CapacityRequest" {
					return true
				}
				literals++
				if !setsBatchTrue(node) {
					t.Errorf("%s: a CapacityRequest literal does not set Batch: true", fset.Position(node.Pos()))
				}
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if ok && sel.Sel.Name == "Batch" {
						t.Errorf("%s: Batch is assigned outside the single construction site",
							fset.Position(node.Pos()))
					}
				}
			}
			return true
		})
	}

	if literals != 1 {
		t.Errorf("found %d CapacityRequest literals in non-test code, want exactly 1 "+
			"(Service.capacityRequest); a second construction site is a second chance to forget the flag",
			literals)
	}
}

func setsBatchTrue(lit *ast.CompositeLit) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Batch" {
			continue
		}
		val, ok := kv.Value.(*ast.Ident)
		return ok && val.Name == "true"
	}
	return false
}
