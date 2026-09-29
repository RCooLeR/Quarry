package document

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/fileio"
)

func TestOpenFileReturnsStructuredAtomicRecoveryErrorBeforeNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "document.txt")
	journalPath := path + ".quarry.atomic.json"
	evidence := []byte(`{"version":1,"checksum":"corrupt"}`)
	if err := os.WriteFile(journalPath, evidence, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := OpenFileWithOptions(path, OpenOptions{})
	if !errors.Is(err, fileio.ErrAtomicReadRecoveryPending) || !errors.Is(err, fileio.ErrAtomicRecoveryNeedsInspection) {
		t.Fatalf("error = %v, want structured pending recovery error", err)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery evidence was hidden by not-exist fallback: %v", err)
	}
	var structured *fileio.AtomicReadRecoveryError
	if !errors.As(err, &structured) || !structured.State.JournalPresent {
		t.Fatalf("error does not expose recovery state: %#v", structured)
	}
	if got, readErr := os.ReadFile(journalPath); readErr != nil || string(got) != string(evidence) {
		t.Fatalf("arbitrary-source readiness check changed evidence: %q, %v", got, readErr)
	}
}
