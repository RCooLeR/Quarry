//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package document

import "fmt"

func acquireIndexCacheDirectoryLock(string) (func() error, error) {
	return nil, fmt.Errorf("%w: platform does not provide a supported cache lock", ErrIndexCacheBusy)
}
