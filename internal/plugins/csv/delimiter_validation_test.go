package csv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestArtifactWritersRejectInvalidDelimiterBeforeFilesystemAccess(t *testing.T) {
	writers := map[string]func(context.Context, string, string, rune) error{
		"add-column": func(ctx context.Context, src, dst string, delimiter rune) error {
			_, err := AddColumnFile(ctx, src, dst, AddColumnOptions{Delimiter: delimiter, Position: 0, Value: "x"})
			return err
		},
		"dedupe": func(ctx context.Context, src, dst string, delimiter rune) error {
			_, err := DedupeRowsFile(ctx, src, dst, DedupeOptions{Delimiter: delimiter, KeyColumn: -1})
			return err
		},
		"filter": func(ctx context.Context, src, dst string, delimiter rune) error {
			_, err := FilterRowsFile(ctx, src, dst, FilterOptions{Delimiter: delimiter, Column: 0, Op: FilterEqual})
			return err
		},
		"jsonl": func(ctx context.Context, src, dst string, delimiter rune) error {
			_, err := ExportJSONLFile(ctx, src, dst, JSONLOptions{Delimiter: delimiter})
			return err
		},
		"project": func(ctx context.Context, src, dst string, delimiter rune) error {
			_, err := ProjectColumnsFile(ctx, src, dst, ProjectOptions{Delimiter: delimiter, Columns: []int{0}})
			return err
		},
		"redact": func(ctx context.Context, src, dst string, delimiter rune) error {
			_, err := RedactColumnsFile(ctx, src, dst, RedactOptions{Delimiter: delimiter, Columns: map[int]RedactMode{0: RedactNull}})
			return err
		},
		"sample": func(ctx context.Context, src, dst string, delimiter rune) error {
			_, err := SampleRowsFile(ctx, src, dst, SampleOptions{Delimiter: delimiter, EveryN: 2})
			return err
		},
		"sql": func(ctx context.Context, src, dst string, delimiter rune) error {
			_, err := ConvertToSQLFile(ctx, src, dst, SQLConvertOptions{Delimiter: delimiter, TableName: "records", Columns: []string{"id"}})
			return err
		},
	}
	invalid := []rune{0, '"', '\r', '\n', utf8.RuneError}
	for writerName, write := range writers {
		for _, delimiter := range invalid {
			writerName, write, delimiter := writerName, write, delimiter
			t.Run(writerName+"/"+delimiterTestName(delimiter), func(t *testing.T) {
				dir := t.TempDir()
				output := filepath.Join(dir, "output")
				err := write(context.Background(), filepath.Join(dir, "missing.csv"), output, delimiter)
				if err == nil || !strings.Contains(err.Error(), "delimiter") {
					t.Fatalf("error = %v, want delimiter validation", err)
				}
				if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("invalid delimiter created output: %v", statErr)
				}
			})
		}
	}
}

func TestArtifactWriterAcceptsUnicodeDelimiter(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.csv")
	output := filepath.Join(dir, "output.csv")
	if err := os.WriteFile(input, []byte("a§b\n1§2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ProjectColumnsFile(context.Background(), input, output, ProjectOptions{Delimiter: '§', Columns: []int{1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "b§a\n2§1\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func delimiterTestName(delimiter rune) string {
	switch delimiter {
	case 0:
		return "NUL"
	case '\r':
		return "CR"
	case '\n':
		return "LF"
	case '"':
		return "quote"
	default:
		return "replacement-rune"
	}
}
