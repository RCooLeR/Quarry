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

const integrationRecordLimit int64 = 32

func oversizedMultilineCSV() string {
	return "a,b\n1,\"" + strings.Repeat("x", 20) + "\n" + strings.Repeat("y", 20) + "\"\n"
}

func TestCSVReaderOperationsEnforceLogicalRecordLimit(t *testing.T) {
	input := oversizedMultilineCSV()
	ctx := context.Background()
	tests := []struct {
		name string
		run  func(io.Reader) error
	}{
		{
			name: "inspect",
			run: func(r io.Reader) error {
				_, err := InspectReaderContext(ctx, r, InspectOptions{
					MaxBytes: 1024, MaxRows: 10, MaxRecordBytes: integrationRecordLimit,
					Delimiters: []rune{','},
				})
				return err
			},
		},
		{
			name: "schema",
			run: func(r io.Reader) error {
				_, err := InferSchemaContext(ctx, r, SchemaOptions{
					Delimiter: ',', HasHeader: true, MaxBytes: 1024, MaxRows: 10,
					MaxRecordBytes: integrationRecordLimit,
				})
				return err
			},
		},
		{
			name: "profile",
			run: func(r io.Reader) error {
				_, err := ProfileColumns(ctx, r, SchemaOptions{
					Delimiter: ',', HasHeader: true, MaxBytes: 1024, MaxRows: 10,
					MaxRecordBytes: integrationRecordLimit,
				})
				return err
			},
		},
		{
			name: "preview",
			run: func(r io.Reader) error {
				_, err := PreviewRowsContext(ctx, r, PreviewOptions{
					Delimiter: ',', HasHeader: true, MaxBytes: 1024, MaxRows: 10,
					MaxRecordBytes: integrationRecordLimit,
				})
				return err
			},
		},
		{
			name: "column guide",
			run: func(r io.Reader) error {
				_, err := BuildColumnGuideContext(ctx, r, ColumnGuideOptions{
					Delimiter: ',', HasHeader: true, MaxBytes: 1024, MaxRows: 10,
					MaxRecordBytes: integrationRecordLimit,
				})
				return err
			},
		},
		{
			name: "project preview",
			run: func(r io.Reader) error {
				_, err := PreviewProjectedColumnsContext(ctx, r, ProjectPreviewOptions{
					Delimiter: ',', Columns: []int{0}, MaxBytes: 1024, MaxRows: 10,
					MaxRecordBytes: integrationRecordLimit,
				})
				return err
			},
		},
		{
			name: "project",
			run: func(r io.Reader) error {
				var output strings.Builder
				_, err := ProjectColumns(ctx, r, &output, ProjectOptions{
					Delimiter: ',', Columns: []int{0}, MaxRecordBytes: integrationRecordLimit,
				})
				return err
			},
		},
		{
			name: "add column",
			run: func(r io.Reader) error {
				var output strings.Builder
				_, err := AddColumn(ctx, r, &output, AddColumnOptions{
					Delimiter: ',', Position: 1, Value: "new", MaxRecordBytes: integrationRecordLimit,
				})
				return err
			},
		},
		{
			name: "SQL preview",
			run: func(r io.Reader) error {
				_, err := PreviewSQLConversionContext(ctx, r, SQLPreviewOptions{
					SQLConvertOptions: SQLConvertOptions{
						Delimiter: ',', TableName: "records", HasHeader: true,
						MaxRecordBytes: integrationRecordLimit,
					},
					MaxBytes: 1024, MaxRows: 10,
				})
				return err
			},
		},
		{
			name: "SQL conversion",
			run: func(r io.Reader) error {
				var output strings.Builder
				_, err := ConvertToSQL(ctx, r, &output, SQLConvertOptions{
					Delimiter: ',', TableName: "records", HasHeader: true,
					MaxRecordBytes: integrationRecordLimit,
				})
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run(strings.NewReader(input))
			if !errors.Is(err, ErrCSVRecordTooLarge) {
				t.Fatalf("error = %v, want ErrCSVRecordTooLarge", err)
			}
			var limitErr *RecordLimitError
			if !errors.As(err, &limitErr) {
				t.Fatalf("error type = %T, want *RecordLimitError", err)
			}
			if limitErr.Record != 2 || limitErr.StartOffset != 4 || limitErr.LimitBytes != integrationRecordLimit {
				t.Fatalf("limit error = %+v", limitErr)
			}
		})
	}
}

