package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/sourceio"
)

type csvArtifactTransformCase struct {
	name string
	run  func(*FileService, string, uint64) (TransformResult, error)
}

func csvArtifactTransformCases() []csvArtifactTransformCase {
	return []csvArtifactTransformCase{
		{"project", func(s *FileService, id string, generation uint64) (TransformResult, error) {
			return s.CsvProjectViaDialog(id, generation, ",", []int{0})
		}},
		{"add-column", func(s *FileService, id string, generation uint64) (TransformResult, error) {
			return s.CsvAddColumnViaDialog(id, generation, ",", 1, "constant")
		}},
		{"sql-default", func(s *FileService, id string, generation uint64) (TransformResult, error) {
			return s.CsvToSQLViaDialog(id, generation, ",", "records", true, false)
		}},
		{"redact", func(s *FileService, id string, generation uint64) (TransformResult, error) {
			return s.CsvRedactViaDialog(id, generation, ",", true, []CsvRedactColumn{{Index: 1, Mode: "fixed"}}, "MASKED")
		}},
		{"filter", func(s *FileService, id string, generation uint64) (TransformResult, error) {
			return s.CsvFilterViaDialog(id, generation, ",", true, 0, "eq", "1", false)
		}},
		{"dedupe", func(s *FileService, id string, generation uint64) (TransformResult, error) {
			return s.CsvDedupeViaDialog(id, generation, ",", true, -1)
		}},
		{"sample", func(s *FileService, id string, generation uint64) (TransformResult, error) {
			return s.CsvSampleViaDialog(id, generation, ",", true, 2)
		}},
		{"jsonl", func(s *FileService, id string, generation uint64) (TransformResult, error) {
			return s.CsvExportJSONLViaDialog(id, generation, ",", true, true)
		}},
		{"sql-config", func(s *FileService, id string, generation uint64) (TransformResult, error) {
			return s.CsvToSQLConfigViaDialog(id, generation, CsvSqlConfig{
				Delimiter: ",", HasHeader: true, TableName: "records", IncludeCreate: false,
				InsertMode: "insert", BatchSize: 500, OnInvalid: "fail",
				Columns: []CsvSqlColumnConfig{{Source: 0, Name: "id", Type: "TEXT", Include: true}},
			})
		}},
	}
}

func TestCSVArtifactTransformsRejectPathSubstitutionAfterDialog(t *testing.T) {
	for _, test := range csvArtifactTransformCases() {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "source.csv")
			held := filepath.Join(dir, "retained-original.csv")
			dst := filepath.Join(dir, "output")
			original := []byte("id,value\n1,original\n")
			substitute := []byte("id,value\n9,substitute\n")
			sentinel := []byte("existing destination must remain unchanged")
			if err := os.WriteFile(src, original, 0o600); err != nil {
				t.Fatal(err)
			}
			originalInfo, err := os.Stat(src)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, sentinel, 0o600); err != nil {
				t.Fatal(err)
			}

			svc := NewFileService()
			meta, err := svc.OpenFile(src)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })
			inspection, err := svc.CsvInspect(meta.FileID)
			if err != nil {
				t.Fatal(err)
			}
			if inspection.Generation == 0 {
				t.Fatal("CSV inspection returned an empty source generation")
			}

			previousDialog := csvSaveDialog
			dialogCalls := 0
			pathReplaced := false
			csvSaveDialog = func(string, string) (string, error) {
				dialogCalls++
				if err := os.Rename(src, held); err != nil {
					changed := originalInfo.ModTime().Add(2 * time.Second)
					if err := os.Chtimes(src, changed, changed); err != nil {
						return "", err
					}
					return dst, nil
				}
				pathReplaced = true
				if err := os.WriteFile(src, substitute, 0o600); err != nil {
					return "", err
				}
				return dst, nil
			}
			t.Cleanup(func() { csvSaveDialog = previousDialog })

			_, err = test.run(svc, meta.FileID, inspection.Generation)
			if !errors.Is(err, sourceio.ErrSourceChanged) {
				t.Fatalf("error = %v, want source-generation rejection", err)
			}
			if dialogCalls != 1 {
				t.Fatalf("save dialog calls = %d, want 1", dialogCalls)
			}
			assertFileBytes(t, dst, sentinel)
			if pathReplaced {
				assertFileBytes(t, held, original)
				assertFileBytes(t, src, substitute)
			} else {
				assertFileBytes(t, src, original)
			}
		})
	}
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
