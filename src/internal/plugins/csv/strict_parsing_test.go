package csv

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const malformedBareQuoteCSV = "id,name\n1,bad\"quote\n"

func TestCSVAnalysisAndConversionPathsRejectMalformedQuotes(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		run  func(io.Reader) error
	}{
		{"schema", func(r io.Reader) error {
			_, err := InferSchemaContext(ctx, r, SchemaOptions{Delimiter: ',', HasHeader: true, MaxBytes: 1024, MaxRows: 10})
			return err
		}},
		{"profile", func(r io.Reader) error {
			_, err := ProfileColumns(ctx, r, SchemaOptions{Delimiter: ',', HasHeader: true, MaxBytes: 1024, MaxRows: 10})
			return err
		}},
		{"preview", func(r io.Reader) error {
			_, err := PreviewRowsContext(ctx, r, PreviewOptions{Delimiter: ',', HasHeader: true, MaxBytes: 1024, MaxRows: 10})
			return err
		}},
		{"project preview", func(r io.Reader) error {
			_, err := PreviewProjectedColumnsContext(ctx, r, ProjectPreviewOptions{Delimiter: ',', Columns: []int{0}, MaxBytes: 1024, MaxRows: 10})
			return err
		}},
		{"project", func(r io.Reader) error {
			_, err := ProjectColumns(ctx, r, io.Discard, ProjectOptions{Delimiter: ',', Columns: []int{0}})
			return err
		}},
		{"SQL preview", func(r io.Reader) error {
			_, err := PreviewSQLConversionContext(ctx, r, SQLPreviewOptions{SQLConvertOptions: SQLConvertOptions{Delimiter: ',', TableName: "records", HasHeader: true}, MaxBytes: 1024, MaxRows: 10})
			return err
		}},
		{"SQL conversion", func(r io.Reader) error {
			_, err := ConvertToSQL(ctx, r, io.Discard, SQLConvertOptions{Delimiter: ',', TableName: "records", HasHeader: true})
			return err
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(strings.NewReader(malformedBareQuoteCSV)); err == nil {
				t.Fatal("malformed CSV was permissively accepted")
			}
		})
	}
}

func TestCSVFileTransformsRejectMalformedQuotesWithoutPublishing(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		run  func(string, string) error
	}{
		{"project", func(src, dst string) error {
			_, err := ProjectColumnsFile(ctx, src, dst, ProjectOptions{Delimiter: ',', Columns: []int{0}})
			return err
		}},
		{"filter", func(src, dst string) error {
			_, err := FilterRowsFile(ctx, src, dst, FilterOptions{Delimiter: ',', HasHeader: true, Column: 0, Op: FilterNonEmpty})
			return err
		}},
		{"dedupe", func(src, dst string) error {
			_, err := DedupeRowsFile(ctx, src, dst, DedupeOptions{Delimiter: ',', HasHeader: true, KeyColumn: -1})
			return err
		}},
		{"sample", func(src, dst string) error {
			_, err := SampleRowsFile(ctx, src, dst, SampleOptions{Delimiter: ',', HasHeader: true, EveryN: 1})
			return err
		}},
		{"SQL", func(src, dst string) error {
			_, err := ConvertToSQLFile(ctx, src, dst, SQLConvertOptions{Delimiter: ',', TableName: "records", HasHeader: true})
			return err
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "source.csv")
			dst := filepath.Join(dir, "output.dat")
			if err := os.WriteFile(src, []byte(malformedBareQuoteCSV), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := test.run(src, dst); err == nil {
				t.Fatal("malformed CSV was permissively accepted")
			}
			if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed transform published output: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "source.csv" {
				t.Fatalf("failed transform left artifacts: %v", entryNames(entries))
			}
		})
	}
}
