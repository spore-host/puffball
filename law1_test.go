package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Law 1: only pkg/gate makes billable calls. These checks read every Go file
// in the repo and fail if a billable operation, the Cost Explorer client, or
// a raw Braket SDK client (which carries CreateQuantumTask) appears anywhere
// else.
//
// scripts/probe is the G0 probe tool, which predates pkg/gate and has its
// own --real gating (#28, #30). pkg/testutil may define fake Create methods.

const (
	braketSDK = "github.com/aws/aws-sdk-go-v2/service/braket"
	ceSDK     = "github.com/aws/aws-sdk-go-v2/service/costexplorer"
)

var billableMethods = map[string]bool{"CreateQuantumTask": true, "CreateJob": true}

func under(path string, dirs ...string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(path, d+"/") {
			return true
		}
	}
	return false
}

func TestLaw1OnlyGateMakesBillableCalls(t *testing.T) {
	fset := token.NewFileSet()
	sawGateCall := false
	files := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "testdata" || name == "vendor" || (strings.HasPrefix(name, ".") && path != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		path = filepath.ToSlash(path)
		isTest := strings.HasSuffix(path, "_test.go")
		inGate := under(path, "pkg/gate")
		exempt := inGate || under(path, "scripts/probe")

		braketName := ""
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == ceSDK && !exempt {
				t.Errorf("%s imports Cost Explorer; every Cost Explorer request is billable and belongs in pkg/gate (law 1)", path)
			}
			// A dot-import makes NewFromConfig and friends bare identifiers
			// that the selector checks below can't see.
			if (p == braketSDK || p == ceSDK) && imp.Name != nil && imp.Name.Name == "." {
				t.Errorf("%s dot-imports %s; import it by name so law 1 can be checked", path, p)
			}
			if p == braketSDK {
				braketName = "braket"
				if imp.Name != nil {
					braketName = imp.Name.Name
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			// reflect's MethodByName("CreateQuantumTask") and the like.
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				// This file has to name them.
				if v, err := strconv.Unquote(lit.Value); err == nil && billableMethods[v] && !exempt && !under(path, "pkg/testutil") && path != "law1_test.go" {
					t.Errorf("%s: string %q names a billable method; reflection must not reach it (law 1)", fset.Position(lit.Pos()), v)
				}
				return true
			}
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if billableMethods[sel.Sel.Name] {
				switch {
				case inGate && !isTest:
					sawGateCall = true
				case exempt, under(path, "pkg/testutil"):
				default:
					t.Errorf("%s: %s is billable and may only be called from pkg/gate (law 1)", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			// A raw SDK client carries CreateQuantumTask. pkg/braket wraps
			// it read-only; tests may build one against a local server.
			if x, ok := sel.X.(*ast.Ident); ok && braketName != "" && x.Name == braketName &&
				(sel.Sel.Name == "NewFromConfig" || sel.Sel.Name == "New") &&
				!exempt && !isTest && !under(path, "pkg/braket") {
				t.Errorf("%s: %s.%s builds a Braket client with billable methods; use pkg/braket's Factory (read) or pkg/gate (billable)", fset.Position(sel.Pos()), x.Name, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 || !sawGateCall {
		t.Fatalf("scanned %d files and found no billable call in pkg/gate; a check that finds nothing must not pass", files)
	}
}
