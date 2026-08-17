package main

import (
	"math"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/plugins/csv"
)

func TestParseCSVDelimiterIsExactAndFailClosed(t *testing.T) {
	valid := map[string]rune{
		",": ',', "comma": ',', "\t": '\t', "\\t": '\t', "tab": '\t',
		";": ';', "semicolon": ';', "|": '|', "pipe": '|', " ": ' ', "space": ' ', "§": '§',
	}
	for input, want := range valid {
		input, want := input, want
		t.Run("valid-"+input, func(t *testing.T) {
			got, err := parseCSVDelimiter(input)
			if err != nil || got != want {
				t.Fatalf("delimiter %q = %q, %v; want %q", input, got, err, want)
			}
		})
	}
	for _, input := range []string{"", "||", "comma,", "Comma", " tab", "tab ", "\x00", "\r", "\n", "\"", "�", string([]byte{0xff}), strings.Repeat("x", 1<<20)} {
		if _, err := parseCSVDelimiter(input); err == nil {
			t.Fatalf("delimiter %q was accepted", input)
		}
	}
}

func TestCSVFilterOptionsRejectUnknownOperationsExactly(t *testing.T) {
	for _, op := range []string{"", "equals", "EQ", " eq", "eq ", "future"} {
		if _, err := csvFilterOptions(",", true, 0, op, "x", false); err == nil || !strings.Contains(err.Error(), "filter operation") {
			t.Fatalf("operation %q error = %v", op, err)
		}
	}
	if _, err := csvFilterOptions(",", true, -1, "eq", "x", false); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative column error = %v", err)
	}
	for _, op := range []string{"eq", "ne", "contains", "gt", "lt", "empty", "nonempty"} {
		if _, err := csvFilterOptions(",", true, 0, op, "x", false); err != nil {
			t.Fatalf("valid operation %q rejected: %v", op, err)
		}
	}
}

func TestCSVSQLInsertModesAreExact(t *testing.T) {
	base := CsvSqlConfig{
		Delimiter: ",",
		TableName: "records",
		Columns:   []CsvSqlColumnConfig{{Source: 0, Name: "id", Type: csv.SQLTypeText, Include: true}},
	}
	valid := map[string]csv.SQLInsertMode{
		"insert": csv.SQLInsertModeInsert, "ignore": csv.SQLInsertModeInsertIgnore, "replace": csv.SQLInsertModeReplace,
	}
	for mode, wantMode := range valid {
		cfg := base
		cfg.InsertMode = mode
		opts, err := csvSqlOptions(cfg)
		if err != nil || opts.InsertMode != wantMode {
			t.Fatalf("mode %q = plugin mode %q, err %v; want %q", mode, opts.InsertMode, err, wantMode)
		}
	}
	for _, mode := range []string{"", "INSERT", " insert", "insert ", "upsert", "future"} {
		cfg := base
		cfg.InsertMode = mode
		if _, err := csvSqlOptions(cfg); err == nil || !strings.Contains(err.Error(), "insert mode") {
			t.Fatalf("mode %q error = %v", mode, err)
		}
	}
}

func TestCSVSQLConfigRejectsUnknownBooleanTypeBeforeServiceAccess(t *testing.T) {
	cfg := CsvSqlConfig{
		Delimiter: ",", TableName: "records", InsertMode: "insert",
		Columns: []CsvSqlColumnConfig{{Source: 0, Name: "active", Type: "BOOLISH", Include: true}},
	}
	if _, err := csvSqlOptions(cfg); err == nil || !strings.Contains(err.Error(), "invalid SQL column type") {
		t.Fatalf("error = %v, want invalid SQL column type", err)
	}
	var service *FileService
	if _, err := service.CsvToSQLConfigViaDialog("missing", 0, cfg); err == nil || !strings.Contains(err.Error(), "invalid SQL column type") {
		t.Fatalf("service error = %v, want validation before service access", err)
	}
}

