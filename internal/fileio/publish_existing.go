package fileio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrAtomicNoClobberUnavailable reports that the host has no supported atomic
// primitive for publishing an existing named file without replacing a
// destination. Callers must preserve the temporary file for inspection rather
// than fall back to os.Rename, whose replacement behavior is platform-specific.
var ErrAtomicNoClobberUnavailable = errors.New("atomic no-clobber publication is unavailable")

// beforePublishExistingNoClobber is a deterministic test seam for a competing
// destination creator between validation and the atomic publication point.
var beforePublishExistingNoClobber = func() {}

// PublishExistingNoClobber atomically moves a complete, regular temporary file
// to an absent final name without ever replacing an object at finalPath.
//
// This helper exists for legacy streaming transforms which already own a named
// temporary file. New code should use AtomicOutput instead: unlike AtomicOutput,
// this path-based compatibility helper cannot defend against hostile pathname
// substitution of tempPath by another process. The caller must therefore keep
// the temporary name private and operation-owned until this call returns.
//
// Both paths must use exact spelling and name files in the same exact parent.
// A successful platform move is followed by a parent-directory sync. A sync
// failure is returned as PublicationError because the final output is already
// visible even though its crash durability is unconfirmed.
func PublishExistingNoClobber(tempPath string, finalPath string) error {
	if tempPath == "" {
		return errors.New("temporary output path is required")
	}
	if finalPath == "" {
		return errors.New("final output path is required")
	}
	if err := ValidateExactOutputPath(tempPath); err != nil {
		return err
	}
	if err := ValidateExactOutputPath(finalPath); err != nil {
		return err
	}

	tempDir, _ := filepath.Split(tempPath)
	finalDir, _ := filepath.Split(finalPath)
	if tempDir != finalDir {
		return errors.New("temporary and final output must have the same exact parent directory")
	}
	if tempPath == finalPath {
		return errors.New("temporary and final output paths must be different")
	}

	info, err := os.Lstat(tempPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("temporary output %q must be a regular file", tempPath)
	}
	if _, err := os.Lstat(finalPath); err == nil {
		return existingOutputError(finalPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	beforePublishExistingNoClobber()
	if err := publishExistingNoClobber(tempPath, finalPath); err != nil {
		return err
	}
	if err := syncDirPath(finalPath); err != nil {
		return &PublicationError{FinalPath: finalPath, Durable: false, Err: err}
	}
	return nil
}

func existingOutputError(path string) error {
	// Preserve the package sentinel while also matching the standard filesystem
	// error expected by callers which do not import fileio.
	return fmt.Errorf("%w: %w: %s", ErrExists, os.ErrExist, path)
}
