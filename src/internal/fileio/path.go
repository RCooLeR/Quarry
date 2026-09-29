package fileio

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// SamePath reports whether two paths identify the same filesystem target under
// the current platform's path rules. It is case-insensitive on Windows, keeps
// case-sensitive spelling distinct on Unix-like systems, and uses os.SameFile
// when both paths already exist so hard links and symlinks are handled safely.
func SamePath(a string, b string) (bool, error) {
	if a == "" || b == "" {
		return false, nil
	}

	// Stat the caller-provided spellings before doing any lexical comparison.
	// On POSIX, cleaning "symlink/../name" can identify a different object from
	// the one the kernel reaches while walking the original spelling.
	infoA, errA := statPath(a)
	infoB, errB := statPath(b)
	if errA == nil && errB == nil {
		return os.SameFile(infoA, infoB), nil
	}
	if errA != nil && !errors.Is(errA, os.ErrNotExist) {
		return false, errA
	}
	if errB != nil && !errors.Is(errB, os.ErrNotExist) {
		return false, errB
	}
	// If exactly one original spelling exists, lexical cleaning is not evidence
	// that they identify the same target; it may be the discrepancy itself.
	if (errA == nil) != (errB == nil) {
		return false, nil
	}

	cleanA, err := cleanAbsPath(a)
	if err != nil {
		return false, err
	}
	cleanB, err := cleanAbsPath(b)
	if err != nil {
		return false, err
	}
	if pathsEqualByPlatform(cleanA, cleanB) {
		return true, nil
	}
	return false, nil
}

func cleanAbsPath(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func pathsEqualByPlatform(a string, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
