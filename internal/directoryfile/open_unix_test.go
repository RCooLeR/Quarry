//go:build !windows

package directoryfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "directory.fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	assertDirectoryOpenErrorWithin(t, func() (*os.File, error) { return Open(path) }, ErrNotDirectory)
}

func TestOpenRejectsFIFOExchangedAfterDirectoryPreflight(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "directory")
	held := filepath.Join(parent, "directory.held")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	assertDirectoryOpenErrorWithin(t, func() (*os.File, error) {
		return openDirectory(path, false, func() error {
			if err := os.Rename(path, held); err != nil {
				return err
			}
			return unix.Mkfifo(path, 0o600)
		})
	}, ErrPathChanged)
}

func TestOpenClearsNonblockingFlag(t *testing.T) {
	file, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.O_NONBLOCK != 0 {
		t.Fatalf("accepted directory flags = %#x, O_NONBLOCK remained set", flags)
	}
}

func assertDirectoryOpenErrorWithin(t *testing.T, open func() (*os.File, error), want error) {
	t.Helper()
	type result struct {
		file *os.File
		err  error
	}
	done := make(chan result, 1)
	go func() {
		file, err := open()
		done <- result{file: file, err: err}
	}()
	select {
	case result := <-done:
		if result.file != nil {
			_ = result.file.Close()
		}
		if !errors.Is(result.err, want) {
			t.Fatalf("open directory error = %v, want %v", result.err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("directory open did not return within two seconds")
	}
}
