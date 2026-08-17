package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/encodingx"
	"github.com/quarry/quarry-wails3/internal/plugins/csv"
)

func TestCSVServiceSamplesDecodeDocumentEncoding(t *testing.T) {
	t.Parallel()
	const content = "id,note\r\n1,Привет\r\n2,мир\r\n"
	tests := []struct {
		name     string
		encoding string
		bom      bool
	}{
		{name: "UTF-8 BOM", encoding: "UTF-8", bom: true},
		{name: "UTF-16LE BOM", encoding: "UTF-16LE", bom: true},
		{name: "UTF-16BE BOM", encoding: "UTF-16BE", bom: true},
		{name: "UTF-16LE no BOM", encoding: "UTF-16LE"},
		{name: "UTF-16BE no BOM", encoding: "UTF-16BE"},
		{name: "Windows-1251", encoding: "Windows-1251"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := writeCSVServiceEncodingFixture(t, test.encoding, test.bom, content)
			svc := NewFileService()
			meta, err := svc.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer svc.CloseFile(meta.FileID)
			if !strings.EqualFold(meta.Encoding, test.encoding) {
				t.Fatalf("detected encoding = %q, want %q", meta.Encoding, test.encoding)
			}

			inspect, err := svc.CsvInspect(meta.FileID)
			if err != nil {
				t.Fatalf("CsvInspect() error = %v", err)
			}
			if inspect.Delimiter != "," || inspect.Columns != 2 {
				t.Fatalf("inspect = %+v, want comma/two columns", inspect)
			}

			preview, err := svc.CsvPreview(meta.FileID, ",", true, 10)
			if err != nil {
				t.Fatalf("CsvPreview() error = %v", err)
			}
			if !reflect.DeepEqual(preview.Header, []string{"id", "note"}) {
				t.Fatalf("header = %#v", preview.Header)
			}
			wantRows := [][]string{{"1", "Привет"}, {"2", "мир"}}
			if !reflect.DeepEqual(preview.Rows, wantRows) {
				t.Fatalf("rows = %#v, want %#v", preview.Rows, wantRows)
			}
		})
	}
}

func TestGetCSVGridUsesParserConfirmedRawCursors(t *testing.T) {
	t.Parallel()
	const content = "id,note\n1,\"line one\nline two\"\n2,tail\n"
	path := writeCSVServiceEncodingFixture(t, "UTF-8", false, content)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)

	// The raw budget ends after the physical newline inside the quoted field.
	// Only the header is a complete logical record at that seam.
	first, err := svc.GetCsvGrid(meta.FileID, ",", 0, len("id,note\n1,\"line one\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Rows, [][]string{{"id", "note"}}) {
		t.Fatalf("first rows = %#v", first.Rows)
	}
	if first.StartByte != 0 || first.NextByte != int64(len("id,note\n")) {
		t.Fatalf("first cursor = [%d,%d), want [0,%d)", first.StartByte, first.NextByte, len("id,note\n"))
	}
	if first.Generation == 0 || first.FileID != meta.FileID || first.Encoding != "UTF-8" {
		t.Fatalf("first identity = %+v", first)
	}

	second, err := svc.GetCsvGrid(meta.FileID, ",", first.NextByte, 128)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"1", "line one\nline two"}, {"2", "tail"}}
	if !reflect.DeepEqual(second.Rows, want) {
		t.Fatalf("second rows = %#v, want %#v", second.Rows, want)
	}
	if second.StartByte != first.NextByte || second.NextByte != int64(len(content)) || !second.AtEof {
		t.Fatalf("second cursor = %+v", second)
	}
}

func TestGetCSVGridRejectsUnissuedWrongDelimiterAndStaleGenerationCursors(t *testing.T) {
	t.Parallel()
	const content = "id,note\n1,\"line one\nline two\"\n2,tail\n"
	path := writeCSVServiceEncodingFixture(t, "UTF-8", false, content)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)

	first, err := svc.GetCsvGrid(meta.FileID, ",", 0, len("id,note\n1,\"line one\n"))
	if err != nil {
		t.Fatal(err)
	}
	midRecord := int64(strings.Index(content, "line one") + 2)
	if _, err := svc.GetCsvGrid(meta.FileID, ",", midRecord, 128); !errors.Is(err, ErrCSVGridCursorInvalid) {
		t.Fatalf("mid-record cursor error = %v, want ErrCSVGridCursorInvalid", err)
	}
	if _, err := svc.GetCsvGrid(meta.FileID, ";", first.NextByte, 128); !errors.Is(err, ErrCSVGridCursorInvalid) {
		t.Fatalf("wrong-delimiter cursor error = %v, want ErrCSVGridCursorInvalid", err)
	}
	if _, err := svc.RefreshFile(meta.FileID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetCsvGrid(meta.FileID, ",", first.NextByte, 128); !errors.Is(err, ErrCSVGridCursorInvalid) {
		t.Fatalf("stale-generation cursor error = %v, want ErrCSVGridCursorInvalid", err)
	}
}

