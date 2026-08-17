package main

import (
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/plugins/csv"
)

func TestCSVSimpleSQLOptionsBoundTableName(t *testing.T) {
	maximum := strings.Repeat("😀", csv.MaxSQLIdentifierRunes)
	opts, err := csvSimpleSQLOptions(",", maximum, true, true)
	if err != nil {
		t.Fatalf("maximum table name rejected: %v", err)
	}
	if opts.TableName != maximum || opts.Delimiter != ',' || !opts.HasHeader || !opts.IncludeCreateTable {
		t.Fatalf("options = %+v", opts)
	}

	if _, err := csvSimpleSQLOptions(",", maximum+"x", true, true); err == nil || !strings.Contains(err.Error(), "byte") {
		t.Fatalf("maximum+1 table name error = %v", err)
	}
	if _, err := csvSimpleSQLOptions(",", strings.Repeat("x", csv.MaxSQLIdentifierRunes+1), true, true); err == nil || !strings.Contains(err.Error(), "characters") {
		t.Fatalf("character-overflow error = %v", err)
	}
}

func TestCSVSimpleSQLRejectsOversizedTableBeforeServiceOrDialog(t *testing.T) {
	previousDialog := csvSaveDialog
	dialogCalls := 0
	csvSaveDialog = func(string, string) (string, error) {
		dialogCalls++
		return "unused", nil
	}
	t.Cleanup(func() { csvSaveDialog = previousDialog })

	tableName := strings.Repeat("x", csv.MaxSQLIdentifierBytes+1)
	var service *FileService
	if _, err := service.CsvToSQLPreview("f1", ",", tableName, true, false); err == nil {
		t.Fatal("oversized preview table name reached service access")
	}
	if _, err := service.CsvToSQLViaDialog("f1", 1, ",", tableName, true, false); err == nil {
		t.Fatal("oversized transform table name reached service access")
	}
	if dialogCalls != 0 {
		t.Fatalf("oversized table name opened save dialog %d times", dialogCalls)
	}
}
