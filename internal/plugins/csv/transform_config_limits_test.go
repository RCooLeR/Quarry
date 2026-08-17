package csv

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
)

func sequentialColumnIndexes(count int) []int {
	columns := make([]int, count)
	for i := range columns {
		columns[i] = i
	}
	return columns
}

func TestProjectTransformConfigLimits(t *testing.T) {
	maxColumns := sequentialColumnIndexes(MaxTransformColumnMappings)
	record := strings.TrimSuffix(strings.Repeat("x,", MaxTransformColumnMappings), ",") + "\n"
	if summary, err := ProjectColumns(context.Background(), strings.NewReader(record), io.Discard, ProjectOptions{Columns: maxColumns}); err != nil {
		t.Fatalf("maximum projection mapping rejected: %v", err)
	} else if summary.ColumnsWritten != MaxTransformColumnMappings {
		t.Fatalf("columns written = %d, want %d", summary.ColumnsWritten, MaxTransformColumnMappings)
	}

	invalidMappings := []struct {
		name    string
		columns []int
		part    string
	}{
		{name: "maximum plus one", columns: sequentialColumnIndexes(MaxTransformColumnMappings + 1), part: "maximum"},
		{name: "negative", columns: []int{0, -1}, part: "negative"},
		{name: "duplicate", columns: []int{0, 1, 0}, part: "duplicates"},
	}
	for _, test := range invalidMappings {
		t.Run(test.name, func(t *testing.T) {
			source := &countReadsReader{reader: strings.NewReader("a,b\n")}
			output := &countWritesWriter{}
			_, err := ProjectColumns(context.Background(), source, output, ProjectOptions{Columns: test.columns})
			if err == nil || !strings.Contains(err.Error(), test.part) {
				t.Fatalf("error = %v, want %q", err, test.part)
			}
			if source.reads != 0 || output.writes != 0 {
				t.Fatalf("invalid projection touched I/O: reads=%d writes=%d", source.reads, output.writes)
			}
		})
	}

	if _, err := ProjectColumns(context.Background(), strings.NewReader(""), io.Discard, ProjectOptions{
		Columns: []int{0}, MissingValue: strings.Repeat("x", MaxTransformConfigStringBytes),
	}); err != nil {
		t.Fatalf("maximum projection string budget rejected: %v", err)
	}
	source := &countReadsReader{reader: strings.NewReader("a\n")}
	output := &countWritesWriter{}
	_, err := ProjectColumns(context.Background(), source, output, ProjectOptions{
		Columns: []int{0}, MissingValue: strings.Repeat("x", MaxTransformConfigStringBytes+1),
	})
	if err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("oversized projection string error = %v", err)
	}
	if source.reads != 0 || output.writes != 0 {
		t.Fatalf("oversized projection strings touched I/O: reads=%d writes=%d", source.reads, output.writes)
	}
}

func TestRedactTransformConfigLimits(t *testing.T) {
	columns := make(map[int]RedactMode, MaxTransformColumnMappings)
	for i := 0; i < MaxTransformColumnMappings; i++ {
		columns[i] = RedactFixed
	}
	if err := ValidateRedactOptions(RedactOptions{Delimiter: ',', Columns: columns}); err != nil {
		t.Fatalf("maximum redaction mapping rejected: %v", err)
	}

	tooMany := make(map[int]RedactMode, MaxTransformColumnMappings+1)
	for i := 0; i <= MaxTransformColumnMappings; i++ {
		tooMany[i] = RedactFixed
	}
	if err := ValidateRedactOptions(RedactOptions{Delimiter: ',', Columns: tooMany}); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("oversized redaction mapping error = %v", err)
	}
	if err := ValidateRedactOptions(RedactOptions{Delimiter: ',', Columns: map[int]RedactMode{-1: RedactFixed}}); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative redaction mapping error = %v", err)
	}

	modeBytes := len(string(RedactFixed))
	exact := RedactOptions{
		Delimiter: ',', Columns: map[int]RedactMode{0: RedactFixed},
		Replacement: strings.Repeat("x", MaxTransformConfigStringBytes-modeBytes),
	}
	if err := ValidateRedactOptions(exact); err != nil {
		t.Fatalf("maximum redaction string budget rejected: %v", err)
	}
	exact.Replacement += "x"
	if err := ValidateRedactOptions(exact); err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("oversized redaction string error = %v", err)
	}
}

