package csv

import (
	"context"

	"github.com/quarry/quarry-wails3/internal/sourceio"
)

// SourceExpectation binds a pathname-based CSV transform to the exact source
// generation retained by the FileService session lease.
type SourceExpectation = sourceio.Expectation
type sourceHandle = sourceio.Handle

func openCSVSource(ctx context.Context, path string, expected *SourceExpectation) (*sourceHandle, error) {
	return sourceio.OpenContext(ctx, path, expected)
}
