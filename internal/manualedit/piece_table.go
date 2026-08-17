package manualedit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/quarry/quarry-wails3/internal/document"
)

const DefaultMaxInsertedBytes int64 = 8 * 1024 * 1024

var (
	ErrInsertedTextTooLarge = errors.New("inserted text exceeds the per-edit safe limit")
	ErrLiveInsertedLimit    = errors.New("live inserted text exceeds the session memory limit")
	ErrPieceCountLimit      = errors.New("piece count exceeds the session memory limit")
	ErrEditSizeOverflow     = errors.New("edited document size exceeds the supported range")
)

type Range struct {
	Start int64
	End   int64
}

type Edit struct {
	Start int64
	End   int64
	Text  []byte
}

type Progress struct {
	BytesWritten int64
	BytesTotal   int64
}

type WriteOptions struct {
	Progress func(Progress)
}

type pieceSource uint8

const (
	pieceOriginal pieceSource = iota
	pieceAdded
)

type piece struct {
	source pieceSource
	start  int64
	length int64
}

// PieceTable is mutable and intentionally unsynchronized. It is safe as a
// single-owner staging structure; concurrent mutation/readers need an external
// lock or immutable snapshot handoff.
type PieceTable struct {
	originalSize    int64
	size            int64
	maxEditText     int64
	maxLiveInserted int64
	maxPieces       int
	added           []byte
	pieces          []piece
	modified        []Range
}

func NewPieceTable(size int64) *PieceTable {
	if size < 0 {
		size = 0
	}
	pt := &PieceTable{
		originalSize:    size,
		size:            size,
		maxEditText:     DefaultMaxInsertedBytes,
		maxLiveInserted: DefaultMaxLiveInsertedBytes,
		maxPieces:       DefaultMaxPieceCount,
	}
	if size > 0 {
		pt.pieces = []piece{{source: pieceOriginal, start: 0, length: size}}
	}
	return pt
}

func (pt *PieceTable) SetMaxInsertedBytes(limit int64) {
	if limit > 0 {
		pt.maxEditText = limit
		pt.maxLiveInserted = limit
	}
}

func (pt *PieceTable) setLimits(limits Limits) {
	pt.maxEditText = limits.MaxEditTextBytes
	pt.maxLiveInserted = limits.MaxLiveInsertedBytes
	pt.maxPieces = limits.MaxPieceCount
}

func (pt *PieceTable) Size() int64 {
	return pt.size
}

// OriginalSize is the source size captured when the table was built. Comparing
// it to the source's current size before a save detects a source that changed
// (shrank/grew/was replaced) since staging began.
func (pt *PieceTable) OriginalSize() int64 {
	return pt.originalSize
}

func (pt *PieceTable) ModifiedRanges() []Range {
	out := make([]Range, len(pt.modified))
	copy(out, pt.modified)
	return out
}

