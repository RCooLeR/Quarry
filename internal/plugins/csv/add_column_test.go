package csv

import (
	"context"
	stdcsv "encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAddColumnRecordIsTotalAndPreservesFields(t *testing.T) {
	tests := []struct {
		name     string
		record   []string
		position int
		want     []string
	}{
		{name: "zero columns", record: nil, position: 0, want: []string{"NEW"}},
		{name: "before one", record: []string{"a"}, position: 0, want: []string{"NEW", "a"}},
		{name: "after one", record: []string{"a"}, position: 1, want: []string{"a", "NEW"}},
		{name: "middle many", record: []string{"a", "b", "c", "d"}, position: 2, want: []string{"a", "b", "NEW", "c", "d"}},
		{name: "short row fill", record: []string{"a"}, position: 3, want: []string{"a", "FILL", "FILL", "NEW"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := append([]string(nil), test.record...)
			got := addColumnRecord(test.record, test.position, "NEW", "FILL")
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("record = %#v, want %#v", got, test.want)
			}
			if !reflect.DeepEqual(test.record, original) {
				t.Fatalf("input record mutated: got %#v want %#v", test.record, original)
			}
		})
	}
}

func TestAddColumnEmptyInputIsTotal(t *testing.T) {
	var output strings.Builder
	summary, err := AddColumn(context.Background(), strings.NewReader(""), &output, AddColumnOptions{
		Delimiter: ',', Position: 0, Value: "new",
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.RecordsRead != 0 || summary.RecordsWritten != 0 || output.Len() != 0 {
		t.Fatalf("summary/output = %+v/%q", summary, output.String())
	}
}

func TestAddColumnPreservesMissingDuplicateHeaderAndRaggedWideRows(t *testing.T) {
	input := ",id,id\n1\n2,a,b,c\n"
	var output strings.Builder
	summary, err := AddColumn(context.Background(), strings.NewReader(input), &output, AddColumnOptions{
		Delimiter: ',', Position: 3, Value: "new\nvalue", FillValue: "FILL",
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.RecordsRead != 3 || summary.RecordsWritten != 3 || summary.Position != 3 {
		t.Fatalf("summary = %+v", summary)
	}
	reader := stdcsv.NewReader(strings.NewReader(output.String()))
	reader.FieldsPerRecord = -1
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"", "id", "id", "new\nvalue"},
		{"1", "FILL", "FILL", "new\nvalue"},
		{"2", "a", "b", "new\nvalue", "c"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %#v, want %#v", rows, want)
	}
}

func TestAddColumnWithoutHeaderInsertsAtRequestedPosition(t *testing.T) {
	var output strings.Builder
	_, err := AddColumn(context.Background(), strings.NewReader("a,b\nc,d\n"), &output, AddColumnOptions{
		Delimiter: ',', Position: 0, Value: "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "first,a,b\nfirst,c,d\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestAddColumnEnforcesSharedOutputFieldLimit(t *testing.T) {
	t.Run("exact output width", func(t *testing.T) {
		input := strings.TrimSuffix(strings.Repeat("x,", MaxCSVFieldsPerRecord-1), ",") + "\n"
		var output strings.Builder
		_, err := AddColumn(context.Background(), strings.NewReader(input), &output, AddColumnOptions{
			Delimiter: ',', Position: 0, Value: "new",
		})
		if err != nil {
			t.Fatal(err)
		}
		reader := stdcsv.NewReader(strings.NewReader(output.String()))
		record, err := reader.Read()
		if err != nil || len(record) != MaxCSVFieldsPerRecord {
			t.Fatalf("output field count = %d, error = %v", len(record), err)
		}
	})

	t.Run("insertion would exceed width", func(t *testing.T) {
		input := strings.TrimSuffix(strings.Repeat("x,", MaxCSVFieldsPerRecord), ",") + "\n"
		var output strings.Builder
		summary, err := AddColumn(context.Background(), strings.NewReader(input), &output, AddColumnOptions{
			Delimiter: ',', Position: 0, Value: "new",
		})
		if err == nil || !strings.Contains(err.Error(), "would contain") {
			t.Fatalf("error = %v, want output field-limit refusal", err)
		}
		if summary.RecordsRead != 1 || summary.RecordsWritten != 0 || output.Len() != 0 {
			t.Fatalf("summary/output = %+v/%q", summary, output.String())
		}
	})

	t.Run("maximum padding position", func(t *testing.T) {
		var output strings.Builder
		_, err := AddColumn(context.Background(), strings.NewReader("x\n"), &output, AddColumnOptions{
			Delimiter: ',', Position: MaxAddColumnPosition, Value: "new",
		})
		if err != nil {
			t.Fatal(err)
		}
		reader := stdcsv.NewReader(strings.NewReader(output.String()))
		record, err := reader.Read()
		if err != nil || len(record) != MaxCSVFieldsPerRecord {
			t.Fatalf("output field count = %d, error = %v", len(record), err)
		}
	})
}

func TestAddColumnFileRejectsInvalidPositionBeforeArtifacts(t *testing.T) {
	for _, test := range []struct {
		name     string
		position int
	}{
		{name: "negative", position: -1},
		{name: "above maximum", position: MaxAddColumnPosition + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "output.csv")
			_, err := AddColumnFile(context.Background(), filepath.Join(dir, "missing.csv"), output, AddColumnOptions{
				Delimiter: ',', Position: test.position, Value: "x",
			})
			if err == nil || !strings.Contains(err.Error(), "position") {
				t.Fatalf("error = %v, want position validation", err)
			}
			if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid position created output: %v", statErr)
			}
		})
	}
}

func TestAddColumnFileMalformedOrCanceledInputHasNoFinal(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		dir := t.TempDir()
		input := filepath.Join(dir, "input.csv")
		output := filepath.Join(dir, "output.csv")
		if err := os.WriteFile(input, []byte("a,b\n1,\"unterminated"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := AddColumnFile(context.Background(), input, output, AddColumnOptions{Delimiter: ',', Position: 1, Value: "x"})
		if err == nil {
			t.Fatal("expected malformed CSV error")
		}
		if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("malformed input created final output: %v", statErr)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		dir := t.TempDir()
		input := filepath.Join(dir, "input.csv")
		output := filepath.Join(dir, "output.csv")
		if err := os.WriteFile(input, []byte("a,b\n1,2\n3,4\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		_, err := AddColumnFile(ctx, input, output, AddColumnOptions{
			Delimiter: ',', Position: 2, Value: "x",
			Progress: func(AddColumnProgress) { cancel() },
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("canceled input created final output: %v", statErr)
		}
	})
}
