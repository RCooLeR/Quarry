//go:build !windows

package fileio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicOutputRejectsSymlinkDotDotSpelling(t *testing.T) {
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
	rawPath := visible + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "result.txt"
	cleanPath := filepath.Clean(rawPath)
	resolvedPath := filepath.Join(physical, "result.txt")

	_, err := OpenAtomicOutput(rawPath, nil, 0o600)
	if !errors.Is(err, ErrInvalidExactPath) {
		t.Fatalf("error = %v, want ErrInvalidExactPath", err)
	}
	for _, path := range []string{cleanPath, resolvedPath} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("rejected spelling created %q: %v", path, statErr)
		}
	}
}

func TestAtomicOutputPreservesTrailingFilenameSpace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.txt ")
	out, err := OpenAtomicOutput(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Cleanup()
	if _, err := out.Write([]byte("trailing space")); err != nil {
		t.Fatal(err)
	}
	if err := out.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "trailing space" {
		t.Fatalf("spaced output = %q, err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "result.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trimmed path unexpectedly exists: %v", err)
	}
}

func TestSamePathUsesOriginalSymlinkDotDotResolution(t *testing.T) {
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
	rawPath := visible + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "target.txt"
	resolvedPath := filepath.Join(physical, "target.txt")
	cleanPath := filepath.Clean(rawPath)
	if err := os.WriteFile(resolvedPath, []byte("resolved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cleanPath, []byte("lexically cleaned"), 0o600); err != nil {
		t.Fatal(err)
	}

	same, err := SamePath(rawPath, resolvedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Fatal("SamePath missed the object reached by the original symlink/.. spelling")
	}
	same, err = SamePath(rawPath, cleanPath)
	if err != nil {
		t.Fatal(err)
	}
	if same {
		t.Fatal("SamePath substituted the lexically cleaned path for the original spelling")
	}
}
