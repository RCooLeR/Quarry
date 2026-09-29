package csv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestJSONLRejectsInvalidUTF8WithoutPublishing(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		header      bool
	}{
		{"header", "bad\xff,other\n1,2\n", true},
		{"colliding-headers", "\xff,\xfe\n1,2\n", true},
		{"first-data", "\xff,value\n", false},
		{"later-data", "a,b\n1,ok\n2,bad\xff\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := writeTemp(t, "source.csv", tc.input)
			dir := t.TempDir()
			dst := filepath.Join(dir, "result.jsonl")
			_, err := ExportJSONLFile(context.Background(), src, dst, JSONLOptions{Delimiter: ',', HasHeader: tc.header})
			if err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
				t.Fatalf("error = %v, want invalid UTF-8", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("output or scratch remains: %v, %v", entries, err)
			}
			if got := readAll(t, src); got != tc.input {
				t.Fatal("source bytes changed")
			}
		})
	}
}

func TestCSVSampleReportsRecordCap(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
		truncated    bool
	}{
		{"more-records", "third\n", true},
		{"whitespace-record", " \n", true},
		{"exact-limit", "", false},
		{"empty-lines", "\r\n\n", false},
		{"trailing-cr", "\r", false},
		{"cr-field", "\r\r\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "first\nsecond\n" + tc.suffix
			schema, err := InferSchema(strings.NewReader(input), SchemaOptions{MaxRows: 2})
			if err != nil || schema.TruncatedSample != tc.truncated || schema.RecordsScanned != 2 {
				t.Fatalf("schema = %+v, %v", schema, err)
			}
			profile, err := ProfileColumns(context.Background(), strings.NewReader(input), SchemaOptions{MaxRows: 2})
			if err != nil || profile.TruncatedSample != tc.truncated || profile.RecordsScanned != 2 {
				t.Fatalf("profile = %+v, %v", profile, err)
			}
			preview, err := PreviewRows(strings.NewReader(input), PreviewOptions{MaxRows: 2})
			if err != nil || preview.TruncatedSample != tc.truncated || len(preview.Rows) != 2 {
				t.Fatalf("preview = %+v, %v", preview, err)
			}
		})
	}
}

func TestTopProfileValuesOrderAndBoundedStorage(t *testing.T) {
	counts := make(map[string]int)
	for i := range 1000 {
		counts[strconv.Itoa(i)] = i % 7
	}
	expected := make([]ValueCount, 0, len(counts))
	for value, count := range counts {
		expected = append(expected, ValueCount{Value: value, Count: count})
	}
	slices.SortFunc(expected, func(a, b ValueCount) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.Value, b.Value)
	})
	for range 10 {
		top := topProfileValues(counts)
		if !slices.Equal(top, expected[:profileTopValuesCap]) {
			t.Fatalf("top = %+v, want %+v", top, expected[:profileTopValuesCap])
		}
		if cap(top) > profileTopValuesCap {
			t.Fatalf("top retains %d entries, maximum %d", cap(top), profileTopValuesCap)
		}
	}
}

func TestProfileDistinctCapOnlyReportsOmittedValues(t *testing.T) {
	var input strings.Builder
	for i := range profileDistinctCap {
		input.WriteString(strconv.Itoa(i))
		input.WriteByte('\n')
	}
	for _, extra := range []string{"", "0\n", "extra\n"} {
		report, err := ProfileColumns(context.Background(), strings.NewReader(input.String()+extra), SchemaOptions{MaxRows: profileDistinctCap + 1})
		if err != nil {
			t.Fatal(err)
		}
		wantCapped := extra == "extra\n"
		if report.Columns[0].Distinct != profileDistinctCap || report.Columns[0].DistinctCapped != wantCapped {
			t.Fatalf("extra %q: profile = %+v", extra, report.Columns[0])
		}
	}
}

