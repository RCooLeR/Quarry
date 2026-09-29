//go:build windows

package winacl

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestOpenFileFailsBeforeCreationWhenVolumeCannotPersistACLs(t *testing.T) {
	original := getVolumeInformation
	getVolumeInformation = func(
		windows.Handle,
		*uint16,
		uint32,
		*uint32,
		*uint32,
		*uint32,
		*uint16,
		uint32,
	) error {
		return nil
	}
	t.Cleanup(func() { getVolumeInformation = original })

	path := filepath.Join(t.TempDir(), "must-not-exist.txt")
	_, err := OpenFile(
		path,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
	)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("OpenFile error = %v, want ErrUnavailable", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported ACL volume created %q: %v", path, err)
	}
}
