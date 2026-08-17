//go:build windows

package document

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

var indexCacheReplaceRetryDelays = [...]time.Duration{
	0,
	5 * time.Millisecond,
	10 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
	80 * time.Millisecond,
	160 * time.Millisecond,
	320 * time.Millisecond,
}

func replaceIndexCacheFile(tempPath, target string) error {
	var err error
	for _, delay := range indexCacheReplaceRetryDelays {
		if delay > 0 {
			time.Sleep(delay)
		}
		err = os.Rename(tempPath, target)
		if err == nil || !isTransientIndexCacheReplaceError(err) {
			return err
		}
	}
	return err
}

func isTransientIndexCacheReplaceError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