func TestSQLTransformConfigLimits(t *testing.T) {
	columns := make([]string, MaxTransformColumnMappings)
	types := make([]string, MaxTransformColumnMappings)
	sources := sequentialColumnIndexes(MaxTransformColumnMappings)
	for i := range columns {
		columns[i] = "c" + strconv.Itoa(i)
		types[i] = SQLTypeText
	}
	if err := ValidateSQLConvertOptions(SQLConvertOptions{
		TableName: "records", Columns: columns, ColumnTypes: types, SourceColumns: sources,
	}); err != nil {
		t.Fatalf("maximum SQL mapping rejected: %v", err)
	}

	invalidMappings := []struct {
		name    string
		sources []int
		part    string
	}{
		{name: "maximum plus one", sources: sequentialColumnIndexes(MaxTransformColumnMappings + 1), part: "maximum"},
		{name: "negative", sources: []int{0, -1}, part: "negative"},
		{name: "duplicate", sources: []int{0, 0}, part: "duplicates"},
	}
	for _, test := range invalidMappings {
		t.Run(test.name, func(t *testing.T) {
			mappedColumns := make([]string, len(test.sources))
			for i := range mappedColumns {
				mappedColumns[i] = "c" + strconv.Itoa(i)
			}
			source := &countReadsReader{reader: strings.NewReader("1\n")}
			output := &countWritesWriter{}
			_, err := ConvertToSQL(context.Background(), source, output, SQLConvertOptions{
				TableName: "records", Columns: mappedColumns, SourceColumns: test.sources,
			})
			if err == nil || !strings.Contains(err.Error(), test.part) {
				t.Fatalf("error = %v, want %q", err, test.part)
			}
			if source.reads != 0 || output.writes != 0 {
				t.Fatalf("invalid SQL mapping touched I/O: reads=%d writes=%d", source.reads, output.writes)
			}
		})
	}

	const table = "r"
	const column = "id"
	exactNullBytes := MaxTransformConfigStringBytes - len(table) - len(column)
	exact := SQLConvertOptions{
		TableName: table, Columns: []string{column}, NullValues: []string{strings.Repeat("x", exactNullBytes)},
	}
	if err := ValidateSQLConvertOptions(exact); err != nil {
		t.Fatalf("maximum SQL string budget rejected: %v", err)
	}
	exact.NullValues[0] += "x"
	if err := ValidateSQLConvertOptions(exact); err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("oversized SQL string error = %v", err)
	}
}

func TestAddColumnAndFilterTransformConfigStringLimits(t *testing.T) {
	exactAdd := AddColumnOptions{
		Delimiter: ',',
		Value:     strings.Repeat("v", MaxTransformConfigStringBytes/2),
		FillValue: strings.Repeat("f", MaxTransformConfigStringBytes-MaxTransformConfigStringBytes/2),
	}
	if err := ValidateAddColumnOptions(exactAdd); err != nil {
		t.Fatalf("maximum add-column string budget rejected: %v", err)
	}
	exactAdd.FillValue += "x"
	if err := ValidateAddColumnOptions(exactAdd); err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("oversized add-column string error = %v", err)
	}

	exactFilter := FilterOptions{
		Delimiter: ',',
		Column:    0,
		Op:        FilterEqual,
		Value:     strings.Repeat("x", MaxTransformConfigStringBytes-len(FilterEqual)),
	}
	if err := ValidateFilterOptions(exactFilter); err != nil {
		t.Fatalf("maximum filter string budget rejected: %v", err)
	}
	exactFilter.Value += "x"
	if err := ValidateFilterOptions(exactFilter); err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("oversized filter string error = %v", err)
	}

	// Reject an attacker-sized unknown operation before formatting it with %q;
	// the error must remain bounded as well as occurring before I/O.
	hugeOp := FilterOptions{
		Delimiter: ',',
		Column:    0,
		Op:        FilterOp(strings.Repeat("q", MaxTransformConfigStringBytes+1)),
	}
	err := ValidateFilterOptions(hugeOp)
	if err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("oversized filter operation error = %v", err)
	}
	if len(err.Error()) > 200 {
		t.Fatalf("oversized filter operation was reflected into a %d-byte error", len(err.Error()))
	}
}
