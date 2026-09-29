package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/fileio"
	"github.com/quarry/quarry-wails3/internal/manualedit"
)

func TestSaveCopyPublishesExactSelectedPathAndPreservesSourceAndStaging(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, " edited copy.txt")
	const source = "alpha\nbeta\n"
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	mustPrepareEditSession(t, svc, meta.FileID)
	before, err := svc.StageEdit(meta.FileID, 0, 5, "omega")
	if err != nil {
		t.Fatal(err)
	}

	result, err := svc.saveCopy(meta.FileID, outputPath)
	if err != nil {
		t.Fatal(err)
	}
	const edited = "omega\nbeta\n"
	if result.Mode != "copy" || result.OutputPath != outputPath || result.BytesWritten != int64(len(edited)) {
		t.Fatalf("SaveCopy result = %+v, want exact selected path and %d bytes", result, len(edited))
	}
	assertServiceCopyContent(t, sourcePath, source)
	assertServiceCopyContent(t, outputPath, edited)
	if _, err := os.Lstat(outputPath + ".quarry.tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy deterministic temp exists: %v", err)
	}
	if _, err := os.Lstat(outputPath + ".quarry.manifest.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy-only manifest exists: %v", err)
	}
	after, err := svc.GetStagingState(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if after.EditCount != before.EditCount || after.EditedSize != before.EditedSize {
		t.Fatalf("SaveCopy discarded staged edits: before=%+v after=%+v", before, after)
	}
}

