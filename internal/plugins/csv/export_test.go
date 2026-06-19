package csv

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestExportJSONL(t *testing.T) {
	src := writeTemp(t, "in.csv", "id,name\n1,alice\n2,bob\n")
	dst := filepath.Join(t.TempDir(), "out.jsonl")
	sum, err := ExportJSONLFile(context.Background(), src, dst, JSONLOptions{Delimiter: ',', HasHeader: true, NumberKeys: true})
	if err != nil {
		t.Fatalf("jsonl: %v", err)
	}
	if sum.RecordsWritten != 2 {
		t.Fatalf("written = %d, want 2", sum.RecordsWritten)
	}
	got := readAll(t, dst)
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[0], `"name":"alice"`) || !strings.Contains(lines[0], `"id":1`) {
		t.Fatalf("first object wrong: %q", lines[0])
	}
}

func TestExportJSONLNoHeader(t *testing.T) {
	src := writeTemp(t, "in.csv", "a,b\nc,d\n")
	dst := filepath.Join(t.TempDir(), "out.jsonl")
	_, err := ExportJSONLFile(context.Background(), src, dst, JSONLOptions{Delimiter: ',', HasHeader: false})
	if err != nil {
		t.Fatalf("jsonl: %v", err)
	}
	got := readAll(t, dst)
	if !strings.Contains(got, `"col1":"a"`) || !strings.Contains(got, `"col2":"b"`) {
		t.Fatalf("expected colN keys: %q", got)
	}
}

func TestExportSQLite(t *testing.T) {
	src := writeTemp(t, "in.csv", "id,name,score\n1,alice,9.5\n2,bob,7\n007,carol,3\n")
	dst := filepath.Join(t.TempDir(), "out.db")
	sum, err := ExportSQLiteFile(context.Background(), src, dst, SQLiteOptions{Delimiter: ',', HasHeader: true, TableName: "people", TypedCells: true})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if sum.RecordsWritten != 3 {
		t.Fatalf("written = %d, want 3", sum.RecordsWritten)
	}
	db, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM people`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("row count = %d, want 3", n)
	}
	// numeric cell stored as a number → SQL numeric comparison works
	var name string
	if err := db.QueryRow(`SELECT name FROM people WHERE score > 9`).Scan(&name); err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "alice" {
		t.Fatalf("score filter wrong: %q", name)
	}
	// leading-zero id preserved as text
	var id string
	if err := db.QueryRow(`SELECT id FROM people WHERE name='carol'`).Scan(&id); err != nil {
		t.Fatalf("query id: %v", err)
	}
	if id != "007" {
		t.Fatalf("leading zero not preserved: %q", id)
	}
}

func TestExportSQLiteRefusesExisting(t *testing.T) {
	src := writeTemp(t, "in.csv", "a\n1\n")
	dst := writeTemp(t, "exists.db", "x")
	_, err := ExportSQLiteFile(context.Background(), src, dst, SQLiteOptions{HasHeader: true})
	if err == nil {
		t.Fatal("expected refusal to overwrite existing file")
	}
}

func TestExportXLSX(t *testing.T) {
	src := writeTemp(t, "in.csv", "id,name\n1,alice\n2,bob\n")
	dst := filepath.Join(t.TempDir(), "out.xlsx")
	sum, err := ExportXLSXFile(context.Background(), src, dst, XLSXOptions{Delimiter: ',', HasHeader: true, SheetName: "data", TypedCells: true})
	if err != nil {
		t.Fatalf("xlsx: %v", err)
	}
	if sum.RecordsWritten != 3 { // header + 2 rows
		t.Fatalf("written = %d, want 3", sum.RecordsWritten)
	}
	fx, err := excelize.OpenFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer fx.Close()
	v, err := fx.GetCellValue("data", "B2")
	if err != nil {
		t.Fatal(err)
	}
	if v != "alice" {
		t.Fatalf("B2 = %q, want alice", v)
	}
}

func TestMarkdownPreview(t *testing.T) {
	md := MarkdownPreview([]string{"id", "name"}, [][]string{{"1", "a|b"}, {"2", "c"}})
	if !strings.Contains(md, "| id | name |") {
		t.Fatalf("header missing: %q", md)
	}
	if !strings.Contains(md, "a\\|b") {
		t.Fatalf("pipe not escaped: %q", md)
	}
	if !strings.Contains(md, "| --- | --- |") {
		t.Fatalf("separator missing: %q", md)
	}
}
