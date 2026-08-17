package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/quarry/quarry-wails3/internal/plugins/csv"
)

func TestCsvExportSQLiteViaDialogFailsBeforeServiceOrDialogAccess(t *testing.T) {
	var service *FileService
	_, err := service.CsvExportSQLiteViaDialog("missing", 0, ",", true, "records", true)
	if !errors.Is(err, csv.ErrSQLiteExportSecurePublicationUnavailable) {
		t.Fatalf("error = %v, want secure-publication unavailable", err)
	}
}

func TestCsvExportXLSXViaDialogFailsBeforeServiceOrDialogAccess(t *testing.T) {
	var service *FileService
	_, err := service.CsvExportXLSXViaDialog("missing", 0, ",", true, "data", true)
	if !errors.Is(err, csv.ErrXLSXExportSecureScratchUnavailable) {
		t.Fatalf("error = %v, want secure-scratch unavailable", err)
	}
}

func TestEnabledTransformWrappersClassifyPublicationErrors(t *testing.T) {
	files := map[string][]string{
		"fileservice_csv.go": {
			"CsvProjectViaDialog",
			"CsvAddColumnViaDialog",
			"CsvToSQLViaDialog",
			"CsvRedactViaDialog",
			"CsvFilterViaDialog",
			"CsvDedupeViaDialog",
			"CsvSampleViaDialog",
			"CsvExportJSONLViaDialog",
			"CsvToSQLConfigViaDialog",
		},
		"fileservice_sql.go": {"SqlReshapeInsertsViaDialog"},
	}

	for filename, functions := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filename, err)
		}
		found := make(map[string]bool, len(functions))
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			wanted := false
			for _, name := range functions {
				if function.Name.Name == name {
					wanted = true
					break
				}
			}
			if !wanted {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				identifier, ok := call.Fun.(*ast.Ident)
				if ok && identifier.Name == "transformResultAfterPublication" {
					found[function.Name.Name] = true
				}
				return true
			})
		}
		for _, name := range functions {
			if !found[name] {
				t.Errorf("%s in %s does not preserve exact-path PublicationError evidence", name, filename)
			}
		}
	}
}
