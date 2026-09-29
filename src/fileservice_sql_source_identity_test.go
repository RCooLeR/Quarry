package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/sourceio"
)

func TestSQLArtifactTransformsRejectPathSubstitutionAfterDialog(t *testing.T) {
	tests := []struct {
		name string
		run  func(*FileService, string) (TransformResult, error)
	}{
		{"reshape", func(s *FileService, id string) (TransformResult, error) {
			return s.SqlReshapeInsertsViaDialog(id, "single", 100)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "source.sql")
			held := filepath.Join(dir, "retained-original.sql")
			dst := filepath.Join(dir, "output.sql")
			original := []byte("INSERT INTO t VALUES (1),(2);\n")
			substitute := []byte("INSERT INTO t VALUES (99);\n")
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

			previousDialog := sqlSaveDialog
			pathReplaced := false
			sqlSaveDialog = func(string, string) (string, error) {
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
			t.Cleanup(func() { sqlSaveDialog = previousDialog })

			_, err = test.run(svc, meta.FileID)
			if !errors.Is(err, sourceio.ErrSourceChanged) {
				t.Fatalf("error = %v, want source-generation rejection", err)
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
