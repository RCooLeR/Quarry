//go:build !windows

package exportx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
)

func TestSplitRejectsSymlinkDotDotBaseBeforeConstructingParts(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "physical")
	child := filepath.Join(physical, "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	visible := filepath.Join(root, "visible")
	if err := os.Symlink(child, visible); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	sourcePath := filepath.Join(root, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	rawBase := visible + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "split.dat"

	_, err = SplitBySize(context.Background(), doc, sourcePath, rawBase, 2, SplitOptions{})
	if !errors.Is(err, fileio.ErrInvalidExactPath) {
		t.Fatalf("split error = %v, want ErrInvalidExactPath", err)
	}
	for _, dir := range []string{root, physical} {
		matches, globErr := filepath.Glob(filepath.Join(dir, "split*"))
		if globErr != nil {
			t.Fatal(globErr)
		}
		if len(matches) != 0 {
			t.Fatalf("rejected split created artifacts in %q: %v", dir, matches)
		}
	}
}

func TestSplitPreservesSpacedBaseAtPublicBoundary(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	basePath := filepath.Join(dir, " split archive.dat ")

	summary, err := SplitBySize(context.Background(), doc, sourcePath, basePath, 2, SplitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Outputs) != 2 || summary.Outputs[0] != partPath(basePath, 1) || summary.Outputs[1] != partPath(basePath, 2) {
		t.Fatalf("spaced outputs = %#v", summary.Outputs)
	}
	for _, path := range append(append([]string(nil), summary.Outputs...), summary.ManifestPath) {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected exact artifact %q: %v", path, err)
		}
	}
	trimmedBase := filepath.Join(dir, "split archive.dat")
	if _, err := os.Stat(partPath(trimmedBase, 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trimmed split path unexpectedly exists: %v", err)
	}
}
