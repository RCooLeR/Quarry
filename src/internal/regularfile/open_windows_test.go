//go:build windows

package regularfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestOpenWindowsAcceptsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("regular\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenWindowsRejectsNamedPipeWithoutWaitingForPeerIO(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\quarry-regularfile-%d-%d`, os.Getpid(), time.Now().UnixNano())
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	server, err := windows.CreateNamedPipe(
		name,
		windows.PIPE_ACCESS_DUPLEX,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT,
		1,
		4096,
		4096,
		0,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(server)

	type result struct {
		file *os.File
		err  error
	}
	done := make(chan result, 1)
	go func() {
		file, err := Open(path)
		done <- result{file: file, err: err}
	}()
	select {
	case result := <-done:
		if result.file != nil {
			_ = result.file.Close()
		}
		if !errors.Is(result.err, ErrNotRegular) {
			t.Fatalf("Open(named pipe) error = %v, want ErrNotRegular", result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open blocked on a Windows named pipe")
	}
}

func TestOpenWindowsRejectsNonRegularHandles(t *testing.T) {
	for _, path := range []string{t.TempDir(), "NUL"} {
		file, err := Open(path)
		if file != nil {
			_ = file.Close()
		}
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("Open(%q) error = %v, want ErrNotRegular", path, err)
		}
	}
}

func TestOpenWindowsRejectsRegularFileExchangedAfterPreflight(t *testing.T) {
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
		return os.WriteFile(path, []byte("second"), 0o600)
	})
	if file != nil {
		_ = file.Close()
	}
	if !errors.Is(err, ErrPathChanged) {
		t.Fatalf("open exchanged regular file error = %v, want ErrPathChanged", err)
	}
}

func TestOpenNoFollowWindowsRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	file, err := OpenNoFollow(link)
	if file != nil {
		_ = file.Close()
	}
	if !errors.Is(err, ErrNotRegular) {
		t.Fatalf("OpenNoFollow(symlink) error = %v, want ErrNotRegular", err)
	}
}