func (pt *PieceTable) Replace(start int64, end int64, text []byte) error {
	if pt == nil {
		return errors.New("piece table is required")
	}
	if start < 0 || end < 0 {
		return errors.New("edit offsets must be non-negative")
	}
	if end < start {
		return errors.New("end offset must be greater than or equal to start offset")
	}
	if start > pt.size || end > pt.size {
		return errors.New("edit range is beyond end of document")
	}
	if pt.maxEditText > 0 && int64(len(text)) > pt.maxEditText {
		return ErrInsertedTextTooLarge
	}
	remaining := pt.size - (end - start)
	if int64(len(text)) > math.MaxInt64-remaining {
		return ErrEditSizeOverflow
	}

	newPieces := make([]piece, 0, len(pt.pieces)+2)
	cursor := int64(0)
	inserted := false

	for _, current := range pt.pieces {
		pieceStart := cursor
		pieceEnd := cursor + current.length

		if pieceEnd <= start || pieceStart >= end {
			if !inserted && pieceStart >= end {
				newPieces = appendInsertedPiece(newPieces, -1, int64(len(text)))
				inserted = true
			}
			newPieces = append(newPieces, current)
			cursor = pieceEnd
			continue
		}

		if pieceStart < start {
			newPieces = append(newPieces, piece{
				source: current.source,
				start:  current.start,
				length: start - pieceStart,
			})
		}
		if !inserted {
			newPieces = appendInsertedPiece(newPieces, -1, int64(len(text)))
			inserted = true
		}
		if pieceEnd > end {
			skip := end - pieceStart
			newPieces = append(newPieces, piece{
				source: current.source,
				start:  current.start + skip,
				length: pieceEnd - end,
			})
		}

		cursor = pieceEnd
	}

	if !inserted {
		newPieces = appendInsertedPiece(newPieces, -1, int64(len(text)))
	}

	newPieces = normalizePieces(newPieces)
	if pt.maxPieces > 0 && len(newPieces) > pt.maxPieces {
		return ErrPieceCountLimit
	}

	var liveInserted int64
	for _, current := range newPieces {
		if current.source != pieceAdded {
			continue
		}
		if current.length > math.MaxInt64-liveInserted {
			return ErrLiveInsertedLimit
		}
		liveInserted += current.length
	}
	if pt.maxLiveInserted > 0 && liveInserted > pt.maxLiveInserted {
		return ErrLiveInsertedLimit
	}
	if liveInserted > int64(maxInt()) {
		return ErrLiveInsertedLimit
	}

	// Repack only the added bytes that remain live after this replacement.
	// Besides making the cumulative limit meaningful, this prevents repeated
	// replacement of inserted text from retaining dead append-buffer regions.
	newAdded := make([]byte, 0, int(liveInserted))
	for idx := range newPieces {
		current := &newPieces[idx]
		if current.source != pieceAdded {
			continue
		}
		newStart := int64(len(newAdded))
		if current.start == -1 {
			newAdded = append(newAdded, text...)
		} else {
			from := current.start
			to := from + current.length
			if from < 0 || to < from || to > int64(len(pt.added)) {
				return errors.New("piece table contains an invalid added-text range")
			}
			newAdded = append(newAdded, pt.added[from:to]...)
		}
		current.start = newStart
	}
	newPieces = normalizePieces(newPieces)
	if pt.maxPieces > 0 && len(newPieces) > pt.maxPieces {
		return ErrPieceCountLimit
	}

	pt.added = newAdded
	pt.pieces = newPieces
	pt.size = remaining + int64(len(text))
	pt.recordModifiedRange(start, max64(end, start+int64(len(text))))
	return nil
}

func (pt *PieceTable) pieceCount() int {
	if pt == nil {
		return 0
	}
	return len(pt.pieces)
}

func (pt *PieceTable) liveInsertedBytes() int64 {
	if pt == nil {
		return 0
	}
	return int64(len(pt.added))
}

func (pt *PieceTable) residentBytes() (int64, error) {
	if pt == nil {
		return 0, nil
	}
	return checkedMemorySum(
		int64(len(pt.added)),
		int64(len(pt.pieces))*pieceAccountingBytes,
		int64(len(pt.modified))*rangeAccountingBytes,
	)
}

// rangeEquals compares a transformed range with the exact bytes the caller
// observed before staging it. It uses a fixed-size scratch buffer even when an
// individual edit is several MiB.
func (pt *PieceTable) rangeEquals(src document.ReaderAtSize, start int64, end int64, expected []byte) (bool, error) {
	if pt == nil {
		return false, errors.New("piece table is required")
	}
	if src == nil {
		return false, errors.New("source document is required")
	}
	if start < 0 || end < start || end > pt.size || int64(len(expected)) != end-start {
		return false, errors.New("expected source bytes do not match the edit range")
	}
	if len(expected) == 0 {
		return true, nil
	}

	buf := make([]byte, sourceCompareBufferBytes)
	cursor := int64(0)
	expectedOffset := int64(0)
	for _, current := range pt.pieces {
		pieceStart := cursor
		pieceEnd := cursor + current.length
		cursor = pieceEnd
		if pieceEnd <= start || pieceStart >= end {
			continue
		}
		within := max64(start, pieceStart) - pieceStart
		remaining := min64(end, pieceEnd) - max64(start, pieceStart)
		for remaining > 0 {
			chunk := remaining
			if chunk > int64(len(buf)) {
				chunk = int64(len(buf))
			}
			want := expected[expectedOffset : expectedOffset+chunk]
			switch current.source {
			case pieceOriginal:
				n, err := src.ReadAt(buf[:chunk], current.start+within)
				if n != int(chunk) {
					shortErr := fmt.Errorf("compare original piece at offset %d: got %d of %d bytes: %w",
						current.start+within, n, chunk, ErrSourceModifiedDuringOperation)
					if err != nil {
						shortErr = errors.Join(shortErr, err)
					}
					return false, shortErr
				}
				if err != nil && !errors.Is(err, io.EOF) {
					return false, err
				}
				if !bytesEqual(buf[:chunk], want) {
					return false, nil
				}
			case pieceAdded:
				from := current.start + within
				if !bytesEqual(pt.added[from:from+chunk], want) {
					return false, nil
				}
			default:
				return false, errors.New("unknown piece source")
			}
			within += chunk
			remaining -= chunk
			expectedOffset += chunk
		}
	}
	return expectedOffset == int64(len(expected)), nil
}

