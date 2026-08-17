package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/fileio"
)

func TestFileServiceBridgeExposesOnlyDialogSaveCopy(t *testing.T) {
	serviceType := reflect.TypeOf((*FileService)(nil))
	if _, ok := serviceType.MethodByName("SaveCopy"); ok {
		t.Fatal("FileService exports path-taking SaveCopy to the Wails bridge")
	}
	if _, ok := serviceType.MethodByName("SaveCopyViaDialog"); !ok {
		t.Fatal("FileService does not export SaveCopyViaDialog")
	}
}

func TestFileServiceBridgeDoesNotExposeGenericReplace(t *testing.T) {
	serviceType := reflect.TypeOf((*FileService)(nil))
	for i := 0; i < serviceType.NumMethod(); i++ {
		method := serviceType.Method(i)
		if method.Name == "SqlReplaceViaDialog" {
			// Retained only as an intentionally disabled compatibility method.
			continue
		}
		if strings.Contains(strings.ToLower(method.Name), "replace") {
			t.Errorf("FileService exports generic replace bridge method %q", method.Name)
		}
	}
}

func TestSaveCopyViaDialogDelegatesToSafeNoClobberCopy(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	destinationPath := filepath.Join(dir, "selected-copy.txt")
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
	saveCopySaveDialog = func(message, defaultName string) (string, error) {
		if message != "Save edited copy as" || defaultName != "" {
			t.Fatalf("save dialog arguments = %q, %q", message, defaultName)
		}
		return destinationPath, nil
	}
	t.Cleanup(func() { saveCopySaveDialog = previousDialog })

	result, err := svc.SaveCopyViaDialog(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != "copy" || result.OutputPath != destinationPath {
		t.Fatalf("SaveCopyViaDialog result = %+v", result)
	}
	assertServiceCopyContent(t, sourcePath, source)
	assertServiceCopyContent(t, destinationPath, "omega\nbeta\n")

	if err := os.WriteFile(destinationPath, []byte("destination sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveCopyViaDialog(meta.FileID); !errors.Is(err, fileio.ErrExists) {
		t.Fatalf("SaveCopyViaDialog existing-destination error = %v, want %v", err, fileio.ErrExists)
	}
	assertServiceCopyContent(t, sourcePath, source)
	assertServiceCopyContent(t, destinationPath, "destination sentinel")
	staging, err := svc.GetStagingState(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if staging.EditCount != 1 {
		t.Fatalf("rejected SaveCopyViaDialog staging = %+v, want one retained edit", staging)
	}

	jobEvents := 0
	svc.jobs().emit = func(string, any) { jobEvents++ }
	saveCopySaveDialog = func(message, defaultName string) (string, error) { return "", nil }
	cancelled, err := svc.SaveCopyViaDialog(meta.FileID)
	if err != nil || cancelled != (SaveResult{}) {
		t.Fatalf("cancelled SaveCopyViaDialog = %+v, %v; want empty success", cancelled, err)
	}
	dialogErr := errors.New("native save dialog unavailable")
	saveCopySaveDialog = func(message, defaultName string) (string, error) { return "", dialogErr }
	failed, err := svc.SaveCopyViaDialog(meta.FileID)
	if failed != (SaveResult{}) || !errors.Is(err, dialogErr) {
		t.Fatalf("failed SaveCopyViaDialog = %+v, %v; want empty/dialog error", failed, err)
	}
	if jobEvents != 0 {
		t.Fatalf("cancelled/failed dialogs emitted %d job events", jobEvents)
	}
	staging, err = svc.GetStagingState(meta.FileID)
	if err != nil || staging.EditCount != 1 {
		t.Fatalf("dialog refusal staging = %+v, %v; want one retained edit", staging, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("cancelled/failed dialogs created artifacts: %v", entries)
	}
}
