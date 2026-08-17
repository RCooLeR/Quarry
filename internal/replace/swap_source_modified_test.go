package replace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSwapGuardDoesNotDisableCopyOnlyTransform(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	original := []byte("hello world hello")
	if err := os.WriteFile(sourcePath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := replacePlainFile(context.Background(), sourcePath, outputPath, []byte("hello"), []byte("bye"), FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Swapped || summary.Matches != 2 {
		t.Fatalf("copy-only summary = %+v", summary)
	}
	if got, readErr := os.ReadFile(sourcePath); readErr != nil || string(got) != string(original) {
		t.Fatalf("source = %q, %v; want unchanged", got, readErr)
	}
	if got, readErr := os.ReadFile(outputPath); readErr != nil || string(got) != "bye world bye" {
		t.Fatalf("output = %q, %v; want transformed copy", got, readErr)
	}
}