func TestCSVValidationPrecedesServiceAndDialogAccess(t *testing.T) {
	var service *FileService
	if _, err := service.CsvProjectViaDialog("missing", 0, "||", []int{0}); err == nil {
		t.Fatal("invalid project delimiter reached service access")
	}
	if _, err := service.CsvAddColumnViaDialog("missing", 0, ",", -1, "x"); err == nil {
		t.Fatal("invalid add position reached service access")
	}
	if _, err := service.CsvFilterViaDialog("missing", 0, ",", true, 0, "equals", "x", false); err == nil {
		t.Fatal("invalid filter operation reached service access")
	}
	if _, err := service.CsvToSQLViaDialog("missing", 0, "\x00", "records", true, false); err == nil {
		t.Fatal("invalid SQL delimiter reached service access")
	}
	cfg := CsvSqlConfig{
		Delimiter: ",", TableName: "records", InsertMode: "upsert",
		Columns: []CsvSqlColumnConfig{{Source: 0, Name: "id", Type: csv.SQLTypeText, Include: true}},
	}
	if _, err := service.CsvToSQLConfigViaDialog("missing", 0, cfg); err == nil {
		t.Fatal("invalid insert mode reached service access")
	}
}

func TestCSVAddColumnOptionsValidatePositionWithoutAllocation(t *testing.T) {
	opts, err := csvAddColumnOptions(",", 2, "new")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Delimiter != ',' || opts.Position != 2 || opts.Value != "new" {
		t.Fatalf("options = %+v", opts)
	}
	if _, err := csvAddColumnOptions(",", csv.MaxAddColumnPosition+1, "new"); err == nil {
		t.Fatal("oversized position accepted")
	}
}

func TestCSVSQLBatchLimitFailsBeforeServiceAndDialogAccess(t *testing.T) {
	cfg := CsvSqlConfig{
		Delimiter: ",", TableName: "records", InsertMode: "insert", BatchSize: math.MaxInt,
		Columns: []CsvSqlColumnConfig{{Source: 0, Name: "id", Type: csv.SQLTypeText, Include: true}},
	}
	if _, err := csvSqlOptions(cfg); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("options error = %v, want maximum batch-size rejection", err)
	}
	previousDialog := csvSaveDialog
	dialogCalls := 0
	csvSaveDialog = func(string, string) (string, error) { dialogCalls++; return "unused", nil }
	defer func() { csvSaveDialog = previousDialog }()
	var service *FileService
	if _, err := service.CsvToSQLConfigViaDialog("missing", 0, cfg); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("service error = %v, want validation before service access", err)
	}
	if dialogCalls != 0 {
		t.Fatalf("invalid batch size opened save dialog %d times", dialogCalls)
	}
}

func TestCSVPreviewRowLimitFailsBeforeServiceAccess(t *testing.T) {
	for _, requested := range []int{-1, csvPreviewMaxRows + 1, math.MaxInt} {
		if _, err := normalizeCSVPreviewRows(requested); err == nil {
			t.Fatalf("row limit %d was accepted", requested)
		}
		var service *FileService
		if _, err := service.CsvPreview("missing", ",", false, requested); err == nil || !strings.Contains(err.Error(), "row limit") {
			t.Fatalf("CsvPreview(%d) error = %v, want validation before service access", requested, err)
		}
		if _, err := service.CsvMarkdownPreview("missing", ",", false, requested); err == nil || !strings.Contains(err.Error(), "row limit") {
			t.Fatalf("CsvMarkdownPreview(%d) error = %v, want validation before service access", requested, err)
		}
	}
	if got, err := normalizeCSVPreviewRows(0); err != nil || got != csvPreviewDefaultRows {
		t.Fatalf("default row limit = %d, %v", got, err)
	}
	if got, err := normalizeCSVPreviewRows(csvPreviewMaxRows); err != nil || got != csvPreviewMaxRows {
		t.Fatalf("maximum row limit = %d, %v", got, err)
	}
}