func (pt *PieceTable) WriteTo(ctx context.Context, src document.ReaderAtSize, dst syncWriter, opts WriteOptions) (int64, error) {
	if pt == nil {
		return 0, errors.New("piece table is required")
	}
	if src == nil {
		return 0, errors.New("source document is required")
	}
	if dst == nil {
		return 0, errors.New("destination is required")
	}

	total := pt.size
	var written int64
	buf := make([]byte, 1024*1024)

	for _, current := range pt.pieces {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		default:
		}

		switch current.source {
		case pieceOriginal:
			reader := io.NewSectionReader(src, current.start, current.length)
			var pieceWritten int64
			for {
				select {
				case <-ctx.Done():
					return written, ctx.Err()
				default:
				}

				n, readErr := reader.Read(buf)
				if n > 0 {
					w, writeErr := dst.Write(buf[:n])
					written += int64(w)
					pieceWritten += int64(w)
					if opts.Progress != nil {
						opts.Progress(Progress{BytesWritten: written, BytesTotal: total})
					}
					if writeErr != nil {
						return written, writeErr
					}
					if w != n {
						return written, io.ErrShortWrite
					}
				}
				if errors.Is(readErr, io.EOF) {
					break
				}
				if readErr != nil {
					return written, readErr
				}
			}
			// A short read means the source shrank/was replaced since staging, so
			// the offsets we're copying no longer reference the bytes we expect.
			// Fail loudly instead of silently writing a truncated/misaligned copy.
			if pieceWritten != current.length {
				return written, fmt.Errorf("source shrank during save (read %d of %d bytes at offset %d): %w",
					pieceWritten, current.length, current.start, ErrSourceModifiedDuringOperation)
			}
		case pieceAdded:
			start := current.start
			end := current.start + current.length
			for pos := start; pos < end; {
				select {
				case <-ctx.Done():
					return written, ctx.Err()
				default:
				}

				chunkEnd := pos + int64(len(buf))
				if chunkEnd > end {
					chunkEnd = end
				}
				w, err := dst.Write(pt.added[pos:chunkEnd])
				written += int64(w)
				if opts.Progress != nil {
					opts.Progress(Progress{BytesWritten: written, BytesTotal: total})
				}
				if err != nil {
					return written, err
				}
				if int64(w) != chunkEnd-pos {
					return written, io.ErrShortWrite
				}
				pos = chunkEnd
			}
		default:
			return written, errors.New("unknown piece source")
		}
	}

	return written, dst.Sync()
}

