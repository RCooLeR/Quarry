//go:build !windows

package document

import "os"

func replaceIndexCacheFile(tempPath, target string) error {
	return os.Rename(tempPath, target)
}