func TestCSVReaderOperationsEnforceFieldLimit(t *testing.T) {
	input := "ok\n" + strings.Repeat(",", MaxCSVFieldsPerRecord) + "\n"
	ctx := context.Background()
	tests := []struct {
		name string
		run  func(io.Reader) error
	}{
		{"inspect", func(r io.Reader) error {
			_, err := InspectReaderContext(ctx, r, InspectOptions{MaxBytes: int64(len(input)), MaxRows: 10, Delimiters: []rune{','}})
			return err
		}},
		{"schema", func(r io.Reader) error {
			_, err := InferSchemaContext(ctx, r, SchemaOptions{Delimiter: ',', HasHeader: true, MaxBytes: int64(len(input)), MaxRows: 10})
			return err
		}},
		{"profile", func(r io.Reader) error {
			_, err := ProfileColumns(ctx, r, SchemaOptions{Delimiter: ',', HasHeader: true, MaxBytes: int64(len(input)), MaxRows: 10})
			return err
		}},
		{"preview", func(r io.Reader) error {
			_, err := PreviewRowsContext(ctx, r, PreviewOptions{Delimiter: ',', HasHeader: true, MaxBytes: int64(len(input)), MaxRows: 10})
			return err
		}},
		{"project", func(r io.Reader) error {
			_, err := ProjectColumns(ctx, r, io.Discard, ProjectOptions{Delimiter: ',', Columns: []int{0}})
			return err
		}},
		{"add column", func(r io.Reader) error {
			_, err := AddColumn(ctx, r, io.Discard, AddColumnOptions{Delimiter: ',', Position: 0, Value: "new"})
			return err
		}},
		{"SQL preview", func(r io.Reader) error {
			_, err := PreviewSQLConversionContext(ctx, r, SQLPreviewOptions{
				SQLConvertOptions: SQLConvertOptions{Delimiter: ',', TableName: "records", HasHeader: true},
				MaxBytes:          int64(len(input)), MaxRows: 10,
			})
			return err
		}},
		{"SQL conversion", func(r io.Reader) error {
			_, err := ConvertToSQL(ctx, r, io.Discard, SQLConvertOptions{Delimiter: ',', TableName: "records", HasHeader: true})
			return err
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run(strings.NewReader(input))
			assertFieldLimitError(t, err, 2, 3, MaxCSVFieldsPerRecord+1)
		})
	}
}

