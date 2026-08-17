package csv

import (
	"context"
	stdcsv "encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestConvertToSQLUsesHeaderAndEscapesValues(t *testing.T) {
	var out strings.Builder
	summary, err := ConvertToSQL(context.Background(), strings.NewReader("id,name,note\n1,Ada,hello\n2,O'Neil,NULL\n3,Grace,C:\\Users\\Public\n"), &out, SQLConvertOptions{
		TableName:       "people",
		HasHeader:       true,
		NullValues:      []string{"NULL"},
		InsertBatchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "INSERT INTO `people` (`id`, `name`, `note`) VALUES\n  ('1', 'Ada', 'hello'),\n  ('2', 'O''Neil', NULL);\nINSERT INTO `people` (`id`, `name`, `note`) VALUES\n  ('3', 'Grace', CONVERT(X'433A5C55736572735C5075626C6963' USING utf8mb4));\n"
	if got := out.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if summary.RecordsRead != 4 || summary.RowsWritten != 3 || summary.Columns != 3 {
		t.Fatalf("summary = %+v, want 4 records, 3 rows, 3 columns", summary)
	}
	if summary.Dialect != SQLDialectMySQL {
		t.Fatalf("dialect = %q, want mysql", summary.Dialect)
	}
}

func TestQuoteSQLStringEscapesMySQLSpecials(t *testing.T) {
	value := "line\nnext\rtab\tback\bslash\\quote'nul" + string(rune(0)) + "ctrlz" + string(rune(0x1A))
	got, err := quoteSQLString(value)
	if err != nil {
		t.Fatal(err)
	}
	want := "CONVERT(X'6C696E650A6E6578740D746162096261636B08736C6173685C71756F7465276E756C006374726C7A1A' USING utf8mb4)"
	if got != want {
		t.Fatalf("quoteSQLString() = %q, want %q", got, want)
	}
}

func TestQuoteSQLStringRejectsUnsafeControlCharacters(t *testing.T) {
	_, err := quoteSQLString("unsafe" + string(rune(0x01)))
	if err == nil {
		t.Fatal("expected control-character error")
	}
	if !strings.Contains(err.Error(), "control character") {
		t.Fatalf("error = %q, want control character context", err)
	}
}

func TestQuoteSQLStringIsIndependentOfMySQLEscapeModeAndConnectionCharset(t *testing.T) {
	value := "C:\\Users\\O'Neil\nПривіт" + string(rune(0))
	got, err := quoteSQLString(value)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("CONVERT(X'%X' USING utf8mb4)", []byte(value))
	if got != want {
		t.Fatalf("quoteSQLString() = %q, want %q", got, want)
	}
	if strings.Contains(got, `\`) || strings.Contains(got, value) {
		t.Fatalf("portable literal leaked mode- or charset-sensitive source text: %q", got)
	}
	for _, r := range got {
		if r > 0x7F {
			t.Fatalf("portable literal contains non-ASCII rune U+%04X: %q", r, got)
		}
	}

	plain, err := quoteSQLString("O'Neil")
	if err != nil || plain != "'O''Neil'" {
		t.Fatalf("printable ASCII literal = %q, %v", plain, err)
	}
}

func TestNormalizeSQLColumnNamesReservesFinalCaseFoldedIdentifiers(t *testing.T) {
	input := []string{"a", "a", "a_2", "A", "column_5", ""}
	got := normalizeSQLColumnNames(input)
	if want := []string{"a", "a_3", "a_2", "A_4", "column_5", "column_6"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("normalized columns = %#v, want %#v", got, want)
	}
	seen := map[string]struct{}{}
	for _, name := range got {
		key := foldSQLIdentifier(name)
		if _, exists := seen[key]; exists {
			t.Fatalf("normalized columns contain a case-insensitive duplicate: %#v", got)
		}
		seen[key] = struct{}{}
	}

	long := strings.Repeat("x", MaxSQLIdentifierRunes)
	got = normalizeSQLColumnNames([]string{long, long})
	if utf8.RuneCountInString(got[1]) != MaxSQLIdentifierRunes || foldSQLIdentifier(got[0]) == foldSQLIdentifier(got[1]) {
		t.Fatalf("long duplicate normalization = %#v", got)
	}
}

func TestSQLInsertModeAndDuplicateMappingRejectBeforeIO(t *testing.T) {
	tests := []SQLConvertOptions{
		{TableName: "records", Columns: []string{"id"}, InsertMode: SQLInsertMode("INSERT INTO x; DROP TABLE y; --")},
		{TableName: "records", Columns: []string{"id", "copy"}, SourceColumns: []int{0, 0}},
		{TableName: "records", Columns: []string{"id"}, MaxBatchBytes: -1},
		{TableName: "records", Columns: []string{"id"}, MaxBatchBytes: MaxSQLInsertBatchBytes + 1},
	}
	for _, opts := range tests {
		source := &countReadsReader{reader: strings.NewReader("1\n")}
		output := &countWritesWriter{}
		if _, err := ConvertToSQL(context.Background(), source, output, opts); err == nil {
			t.Fatalf("invalid options were accepted: %+v", opts)
		}
		if source.reads != 0 || output.writes != 0 {
			t.Fatalf("invalid options touched I/O: reads=%d writes=%d", source.reads, output.writes)
		}
	}
}

type countWritesWriter struct{ writes int }

func (w *countWritesWriter) Write(p []byte) (int, error) {
	w.writes++
	return len(p), nil
}

func TestConvertToSQLUsesExplicitColumnsAndTSV(t *testing.T) {
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), strings.NewReader("1\tAda\n2\tGrace\n"), &out, SQLConvertOptions{
		TableName: "people",
		Delimiter: '\t',
		Columns:   []string{"id", "name"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "INSERT INTO `people` (`id`, `name`) VALUES\n  ('1', 'Ada'),\n  ('2', 'Grace');\n"
	if got := out.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestConvertToSQLBatchesRows(t *testing.T) {
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), strings.NewReader("1,Ada\n2,Grace\n3,Linus\n"), &out, SQLConvertOptions{
		TableName:       "people",
		Columns:         []string{"id", "name"},
		InsertBatchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "INSERT INTO `people` (`id`, `name`) VALUES\n  ('1', 'Ada'),\n  ('2', 'Grace');\nINSERT INTO `people` (`id`, `name`) VALUES\n  ('3', 'Linus');\n"
	if got := out.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestConvertToSQLFlushesAtAggregateBatchByteLimit(t *testing.T) {
	header, err := sqlInsertBatchHeader(SQLInsertModeInsert, "records", []string{"value"})
	if err != nil {
		t.Fatal(err)
	}
	tuple, _, _, err := sqlValuesTuple([]string{"x"}, nil, []string{SQLTypeText}, InvalidValueFail)
	if err != nil {
		t.Fatal(err)
	}
	framingBytes := int64(len(header) + len(sqlInsertTerminator))
	limit := framingBytes + 2*int64(len(tuple)) + int64(len(sqlInsertTupleSeparator))

	var out strings.Builder
	summary, err := ConvertToSQL(context.Background(), strings.NewReader("x\nx\nx\n"), &out, SQLConvertOptions{
		TableName: "records", Columns: []string{"value"},
		InsertBatchSize: MaxSQLInsertBatchSize, MaxBatchBytes: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.RowsWritten != 3 || strings.Count(out.String(), "INSERT INTO") != 2 {
		t.Fatalf("summary = %+v, SQL = %q; want three rows in two byte-bounded statements", summary, out.String())
	}
	for _, statement := range strings.Split(strings.TrimSuffix(out.String(), sqlInsertTerminator), sqlInsertTerminator) {
		if got := int64(len(statement) + len(sqlInsertTerminator)); got > limit {
			t.Fatalf("statement is %d bytes, exceeding configured limit %d", got, limit)
		}
	}
}

func TestConvertToSQLRejectsOneEncodedRowLargerThanBatchLimit(t *testing.T) {
	header, err := sqlInsertBatchHeader(SQLInsertModeInsert, "records", []string{"value"})
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(len(header)+len(sqlInsertTerminator)) + 1024*1024
	value := strings.Repeat("Ж", 512*1024)
	var out strings.Builder
	_, err = ConvertToSQL(context.Background(), strings.NewReader(stdcsvRecord(value)), &out, SQLConvertOptions{
		TableName: "records", Columns: []string{"value"}, MaxBatchBytes: limit,
	})
	if !errors.Is(err, ErrSQLInsertBatchTooLarge) {
		t.Fatalf("error = %v, want ErrSQLInsertBatchTooLarge", err)
	}
	if out.Len() != 0 {
		t.Fatalf("oversized encoded row wrote SQL: %q", out.String())
	}
}

func TestConvertToSQLFileDoesNotPublishEncodedRowAboveBatchLimit(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.csv")
	output := filepath.Join(dir, "output.sql")
	source := []byte(stdcsvRecord(strings.Repeat("Ж", 512*1024)))
	if err := os.WriteFile(input, source, 0o600); err != nil {
		t.Fatal(err)
	}
	header, err := sqlInsertBatchHeader(SQLInsertModeInsert, "records", []string{"value"})
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(len(header)+len(sqlInsertTerminator)) + 1024*1024
	_, err = ConvertToSQLFile(context.Background(), input, output, SQLConvertOptions{
		Delimiter: ',', TableName: "records", Columns: []string{"value"}, MaxBatchBytes: limit,
	})
	if !errors.Is(err, ErrSQLInsertBatchTooLarge) {
		t.Fatalf("error = %v, want ErrSQLInsertBatchTooLarge", err)
	}
	if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("oversized encoded row published output: %v", statErr)
	}
	if got, readErr := os.ReadFile(input); readErr != nil || string(got) != string(source) {
		t.Fatalf("source changed: got %q, err %v", got, readErr)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 || entries[0].Name() != "input.csv" {
		t.Fatalf("failed conversion left artifacts: %v", entryNames(entries))
	}
}

func TestWriteSQLInsertBatchHandlesShortWritesWithoutTruncation(t *testing.T) {
	header, err := sqlInsertBatchHeader(SQLInsertModeInsertIgnore, "records", []string{"id", "name"})
	if err != nil {
		t.Fatal(err)
	}
	tuples := []string{"('1', 'Ada')", "('2', 'Grace')"}
	writer := &chunkedSQLWriter{max: 3}
	if err := writeSQLInsertBatch(writer, header, tuples); err != nil {
		t.Fatal(err)
	}
	want := header + strings.Join(tuples, sqlInsertTupleSeparator) + sqlInsertTerminator
	if got := writer.String(); got != want {
		t.Fatalf("short writer output = %q, want %q", got, want)
	}

	if err := writeSQLInsertBatch(zeroSQLWriter{}, header, tuples); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero-progress writer error = %v, want io.ErrShortWrite", err)
	}
}

type chunkedSQLWriter struct {
	builder strings.Builder
	max     int
}

func (w *chunkedSQLWriter) Write(p []byte) (int, error) {
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.builder.Write(p)
}

func (w *chunkedSQLWriter) String() string { return w.builder.String() }

type zeroSQLWriter struct{}

func (zeroSQLWriter) Write([]byte) (int, error) { return 0, nil }

func TestConvertToSQLWritesCreateTableWithExplicitTypes(t *testing.T) {
	var out strings.Builder
	summary, err := ConvertToSQL(context.Background(), strings.NewReader("1,Ada\n"), &out, SQLConvertOptions{
		TableName:          "people",
		Columns:            []string{"id", "name"},
		IncludeCreateTable: true,
		ColumnTypes:        []string{SQLTypeBigInt, SQLTypeText},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "CREATE TABLE IF NOT EXISTS `people` (\n  `id` BIGINT,\n  `name` TEXT\n);\nINSERT INTO `people` (`id`, `name`) VALUES\n  ('1', 'Ada');\n"
	if got := out.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if !summary.CreateTableWritten {
		t.Fatal("expected CREATE TABLE summary flag")
	}
}

func TestConvertToSQLPreservesExplicitTypesWithHeaderColumns(t *testing.T) {
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), strings.NewReader("id,name\n1,Ada\n"), &out, SQLConvertOptions{
		TableName:          "people",
		HasHeader:          true,
		IncludeCreateTable: true,
		ColumnTypes:        []string{SQLTypeBigInt, SQLTypeText},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "`id` BIGINT") || !strings.Contains(got, "`name` TEXT") {
		t.Fatalf("output = %q, want explicit header column types", got)
	}
}

func TestConvertToSQLFileInfersCreateTableSchema(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.csv")
	output := filepath.Join(dir, "output.sql")
	if err := os.WriteFile(input, []byte("id,price,active,note\n1,12.5,true,hello\n2,14,false,NULL\n"), 0o666); err != nil {
		t.Fatal(err)
	}

	summary, err := ConvertToSQLFile(context.Background(), input, output, SQLConvertOptions{
		Delimiter:          ',',
		TableName:          "items",
		HasHeader:          true,
		NullValues:         []string{"NULL"},
		IncludeCreateTable: true,
		InsertBatchSize:    2,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "CREATE TABLE IF NOT EXISTS `items` (\n  `id` BIGINT,\n  `price` DOUBLE,\n  `active` BOOLEAN,\n  `note` TEXT\n);\n"
	if got := string(data); !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("output = %q, want CREATE TABLE prefix %q", got, wantPrefix)
	}
	if got := string(data); !strings.Contains(got, "('1', '12.5', 1, 'hello')") || !strings.Contains(got, "('2', '14', 0, NULL)") {
		t.Fatalf("inferred BOOLEAN values were not emitted as canonical 1/0: %q", got)
	}
	if summary.Columns != 4 || summary.RowsWritten != 2 || !summary.CreateTableWritten {
		t.Fatalf("summary = %+v, want 4 columns, 2 rows, CREATE TABLE", summary)
	}
}

func TestSchemaColumnsForSQLSplitsNamesAndTypes(t *testing.T) {
	names, types := schemaColumnsForSQL([]SchemaColumn{
		{Name: "id", SQLType: SQLTypeBigInt},
		{Name: "active", SQLType: SQLTypeBoolean},
	})

	if got, want := strings.Join(names, ","), "id,active"; got != want {
		t.Fatalf("names = %q, want %q", got, want)
	}
	if got, want := strings.Join(types, ","), "BIGINT,BOOLEAN"; got != want {
		t.Fatalf("types = %q, want %q", got, want)
	}
}

func TestConvertToSQLEmitsCanonicalMySQLBooleans(t *testing.T) {
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), strings.NewReader("TRUE\nfalse\n1\n0\nNULL\n\"\"\n"), &out, SQLConvertOptions{
		TableName:   "flags",
		Columns:     []string{"active"},
		ColumnTypes: []string{SQLTypeBoolean},
		NullValues:  []string{"NULL", ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "INSERT INTO `flags` (`active`) VALUES\n  (1),\n  (0),\n  (1),\n  (0),\n  (NULL),\n  (NULL);\n"
	if got := out.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestConvertToSQLBooleanRejectsInvalidAndWhitespaceTokens(t *testing.T) {
	for _, value := range []string{"yes", "no", " true", "false ", "", "2"} {
		t.Run(strconv.Quote(value), func(t *testing.T) {
			var out strings.Builder
			_, err := ConvertToSQL(context.Background(), strings.NewReader(stdcsvRecord(value)), &out, SQLConvertOptions{
				TableName: "flags", Columns: []string{"active"}, ColumnTypes: []string{SQLTypeBoolean},
			})
			if err == nil || !strings.Contains(err.Error(), "MySQL BOOLEAN") {
				t.Fatalf("error = %v, want MySQL BOOLEAN validation", err)
			}
			if out.Len() != 0 {
				t.Fatalf("invalid boolean wrote SQL: %q", out.String())
			}
		})
	}
}

func TestConvertToSQLMixedBooleanTextInfersText(t *testing.T) {
	report, err := InferSchema(strings.NewReader("active\ntrue\nyes\nfalse\n"), SchemaOptions{
		Delimiter: ',', HasHeader: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Columns) != 1 || report.Columns[0].SQLType != SQLTypeText {
		t.Fatalf("columns = %+v, want mixed boolean/text as TEXT", report.Columns)
	}
}

func TestConvertToSQLFileRejectsLateInvalidInferredBooleanWithoutFinal(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.csv")
	output := filepath.Join(dir, "output.sql")
	var data strings.Builder
	data.WriteString("active\n")
	for i := 0; i < DefaultSchemaMaxRows+25; i++ {
		data.WriteString("true\n")
	}
	data.WriteString("yes\n")
	if err := os.WriteFile(input, []byte(data.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ConvertToSQLFile(context.Background(), input, output, SQLConvertOptions{
		Delimiter: ',', TableName: "flags", HasHeader: true, IncludeCreateTable: true,
	})
	if err == nil || !strings.Contains(err.Error(), "MySQL BOOLEAN") {
		t.Fatalf("error = %v, want late boolean validation", err)
	}
	if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("late invalid boolean created final output: %v", statErr)
	}
}

func TestConvertToSQLFileRejectsUnknownExplicitTypeBeforeArtifacts(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "output.sql")
	_, err := ConvertToSQLFile(context.Background(), filepath.Join(dir, "missing.csv"), output, SQLConvertOptions{
		Delimiter: ',', TableName: "flags", Columns: []string{"active"}, ColumnTypes: []string{"BOOLISH"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid SQL column type") {
		t.Fatalf("error = %v, want invalid type", err)
	}
	if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid type created output: %v", statErr)
	}
}

func stdcsvRecord(value string) string {
	if value == "" {
		return "\"\"\n"
	}
	var out strings.Builder
	w := stdcsv.NewWriter(&out)
	_ = w.Write([]string{value})
	w.Flush()
	return out.String()
}

func TestPreviewSQLConversionUsesBoundedRowsAndFormatsReport(t *testing.T) {
	report, err := PreviewSQLConversion(strings.NewReader("id,name\n1,Ada\n2,Grace\n3,Linus\n"), SQLPreviewOptions{
		SQLConvertOptions: SQLConvertOptions{
			TableName:       "people",
			HasHeader:       true,
			InsertBatchSize: 2,
		},
		MaxRows: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.RowsPreviewed != 2 || report.Summary.RowsWritten != 2 {
		t.Fatalf("report rows = preview %d written %d, want 2/2", report.RowsPreviewed, report.Summary.RowsWritten)
	}
	if !strings.Contains(report.SQL, "'Ada'") || !strings.Contains(report.SQL, "'Grace'") {
		t.Fatalf("preview SQL = %q, want first two data rows", report.SQL)
	}
	if strings.Contains(report.SQL, "Linus") {
		t.Fatalf("preview SQL included row beyond preview limit: %q", report.SQL)
	}
	formatted := FormatSQLPreviewReport(report)
	if !strings.Contains(formatted, "Rows previewed: 2") || !strings.Contains(formatted, "SQL preview:") || !strings.Contains(formatted, "preview limited to 2 data rows") {
		t.Fatalf("formatted report = %q, want summary, SQL, and row-limit warning", formatted)
	}
}

func TestPreviewSQLConversionInfersCreateTableFromSample(t *testing.T) {
	report, err := PreviewSQLConversion(strings.NewReader("id,price,active,note\n1,12.5,true,hello\n2,14,false,NULL\n"), SQLPreviewOptions{
		SQLConvertOptions: SQLConvertOptions{
			TableName:          "items",
			HasHeader:          true,
			NullValues:         []string{"NULL"},
			IncludeCreateTable: true,
		},
		MaxRows: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CREATE TABLE IF NOT EXISTS `items`", "`id` BIGINT", "`price` DOUBLE", "`active` BOOLEAN", "`note` TEXT"} {
		if !strings.Contains(report.SQL, want) {
			t.Fatalf("preview SQL = %q, want %q", report.SQL, want)
		}
	}
	if !strings.Contains(report.SQL, "'12.5', 1") || !strings.Contains(report.SQL, "'14', 0") {
		t.Fatalf("preview SQL did not canonicalize BOOLEAN values: %q", report.SQL)
	}
	if !report.Summary.CreateTableWritten {
		t.Fatal("expected preview summary to report CREATE TABLE output")
	}
}

func TestPreviewSQLConversionTrimsTrailingPartialRecord(t *testing.T) {
	report, err := PreviewSQLConversion(strings.NewReader("id,name\n1,Ada\n2,\"unterminated"), SQLPreviewOptions{
		SQLConvertOptions: SQLConvertOptions{
			TableName: "people",
			HasHeader: true,
		},
		MaxBytes: 19,
		MaxRows:  10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.TruncatedSample {
		t.Fatal("expected truncated sample")
	}
	if strings.Contains(report.SQL, "unterminated") {
		t.Fatalf("preview SQL included trailing partial record: %q", report.SQL)
	}
	if !strings.Contains(strings.Join(report.Warnings, "\n"), "trailing partial record omitted") {
		t.Fatalf("warnings = %#v, want partial-record warning", report.Warnings)
	}
}

func TestConvertToSQLNormalizesHeaderNames(t *testing.T) {
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), strings.NewReader("id,,id\n1,Ada,2\n"), &out, SQLConvertOptions{
		TableName: "people",
		HasHeader: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "`id`, `column_2`, `id_2`") {
		t.Fatalf("output = %q, want normalized unique column names", got)
	}
}

func TestConvertToSQLRejectsOutOfRangeSourceMappingBeforeWriting(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		hasHeader bool
	}{
		{name: "header", input: "id,name\n1,Ada\n", hasHeader: true},
		{name: "first data row", input: "1,Ada\n", hasHeader: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out strings.Builder
			_, err := ConvertToSQL(context.Background(), strings.NewReader(test.input), &out, SQLConvertOptions{
				TableName: "people", HasHeader: test.hasHeader,
				Columns: []string{"missing"}, SourceColumns: []int{2},
				IncludeCreateTable: true, ColumnTypes: []string{SQLTypeText},
			})
			if err == nil || !strings.Contains(err.Error(), "out of range") {
				t.Fatalf("error = %v, want out-of-range mapping", err)
			}
			if out.Len() != 0 {
				t.Fatalf("invalid source mapping wrote SQL: %q", out.String())
			}
		})
	}
}

func TestConvertToSQLFileRejectsOutOfRangeSourceMappingWithoutPublishing(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.csv")
	output := filepath.Join(dir, "output.sql")
	if err := os.WriteFile(input, []byte("id,name\n1,Ada\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ConvertToSQLFile(context.Background(), input, output, SQLConvertOptions{
		Delimiter: ',', TableName: "people", HasHeader: true,
		Columns: []string{"missing"}, SourceColumns: []int{2},
	})
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("error = %v, want out-of-range mapping", err)
	}
	if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid source mapping published output: %v", statErr)
	}
}

func TestConvertToSQLRejectsMismatchedRecordWidth(t *testing.T) {
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), strings.NewReader("1,Ada\n2\n"), &out, SQLConvertOptions{
		TableName: "people",
		Columns:   []string{"id", "name"},
	})
	if err == nil {
		t.Fatal("expected mismatched-width error")
	}
	if !strings.Contains(err.Error(), "record 2") {
		t.Fatalf("error = %q, want record context", err)
	}
}

func TestConvertToSQLRejectsUnsupportedDialect(t *testing.T) {
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), strings.NewReader("1,Ada\n"), &out, SQLConvertOptions{
		TableName: "people",
		Columns:   []string{"id", "name"},
		Dialect:   SQLDialect("postgres"),
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported SQL dialect") {
		t.Fatalf("err = %v, want unsupported dialect", err)
	}
}

func TestConvertToSQLRejectsOversizedField(t *testing.T) {
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), strings.NewReader("1,toolong\n"), &out, SQLConvertOptions{
		TableName:     "people",
		Columns:       []string{"id", "name"},
		MaxFieldBytes: 4,
	})
	if err == nil || !strings.Contains(err.Error(), "max field size") {
		t.Fatalf("err = %v, want max field size error", err)
	}
}

func TestConvertToSQLRejectsUnsafeIdentifiers(t *testing.T) {
	tests := []struct {
		name string
		opts SQLConvertOptions
		want string
	}{
		{
			name: "table control",
			opts: SQLConvertOptions{
				TableName: "bad\nname",
				Columns:   []string{"id"},
			},
			want: "table name",
		},
		{
			name: "explicit column control",
			opts: SQLConvertOptions{
				TableName: "people",
				Columns:   []string{"bad\tcolumn"},
			},
			want: "column 1",
		},
		{
			name: "header column control",
			opts: SQLConvertOptions{
				TableName: "people",
				HasHeader: true,
			},
			want: "column 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input string
			if tt.opts.HasHeader {
				input = "\"bad\tcolumn\"\n1\n"
			} else {
				input = "1\n"
			}
			var out strings.Builder
			_, err := ConvertToSQL(context.Background(), strings.NewReader(input), &out, tt.opts)
			if err == nil {
				t.Fatal("expected unsafe identifier error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err, tt.want)
			}
			if out.Len() != 0 {
				t.Fatalf("wrote output before rejecting identifier: %q", out.String())
			}
		})
	}
}

func TestConvertToSQLRejectsOverlongMySQLIdentifierBeforeReading(t *testing.T) {
	source := &countReadsReader{reader: strings.NewReader("1\n")}
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), source, &out, SQLConvertOptions{
		TableName: strings.Repeat("t", MaxSQLIdentifierRunes+1), Columns: []string{"id"},
	})
	if err == nil || !strings.Contains(err.Error(), "limited to 64") {
		t.Fatalf("error = %v, want MySQL identifier limit", err)
	}
	if source.reads != 0 || out.Len() != 0 {
		t.Fatalf("overlong identifier touched I/O: reads=%d output=%q", source.reads, out.String())
	}
}

func TestConvertToSQLStripsUTF8BOM(t *testing.T) {
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), strings.NewReader("\xEF\xBB\xBFid,name\n1,alice\n"), &out, SQLConvertOptions{
		TableName: "users",
		HasHeader: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sql := out.String()
	if strings.Contains(sql, "FEFF") {
		t.Fatalf("BOM leaked into output: %q", sql)
	}
	if !strings.Contains(sql, "`id`") {
		t.Fatalf("first column should be `id`, got: %q", sql)
	}
}

func TestConvertToSQLRejectsUTF16Input(t *testing.T) {
	// UTF-16LE BOM + "id\n" interleaved with NULs.
	in := "\xFF\xFEi\x00d\x00\n\x00"
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), strings.NewReader(in), &out, SQLConvertOptions{
		TableName: "t",
		HasHeader: true,
	})
	if !errors.Is(err, ErrUTF16Input) {
		t.Fatalf("err = %v, want ErrUTF16Input", err)
	}
}

func TestConvertToSQLPreservesUnquotedLeadingWhitespace(t *testing.T) {
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), strings.NewReader("1, NULL\n2,  padded\n3,\tTabbed\n4, \n5, +001\n6,-001\n7,\n"), &out, SQLConvertOptions{
		TableName:  "t",
		Columns:    []string{"a", "b"},
		NullValues: []string{"NULL"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"'1', ' NULL'", "'2', '  padded'", "'3', CONVERT(X'09546162626564' USING utf8mb4)", "'4', ' '", "'5', ' +001'", "'6', '-001'", "'7', ''"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, want %q", out.String(), want)
		}
	}
}

func TestConvertToSQLKeepsQuotedLeadingSpace(t *testing.T) {
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), strings.NewReader("1,\" NULL\"\n"), &out, SQLConvertOptions{
		TableName:  "t",
		Columns:    []string{"a", "b"},
		NullValues: []string{"NULL"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "' NULL'") {
		t.Fatalf("quoted \" NULL\" should stay a string, got: %q", out.String())
	}
}

func TestConvertToSQLReplacePolicySanitizes(t *testing.T) {
	var out strings.Builder
	summary, err := ConvertToSQL(context.Background(), strings.NewReader("1,bad\x01value\n"), &out, SQLConvertOptions{
		TableName:      "t",
		Columns:        []string{"id", "note"},
		OnInvalidValue: InvalidValueReplace,
	})
	if err != nil {
		t.Fatalf("replace policy should not error: %v", err)
	}
	if summary.SanitizedRows != 1 {
		t.Fatalf("SanitizedRows = %d, want 1", summary.SanitizedRows)
	}
	if summary.RowsWritten != 1 {
		t.Fatalf("RowsWritten = %d, want 1", summary.RowsWritten)
	}
	if strings.ContainsRune(out.String(), 0x01) {
		t.Fatalf("control byte leaked into output: %q", out.String())
	}
}

func TestConvertToSQLSkipRowPolicyDropsBadRecord(t *testing.T) {
	var out strings.Builder
	summary, err := ConvertToSQL(context.Background(), strings.NewReader("1,ok\n2,bad\x01value\n3,fine\n"), &out, SQLConvertOptions{
		TableName:      "t",
		Columns:        []string{"id", "note"},
		OnInvalidValue: InvalidValueSkipRow,
	})
	if err != nil {
		t.Fatalf("skip-row policy should not error: %v", err)
	}
	if summary.SkippedRows != 1 {
		t.Fatalf("SkippedRows = %d, want 1", summary.SkippedRows)
	}
	if summary.RowsWritten != 2 {
		t.Fatalf("RowsWritten = %d, want 2 (rows 1 and 3)", summary.RowsWritten)
	}
}

func TestConvertToSQLFileDoesNotPublishPartialOutputOnDataError(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.csv")
	output := filepath.Join(dir, "output.sql")
	// Rows 1-2 are clean; row 3 has a control byte. Default policy (fail) aborts
	// at row 3 without exposing the already-written rows at the final pathname.
	if err := os.WriteFile(input, []byte("1,ok\n2,fine\n3,bad\x01value\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	_, err := ConvertToSQLFile(context.Background(), input, output, SQLConvertOptions{
		Delimiter:       ',',
		TableName:       "t",
		Columns:         []string{"id", "note"},
		InsertBatchSize: 1,
	})
	if err == nil {
		t.Fatal("expected a data error on row 3")
	}
	if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial output was published on a data error: %v", statErr)
	}
}

func TestConvertToSQLRejectsUnsafeFieldControlCharacters(t *testing.T) {
	var out strings.Builder
	_, err := ConvertToSQL(context.Background(), strings.NewReader("1,bad\x01value\n"), &out, SQLConvertOptions{
		TableName: "people",
		Columns:   []string{"id", "note"},
	})
	if err == nil {
		t.Fatal("expected unsafe field error")
	}
	if !strings.Contains(err.Error(), "record 1") || !strings.Contains(err.Error(), "field 2") {
		t.Fatalf("error = %q, want record and field context", err)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote output before rejecting unsafe field: %q", out.String())
	}
}

func TestConvertToSQLFileDeletesPartialOutputOnCancel(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.csv")
	output := filepath.Join(dir, "output.sql")
	if err := os.WriteFile(input, []byte("id,name\n1,Ada\n2,Grace\n"), 0o666); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, err := ConvertToSQLFile(ctx, input, output, SQLConvertOptions{
		Delimiter:       ',',
		TableName:       "people",
		HasHeader:       true,
		InsertBatchSize: 1,
		Progress: func(SQLConvertProgress) {
			cancel()
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial output still exists: %v", statErr)
	}
}

func TestConvertToSQLFileRejectsSamePathAndExistingOutput(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.csv")
	output := filepath.Join(dir, "output.sql")
	if err := os.WriteFile(input, []byte("id,name\n1,Ada\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertToSQLFile(context.Background(), input, input, SQLConvertOptions{Delimiter: ',', TableName: "people", HasHeader: true}); err == nil {
		t.Fatal("expected same-path error")
	}
	if err := os.WriteFile(output, []byte("preexisting"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertToSQLFile(context.Background(), input, output, SQLConvertOptions{Delimiter: ',', TableName: "people", HasHeader: true}); err == nil {
		t.Fatal("expected existing-output error")
	}
	if data, err := os.ReadFile(output); err != nil || string(data) != "preexisting" {
		t.Fatalf("preexisting output changed, data=%q err=%v", data, err)
	}
}

func TestSQLInsertBatchLimitRejectsBeforeSourceOrOutputAccess(t *testing.T) {
	opts := SQLConvertOptions{
		Delimiter: ',', TableName: "records", Columns: []string{"id"}, InsertBatchSize: math.MaxInt,
	}
	if err := ValidateSQLConvertOptions(opts); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("validation error = %v, want maximum batch-size rejection", err)
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "existing.sql")
	sentinel := []byte("existing output")
	if err := os.WriteFile(dst, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ConvertToSQLFile(context.Background(), filepath.Join(dir, "missing.csv"), dst, opts)
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("file conversion error = %v, want maximum batch-size rejection", err)
	}
	if got, readErr := os.ReadFile(dst); readErr != nil || string(got) != string(sentinel) {
		t.Fatalf("invalid batch size changed destination: got %q, err %v", got, readErr)
	}
}
