package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/plugins/csv"
)

func TestCSVRedactOptionsRejectsInvalidSelections(t *testing.T) {
	tests := []struct {
		name      string
		columns   []CsvRedactColumn
		errorPart string
	}{
		{name: "none", errorPart: "select at least one column"},
		{name: "negative index", columns: []CsvRedactColumn{{Index: -1, Mode: "hash"}}, errorPart: "is negative"},
		{name: "duplicate index", columns: []CsvRedactColumn{{Index: 1, Mode: "hash"}, {Index: 1, Mode: "null"}}, errorPart: "selected more than once"},
		{name: "empty mode", columns: []CsvRedactColumn{{Index: 1, Mode: ""}}, errorPart: "invalid redaction mode"},
		{name: "unknown mode", columns: []CsvRedactColumn{{Index: 1, Mode: "hashed"}}, errorPart: "invalid redaction mode"},
		{name: "future mode", columns: []CsvRedactColumn{{Index: 1, Mode: "tokenize"}}, errorPart: "invalid redaction mode"},
		{name: "mixed case mode", columns: []CsvRedactColumn{{Index: 1, Mode: "Hash"}}, errorPart: "invalid redaction mode"},
		{name: "upper case mode", columns: []CsvRedactColumn{{Index: 1, Mode: "HASH"}}, errorPart: "invalid redaction mode"},
		{name: "leading whitespace", columns: []CsvRedactColumn{{Index: 1, Mode: " hash"}}, errorPart: "invalid redaction mode"},
		{name: "trailing whitespace", columns: []CsvRedactColumn{{Index: 1, Mode: "hash "}}, errorPart: "invalid redaction mode"},
		{name: "multi-rune delimiter", columns: []CsvRedactColumn{{Index: 1, Mode: "hash"}}, errorPart: "one exact valid rune"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			delimiter := ","
			if test.name == "multi-rune delimiter" {
				delimiter = "||"
			}
			_, err := csvRedactOptions(delimiter, true, test.columns, "REDACTED")
			if err == nil || !strings.Contains(err.Error(), test.errorPart) {
				t.Fatalf("error = %v, want substring %q", err, test.errorPart)
			}
		})
	}
}

func TestCSVRedactOptionsRejectsAmbiguousDelimiters(t *testing.T) {
	for _, delimiter := range []string{"||", "comma,", "\x00", "\r", "\n", "\""} {
		_, err := csvRedactOptions(delimiter, true, []CsvRedactColumn{{Index: 1, Mode: "hash"}}, "")
		if err == nil {
			t.Fatalf("delimiter %q was accepted", delimiter)
		}
	}
}

func TestCSVRedactOptionsAcceptsOnlyExactModes(t *testing.T) {
	columns := []CsvRedactColumn{
		{Index: 0, Mode: "null"},
		{Index: 1, Mode: "fixed"},
		{Index: 2, Mode: "hash"},
		{Index: 3, Mode: "email"},
	}
	opts, err := csvRedactOptions("comma", true, columns, "PRIVATE")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Delimiter != ',' || !opts.HasHeader || opts.Replacement != "PRIVATE" {
		t.Fatalf("options = %+v", opts)
	}
	wantModes := []csv.RedactMode{csv.RedactNull, csv.RedactFixed, csv.RedactHash, csv.RedactEmail}
	for index, want := range wantModes {
		if got := opts.Columns[index]; got != want {
			t.Fatalf("column %d mode = %q, want %q", index, got, want)
		}
	}
}

func TestCSVRedactOptionsGenerateFreshOperationKeys(t *testing.T) {
	columns := []CsvRedactColumn{{Index: 1, Mode: "hash"}}
	first, err := csvRedactOptions(",", true, columns, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := csvRedactOptions(",", true, columns, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.PseudonymKey) != csv.PseudonymKeyBytes || len(second.PseudonymKey) != csv.PseudonymKeyBytes {
		t.Fatalf("generated key lengths = %d and %d", len(first.PseudonymKey), len(second.PseudonymKey))
	}
	if bytes.Equal(first.PseudonymKey, second.PseudonymKey) {
		t.Fatal("separate redaction operations reused a pseudonym key")
	}
}

func TestCSVRedactEmailRequiresUTF8BeforeDialog(t *testing.T) {
	email := csv.RedactOptions{Columns: map[int]csv.RedactMode{1: csv.RedactEmail}}
	for _, encoding := range []string{"Windows-1251", "Windows-1252", "UTF-16LE"} {
		if err := validateCsvRedactEncoding(encoding, email); err == nil {
			t.Fatalf("encoding %q accepted for email redaction", encoding)
		}
	}
	for _, encoding := range []string{"UTF-8", "utf8", "ASCII"} {
		if err := validateCsvRedactEncoding(encoding, email); err != nil {
			t.Fatalf("encoding %q rejected: %v", encoding, err)
		}
	}
	fixed := csv.RedactOptions{Columns: map[int]csv.RedactMode{1: csv.RedactFixed}}
	if err := validateCsvRedactEncoding("Windows-1252", fixed); err != nil {
		t.Fatalf("byte-safe fixed redaction rejected: %v", err)
	}
}
