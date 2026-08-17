package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestProductionSurfacesDoNotWireSourceMutation(t *testing.T) {
	if err := inspectSourceTree(".", func(path string) bool {
		return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
	}, func(path string, data []byte) {
		violations, err := productionGoMutationViolations(path, data)
		if err != nil {
			t.Errorf("parse production Go source %q: %v", path, err)
			return
		}
		for _, violation := range violations {
			t.Errorf("production Go source %q wires disabled source mutation at %s", path, violation)
		}
	}); err != nil {
		t.Fatal(err)
	}

	forbiddenFrontend := []string{"SavePatch(", "swapOriginal", "SwapOriginal"}
	if err := inspectSourceTree(filepath.Join("frontend", "src"), func(path string) bool {
		if ext := filepath.Ext(path); ext != ".ts" && ext != ".tsx" {
			return false
		}
		return true
	}, func(path string, data []byte) {
		for _, forbidden := range forbiddenFrontend {
			if strings.Contains(string(data), forbidden) {
				t.Errorf("frontend source %q wires disabled source mutation through %q", path, forbidden)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFrontendSQLReplaceOnlyCallsSerializationAwareBridge(t *testing.T) {
	forbidden := []string{
		"ReplaceBatchPlainFileAtomic", "ReplaceBatchRegexpFileAtomic",
		"ReplaceBatchPlainFile", "ReplaceBatchRegexpFile",
		"ReplacePlainFile", "ReplaceRegexpFile",
		"ReplacePlainFileInPlace", "ConvertEncodingFile", "ConvertLineEndingsFile",
	}
	if err := inspectSourceTree(filepath.Join("frontend", "src"), func(path string) bool {
		ext := filepath.Ext(path)
		return ext == ".ts" || ext == ".tsx"
	}, func(path string, data []byte) {
		source := string(data)
		for _, symbol := range forbidden {
			if strings.Contains(source, symbol) {
				t.Errorf("frontend source %q references generic source transform %q; SQL replacement must go through SqlReplaceViaDialog", path, symbol)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDormantReplacePackageExportsNoTransformMutationEntryPoints(t *testing.T) {
	forbidden := map[string]struct{}{
		"ReplacePlain": {}, "ReplaceRegexp": {},
		"ReplaceBatchPlain": {}, "ReplaceBatchRegexp": {},
		"ReplacePlainFile": {}, "ReplaceRegexpFile": {},
		"ReplaceBatchPlainFile": {}, "ReplaceBatchRegexpFile": {},
		"ReplaceBatchPlainFileAtomic": {}, "ReplaceBatchRegexpFileAtomic": {},
		"ReplacePlainFileInPlace": {}, "ConvertEncodingFile": {},
		"ConvertLineEndingsFile": {},
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(filepath.Join("internal", "replace"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if _, blocked := forbidden[function.Name.Name]; blocked {
				t.Errorf("dormant replacement package exports mutation entry point %s at %s", function.Name.Name, fset.Position(function.Name.Pos()))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

const (
	inplaceImportPath = "github.com/quarry/quarry-wails3/internal/inplace"
	replaceImportPath = "github.com/quarry/quarry-wails3/internal/replace"
)

// productionGoMutationViolations uses Go syntax and exact import paths so an
// import alias, whitespace change, or a field assignment cannot evade the
// source-preservation regression gate.
func productionGoMutationViolations(filename string, data []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, data, 0)
	if err != nil {
		return nil, err
	}

	inplaceAliases := make(map[string]struct{})
	replaceAliases := make(map[string]struct{})
	var violations []string
	for _, spec := range file.Imports {
		importPath, unquoteErr := strconv.Unquote(spec.Path.Value)
		if unquoteErr != nil {
			return nil, fmt.Errorf("unquote import at %s: %w", fset.Position(spec.Path.Pos()), unquoteErr)
		}
		if importPath == replaceImportPath {
			alias := "replace"
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			switch alias {
			case "_":
				// A blank import cannot call replacement primitives.
			case ".":
				violations = append(violations, fmt.Sprintf("%s: dot-imports %s", fset.Position(spec.Pos()), replaceImportPath))
			default:
				replaceAliases[alias] = struct{}{}
			}
			continue
		}
		if importPath != inplaceImportPath {
			continue
		}

		alias := "inplace"
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		switch alias {
		case "_":
			// A blank import cannot call mutation primitives.
		case ".":
			violations = append(violations, fmt.Sprintf("%s: dot-imports %s", fset.Position(spec.Pos()), inplaceImportPath))
		default:
			inplaceAliases[alias] = struct{}{}
		}
	}

	ast.Inspect(file, func(node ast.Node) bool {
		switch current := node.(type) {
		case *ast.SelectorExpr:
			identifier, ok := current.X.(*ast.Ident)
			if !ok {
				break
			}
			if _, imported := inplaceAliases[identifier.Name]; imported &&
				(current.Sel.Name == "Apply" || current.Sel.Name == "Recover") {
				violations = append(violations, fmt.Sprintf("%s: references %s.%s", fset.Position(current.Pos()), identifier.Name, current.Sel.Name))
			}
			if _, imported := replaceAliases[identifier.Name]; imported &&
				!allowedProductionReplaceSelector(current.Sel.Name) {
				violations = append(violations, fmt.Sprintf("%s: references %s.%s; only serialization-aware SQL replacement may be wired", fset.Position(current.Pos()), identifier.Name, current.Sel.Name))
			}
		case *ast.KeyValueExpr:
			if key, ok := current.Key.(*ast.Ident); ok && key.Name == "SwapOriginal" {
				violations = append(violations, fmt.Sprintf("%s: initializes SwapOriginal", fset.Position(current.Pos())))
			}
		case *ast.AssignStmt:
			for _, target := range current.Lhs {
				if selector, ok := target.(*ast.SelectorExpr); ok && selector.Sel.Name == "SwapOriginal" {
					violations = append(violations, fmt.Sprintf("%s: assigns SwapOriginal", fset.Position(target.Pos())))
				}
			}
		}
		return true
	})

	return violations, nil
}

func allowedProductionReplaceSelector(name string) bool {
	switch name {
	case "ReplaceSQLPlainFileAtomic", "FileOptions", "BatchOptions", "Progress":
		return true
	default:
		return false
	}
}

func TestProductionGoMutationScanIsAliasAndAssignmentAware(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		violations int
	}{
		{
			name: "import alias",
			source: `package sample
import guarded "github.com/quarry/quarry-wails3/internal/inplace"
var apply = guarded.Apply
`,
			violations: 1,
		},
		{
			name: "dormant replace import",
			source: `package sample
import "github.com/quarry/quarry-wails3/internal/replace"
var _ = replace.PreviewPlain
`,
			violations: 1,
		},
		{
			name: "allowed sql replacement bridge",
			source: `package sample
import "github.com/quarry/quarry-wails3/internal/replace"
var _ = replace.ReplaceSQLPlainFileAtomic
`,
		},
		{
			name: "replace dot import",
			source: `package sample
import . "github.com/quarry/quarry-wails3/internal/replace"
`,
			violations: 1,
		},
		{
			name: "dot import",
			source: `package sample
import . "github.com/quarry/quarry-wails3/internal/inplace"
`,
			violations: 1,
		},
		{
			name: "composite field",
			source: `package sample
var value = struct{ SwapOriginal bool }{SwapOriginal: enabled}
var enabled bool
`,
			violations: 1,
		},
		{
			name: "field assignment",
			source: `package sample
func configure(value *struct{ SwapOriginal bool }, enabled bool) {
	value.SwapOriginal = enabled
}
`,
			violations: 1,
		},
		{
			name: "read only",
			source: `package sample
func inspect(value struct{ SwapOriginal bool }) bool { return value.SwapOriginal }
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			violations, err := productionGoMutationViolations(test.name+".go", []byte(test.source))
			if err != nil {
				t.Fatal(err)
			}
			if len(violations) != test.violations {
				t.Fatalf("violations = %v, want %d", violations, test.violations)
			}
		})
	}
}

func inspectSourceTree(root string, accept func(string) bool, inspect func(string, []byte)) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".idea", "node_modules", "dist", "col-review":
				return filepath.SkipDir
			}
			return nil
		}
		if !accept(path) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		inspect(path, data)
		return nil
	})
}
