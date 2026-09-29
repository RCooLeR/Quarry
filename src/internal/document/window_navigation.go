package document

import (
	"errors"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/quarry/quarry-wails3/internal/encodingx"
	"github.com/quarry/quarry-wails3/internal/newlines"
)

const navigationScanChunkBytes = 64 * 1024

// ErrTextBoundary reports a requested decoded range that cuts through a source
// character or UTF-16 surrogate pair. Callers must not repair such a range by
// silently inserting a replacement character.
var ErrTextBoundary = errors.New("decoded range is not aligned to source text boundaries")

// AlignTextOffsetBackward returns the nearest valid source character boundary
// at or before offset. The read is constant-sized and source-generation checked.
func (d *FileDocument) AlignTextOffsetBackward(offset int64) (int64, error) {
	if d == nil {
		return 0, errDocumentClosed
	}
	offset = d.ClampOffset(offset)
	if offset <= 0 {
		return 0, nil
	}
	if d.meta.EncodingRequiresConfirmation {
		return 0, encodingx.ErrEncodingConfirmationRequired
	}

	switch d.meta.Encoding {
	case "UTF-16LE", "UTF-16BE":
		if offset < 2 && d.size >= 2 {
			return 0, nil
		}
		offset &^= 1
		if offset <= 0 || offset >= d.size {
			return offset, nil
		}
		start := offset - 2
		end := min(offset+2, d.size)
		data, err := d.ReadRangeWithLimit(start, end, 4)
		if err != nil {
			return 0, err
		}
		if len(data) >= 4 {
			previous := decodeUTF16Unit(data[0:2], d.meta.Encoding)
			current := decodeUTF16Unit(data[2:4], d.meta.Encoding)
			if utf16.IsSurrogate(rune(previous)) && previous >= 0xD800 && previous <= 0xDBFF && current >= 0xDC00 && current <= 0xDFFF {
				offset -= 2
			}
		}
		return offset, nil
	case "UTF-8", "":
		if offset < 3 && d.size >= 3 {
			bom, err := d.ReadRangeWithLimit(0, min64(d.size, 3), 3)
			if err != nil {
				return 0, err
			}
			if len(bom) == 3 && bom[0] == 0xEF && bom[1] == 0xBB && bom[2] == 0xBF {
				return 0, nil
			}
		}
		if offset >= d.size {
			return offset, nil
		}
		start := max(offset-3, 0)
		data, err := d.ReadRangeWithLimit(start, offset+1, 4)
		if err != nil {
			return 0, err
		}
		i := int(offset - start)
		for i > 0 && i < len(data) && !utf8.RuneStart(data[i]) {
			i--
		}
		return start + int64(i), nil
	default:
		return offset, nil
	}
}

// AlignTextOffsetForward returns the nearest valid source character boundary
// at or after offset. At most one UTF-8 rune or one UTF-16 surrogate pair is
// inspected.
func (d *FileDocument) AlignTextOffsetForward(offset int64) (int64, error) {
	if d == nil {
		return 0, errDocumentClosed
	}
	offset = d.ClampOffset(offset)
	if offset >= d.size {
		return d.size, nil
	}
	back, err := d.AlignTextOffsetBackward(offset)
	if err != nil {
		return 0, err
	}
	if back == offset {
		return offset, nil
	}

	end := min(back+4, d.size)
	data, err := d.ReadRangeWithLimit(back, end, 4)
	if err != nil {
		return 0, err
	}
	switch d.meta.Encoding {
	case "UTF-16LE", "UTF-16BE":
		width := int64(2)
		if len(data) >= 4 {
			first := decodeUTF16Unit(data[:2], d.meta.Encoding)
			second := decodeUTF16Unit(data[2:4], d.meta.Encoding)
			if first >= 0xD800 && first <= 0xDBFF && second >= 0xDC00 && second <= 0xDFFF {
				width = 4
			}
		}
		return min64(d.size, back+width), nil
	case "UTF-8", "":
		_, width := utf8.DecodeRune(data)
		if width <= 0 {
			width = 1
		}
		return min64(d.size, back+int64(width)), nil
	default:
		return offset, nil
	}
}

// AlignedWindowStart resolves the logical line containing requested when that
// line start lies within maxLookback bytes. A line longer than that bound uses
// a character-aligned continuation start and reports continuesLine=true.
func (d *FileDocument) AlignedWindowStart(requested int64, maxLookback int64) (start int64, continuesLine bool, err error) {
	if d == nil {
		return 0, false, errDocumentClosed
	}
	if maxLookback < 0 {
		return 0, false, errors.New("window lookback must not be negative")
	}
	requested = d.ClampOffset(requested)
	start, exceeded, err := d.findLineStartWithinLimit(requested, maxLookback)
	if err != nil {
		return 0, false, err
	}
	if !exceeded {
		return start, false, nil
	}
	start, err = d.AlignTextOffsetBackward(requested)
	if err != nil {
		return 0, false, err
	}
	return start, true, nil
}

// StartsInsideLine reports whether offset is a character position other than
// BOF or the byte immediately after a complete encoding-aware line terminator.
func (d *FileDocument) StartsInsideLine(offset int64) (bool, error) {
	return d.offsetStartsInsideLine(offset)
}