func TestCSVFileOperationDoesNotPublishOnFieldLimit(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.csv")
	output := filepath.Join(dir, "output.csv")
	input := "ok\n" + strings.Repeat(",", MaxCSVFieldsPerRecord) + "\n"
	if err := os.WriteFile(source, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ProjectColumnsFile(context.Background(), source, output, ProjectOptions{Delimiter: ',', Columns: []int{0}})
	if !errors.Is(err, ErrCSVTooManyFields) {
		t.Fatalf("error = %v, want ErrCSVTooManyFields", err)
	}
	if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("final output exists after field-limit refusal: %v", statErr)
	}
	got, readErr := os.ReadFile(source)
	if readErr != nil || string(got) != input {
		t.Fatalf("source changed: read error %v, bytes %q", readErr, got)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 || entries[0].Name() != "source.csv" {
		t.Fatalf("temporary output was not cleaned up: %v", entryNames(entries))
	}
}

func TestCSVReaderOperationsValidateRecordLimitBeforeReading(t *testing.T) {
	ctx := context.Background()
	invalid := MaxLogicalRecordBytes + 1
	tests := []struct {
		name string
		run  func(io.Reader) error
	}{
		{"inspect", func(r io.Reader) error {
			_, err := InspectReaderContext(ctx, r, InspectOptions{MaxRecordBytes: invalid})
			return err
		}},
		{"schema", func(r io.Reader) error {
			_, err := InferSchemaContext(ctx, r, SchemaOptions{MaxRecordBytes: invalid})
			return err
		}},
		{"profile", func(r io.Reader) error {
			_, err := ProfileColumns(ctx, r, SchemaOptions{MaxRecordBytes: invalid})
			return err
		}},
		{"preview", func(r io.Reader) error {
			_, err := PreviewRowsContext(ctx, r, PreviewOptions{MaxRecordBytes: invalid})
			return err
		}},
		{"column guide", func(r io.Reader) error {
			_, err := BuildColumnGuideContext(ctx, r, ColumnGuideOptions{MaxRecordBytes: invalid})
			return err
		}},
		{"project preview", func(r io.Reader) error {
			_, err := PreviewProjectedColumnsContext(ctx, r, ProjectPreviewOptions{Columns: []int{0}, MaxRecordBytes: invalid})
			return err
		}},
		{"project", func(r io.Reader) error {
			_, err := ProjectColumns(ctx, r, io.Discard, ProjectOptions{Columns: []int{0}, MaxRecordBytes: invalid})
			return err
		}},
		{"add column", func(r io.Reader) error {
			_, err := AddColumn(ctx, r, io.Discard, AddColumnOptions{Delimiter: ',', MaxRecordBytes: invalid})
			return err
		}},
		{"SQL preview", func(r io.Reader) error {
			_, err := PreviewSQLConversionContext(ctx, r, SQLPreviewOptions{SQLConvertOptions: SQLConvertOptions{TableName: "records", MaxRecordBytes: invalid}})
			return err
		}},
		{"SQL conversion", func(r io.Reader) error {
			_, err := ConvertToSQL(ctx, r, io.Discard, SQLConvertOptions{TableName: "records", MaxRecordBytes: invalid})
			return err
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &countReadsReader{reader: strings.NewReader("a,b\n")}
			if err := test.run(source); err == nil {
				t.Fatal("invalid record limit was accepted")
			}
			if source.reads != 0 {
				t.Fatalf("source was read %d times before limit validation", source.reads)
			}
		})
	}
}

func TestCSVFileOperationsDoNotPublishPartialOutputOnRecordLimit(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		run  func(string, string) error
	}{
		{"project", func(src, dst string) error {
			_, err := ProjectColumnsFile(ctx, src, dst, ProjectOptions{Delimiter: ',', Columns: []int{0}, MaxRecordBytes: integrationRecordLimit})
			return err
		}},
		{"filter", func(src, dst string) error {
			_, err := FilterRowsFile(ctx, src, dst, FilterOptions{Delimiter: ',', HasHeader: true, Column: 0, Op: FilterNonEmpty, MaxRecordBytes: integrationRecordLimit})
			return err
		}},
		{"dedupe", func(src, dst string) error {
			_, err := DedupeRowsFile(ctx, src, dst, DedupeOptions{Delimiter: ',', HasHeader: true, KeyColumn: -1, MaxRecordBytes: integrationRecordLimit})
			return err
		}},
		{"sample", func(src, dst string) error {
			_, err := SampleRowsFile(ctx, src, dst, SampleOptions{Delimiter: ',', HasHeader: true, EveryN: 1, MaxRecordBytes: integrationRecordLimit})
			return err
		}},
		{"redact", func(src, dst string) error {
			_, err := RedactColumnsFile(ctx, src, dst, RedactOptions{Delimiter: ',', HasHeader: true, Columns: map[int]RedactMode{0: RedactFixed}, MaxRecordBytes: integrationRecordLimit})
			return err
		}},
		{"JSONL", func(src, dst string) error {
			_, err := ExportJSONLFile(ctx, src, dst, JSONLOptions{Delimiter: ',', HasHeader: true, MaxRecordBytes: integrationRecordLimit})
			return err
		}},
		{"add column", func(src, dst string) error {
			_, err := AddColumnFile(ctx, src, dst, AddColumnOptions{Delimiter: ',', Position: 1, Value: "new", MaxRecordBytes: integrationRecordLimit})
			return err
		}},
		{"SQL", func(src, dst string) error {
			_, err := ConvertToSQLFile(ctx, src, dst, SQLConvertOptions{Delimiter: ',', TableName: "records", HasHeader: true, MaxRecordBytes: integrationRecordLimit})
			return err
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "source.csv")
			dst := filepath.Join(dir, "output.dat")
			if err := os.WriteFile(src, []byte(oversizedMultilineCSV()), 0o600); err != nil {
				t.Fatal(err)
			}

			err := test.run(src, dst)
			if !errors.Is(err, ErrCSVRecordTooLarge) {
				t.Fatalf("error = %v, want ErrCSVRecordTooLarge", err)
			}
			if _, statErr := os.Lstat(dst); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("final output exists after failed transform: %v", statErr)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 1 || entries[0].Name() != "source.csv" {
				t.Fatalf("temporary output was not cleaned up: %v", entryNames(entries))
			}
		})
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}