func TestGetCSVGridMapsUTF16BOMAndSurrogateSeamsToRawOffsets(t *testing.T) {
	t.Parallel()
	const content = "id,note\n1,\"😀 first\nsecond\"\n2,tail\n"
	path := writeCSVServiceEncodingFixture(t, "UTF-16LE", true, content)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)

	headerRawEnd := int64(2 + 2*len("id,note\n"))
	// Stop after the high surrogate of the first data record. The decoder must
	// omit that incomplete pair and the CSV cursor must remain at the header.
	budget := int(headerRawEnd + int64(2*len("1,\"")+2))
	first, err := svc.GetCsvGrid(meta.FileID, ",", 0, budget)
	if err != nil {
		t.Fatal(err)
	}
	if first.NextByte != headerRawEnd || !reflect.DeepEqual(first.Rows, [][]string{{"id", "note"}}) {
		t.Fatalf("UTF-16 first page = %+v", first)
	}
	second, err := svc.GetCsvGrid(meta.FileID, ",", first.NextByte, 256)
	if err != nil {
		t.Fatal(err)
	}
	if second.Rows[0][1] != "😀 first\nsecond" || second.NextByte != meta.Size {
		t.Fatalf("UTF-16 second page = %+v", second)
	}
}

func TestGetCSVGridRejectsOversizedLogicalRecordWithoutAdvancing(t *testing.T) {
	t.Parallel()
	path := writeCSVServiceEncodingFixture(t, "UTF-8", false, "1,\""+strings.Repeat("x", 4096)+"\"\n")
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	if _, err := svc.GetCsvGrid(meta.FileID, ",", 0, 128); !errors.Is(err, csv.ErrCSVGridRecordExceedsWindow) {
		t.Fatalf("oversized grid record error = %v", err)
	}
	if _, err := svc.GetCsvGrid(meta.FileID, ",", 0, csvGridMaxRawBytes+1); err == nil {
		t.Fatal("oversized raw grid budget unexpectedly accepted")
	}
}

func TestGetCSVGridBOMOnlyAndTinyBudgetsNeverRepeatAnEmptyCursor(t *testing.T) {
	t.Parallel()
	for _, encoding := range []string{"UTF-8", "UTF-16LE", "UTF-16BE"} {
		encoding := encoding
		t.Run(encoding, func(t *testing.T) {
			t.Parallel()
			path := writeCSVServiceEncodingFixture(t, encoding, true, "")
			svc := NewFileService()
			meta, err := svc.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer svc.CloseFile(meta.FileID)
			bomBytes := len(encodingx.BOMBytes(encoding))
			if _, err := svc.GetCsvGrid(meta.FileID, ",", 0, bomBytes-1); err == nil {
				t.Fatal("budget splitting the BOM unexpectedly returned a repeatable cursor")
			}
			grid, err := svc.GetCsvGrid(meta.FileID, ",", 0, bomBytes)
			if err != nil {
				t.Fatal(err)
			}
			if grid.NextByte != meta.Size || grid.NextByte <= grid.StartByte || !grid.AtEof || len(grid.Rows) != 0 {
				t.Fatalf("BOM-only grid = %+v", grid)
			}
		})
	}

	emptyPath := writeCSVServiceEncodingFixture(t, "UTF-8", false, "")
	emptyService := NewFileService()
	emptyMeta, err := emptyService.OpenFile(emptyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer emptyService.CloseFile(emptyMeta.FileID)
	empty, err := emptyService.GetCsvGrid(emptyMeta.FileID, ",", 0, 1)
	if err != nil || empty.NextByte != 0 || !empty.AtEof {
		t.Fatalf("empty grid = %+v, %v", empty, err)
	}
}

func TestGetCSVGridEnforcesSmallRowAndCellResponseCaps(t *testing.T) {
	t.Parallel()
	rows := strings.Repeat("x\n", csvGridMaxRows+1)
	path := writeCSVServiceEncodingFixture(t, "UTF-8", false, rows)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	first, err := svc.GetCsvGrid(meta.FileID, ",", 0, len(rows))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Rows) != csvGridMaxRows || first.AtEof || first.NextByte <= 0 || first.NextByte >= meta.Size {
		t.Fatalf("row-capped page = rows %d, cursor %+v", len(first.Rows), first)
	}
	second, err := svc.GetCsvGrid(meta.FileID, ",", first.NextByte, len(rows))
	if err != nil || len(second.Rows) != 1 || !second.AtEof {
		t.Fatalf("row-capped continuation = %+v, %v", second, err)
	}

	columns := 201
	row := strings.Repeat("x,", columns-1) + "x\n"
	hostile := strings.Repeat(row, csvGridMaxCells/columns+1)
	hostilePath := writeCSVServiceEncodingFixture(t, "UTF-8", false, hostile)
	hostileService := NewFileService()
	hostileMeta, err := hostileService.OpenFile(hostilePath)
	if err != nil {
		t.Fatal(err)
	}
	defer hostileService.CloseFile(hostileMeta.FileID)
	if _, err := hostileService.GetCsvGrid(hostileMeta.FileID, ",", 0, len(hostile)); !errors.Is(err, csv.ErrCSVGridShapeLimit) {
		t.Fatalf("hostile tiny-cell shape error = %v, want ErrCSVGridShapeLimit", err)
	}
}

