//go:build !windows

package document

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenFileRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	err := openFileErrorWithin(t, path, 2*time.Second)
	if !errors.Is(err, ErrNonRegularSource) {
		t.Fatalf("OpenFile(FIFO) error = %v, want ErrNonRegularSource", err)
	}
}

func TestOpenFileRejectsDevicePromptly(t *testing.T) {
	err := openFileErrorWithin(t, "/dev/null", 2*time.Second)
	if !errors.Is(err, ErrNonRegularSource) {
		t.Fatalf("OpenFile(/dev/null) error = %v, want ErrNonRegularSource", err)
	}
}

func TestOpenFileRejectsDirectoryPromptly(t *testing.T) {
	err := openFileErrorWithin(t, t.TempDir(), 2*time.Second)
	if !errors.Is(err, ErrNonRegularSource) {
		t.Fatalf("OpenFile(directory) error = %v, want ErrNonRegularSource", err)
	}
}

func TestOpenFileClearsNonblockingFlagForAcceptedRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "regular.txt")
	if err := os.WriteFile(path, []byte("regular source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	flags, err := unix.FcntlInt(doc.file.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.O_NONBLOCK != 0 {
		t.Fatalf("retained regular source flags = %#x, O_NONBLOCK remained set", flags)
	}
}

func TestInspectExternalModificationRejectsFIFOPathReplacementWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.txt")
	held := filepath.Join(dir, "source.held")
	if err := os.WriteFile(path, []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if err := os.Rename(path, held); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := doc.InspectExternalModification()
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNonRegularSource) {
			t.Fatalf("InspectExternalModification error = %v, want ErrNonRegularSource", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("InspectExternalModification blocked on FIFO pathname replacement")
	}
}

func TestIndexCacheDirectorySyncRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- syncIndexCacheDirectory(path) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("syncIndexCacheDirectory accepted a FIFO")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("syncIndexCacheDirectory blocked waiting for a FIFO writer")
	}
}

func openFileErrorWithin(t *testing.T, path string, timeout time.Duration) error {
	t.Helper()
	type result struct {
		doc *FileDocument
		err error
	}
	done := make(chan result, 1)
	go func() {
		doc, err := OpenFile(path)
		done <- result{doc: doc, err: err}
	}()
	select {
	case result := <-done:
		if result.doc != nil {
			_ = result.doc.Close()
		}
		return result.err
	case <-time.After(timeout):
		t.Fatalf("OpenFile(%q) did not return within %s", path, timeout)
		return nil
	}
}
