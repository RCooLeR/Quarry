package csv

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

type sourceReplacementTransform func(context.Context, string, string, *SourceExpectation, func()) error

func TestCSVArtifactTransformsRejectPathReplacementDuringStreaming(t *testing.T) {
	tests := []struct {
		name string
		run  sourceReplacementTransform
	}{
		{"add-column", func(ctx context.Context, src, dst string, expected *SourceExpectation, mutate func()) error {
			_, err := AddColumnFile(ctx, src, dst, AddColumnOptions{
				Delimiter: ',', Position: 1, Value: "constant", ExpectedSource: expected,
				Progress: func(AddColumnProgress) { mutate() },
			})
			return err
		}},
		{"project", func(ctx context.Context, src, dst string, expected *SourceExpectation, mutate func()) error {
			_, err := ProjectColumnsFile(ctx, src, dst, ProjectOptions{
				Delimiter: ',', Columns: []int{0}, ExpectedSource: expected,
				Progress: func(ProjectProgress) { mutate() },
			})
			return err
		}},
		{"redact", func(ctx context.Context, src, dst string, expected *SourceExpectation, mutate func()) error {
			_, err := RedactColumnsFile(ctx, src, dst, RedactOptions{
				Delimiter: ',', HasHeader: true, Columns: map[int]RedactMode{1: RedactFixed}, ExpectedSource: expected,
				Progress: func(int64, int64) { mutate() },
			})
			return err
		}},
		{"filter", func(ctx context.Context, src, dst string, expected *SourceExpectation, mutate func()) error {
			_, err := FilterRowsFile(ctx, src, dst, FilterOptions{
				Delimiter: ',', HasHeader: true, Column: 0, Op: FilterEqual, Value: "1", ExpectedSource: expected,
				Progress: func(int64) { mutate() },
			})
			return err
		}},
		{"dedupe", func(ctx context.Context, src, dst string, expected *SourceExpectation, mutate func()) error {
			_, err := DedupeRowsFile(ctx, src, dst, DedupeOptions{
				Delimiter: ',', HasHeader: true, KeyColumn: -1, ExpectedSource: expected,
				Progress: func(int64) { mutate() },
			})
			return err
		}},
		{"sample", func(ctx context.Context, src, dst string, expected *SourceExpectation, mutate func()) error {
			_, err := SampleRowsFile(ctx, src, dst, SampleOptions{
				Delimiter: ',', HasHeader: true, EveryN: 10, ExpectedSource: expected,
				Progress: func(int64) { mutate() },
			})
			return err
		}},
		{"jsonl", func(ctx context.Context, src, dst string, expected *SourceExpectation, mutate func()) error {
			_, err := ExportJSONLFile(ctx, src, dst, JSONLOptions{
				Delimiter: ',', HasHeader: true, NumberKeys: true, ExpectedSource: expected,
				Progress: func(int64) { mutate() },
			})
			return err
		}},
		{"sql", func(ctx context.Context, src, dst string, expected *SourceExpectation, mutate func()) error {
			_, err := ConvertToSQLFile(ctx, src, dst, SQLConvertOptions{
				Delimiter: ',', HasHeader: true, TableName: "records", IncludeCreateTable: false,
				InsertBatchSize: 500, ExpectedSource: expected,
				Progress: func(SQLConvertProgress) { mutate() },
			})
			return err
		}},
	}

	var fixture bytes.Buffer
	fixture.WriteString("id,value\n")
	for i := 0; i < 50_000; i++ {
		fixture.WriteString("1,alpha\n")
	}
	original := append([]byte(nil), fixture.Bytes()...)
	substitute := []byte("id,value\n9,path-replacement\n")

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "source.csv")
			held := filepath.Join(dir, "streamed-generation.csv")
			dst := filepath.Join(dir, "output")
			if err := os.WriteFile(src, original, 0o600); err != nil {
				t.Fatal(err)
			}
			originalInfo, err := os.Stat(src)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := document.OpenFile(src)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = doc.Close() })
			expected, err := sourceio.ExpectDocument(doc)
			if err != nil {
				t.Fatal(err)
			}

			var once sync.Once
			var mutationErr error
			mutated := false
			pathReplaced := false
			mutate := func() {
				once.Do(func() {
					mutated = true
					if err := os.Rename(src, held); err != nil {
						// Some Windows source handles intentionally deny rename/delete
						// sharing. Exercise the same pre-publication guard with a
						// metadata mutation while retaining replacement coverage on
						// platforms where pathname substitution is permitted.
						changed := originalInfo.ModTime().Add(2 * time.Second)
						mutationErr = os.Chtimes(src, changed, changed)
						return
					}
					pathReplaced = true
					mutationErr = os.WriteFile(src, substitute, 0o600)
				})
			}

			err = test.run(context.Background(), src, dst, expected, mutate)
			if mutationErr != nil {
				t.Fatalf("replace source during stream: %v", mutationErr)
			}
			if !mutated {
				t.Fatal("transform completed without reaching the streaming mutation point")
			}
			if !errors.Is(err, sourceio.ErrSourceChanged) {
				t.Fatalf("error = %v, want source-generation rejection", err)
			}
			if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed transform published final output: %v", statErr)
			}
			if pathReplaced {
				assertSourceIdentityFile(t, held, original)
				assertSourceIdentityFile(t, src, substitute)
			} else {
				assertSourceIdentityFile(t, src, original)
				if _, statErr := os.Stat(held); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("blocked replacement unexpectedly created retained path: %v", statErr)
				}
			}
		})
	}
}

func TestCSVTransformRejectsSameSizeRewriteWithRestoredMtime(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.csv")
	dst := filepath.Join(dir, "output.csv")
	original := []byte("id,value\n1,original\n")
	changed := []byte("id,value\n9,altered!\n")
	if len(original) != len(changed) {
		t.Fatal("fixture must preserve source size")
	}
	if err := os.WriteFile(src, original, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(src)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	expected, err := sourceio.ExpectDocumentContext(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(src, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	_, err = ProjectColumnsFile(context.Background(), src, dst, ProjectOptions{
		Delimiter: ',', Columns: []int{0}, ExpectedSource: expected,
	})
	if !errors.Is(err, sourceio.ErrSourceChanged) {
		t.Fatalf("error = %v, want source-generation rejection", err)
	}
	if _, statErr := os.Lstat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected transform published output: %v", statErr)
	}
	assertSourceIdentityFile(t, src, changed)
}

func assertSourceIdentityFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s changed: got %d bytes, want %d", path, len(got), len(want))
	}
}