func TestSaveCopyRefusesExistingDestinationAndSourceAlias(t *testing.T) {
	tests := []struct {
		name      string
		output    func(string, string) string
		wantError error
	}{
		{
			name: "existing destination",
			output: func(_ string, dir string) string {
				return filepath.Join(dir, "existing.txt")
			},
			wantError: fileio.ErrExists,
		},
		{
			name:      "source alias",
			output:    func(sourcePath string, _ string) string { return sourcePath },
			wantError: fileio.ErrSourceAlias,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "source.txt")
			const source = "alpha\nbeta\n"
			if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			outputPath := tt.output(sourcePath, dir)
			if errors.Is(tt.wantError, fileio.ErrExists) {
				if err := os.WriteFile(outputPath, []byte("destination sentinel"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			svc := NewFileService()
			meta, err := svc.OpenFile(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			defer svc.CloseFile(meta.FileID)
			mustPrepareEditSession(t, svc, meta.FileID)
			before, err := svc.StageEdit(meta.FileID, 0, 5, "omega")
			if err != nil {
				t.Fatal(err)
			}

			result, err := svc.saveCopy(meta.FileID, outputPath)
			if !errors.Is(err, tt.wantError) {
				t.Fatalf("SaveCopy error = %v, want %v", err, tt.wantError)
			}
			if result != (SaveResult{}) {
				t.Fatalf("failed unpublished SaveCopy result = %+v, want empty", result)
			}
			assertServiceCopyContent(t, sourcePath, source)
			if errors.Is(tt.wantError, fileio.ErrExists) {
				assertServiceCopyContent(t, outputPath, "destination sentinel")
			}
			after, err := svc.GetStagingState(meta.FileID)
			if err != nil {
				t.Fatal(err)
			}
			if after.EditCount != before.EditCount || after.EditedSize != before.EditedSize {
				t.Fatalf("failed SaveCopy discarded staging: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestSaveCopyRejectsSameSizeRestoredTimestampDriftAndPreservesStaging(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const source = "alpha\nbravo\ncharlie\n"
	const rewritten = "ALPHA\nbravo\ncharlie\n"
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}

	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	mustPrepareEditSession(t, svc, meta.FileID)
	before, err := svc.StageEdit(meta.FileID, 6, 5, "delta")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourcePath, beforeInfo.ModTime(), beforeInfo.ModTime()); err != nil {
		t.Fatal(err)
	}

	result, err := svc.saveCopy(meta.FileID, outputPath)
	if !errors.Is(err, manualedit.ErrSessionSourceChanged) {
		t.Fatalf("SaveCopy error = %v, want ErrSessionSourceChanged", err)
	}
	if result != (SaveResult{}) {
		t.Fatalf("rejected SaveCopy result = %+v, want empty", result)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output exists after stale-session rejection: %v", err)
	}
	after, err := svc.GetStagingState(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if after.EditCount != before.EditCount || after.EditedSize != before.EditedSize {
		t.Fatalf("rejected SaveCopy discarded staging: before=%+v after=%+v", before, after)
	}
	assertServiceCopyContent(t, sourcePath, rewritten)
}

func TestConfirmedSaveCopyResultRejectsUncertainOrMismatchedPublication(t *testing.T) {
	selected := filepath.Join(t.TempDir(), "selected.txt")
	base := manualedit.FileSummary{
		OutputPath:   selected,
		BytesWritten: 7,
		Complete:     true,
		Published:    true,
	}

	if result, ok := confirmedSaveCopyResult(base, selected); !ok || result.OutputPath != selected || result.BytesWritten != 7 {
		t.Fatalf("confirmed result = %+v, ok=%v", result, ok)
	}

	uncertain := base
	uncertain.PublicationUncertain = true
	if result, ok := confirmedSaveCopyResult(uncertain, selected); ok || result != (SaveResult{}) {
		t.Fatalf("uncertain result = %+v, ok=%v, want empty/unconfirmed", result, ok)
	}

	other := base
	other.OutputPath = selected + ".other"
	if result, ok := confirmedSaveCopyResult(other, selected); ok || result != (SaveResult{}) {
		t.Fatalf("mismatched result = %+v, ok=%v, want empty/unconfirmed", result, ok)
	}
}

func TestSaveCopyViaDialogRejectsStagingRevisionChangedDuringDialog(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const source = "alpha\nbeta\n"
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })
	mustPrepareEditSession(t, svc, meta.FileID)
	if _, err := svc.StageEdit(meta.FileID, 0, 5, "omega"); err != nil {
		t.Fatal(err)
	}

	previousDialog := saveCopySaveDialog
	saveCopySaveDialog = func(string, string) (string, error) {
		if _, err := svc.StageEdit(meta.FileID, 6, 4, "BETA"); err != nil {
			return "", err
		}
		return outputPath, nil
	}
	t.Cleanup(func() { saveCopySaveDialog = previousDialog })

	result, err := svc.SaveCopyViaDialog(meta.FileID)
	if !errors.Is(err, ErrSaveCopyStateChanged) {
		t.Fatalf("SaveCopyViaDialog error = %v, want ErrSaveCopyStateChanged", err)
	}
	if result != (SaveResult{}) {
		t.Fatalf("rejected result = %+v, want empty", result)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output exists after changed approval: %v", err)
	}
	state, err := svc.GetStagingState(meta.FileID)
	if err != nil || state.EditCount != 2 {
		t.Fatalf("changed staging = %+v, %v; want two retained edits", state, err)
	}
	assertServiceCopyContent(t, sourcePath, source)
}

func TestSaveCopyViaDialogRejectsRecreatedSessionDuringDialog(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const source = "alpha\nbeta\n"
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })
	mustPrepareEditSession(t, svc, meta.FileID)
	if _, err := svc.StageEdit(meta.FileID, 0, 5, "omega"); err != nil {
		t.Fatal(err)
	}

	previousDialog := saveCopySaveDialog
	saveCopySaveDialog = func(string, string) (string, error) {
		if _, err := svc.DiscardEdits(meta.FileID); err != nil {
			return "", err
		}
		if _, err := svc.RefreshFile(meta.FileID); err != nil {
			return "", err
		}
		if _, err := svc.PrepareEditSession(meta.FileID); err != nil {
			return "", err
		}
		if _, err := svc.StageEdit(meta.FileID, 0, 5, "sigma"); err != nil {
			return "", err
		}
		return outputPath, nil
	}
	t.Cleanup(func() { saveCopySaveDialog = previousDialog })

	result, err := svc.SaveCopyViaDialog(meta.FileID)
	if !errors.Is(err, ErrSaveCopyStateChanged) {
		t.Fatalf("SaveCopyViaDialog error = %v, want ErrSaveCopyStateChanged", err)
	}
	if result != (SaveResult{}) {
		t.Fatalf("rejected result = %+v, want empty", result)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output exists after recreated session: %v", err)
	}
	assertServiceCopyContent(t, sourcePath, source)
}

func TestSaveCopyViaDialogWithoutStagedEditsDoesNotOpenDialog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })

	called := false
	previousDialog := saveCopySaveDialog
	saveCopySaveDialog = func(string, string) (string, error) {
		called = true
		return "", nil
	}
	t.Cleanup(func() { saveCopySaveDialog = previousDialog })

	if result, err := svc.SaveCopyViaDialog(meta.FileID); err == nil || result != (SaveResult{}) {
		t.Fatalf("SaveCopyViaDialog without edits = %+v, %v; want empty error", result, err)
	}
	if called {
		t.Fatal("save dialog opened without staged edits")
	}
}

func assertServiceCopyContent(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, string(got), want)
	}
}