func TestSchemaPreservesOutOfRangeIntegerTokens(t *testing.T) {
	for _, token := range []string{"9223372036854775808", "-9223372036854775809", "18446744073709551615", "0009223372036854775808"} {
		if got := classifySchemaValue(token); got != schemaText {
			t.Fatalf("%q inferred as %v, want text", token, got)
		}
		report, err := PreviewSQLConversion(strings.NewReader("id\n"+token+"\n"), SQLPreviewOptions{
			SQLConvertOptions: SQLConvertOptions{Delimiter: ',', HasHeader: true, TableName: "ids", IncludeCreateTable: true},
		})
		if err != nil || !strings.Contains(report.SQL, "`id` TEXT") || !strings.Contains(report.SQL, "'"+token+"'") {
			t.Fatalf("preview = %q, %v", report.SQL, err)
		}
	}
}

type mustNotReadSchemaInput struct{}

func (mustNotReadSchemaInput) Read([]byte) (int, error) {
	return 0, errors.New("input was read before rejecting oversized configuration")
}

func TestSchemaRejectsUnboundedNullSentinelsBeforeReading(t *testing.T) {
	for _, nulls := range [][]string{
		make([]string, MaxTransformColumnMappings+1),
		{strings.Repeat("x", MaxTransformConfigStringBytes+1)},
	} {
		opts := SchemaOptions{NullValues: nulls}
		_, schemaErr := InferSchema(mustNotReadSchemaInput{}, opts)
		_, profileErr := ProfileColumns(context.Background(), mustNotReadSchemaInput{}, opts)
		for _, err := range []error{schemaErr, profileErr} {
			if err == nil || !strings.Contains(err.Error(), "schema null sentinels") {
				t.Fatalf("error = %v, want null-sentinel budget validation", err)
			}
		}
	}
}

func TestSQLNumericTypesRejectLateInvalidValuesBeforePublication(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, invalid, sqlType string
	}{
		{"integer-text", "42", "not-a-number", SQLTypeBigInt},
		{"integer-overflow", "42", "9223372036854775808", SQLTypeBigInt},
		{"integer-fraction", "42", "1.5", SQLTypeBigInt},
		{"double-text", "1.25", "not-a-number", SQLTypeDouble},
		{"double-infinity", "1.25", "Inf", SQLTypeDouble},
		{"double-overflow", "1.25", "1e999", SQLTypeDouble},
		{"double-hex", "1.25", "0x1p2", SQLTypeDouble},
		{"double-underscore", "1.25", "1_000", SQLTypeDouble},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "value\n" + strings.Repeat(tc.prefix+"\n", DefaultSchemaMaxRows+1) + tc.invalid + "\n"
			src := writeTemp(t, "source.csv", input)
			dir := t.TempDir()
			_, err := ConvertToSQLFile(context.Background(), src, filepath.Join(dir, "out.sql"), SQLConvertOptions{
				Delimiter: ',', TableName: "values", HasHeader: true, IncludeCreateTable: true,
			})
			if err == nil || !strings.Contains(err.Error(), "MySQL "+tc.sqlType) {
				t.Fatalf("error = %v, want %s validation", err, tc.sqlType)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("output or scratch remains: %v, %v", entries, err)
			}
			if readAll(t, src) != input {
				t.Fatal("source bytes changed")
			}
		})
	}
}

func TestSQLNumericValidationPreservesValidSpelling(t *testing.T) {
	for _, tc := range []struct {
		token, sqlType string
	}{
		{"-9223372036854775808", SQLTypeBigInt},
		{"9223372036854775807", SQLTypeBigInt},
		{"+007", SQLTypeBigInt},
		{"-1.25e+30", SQLTypeDouble},
		{".5", SQLTypeDouble},
		{"1.", SQLTypeDouble},
		{"+007.0", SQLTypeDouble},
	} {
		var out strings.Builder
		_, err := ConvertToSQL(context.Background(), strings.NewReader(tc.token+"\n"), &out, SQLConvertOptions{
			TableName: "values", Columns: []string{"value"}, ColumnTypes: []string{tc.sqlType},
		})
		if err != nil || !strings.Contains(out.String(), "'"+tc.token+"'") {
			t.Fatalf("%s token %q: output = %q, error = %v", tc.sqlType, tc.token, out.String(), err)
		}
	}
}