func TestCSVArtifactMethodsRejectNonUTF8BeforeDialogOrOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "source.csv")
	encoded, err := encodingx.EncodeString("Windows-1251", "id,note\n1,Привет\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	inspection, err := svc.CsvInspect(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	sourceGeneration := inspection.Generation

	calls := []struct {
		name string
		call func() error
	}{
		{name: "project", call: func() error {
			_, err := svc.CsvProjectViaDialog(meta.FileID, sourceGeneration, ",", []int{0})
			return err
		}},
		{name: "add column", call: func() error {
			_, err := svc.CsvAddColumnViaDialog(meta.FileID, sourceGeneration, ",", 1, "x")
			return err
		}},
		{name: "SQL", call: func() error {
			_, err := svc.CsvToSQLViaDialog(meta.FileID, sourceGeneration, ",", "items", true, true)
			return err
		}},
		{name: "redact", call: func() error {
			_, err := svc.CsvRedactViaDialog(meta.FileID, sourceGeneration, ",", true, []CsvRedactColumn{{Index: 1, Mode: "fixed"}}, "MASK")
			return err
		}},
		{name: "filter", call: func() error {
			_, err := svc.CsvFilterViaDialog(meta.FileID, sourceGeneration, ",", true, 0, "eq", "1", false)
			return err
		}},
		{name: "dedupe", call: func() error {
			_, err := svc.CsvDedupeViaDialog(meta.FileID, sourceGeneration, ",", true, -1)
			return err
		}},
		{name: "sample", call: func() error {
			_, err := svc.CsvSampleViaDialog(meta.FileID, sourceGeneration, ",", true, 2)
			return err
		}},
		{name: "JSONL", call: func() error {
			_, err := svc.CsvExportJSONLViaDialog(meta.FileID, sourceGeneration, ",", true, false)
			return err
		}},
		{name: "configured SQL", call: func() error {
			_, err := svc.CsvToSQLConfigViaDialog(meta.FileID, sourceGeneration, CsvSqlConfig{
				Delimiter: ",", HasHeader: true, TableName: "items", InsertMode: "insert", BatchSize: 100,
				OnInvalid: "fail", Columns: []CsvSqlColumnConfig{{Source: 0, Name: "id", Type: "TEXT", Include: true}},
			})
			return err
		}},
	}
	for _, call := range calls {
		if err := call.call(); !errors.Is(err, ErrCSVTransformEncodingUnsupported) {
			t.Errorf("%s error = %v, want ErrCSVTransformEncodingUnsupported", call.name, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "source.csv" {
		t.Fatalf("artifact rejection touched output paths: %#v", entries)
	}
}

func writeCSVServiceEncodingFixture(t *testing.T, encoding string, bom bool, content string) string {
	t.Helper()
	data, err := encodingx.EncodeString(encoding, content)
	if err != nil {
		t.Fatal(err)
	}
	if bom {
		data = append(append([]byte(nil), encodingx.BOMBytes(encoding)...), data...)
	}
	path := filepath.Join(t.TempDir(), "fixture.csv")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