// PreviousWindowStart returns a bounded start for a page ending exclusively at
// currentStart. It retains only maxLines+1 newline offsets while scanning, so
// memory is independent of source span and line count. The chosen span is never
// greater than maxBytes.
func (d *FileDocument) PreviousWindowStart(currentStart int64, maxBytes int64, maxLines int) (int64, error) {
	if d == nil {
		return 0, errDocumentClosed
	}
	if maxBytes <= 0 {
		return 0, errors.New("previous-window byte budget must be positive")
	}
	if maxLines <= 0 {
		return 0, errors.New("previous-window line limit must be positive")
	}
	currentStart = d.ClampOffset(currentStart)
	if currentStart <= 0 {
		return 0, nil
	}
	rangeStart := max(currentStart-maxBytes, 0)
	rangeStart, err := d.AlignTextOffsetForward(rangeStart)
	if err != nil {
		return 0, err
	}
	if rangeStart >= currentStart {
		return rangeStart, nil
	}

	capacity := maxLines + 1
	breakRing := make([]int64, capacity)
	breakCount := 0
	breakNext := 0
	rememberBreak := func(end int64) {
		breakRing[breakNext] = end
		breakNext = (breakNext + 1) % capacity
		if breakCount < capacity {
			breakCount++
		}
	}
	scanner := newlines.New(d.meta.Encoding)
	bufSize := navigationScanChunkBytes
	if span := currentStart - rangeStart; span < int64(bufSize) {
		bufSize = int(span)
	}
	if bufSize <= 0 {
		return rangeStart, nil
	}
	pos := rangeStart
	for pos < currentStart {
		end := min(pos+int64(bufSize), currentStart)
		data, readErr := d.ReadRangeWithLimit(pos, end, int64(bufSize))
		if readErr != nil {
			return 0, readErr
		}
		scanner.Scan(data, pos, func(br newlines.Break) bool {
			if br.End > currentStart {
				return false
			}
			rememberBreak(br.End)
			return true
		})
		pos = end
	}
	if currentStart == d.size {
		scanner.Finish(func(br newlines.Break) bool {
			if br.End <= currentStart {
				rememberBreak(br.End)
			}
			return true
		})
	} else {
		lookaheadEnd := min64(d.size, currentStart+4)
		lookahead, readErr := d.ReadRangeWithLimit(currentStart, lookaheadEnd, 4)
		if readErr != nil {
			return 0, readErr
		}
		scanner.Scan(lookahead, currentStart, func(br newlines.Break) bool {
			if br.End <= currentStart {
				rememberBreak(br.End)
			}
			return false
		})
	}
	breakEnds := make([]int64, 0, breakCount)
	if breakCount < capacity {
		breakEnds = append(breakEnds, breakRing[:breakCount]...)
	} else {
		breakEnds = append(breakEnds, breakRing[breakNext:]...)
		breakEnds = append(breakEnds, breakRing[:breakNext]...)
	}

	trailingRow := len(breakEnds) == 0 || breakEnds[len(breakEnds)-1] < currentStart
	allowedBreaks := maxLines
	if trailingRow {
		allowedBreaks--
	}
	if allowedBreaks < 0 {
		allowedBreaks = 0
	}
	if len(breakEnds) > allowedBreaks {
		index := len(breakEnds) - allowedBreaks - 1
		if index >= 0 {
			return breakEnds[index], nil
		}
	}
	return rangeStart, nil
}

// DecodeAlignedRange strictly decodes an already aligned bounded source range.
// It never substitutes U+FFFD for malformed UTF-8 or UTF-16 input.
func (d *FileDocument) DecodeAlignedRange(start, end int64, maxBytes int64) (string, error) {
	if d == nil {
		return "", errDocumentClosed
	}
	if d.meta.EncodingRequiresConfirmation {
		return "", encodingx.ErrEncodingConfirmationRequired
	}
	if start < 0 || end < start || end > d.size || end-start > maxBytes {
		return "", fmt.Errorf("invalid decoded range [%d,%d) with limit %d", start, end, maxBytes)
	}
	startBoundary, err := d.AlignTextOffsetBackward(start)
	if err != nil {
		return "", err
	}
	endBoundary, err := d.AlignTextOffsetBackward(end)
	if err != nil {
		return "", err
	}
	if startBoundary != start || endBoundary != end {
		return "", ErrTextBoundary
	}
	raw, err := d.ReadRangeWithLimit(start, end, maxBytes)
	if err != nil {
		return "", err
	}
	if err := validateEncodedText(d.meta.Encoding, raw); err != nil {
		return "", err
	}
	return decodeVisibleText(d.meta.Encoding, raw, start == 0), nil
}

func validateEncodedText(encodingName string, data []byte) error {
	switch encodingName {
	case "UTF-8", "":
		if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
			data = data[3:]
		}
		if !utf8.Valid(data) {
			return errors.New("invalid UTF-8 in decoded range")
		}
	case "UTF-16LE", "UTF-16BE":
		if len(data) >= 2 && ((encodingName == "UTF-16LE" && data[0] == 0xFF && data[1] == 0xFE) || (encodingName == "UTF-16BE" && data[0] == 0xFE && data[1] == 0xFF)) {
			data = data[2:]
		}
		if len(data)%2 != 0 {
			return errors.New("odd-length UTF-16 decoded range")
		}
		for i := 0; i < len(data); i += 2 {
			unit := decodeUTF16Unit(data[i:i+2], encodingName)
			switch {
			case unit >= 0xD800 && unit <= 0xDBFF:
				if i+3 >= len(data) {
					return errors.New("truncated UTF-16 surrogate pair")
				}
				next := decodeUTF16Unit(data[i+2:i+4], encodingName)
				if next < 0xDC00 || next > 0xDFFF {
					return errors.New("invalid UTF-16 surrogate pair")
				}
				i += 2
			case unit >= 0xDC00 && unit <= 0xDFFF:
				return errors.New("unpaired UTF-16 low surrogate")
			}
		}
	}
	return nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
