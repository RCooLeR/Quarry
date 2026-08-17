//go:build !darwin && !linux && !windows

package fileio

func publishExistingNoClobber(string, string) error {
	return ErrAtomicNoClobberUnavailable
}
