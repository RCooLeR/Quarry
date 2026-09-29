package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/manualedit"
)

func TestGetStagedEditsBatchedPreviewsPreserveEditOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	const source = "alpha\nbeta\ngamma\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewFileService()
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.CloseFile(meta.FileID) })
	mustPrepareEditSession(t, service, meta.FileID)
	if _, err := service.StageEdit(meta.FileID, 0, 5, "ALPHA"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StageEdit(meta.FileID, 6, 4, "BETA"); err != nil {
		t.Fatal(err)
	}

	edits, err := service.GetStagedEdits(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 2 {
		t.Fatalf("staged edit count = %d, want 2", len(edits))
	}
	if edits[0].Start != 0 || edits[0].Old != "alpha" || edits[0].New != "ALPHA" {
		t.Fatalf("first staged preview = %+v", edits[0])
	}
	if edits[1].Start != 6 || edits[1].Old != "beta" || edits[1].New != "BETA" {
		t.Fatalf("second staged preview = %+v", edits[1])
	}
}

func TestStagedEditReadsRejectSameSizeRestoredTimestampSourceDrift(t *testing.T) {
	tests := []struct {
		name string
		run  func(*FileService, string) error
	}{
		{
			name: "edited window including only inserted bytes",
			run: func(service *FileService, fileID string) error {
				_, err := service.GetEditWindow(fileID, 0, 64)
				return err
			},
		},
		{
			name: "edited tail including only inserted bytes",
			run: func(service *FileService, fileID string) error {
				_, err := service.GetEditTailWindow(fileID, 64)
				return err
			},
		},
		{
			name: "diff window",
			run: func(service *FileService, fileID string) error {
				_, err := service.GetDiffWindow(fileID, 0, 64)
				return err
			},
		},
		{
			name: "staged edit previews",
			run: func(service *FileService, fileID string) error {
				_, err := service.GetStagedEdits(fileID)
				return err
			},
		},
		{
			name: "subsequent stage",
			run: func(service *FileService, fileID string) error {
				_, err := service.StageEdit(fileID, 0, int64(len("OMEGA\nBETA\n")), "second state\n")
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "source.txt")
			const source = "alpha\nbeta\n"
			const edited = "OMEGA\nBETA\n"
			const rewritten = "ALPHA\nbeta\n"
			if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			service := NewFileService()
			meta, err := service.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.CloseFile(meta.FileID) })
			mustPrepareEditSession(t, service, meta.FileID)
			state, err := service.StageEdit(meta.FileID, 0, int64(len(source)), edited)
			if err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := test.run(service, meta.FileID); !errors.Is(err, manualedit.ErrSessionSourceChanged) {
				t.Fatalf("operation error = %v, want ErrSessionSourceChanged", err)
			}
			after, err := service.GetStagingState(meta.FileID)
			if err != nil {
				t.Fatal(err)
			}
			if after.EditCount != state.EditCount || after.EditedSize != state.EditedSize {
				t.Fatalf("rejected read mutated staging: before=%+v after=%+v", state, after)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != rewritten {
				t.Fatalf("source after rejected read = %q, %v", got, err)
			}
		})
	}
}
