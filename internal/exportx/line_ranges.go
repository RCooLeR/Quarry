package exportx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/newlines"
)

const exactLineRangeScanBytes = 1024 * 1024

// exactLineRangeOffsets resolves line boundaries from the same verified reader
// used to produce the artifact. It deliberately does not consult a document's
// sparse index or memoized offsets, because those caches may have been built by
// an earlier read generation.
func exactLineRangeOffsets(ctx context.Context, reader document.ReaderAtSize, encoding string, startLine, endLine int64) (int64, int64, error) {
	if reader == nil {
		return 0, 0, errors.New("document is required")
	}
	if startLine <= 0 || endLine <= 0 {
		return 0, 0, errors.New("line numbers must be positive")
	}
	if endLine < startLine {
		return 0, 0, errors.New("end line must be greater than or equal to start line")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	size := reader.Size()
	if size <= 0 {
		return 0, 0, errors.New("start line is beyond end of file")
	}

	currentLine := int64(1)
	startOffset := int64(0)
	foundStart := startLine == 1
	endOffset := size
	foundEnd := false
	wantEndBoundary := endLine < math.MaxInt64
	endBoundaryLine := endLine + 1
	scanner := newlines.New(encoding)
	emit := func(br newlines.Break) bool {
		currentLine++
		if !foundStart && currentLine == startLine {
			startOffset = br.End
			foundStart = true
		}
		if wantEndBoundary && currentLine == endBoundaryLine {
			endOffset = br.End
			foundEnd = true
			return false
		}
		return true
	}

	buffer := make([]byte, exactLineRangeScanBytes)
	for offset := int64(0); offset < size && !foundEnd; {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		want := int64(len(buffer))
		if remaining := size - offset; want > remaining {
			want = remaining
		}
		n, readErr := reader.ReadAt(buffer[:int(want)], offset)
		if n != int(want) {
			if readErr == nil {
				readErr = io.ErrUnexpectedEOF
			}
			return 0, 0, readErr
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return 0, 0, readErr
		}
		if !scanner.Scan(buffer[:n], offset, emit) {
			foundEnd = true
			break
		}
		offset += int64(n)
	}
	if !foundEnd {
		scanner.Finish(emit)
	}
	if !foundStart || startOffset >= size {
		return 0, 0, errors.New("start line is beyond end of file")
	}
	if endOffset < startOffset {
		return 0, 0, errors.New("resolved line range is invalid")
	}
	return startOffset, endOffset, ctx.Err()
}

// exactLineSplitRanges performs one bounded sequential scan and retains only
// the at-most-maxParts byte boundaries needed by a multi-output split.
func exactLineSplitRanges(ctx context.Context, reader document.ReaderAtSize, encoding string, linesPerPart int64, maxParts int64) ([][2]int64, error) {
	if reader == nil {
		return nil, errors.New("document is required")
	}
	if linesPerPart <= 0 {
		return nil, errors.New("lines per part must be positive")
	}
	if maxParts <= 0 {
		return nil, errors.New("maximum split part count must be positive")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	size := reader.Size()
	if size <= 0 {
		return nil, nil
	}

	ranges := make([][2]int64, 0, 16)
	partStart := int64(0)
	lines := int64(0)
	tooMany := false
	scanner := newlines.New(encoding)
	emit := func(br newlines.Break) bool {
		lines++
		if lines != linesPerPart {
			return true
		}
		if int64(len(ranges)) >= maxParts {
			tooMany = true
			return false
		}
		ranges = append(ranges, [2]int64{partStart, br.End})
		partStart = br.End
		lines = 0
		return true
	}

	buffer := make([]byte, exactLineRangeScanBytes)
	for offset := int64(0); offset < size && !tooMany; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		want := int64(len(buffer))
		if remaining := size - offset; want > remaining {
			want = remaining
		}
		n, readErr := reader.ReadAt(buffer[:int(want)], offset)
		if n != int(want) {
			if readErr == nil {
				readErr = io.ErrUnexpectedEOF
			}
			return nil, readErr
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		if !scanner.Scan(buffer[:n], offset, emit) {
			break
		}
		offset += int64(n)
	}
	if !tooMany {
		scanner.Finish(emit)
	}
	if tooMany {
		return nil, fmt.Errorf("split would create more than %d parts", maxParts)
	}
	if partStart < size {
		if int64(len(ranges)) >= maxParts {
			return nil, fmt.Errorf("split would create more than %d parts", maxParts)
		}
		ranges = append(ranges, [2]int64{partStart, size})
	}
	return ranges, ctx.Err()
}
