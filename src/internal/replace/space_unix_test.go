//go:build !windows

package replace

import (
	"errors"
	"math"
	"testing"
)

func TestAvailableDiskBytesReturnsFiniteValue(t *testing.T) {
	if _, err := availableDiskBytes(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got, err := checkedStatfsDiskBytes(7, 4096); err != nil || got != 7*4096 {
		t.Fatalf("checked statfs space = %d, %v", got, err)
	}
	if _, err := checkedStatfsDiskBytes(math.MaxUint64, 1); !errors.Is(err, errInvalidDiskSpace) {
		t.Fatalf("converted negative block count error = %v, want errInvalidDiskSpace", err)
	}
	if _, err := checkedStatfsDiskBytes(1, math.MaxUint64); !errors.Is(err, errInvalidDiskSpace) {
		t.Fatalf("converted negative block size error = %v, want errInvalidDiskSpace", err)
	}
	if got, err := checkedDiskBytes(7, 4096); err != nil || got != 7*4096 {
		t.Fatalf("checked finite space = %d, %v", got, err)
	}
	if _, err := checkedDiskBytes(math.MaxUint64, 2); !errors.Is(err, errInvalidDiskSpace) {
		t.Fatalf("overflow error = %v, want errInvalidDiskSpace", err)
	}
}
