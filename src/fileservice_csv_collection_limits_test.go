package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/plugins/csv"
)

func serviceColumnIndexes(count int) []int {
	columns := make([]int, count)
	for i := range columns {
		columns[i] = i
	}
	return columns
}

func serviceSQLColumns(count int) []CsvSqlColumnConfig {
	columns := make([]CsvSqlColumnConfig, count)
	for i := range columns {
		columns[i] = CsvSqlColumnConfig{Source: i, Name: "c" + strconv.Itoa(i), Type: csv.SQLTypeText, Include: true}
	}
	return columns
}

func serviceRedactColumns(count int) []CsvRedactColumn {
	columns := make([]CsvRedactColumn, count)
	for i := range columns {
		columns[i] = CsvRedactColumn{Index: i, Mode: string(csv.RedactFixed)}
	}
	return columns
}

func TestCSVProjectionRPCRejectsHostileMappingsBeforeServiceAndDialog(t *testing.T) {
	if err := validateCSVProjectColumns(serviceColumnIndexes(csv.MaxTransformColumnMappings)); err != nil {
		t.Fatalf("maximum projection mapping rejected: %v", err)
	}

	previousDialog := csvSaveDialog
	dialogCalls := 0
	csvSaveDialog = func(string, string) (string, error) { dialogCalls++; return "unused", nil }
	t.Cleanup(func() { csvSaveDialog = previousDialog })

	invalid := [][]int{
		serviceColumnIndexes(csv.MaxTransformColumnMappings + 1),
		{0, -1},
		{0, 1, 0},
	}
	var service *FileService
	for _, columns := range invalid {
		if _, err := service.CsvProjectViaDialog("missing", 1, ",", columns); err == nil {
			t.Fatalf("invalid projection mapping accepted: %#v", columns)
		}
	}
	if dialogCalls != 0 {
		t.Fatalf("invalid projection mappings opened save dialog %d times", dialogCalls)
	}
}

func TestCSVSQLRPCCollectionAndStringLimits(t *testing.T) {
	base := CsvSqlConfig{
		Delimiter: ",", TableName: "records", InsertMode: "insert", OnInvalid: "fail",
		Columns: serviceSQLColumns(csv.MaxTransformColumnMappings),
	}
	if _, err := csvSqlOptions(base); err != nil {
		t.Fatalf("maximum SQL mapping rejected: %v", err)
	}

	previousDialog := csvSaveDialog
	dialogCalls := 0
	csvSaveDialog = func(string, string) (string, error) { dialogCalls++; return "unused", nil }
	t.Cleanup(func() { csvSaveDialog = previousDialog })
	var service *FileService
	invalid := []CsvSqlConfig{
		{Delimiter: ",", TableName: "records", InsertMode: "insert", OnInvalid: "fail", Columns: serviceSQLColumns(csv.MaxTransformColumnMappings + 1)},
		{Delimiter: ",", TableName: "records", InsertMode: "insert", OnInvalid: "fail", Columns: []CsvSqlColumnConfig{{Source: -1, Name: "bad", Type: csv.SQLTypeText, Include: true}}},
		{Delimiter: ",", TableName: "records", InsertMode: "insert", OnInvalid: "fail", Columns: []CsvSqlColumnConfig{{Source: 0, Name: "a", Type: csv.SQLTypeText, Include: true}, {Source: 0, Name: "b", Type: csv.SQLTypeText, Include: true}}},
	}
	for _, cfg := range invalid {
		if _, err := service.CsvToSQLConfigViaDialog("missing", 1, cfg); err == nil {
			t.Fatal("invalid SQL mapping reached service access")
		}
	}

	exact := CsvSqlConfig{
		Delimiter: ",", TableName: "r", InsertMode: "insert", OnInvalid: "fail",
		Columns: []CsvSqlColumnConfig{{Source: 0, Name: "id", Type: csv.SQLTypeText, Include: true}},
	}
	baseBytes := len(exact.Delimiter) + len(exact.TableName) + len(exact.InsertMode) + len(exact.OnInvalid) + len(exact.Columns[0].Name) + len(exact.Columns[0].Type)
	exact.NullValues = []string{strings.Repeat("x", csv.MaxTransformConfigStringBytes-baseBytes)}
	if _, err := csvSqlOptions(exact); err != nil {
		t.Fatalf("maximum SQL string budget rejected: %v", err)
	}
	exact.NullValues[0] += "x"
	if _, err := service.CsvToSQLConfigViaDialog("missing", 1, exact); err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("oversized SQL string error = %v", err)
	}
	if dialogCalls != 0 {
		t.Fatalf("invalid SQL configs opened save dialog %d times", dialogCalls)
	}
}

func TestCSVRedactionRPCCollectionAndStringLimits(t *testing.T) {
	if _, err := csvRedactOptions(",", true, serviceRedactColumns(csv.MaxTransformColumnMappings), ""); err != nil {
		t.Fatalf("maximum redaction mapping rejected: %v", err)
	}

	previousDialog := csvSaveDialog
	dialogCalls := 0
	csvSaveDialog = func(string, string) (string, error) { dialogCalls++; return "unused", nil }
	t.Cleanup(func() { csvSaveDialog = previousDialog })
	var service *FileService
	invalid := [][]CsvRedactColumn{
		serviceRedactColumns(csv.MaxTransformColumnMappings + 1),
		{{Index: -1, Mode: string(csv.RedactFixed)}},
		{{Index: 0, Mode: string(csv.RedactFixed)}, {Index: 0, Mode: string(csv.RedactNull)}},
	}
	for _, columns := range invalid {
		if _, err := service.CsvRedactViaDialog("missing", 1, ",", true, columns, ""); err == nil {
			t.Fatal("invalid redaction mapping reached service access")
		}
	}

	mode := string(csv.RedactFixed)
	exactReplacement := strings.Repeat("x", csv.MaxTransformConfigStringBytes-len(",")-len(mode))
	if _, err := csvRedactOptions(",", true, []CsvRedactColumn{{Index: 0, Mode: mode}}, exactReplacement); err != nil {
		t.Fatalf("maximum redaction string budget rejected: %v", err)
	}
	if _, err := service.CsvRedactViaDialog("missing", 1, ",", true, []CsvRedactColumn{{Index: 0, Mode: mode}}, exactReplacement+"x"); err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("oversized redaction string error = %v", err)
	}
	if dialogCalls != 0 {
		t.Fatalf("invalid redaction configs opened save dialog %d times", dialogCalls)
	}
}

func TestCSVAddColumnAndFilterRPCStringLimitsPrecedeServiceAndDialog(t *testing.T) {
	previousDialog := csvSaveDialog
	dialogCalls := 0
	csvSaveDialog = func(string, string) (string, error) { dialogCalls++; return "unused", nil }
	t.Cleanup(func() { csvSaveDialog = previousDialog })

	var service *FileService
	oversized := strings.Repeat("x", csv.MaxTransformConfigStringBytes+1)
	if _, err := service.CsvAddColumnViaDialog("missing", 1, ",", 0, oversized); err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("oversized add-column service error = %v", err)
	}
	if _, err := service.CsvFilterViaDialog("missing", 1, ",", true, 0, "eq", oversized, false); err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("oversized filter value service error = %v", err)
	}
	if _, err := service.CsvFilterViaDialog("missing", 1, ",", true, 0, oversized, "", false); err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("oversized filter operation service error = %v", err)
	}
	if dialogCalls != 0 {
		t.Fatalf("oversized add/filter configs opened save dialog %d times", dialogCalls)
	}
}
