//go:build !windows

package regularfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	assertOpenErrorWithin(t, path, func() (*os.File, error) { return Open(path) }, ErrNotRegular)
}

func TestOpenRejectsFIFOExchangedAfterRegularPreflight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.txt")
	oldPath := filepath.Join(dir, "source.old")
	if err := os.WriteFile(path, []byte("regular"), 0o600); err != nil {
		t.Fatal(err)
	}

	open := func() (*os.File, error) {
		return openRegular(path, false, func() error {
			if err := os.Rename(path, oldPath); err != nil {
				return err
			}
			if err := unix.Mkfifo(path, 0o600); err != nil {
				return err
			}
			return nil
		})
	}
	assertOpenErrorWithin(t, path, open, ErrNotRegular)
}

func TestOpenRejectsRegularFileExchangedAfterPreflight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.txt")
	oldPath := filepath.Join(dir, "source.old")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}

	file, err := openRegular(path, false, func() error {
		if err := os.Rename(path, oldPath); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
			return err
		}
		return nil
	})
	if file != nil {
		_ = file.Close()
	}
	if !errors.Is(err, ErrPathChanged) {
		t.Fatalf("open exchanged regular file error = %v, want ErrPathChanged", err)
	}
}

func TestOpenNoFollowRejectsSymlinkExchangedAfterPreflight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.bin")
	oldPath := filepath.Join(dir, "cache.old")
	if err := os.WriteFile(path, []byte("cache"), 0o600); err != nil {
		t.Fatal(err)
	}

	file, err := openRegular(path, true, func() error {
		if err := os.Rename(path, oldPath); err != nil {
			return err
		}
		if err := os.Symlink(oldPath, path); err != nil {
			return err
		}
		return nil
	})
	if file != nil {
		_ = file.Close()
	}
	if err == nil {
		t.Fatal("OpenNoFollow accepted a symlink substituted after preflight")
	}
}

func TestOpenClearsNonblockingFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("regular"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.O_NONBLOCK != 0 {
		t.Fatalf("accepted regular descriptor flags = %#x, O_NONBLOCK remained set", flags)
	}
}

func assertOpenErrorWithin(t *testing.T, path string, open func() (*os.File, error), want error) {
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
			t.Fatalf("open %q error = %v, want %v", path, result.err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("open %q did not return within two seconds", path)
	}
}
