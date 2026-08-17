//go:build !windows

package replace

import (
	"errors"
	"math"

	"golang.org/x/sys/unix"
)

var errInvalidDiskSpace = errors.New("filesystem reported an invalid available-space value")

func availableDiskBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	// Statfs_t uses unsigned fields on Linux and signed fields on several BSDs.
	// Conversion maps a negative signed value above MaxInt64; values above that
	// bound are not useful byte counts, so this remains fail-closed on both
	// field representations without an impossible unsigned comparison.
	blocks, blockSize := uint64(stat.Bavail), uint64(stat.Bsize)
	return checkedStatfsDiskBytes(blocks, blockSize)
}

func checkedStatfsDiskBytes(blocks, blockSize uint64) (uint64, error) {
	if blocks > math.MaxInt64 || blockSize > math.MaxInt64 {
		return 0, errInvalidDiskSpace
	}
	return checkedDiskBytes(blocks, blockSize)
}

func checkedDiskBytes(blocks, blockSize uint64) (uint64, error) {
	if blockSize != 0 && blocks > math.MaxUint64/blockSize {
		return 0, errInvalidDiskSpace
	}
	return blocks * blockSize, nil
}