// ReadRange returns the transformed (edited) bytes in [start, end), reading
// original spans from src and added spans from the staging buffer. Used to
// render a window that reflects staged edits.
func (pt *PieceTable) ReadRange(src document.ReaderAtSize, start int64, end int64) ([]byte, error) {
	if pt == nil {
		return nil, errors.New("piece table is required")
	}
	if src == nil {
		return nil, errors.New("source document is required")
	}
	start = clamp64(start, 0, pt.size)
	end = clamp64(end, 0, pt.size)
	if end <= start {
		return []byte{}, nil
	}
	out := make([]byte, 0, end-start)
	cursor := int64(0)
	for _, current := range pt.pieces {
		pieceStart := cursor
		pieceEnd := cursor + current.length
		cursor = pieceEnd
		if pieceEnd <= start || pieceStart >= end {
			continue
		}
		within := max64(start, pieceStart) - pieceStart
		n := min64(end, pieceEnd) - max64(start, pieceStart)
		switch current.source {
		case pieceOriginal:
			buf := make([]byte, n)
			readN, err := src.ReadAt(buf, current.start+within)
			if readN != len(buf) {
				shortErr := fmt.Errorf(
					"read original piece at offset %d: got %d of %d bytes: %w",
					current.start+within, readN, len(buf), ErrSourceModifiedDuringOperation,
				)
				if err != nil && !errors.Is(err, io.EOF) {
					shortErr = errors.Join(shortErr, err)
				}
				return nil, shortErr
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			out = append(out, buf...)
		case pieceAdded:
			from := current.start + within
			out = append(out, pt.added[from:from+n]...)
		default:
			return nil, errors.New("unknown piece source")
		}
	}
	return out, nil
}

func appendInsertedPiece(pieces []piece, start int64, length int64) []piece {
	if length <= 0 {
		return pieces
	}
	return append(pieces, piece{source: pieceAdded, start: start, length: length})
}

func normalizePieces(in []piece) []piece {
	out := make([]piece, 0, len(in))
	for _, current := range in {
		if current.length <= 0 {
			continue
		}
		if len(out) == 0 {
			out = append(out, current)
			continue
		}
		last := &out[len(out)-1]
		if last.source == current.source && last.start+last.length == current.start {
			last.length += current.length
			continue
		}
		out = append(out, current)
	}
	return out
}

func (pt *PieceTable) recordModifiedRange(start int64, end int64) {
	if end < start {
		end = start
	}
	next := Range{Start: start, End: end}
	if len(pt.modified) == 0 {
		pt.modified = append(pt.modified, next)
		return
	}

	merged := make([]Range, 0, len(pt.modified)+1)
	inserted := false
	for _, current := range pt.modified {
		if next.End < current.Start {
			if !inserted {
				merged = append(merged, next)
				inserted = true
			}
			merged = append(merged, current)
			continue
		}
		if current.End < next.Start {
			merged = append(merged, current)
			continue
		}
		next.Start = min64(next.Start, current.Start)
		next.End = max64(next.End, current.End)
	}
	if !inserted {
		merged = append(merged, next)
	}
	pt.modified = merged
}

func (pt *PieceTable) sourceRangeForTransformedRange(start int64, end int64) Range {
	if pt == nil {
		return Range{}
	}
	if end < start {
		end = start
	}
	start = clamp64(start, 0, pt.size)
	end = clamp64(end, 0, pt.size)
	if start == end {
		anchor := pt.sourceOffsetForTransformedPosition(start)
		return Range{Start: anchor, End: anchor}
	}

	var out Range
	foundOriginal := false
	cursor := int64(0)
	for _, current := range pt.pieces {
		pieceStart := cursor
		pieceEnd := cursor + current.length
		if current.source == pieceOriginal && start < pieceEnd && end > pieceStart {
			overlapStart := max64(start, pieceStart)
			overlapEnd := min64(end, pieceEnd)
			sourceStart := current.start + (overlapStart - pieceStart)
			sourceEnd := current.start + (overlapEnd - pieceStart)
			if !foundOriginal {
				out = Range{Start: sourceStart, End: sourceEnd}
				foundOriginal = true
			} else {
				out.Start = min64(out.Start, sourceStart)
				out.End = max64(out.End, sourceEnd)
			}
		}
		cursor = pieceEnd
	}
	if foundOriginal {
		return out
	}

	anchor := pt.sourceOffsetForTransformedPosition(start)
	return Range{Start: anchor, End: anchor}
}

func (pt *PieceTable) sourceRangeToTransformedRange(start int64, end int64) (Range, bool) {
	if pt == nil {
		return Range{}, false
	}
	if end < start {
		end = start
	}
	start = clamp64(start, 0, pt.originalSize)
	end = clamp64(end, 0, pt.originalSize)
	if start == end {
		pos, ok := pt.transformedPositionForSourceOffset(start)
		if !ok {
			return Range{}, false
		}
		return Range{Start: pos, End: pos}, true
	}

	var out Range
	var covered int64
	foundOriginal := false
	cursor := int64(0)
	for _, current := range pt.pieces {
		pieceStart := cursor
		pieceEnd := cursor + current.length
		if current.source == pieceOriginal && start < current.start+current.length && end > current.start {
			sourceStart := max64(start, current.start)
			sourceEnd := min64(end, current.start+current.length)
			transformedStart := pieceStart + (sourceStart - current.start)
			transformedEnd := pieceStart + (sourceEnd - current.start)
			if !foundOriginal {
				out = Range{Start: transformedStart, End: transformedEnd}
				foundOriginal = true
			} else {
				out.Start = min64(out.Start, transformedStart)
				out.End = max64(out.End, transformedEnd)
			}
			covered += sourceEnd - sourceStart
		}
		cursor = pieceEnd
	}
	if !foundOriginal || covered != end-start {
		return Range{}, false
	}
	return out, true
}

// transformedRangeToSourceRange maps a transformed half-open range back to a
// conservative source span. Original pieces map byte-for-byte. A boundary
// inside added text maps to the source gap between the adjacent original
// pieces, so a replacement-only window still exposes the bytes that were
// replaced. Pure insertions naturally map to a zero-width source range.
//
// The mapping is intentionally conservative: it can include a complete
// replaced source gap when the requested transformed range contains only part
// of its replacement. Callers must independently bound the mapped source span
// before reading it.
func (pt *PieceTable) transformedRangeToSourceRange(start int64, end int64) (Range, bool) {
	if pt == nil || start < 0 || end < start || end > pt.size {
		return Range{}, false
	}
	if start == end {
		before, ok := pt.sourceImmediatelyBefore(start)
		if !ok {
			return Range{}, false
		}
		after, ok := pt.sourceImmediatelyAfter(start)
		if !ok || after < before {
			return Range{}, false
		}
		return Range{Start: before, End: after}, true
	}

	sourceStart, ok := pt.sourceBoundaryForRangeStart(start)
	if !ok {
		return Range{}, false
	}
	sourceEnd, ok := pt.sourceBoundaryForRangeEnd(end)
	if !ok || sourceEnd < sourceStart {
		return Range{}, false
	}
	return Range{Start: sourceStart, End: sourceEnd}, true
}

// sourceBoundaryForRangeStart uses the piece to the right when pos is exactly
// between pieces. This excludes an edit that ended immediately before the
// requested transformed range.
func (pt *PieceTable) sourceBoundaryForRangeStart(pos int64) (int64, bool) {
	if pt == nil || pos < 0 || pos > pt.size {
		return 0, false
	}
	if pos == pt.size {
		return pt.originalSize, true
	}
	cursor := int64(0)
	for idx, current := range pt.pieces {
		pieceStart := cursor
		pieceEnd := cursor + current.length
		if pos >= pieceStart && pos < pieceEnd {
			if current.source == pieceOriginal {
				return current.start + (pos - pieceStart), true
			}
			before, _, ok := pt.sourceGapAroundAddedPiece(idx)
			return before, ok
		}
		cursor = pieceEnd
	}
	return 0, false
}

// sourceBoundaryForRangeEnd uses the piece to the left when pos is exactly
// between pieces. This excludes an edit that begins immediately after the
// requested transformed range.
func (pt *PieceTable) sourceBoundaryForRangeEnd(pos int64) (int64, bool) {
	if pt == nil || pos < 0 || pos > pt.size {
		return 0, false
	}
	if pos == 0 {
		return 0, true
	}
	cursor := int64(0)
	for idx, current := range pt.pieces {
		pieceStart := cursor
		pieceEnd := cursor + current.length
		if pos > pieceStart && pos <= pieceEnd {
			if current.source == pieceOriginal {
				return current.start + (pos - pieceStart), true
			}
			_, after, ok := pt.sourceGapAroundAddedPiece(idx)
			return after, ok
		}
		cursor = pieceEnd
	}
	return 0, false
}

func (pt *PieceTable) sourceGapAroundAddedPiece(index int) (int64, int64, bool) {
	if pt == nil || index < 0 || index >= len(pt.pieces) || pt.pieces[index].source != pieceAdded {
		return 0, 0, false
	}
	before := int64(0)
	for idx := index - 1; idx >= 0; idx-- {
		current := pt.pieces[idx]
		if current.source == pieceOriginal {
			before = current.start + current.length
			break
		}
	}
	after := pt.originalSize
	for idx := index + 1; idx < len(pt.pieces); idx++ {
		current := pt.pieces[idx]
		if current.source == pieceOriginal {
			after = current.start
			break
		}
	}
	if after < before {
		return 0, 0, false
	}
	return before, after, true
}

func (pt *PieceTable) sourceImmediatelyBefore(pos int64) (int64, bool) {
	if pt == nil || pos < 0 || pos > pt.size {
		return 0, false
	}
	if pos == 0 {
		return 0, true
	}
	cursor := int64(0)
	for idx, current := range pt.pieces {
		pieceStart := cursor
		pieceEnd := cursor + current.length
		if pos > pieceStart && pos <= pieceEnd {
			if current.source == pieceOriginal {
				return current.start + (pos - pieceStart), true
			}
			before, _, ok := pt.sourceGapAroundAddedPiece(idx)
			return before, ok
		}
		cursor = pieceEnd
	}
	// An empty transformed document can represent deletion of the entire
	// source. Its left source boundary is the beginning of the source.
	if pt.size == 0 {
		return 0, true
	}
	return 0, false
}

func (pt *PieceTable) sourceImmediatelyAfter(pos int64) (int64, bool) {
	if pt == nil || pos < 0 || pos > pt.size {
		return 0, false
	}
	if pos == pt.size {
		return pt.originalSize, true
	}
	cursor := int64(0)
	for idx, current := range pt.pieces {
		pieceStart := cursor
		pieceEnd := cursor + current.length
		if pos >= pieceStart && pos < pieceEnd {
			if current.source == pieceOriginal {
				return current.start + (pos - pieceStart), true
			}
			_, after, ok := pt.sourceGapAroundAddedPiece(idx)
			return after, ok
		}
		cursor = pieceEnd
	}
	return 0, false
}

func (pt *PieceTable) transformedPositionForSourceOffset(sourceOffset int64) (int64, bool) {
	if pt == nil {
		return 0, false
	}
	sourceOffset = clamp64(sourceOffset, 0, pt.originalSize)
	cursor := int64(0)
	for _, current := range pt.pieces {
		pieceStart := cursor
		pieceEnd := cursor + current.length
		if current.source == pieceOriginal && sourceOffset >= current.start && sourceOffset <= current.start+current.length {
			return pieceStart + (sourceOffset - current.start), true
		}
		cursor = pieceEnd
	}
	if sourceOffset == pt.originalSize {
		return pt.size, true
	}
	return 0, false
}

func (pt *PieceTable) sourceOffsetForTransformedPosition(pos int64) int64 {
	if pt == nil {
		return 0
	}
	pos = clamp64(pos, 0, pt.size)
	cursor := int64(0)
	previousOriginalEnd := int64(0)
	for idx, current := range pt.pieces {
		pieceStart := cursor
		pieceEnd := cursor + current.length
		if current.source == pieceOriginal {
			if pos >= pieceStart && pos <= pieceEnd {
				return current.start + clamp64(pos-pieceStart, 0, current.length)
			}
			previousOriginalEnd = current.start + current.length
		} else if pos >= pieceStart && pos <= pieceEnd {
			if previousOriginalEnd > 0 || pieceStart == 0 {
				return previousOriginalEnd
			}
			if next, ok := nextOriginalStart(pt.pieces[idx+1:]); ok {
				return next
			}
			return previousOriginalEnd
		}
		cursor = pieceEnd
	}
	return pt.originalSize
}

func nextOriginalStart(pieces []piece) (int64, bool) {
	for _, current := range pieces {
		if current.source == pieceOriginal {
			return current.start, true
		}
	}
	return 0, false
}

func clamp64(value int64, low int64, high int64) int64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func min64(a int64, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a int64, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for idx := range left {
		if left[idx] != right[idx] {
			return false
		}
	}
	return true
}

type syncWriter interface {
	io.Writer
	Sync() error
}