func TestRaggedCSVCountsMissingCellsInEveryDataRow(t *testing.T) {
	for _, header := range []bool{false, true} {
		input := "1\n2,10\n3\n4,\n"
		if header {
			input = "id\n" + input
		}
		// Missing fields are null; an explicitly present empty field remains
		// data unless the caller lists the empty string as a null sentinel.
		for _, nulls := range [][]string{nil, {""}} {
			opts := SchemaOptions{HasHeader: header, NullValues: nulls}
			schema, err := InferSchema(strings.NewReader(input), opts)
			if err != nil {
				t.Fatal(err)
			}
			profile, err := ProfileColumns(context.Background(), strings.NewReader(input), opts)
			if err != nil {
				t.Fatal(err)
			}
			wantNulls := 2
			if len(nulls) > 0 {
				wantNulls++
			}
			if got := schema.Columns[1]; got.NullCount != wantNulls || got.NonNullCount != 4-wantNulls {
				t.Fatalf("header %t, nulls %v: schema = %+v", header, nulls, got)
			}
			if got := profile.Columns[1]; got.Null != wantNulls || got.NonNull != 4-wantNulls {
				t.Fatalf("header %t, nulls %v: profile = %+v", header, nulls, got)
			}
		}
	}
}

func BenchmarkTopProfileValues(b *testing.B) {
	counts := make(map[string]int, profileDistinctCap)
	for i := range profileDistinctCap {
		counts[strconv.Itoa(i)] = i % 100
	}
	b.ReportAllocs()
	for b.Loop() {
		if top := topProfileValues(counts); len(top) != profileTopValuesCap {
			b.Fatalf("top count = %d, want %d", len(top), profileTopValuesCap)
		}
	}
}

func TestSQLInferredBigIntRejectsLateLeadingZeroIdentifiers(t *testing.T) {
	for _, token := range []string{"007", "+007", "-0042"} {
		t.Run(token, func(t *testing.T) {
			input := "id\n" + strings.Repeat("42\n", DefaultSchemaMaxRows+1) + token + "\n"
			src := writeTemp(t, "source.csv", input)
			dir := t.TempDir()
			_, err := ConvertToSQLFile(context.Background(), src, filepath.Join(dir, "out.sql"), SQLConvertOptions{
				Delimiter: ',', TableName: "ids", HasHeader: true, IncludeCreateTable: true,
			})
			if err == nil || !strings.Contains(err.Error(), "leading-zero integer") {
				t.Fatalf("error = %v, want inferred identifier preservation refusal", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("output or scratch remains: %v, %v", entries, err)
			}
			if readAll(t, src) != input {
				t.Fatal("source bytes changed")
			}
		})
	}
}

func TestSQLPreviewEnforcesInferredIdentifierPreservation(t *testing.T) {
	_, err := PreviewSQLConversion(strings.NewReader("id\n42\n007\n"), SQLPreviewOptions{
		SQLConvertOptions: SQLConvertOptions{Delimiter: ',', HasHeader: true, TableName: "ids", IncludeCreateTable: true},
		MaxRows:           2,
	})
	if err == nil || !strings.Contains(err.Error(), "leading-zero integer") {
		t.Fatalf("error = %v, want inferred identifier preservation refusal", err)
	}
}

func TestSQLExplicitBigIntAllowsLeadingZeroFormattingAndNullSentinels(t *testing.T) {
	for _, inferred := range []bool{false, true} {
		var out strings.Builder
		opts := SQLConvertOptions{TableName: "ids", Columns: []string{"id"}, ColumnTypes: []string{SQLTypeBigInt}}
		want := "'+007'"
		if inferred {
			opts.inferredColumnTypes = true
			opts.NullValues = []string{"+007"}
			want = "NULL"
		}
		_, err := ConvertToSQL(context.Background(), strings.NewReader("+007\n"), &out, opts)
		if err != nil || !strings.Contains(out.String(), "("+want+")") {
			t.Fatalf("inferred=%t: output %q, error %v", inferred, out.String(), err)
		}
	}
}
