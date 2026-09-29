// Package regularfile opens untrusted pathnames for bounded, read-only use.
//
// The platform implementations ensure that a pathname exchanged for a FIFO,
// device, directory, or other non-regular object between a preflight stat and
// the actual open cannot make the caller block or accept the substituted
// object. The returned descriptor and the pathname are also required to still
// identify the same regular file when Open returns.
package regularfile

import (
	"errors"
	"fmt"
	"os"
)

var (
	// ErrNotRegular identifies a path or opened handle that is not a regular
	// file. Callers may wrap this with a domain-specific error.
	ErrNotRegular = errors.New("path does not identify a regular file")

	// ErrPathChanged identifies a pathname exchange observed while opening.
	ErrPathChanged = errors.New("path changed while opening regular file")
)

// Open follows symbolic links, then returns a stable read-only descriptor for
// the regular target. It preserves the behavior of os.Open for callers that
// intentionally allow source-path symlinks.
func Open(path string) (*os.File, error) {
	return openRegular(path, false, nil)
}

// OpenNoFollow rejects symbolic links and other reparse objects in addition to
// non-regular files. It is intended for untrusted cache and recovery leaves
// whose pathname itself, rather than a linked target, is the admitted object.
func OpenNoFollow(path string) (*os.File, error) {
	return openRegular(path, true, nil)
}

func validateOpened(file *os.File, path string) (os.FileInfo, error) {
	if file == nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrInvalid}
	}
	info, err := file.Stat()
	if err != nil {
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrNotRegular}
	}
	return info, nil
}

func rejectPreflight(path string, info os.FileInfo) error {
	if info == nil || !info.Mode().IsRegular() {
		return &os.PathError{Op: "open", Path: path, Err: ErrNotRegular}
	}
	return nil
}

// pinPreflightIdentity forces platforms with lazily populated FileInfo
// identities (notably Windows) to resolve the file ID while the preflight
// pathname is still known to name that object. Without this, a later SameFile
// call can resolve both FileInfo values through the replacement pathname and
// miss an exchange that occurred between stat and open.
func pinPreflightIdentity(path string, info os.FileInfo) error {
	if info == nil || !os.SameFile(info, info) {
		// Windows can report named-pipe/device metadata with regular-looking
		// mode bits but cannot resolve a stable disk file ID. Preserve both the
		// identity failure and the non-regular classification for callers.
		return &os.PathError{Op: "stat", Path: path, Err: errors.Join(ErrPathChanged, ErrNotRegular)}
	}
	return nil
}

func rejectChanged(path string) error {
	return &os.PathError{Op: "open", Path: path, Err: ErrPathChanged}
}

func closeAfterError(file *os.File, err error) error {
	if file == nil {
		return err
	}
	if closeErr := file.Close(); closeErr != nil {
		return errors.Join(err, fmt.Errorf("close rejected regular-file handle: %w", closeErr))
	}
	return err
}
