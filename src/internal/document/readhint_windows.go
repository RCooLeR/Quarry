//go:build windows

package document

import "os"

// openRegularSource creates the retained Windows handle with
// FILE_FLAG_SEQUENTIAL_SCAN. Reopening by pathname here would create another
// pathname race and would require repeating the disk/regular-file validation.
func applyReadHint(f *os.File, _ string) *os.File { return f }
