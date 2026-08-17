package csv

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestExportJSONLNonFiniteAndBigInt(t *testing.T) {
	// NaN/Inf and integers past int64 must stay strings instead of being
	// silently converted to lossy or non-JSON floating-point values.
	src := writeTemp(t, "in.csv", "a,b,c,d\nNaN,Inf,12345678901234567890,3.5\n")
	dst := filepath.Join(t.TempDir(), "out.jsonl")
	_, err := ExportJSONLFile(context.Background(), src, dst, JSONLOptions{Delimiter: ',', HasHeader: true, NumberKeys: true})
	if err != nil {
		t.Fatalf("export aborted on NaN/Inf/bigint: %v", err)
	}
	got := readAll(t, dst)
	if !strings.Contains(got, `"a":"NaN"`) || !strings.Contains(got, `"b":"Inf"`) {
		t.Fatalf("NaN/Inf not kept as strings: %q", got)
	}
	if !strings.Contains(got, `"c":12345678901234567890`) {
		t.Fatalf("high-precision integer token not preserved exactly: %q", got)
	}
	if !strings.Contains(got, `"d":3.5`) {
		t.Fatalf("genuine float not emitted as number: %q", got)
	}
}

func TestExportJSONLPreservesExactNumericTokensAndIdentifiers(t *testing.T) {
	src := writeTemp(t, "in.csv", "plus,negzero,negid,uid,big,decimal,exp,nan,space\n+007,-0,-007,007,12345678901234567890,0.12345678901234567890,1.2300e+99,NaN,\" 42 \"\n")
	dst := filepath.Join(t.TempDir(), "out.jsonl")
	if _, err := ExportJSONLFile(context.Background(), src, dst, JSONLOptions{Delimiter: ',', HasHeader: true, NumberKeys: true}); err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(readAll(t, dst))), &object); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"plus": `"+007"`, "negzero": `-0`, "negid": `"-007"`, "uid": `"007"`,
		"big": `12345678901234567890`, "decimal": `0.12345678901234567890`,
		"exp": `1.2300e+99`, "nan": `"NaN"`, "space": `" 42 "`,
	}
	for key, expected := range want {
		if got := string(object[key]); got != expected {
			t.Errorf("%s = %s, want %s", key, got, expected)
		}
	}
}

func TestExportJSONLRejectsDuplicateDerivedKeysBeforeOutput(t *testing.T) {
	for _, input := range []string{
		"id,id\nfirst,second\n",
		"col2,\nfirst,second\n",
	} {
		dir := t.TempDir()
		src := writeTemp(t, "in.csv", input)
		dst := filepath.Join(dir, "out.jsonl")
		if _, err := ExportJSONLFile(context.Background(), src, dst, JSONLOptions{Delimiter: ',', HasHeader: true}); err == nil || !strings.Contains(err.Error(), "derived by both CSV columns") {
			t.Fatalf("duplicate-key error = %v", err)
		}
		if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("duplicate keys created output: %v", err)
		}
	}
}

func TestExportJSONLRejectsMalformedAndRaggedRowsWithoutPublishing(t *testing.T) {
	for _, input := range []string{
		"a,b\n1\n",
		"a,b\n1,2,3\n",
		"a,b\n1,\"unterminated\n",
	} {
		dir := t.TempDir()
		src := writeTemp(t, "in.csv", input)
		dst := filepath.Join(dir, "out.jsonl")
		if _, err := ExportJSONLFile(context.Background(), src, dst, JSONLOptions{Delimiter: ',', HasHeader: true}); err == nil {
			t.Fatalf("malformed/ragged input %q unexpectedly succeeded", input)
		}
		if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("malformed/ragged input created output: %v", err)
		}
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

func TestExportSQLiteDisabledBeforeCreatingOutput(t *testing.T) {
	src := writeTemp(t, "in.csv", "id,name\n1,alice\n")
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.db")
	_, err := ExportSQLiteFile(context.Background(), src, dst, SQLiteOptions{Delimiter: ',', HasHeader: true, TableName: "people"})
	if !errors.Is(err, ErrSQLiteExportSecurePublicationUnavailable) {
		t.Fatalf("sqlite error = %v, want secure-publication unavailable", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("disabled export created output or scratch entries: %v", entries)
	}
}

func TestExportSQLiteDisabledPreservesExisting(t *testing.T) {
	src := writeTemp(t, "in.csv", "id,name\n1,alice\n")
	dst := writeTemp(t, "exists.db", "not a database")
	if _, err := ExportSQLiteFile(context.Background(), src, dst, SQLiteOptions{}); !errors.Is(err, ErrSQLiteExportSecurePublicationUnavailable) {
		t.Fatalf("sqlite error = %v, want secure-publication unavailable", err)
	}
	if got := readAll(t, dst); got != "not a database" {
		t.Fatalf("disabled export changed destination: %q", got)
	}
}

func TestExportRefusesExistingAndLeavesNoScratch(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.csv")
	if err := os.WriteFile(src, []byte("id,name\n1,alice\n2,bob\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.jsonl")
	if err := os.WriteFile(dst, []byte("STALE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportJSONLFile(context.Background(), src, dst, JSONLOptions{Delimiter: ',', HasHeader: true}); err == nil {
		t.Fatal("expected existing-output error")
	}
	if got := readAll(t, dst); got != "STALE" {
		t.Fatalf("destination changed: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("unexpected scratch entries: %v", entries)
	}
}

func TestExportXLSXDisabledBeforeCreatingOutput(t *testing.T) {
	src := writeTemp(t, "in.csv", "id,name\n1,alice\n2,bob\n")
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.xlsx")
	_, err := ExportXLSXFile(context.Background(), src, dst, XLSXOptions{Delimiter: ',', HasHeader: true, SheetName: "data", TypedCells: true})
	if !errors.Is(err, ErrXLSXExportSecureScratchUnavailable) {
		t.Fatalf("xlsx error = %v, want secure-scratch unavailable", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("disabled export created output or scratch entries: %v", entries)
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
