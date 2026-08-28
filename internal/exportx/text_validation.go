package exportx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/quarry/quarry-wails3/internal/document"
	"golang.org/x/text/encoding/charmap"
)

// ErrUnalignedTextRange identifies a requested text range whose start or end
// falls inside an encoding unit. Text exports reject these ranges; they never
// expand caller-provided byte coordinates implicitly. Raw byte exports remain
// available when byte-exact, potentially non-text fragments are intentional.
var ErrUnalignedTextRange = errors.New("text range endpoint is not aligned to the source encoding")

// ErrInvalidSourceText identifies malformed bytes in a range declared as text.
// Such a range is never published as a completed text export.
var ErrInvalidSourceText = errors.New("source range is not valid in the declared encoding")

type textEncodingKind uint8

const (
	textEncodingUTF8 textEncodingKind = iota + 1
	textEncodingUTF16LE
	textEncodingUTF16BE
	textEncodingWindows1251
	textEncodingWindows1252
)

func parseTextEncoding(name string) (textEncodingKind, error) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "UTF-8":
		return textEncodingUTF8, nil
	case "UTF-16LE":
		return textEncodingUTF16LE, nil
	case "UTF-16BE":
		return textEncodingUTF16BE, nil
	case "WINDOWS-1251":
		return textEncodingWindows1251, nil
	case "WINDOWS-1252":
		return textEncodingWindows1252, nil
	default:
		return 0, errors.New("unsupported encoding: " + name)
	}
}

func (k textEncodingKind) String() string {
	switch k {
	case textEncodingUTF8:
		return "UTF-8"
	case textEncodingUTF16LE:
		return "UTF-16LE"
	case textEncodingUTF16BE:
		return "UTF-16BE"
	case textEncodingWindows1251:
		return "Windows-1251"
	case textEncodingWindows1252:
		return "Windows-1252"
	default:
		return "unknown encoding"
	}
}

// prepareTextRange applies the text-export boundary policy before an output is
// created. A matching leading source BOM is metadata and is omitted only when
// the requested range starts at byte zero. Endpoints inside that BOM, a UTF-8
// rune, a UTF-16 code unit, or a UTF-16 surrogate pair are rejected.
func prepareTextRange(doc document.ReaderAtSize, start int64, end int64, kind textEncodingKind) (int64, int64, error) {
	size := doc.Size()
	bomLength, err := inspectSourceBOM(doc, size, kind)
	if err != nil {
		return start, end, err
	}
	if bomLength > 0 {
		if start > 0 && start < bomLength {
			return start, end, unalignedTextRangeError(kind, start, "start endpoint is inside the source BOM")
		}
		if end > 0 && end < bomLength {
			return start, end, unalignedTextRangeError(kind, end, "end endpoint is inside the source BOM")
		}
		if start == 0 {
			start = bomLength
		}
	}
	if err := validateTextEndpoint(doc, size, start, kind, "start"); err != nil {
		return start, end, err
	}
	if err := validateTextEndpoint(doc, size, end, kind, "end"); err != nil {
		return start, end, err
	}
	return start, end, nil
}

func inspectSourceBOM(doc document.ReaderAtSize, size int64, kind textEncodingKind) (int64, error) {
	prefixLength := min(size, int64(3))
	if prefixLength <= 0 {
		return 0, nil
	}
	prefix := make([]byte, int(prefixLength))
	if err := readTextBytesAt(doc, prefix, 0); err != nil {
		return 0, err
	}

	switch kind {
	case textEncodingUTF8:
		if len(prefix) >= 3 && bytes.Equal(prefix[:3], []byte{0xEF, 0xBB, 0xBF}) {
			return 3, nil
		}
	case textEncodingUTF16LE:
		if len(prefix) >= 2 && bytes.Equal(prefix[:2], []byte{0xFE, 0xFF}) {
			return 0, fmt.Errorf("%w: UTF-16BE BOM conflicts with declared UTF-16LE source", ErrInvalidSourceText)
		}
		if len(prefix) >= 2 && bytes.Equal(prefix[:2], []byte{0xFF, 0xFE}) {
			return 2, nil
		}
	case textEncodingUTF16BE:
		if len(prefix) >= 2 && bytes.Equal(prefix[:2], []byte{0xFF, 0xFE}) {
			return 0, fmt.Errorf("%w: UTF-16LE BOM conflicts with declared UTF-16BE source", ErrInvalidSourceText)
		}
		if len(prefix) >= 2 && bytes.Equal(prefix[:2], []byte{0xFE, 0xFF}) {
			return 2, nil
		}
	}
	return 0, nil
}

func validateTextEndpoint(doc document.ReaderAtSize, size int64, offset int64, kind textEncodingKind, endpoint string) error {
	if offset < 0 || offset > size {
		return unalignedTextRangeError(kind, offset, endpoint+" endpoint is outside the source")
	}
	switch kind {
	case textEncodingUTF8:
		if offset == size {
			return nil
		}
		var next [1]byte
		if err := readTextBytesAt(doc, next[:], offset); err != nil {
			return err
		}
		if next[0]&0xC0 == 0x80 {
			return unalignedTextRangeError(kind, offset, endpoint+" endpoint is inside a UTF-8 rune")
		}
	case textEncodingUTF16LE, textEncodingUTF16BE:
		if offset%2 != 0 {
			return unalignedTextRangeError(kind, offset, endpoint+" endpoint is inside a UTF-16 code unit")
		}
		if offset >= 2 && offset+2 <= size {
			var seam [4]byte
			if err := readTextBytesAt(doc, seam[:], offset-2); err != nil {
				return err
			}
			previous := decodeUTF16Unit(kind, seam[:2])
			next := decodeUTF16Unit(kind, seam[2:])
			if isHighSurrogate(previous) && isLowSurrogate(next) {
				return unalignedTextRangeError(kind, offset, endpoint+" endpoint is inside a UTF-16 surrogate pair")
			}
		}
	}
	return nil
}

func unalignedTextRangeError(kind textEncodingKind, offset int64, reason string) error {
	return fmt.Errorf("%w: %s at byte offset %d (%s)", ErrUnalignedTextRange, reason, offset, kind)
}

func readTextBytesAt(doc document.ReaderAtSize, buf []byte, offset int64) error {
	n, err := doc.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n != len(buf) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func decodeUTF16Unit(kind textEncodingKind, buf []byte) uint16 {
	if kind == textEncodingUTF16LE {
		return binary.LittleEndian.Uint16(buf)
	}
	return binary.BigEndian.Uint16(buf)
}

func isHighSurrogate(unit uint16) bool {
	return unit >= 0xD800 && unit <= 0xDBFF
}

func isLowSurrogate(unit uint16) bool {
	return unit >= 0xDC00 && unit <= 0xDFFF
}

// validatingTextReader validates the exact selected source stream while
// passing it to the decoder. This second line of defense catches malformed
// interior input and source changes after endpoint checks without buffering the
// range. Invalid chunks are withheld from the decoder and the atomic output is
// never committed.
type validatingTextReader struct {
	reader    io.Reader
	validator textStreamValidator
}

func newValidatingTextReader(reader io.Reader, kind textEncodingKind, startOffset int64) *validatingTextReader {
	return &validatingTextReader{
		reader: reader,
		validator: textStreamValidator{
			kind:   kind,
			offset: startOffset,
		},
	}
}

func (r *validatingTextReader) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	n, readErr := r.reader.Read(buf)
	if n > 0 {
		if err := r.validator.consume(buf[:n]); err != nil {
			return 0, err
		}
	}
	if errors.Is(readErr, io.EOF) {
		if err := r.validator.finish(); err != nil {
			return 0, err
		}
	}
	return n, readErr
}

type textStreamValidator struct {
	kind textEncodingKind

	offset int64

	utf8Remaining    uint8
	utf8Continuation uint8
	utf8FirstMin     byte
	utf8FirstMax     byte
	utf8SequenceAt   int64

	utf16HasByte bool
	utf16Byte    byte
	utf16ByteAt  int64
	utf16HighAt  int64
	utf16HasHigh bool
}

func (v *textStreamValidator) consume(buf []byte) error {
	for _, value := range buf {
		at := v.offset
		v.offset++
		switch v.kind {
		case textEncodingUTF8:
			if err := v.consumeUTF8(value, at); err != nil {
				return err
			}
		case textEncodingUTF16LE, textEncodingUTF16BE:
			if err := v.consumeUTF16(value, at); err != nil {
				return err
			}
		case textEncodingWindows1251:
			if charmap.Windows1251.DecodeByte(value) == utf8.RuneError {
				return invalidSourceTextError(v.kind, at, "undefined code-page byte")
			}
		case textEncodingWindows1252:
			if charmap.Windows1252.DecodeByte(value) == utf8.RuneError {
				return invalidSourceTextError(v.kind, at, "undefined code-page byte")
			}
		}
	}
	return nil
}

func (v *textStreamValidator) consumeUTF8(value byte, at int64) error {
	if v.utf8Remaining == 0 {
		switch {
		case value <= 0x7F:
			return nil
		case value >= 0xC2 && value <= 0xDF:
			v.beginUTF8Sequence(at, 1, 0x80, 0xBF)
		case value >= 0xE1 && value <= 0xEC || value >= 0xEE && value <= 0xEF:
			v.beginUTF8Sequence(at, 2, 0x80, 0xBF)
		case value == 0xE0:
			v.beginUTF8Sequence(at, 2, 0xA0, 0xBF)
		case value == 0xED:
			v.beginUTF8Sequence(at, 2, 0x80, 0x9F)
		case value >= 0xF1 && value <= 0xF3:
			v.beginUTF8Sequence(at, 3, 0x80, 0xBF)
		case value == 0xF0:
			v.beginUTF8Sequence(at, 3, 0x90, 0xBF)
		case value == 0xF4:
			v.beginUTF8Sequence(at, 3, 0x80, 0x8F)
		default:
			return invalidSourceTextError(v.kind, at, "invalid UTF-8 leading byte")
		}
		return nil
	}

	if value < 0x80 || value > 0xBF {
		return invalidSourceTextError(v.kind, at, "invalid UTF-8 continuation byte")
	}
	if v.utf8Continuation == 0 && (value < v.utf8FirstMin || value > v.utf8FirstMax) {
		return invalidSourceTextError(v.kind, at, "invalid UTF-8 first continuation byte")
	}
	v.utf8Continuation++
	v.utf8Remaining--
	return nil
}

func (v *textStreamValidator) beginUTF8Sequence(at int64, remaining uint8, firstMin byte, firstMax byte) {
	v.utf8Remaining = remaining
	v.utf8Continuation = 0
	v.utf8FirstMin = firstMin
	v.utf8FirstMax = firstMax
	v.utf8SequenceAt = at
}

func (v *textStreamValidator) consumeUTF16(value byte, at int64) error {
	if !v.utf16HasByte {
		v.utf16HasByte = true
		v.utf16Byte = value
		v.utf16ByteAt = at
		return nil
	}

	var unit uint16
	if v.kind == textEncodingUTF16LE {
		unit = uint16(v.utf16Byte) | uint16(value)<<8
	} else {
		unit = uint16(v.utf16Byte)<<8 | uint16(value)
	}
	unitAt := v.utf16ByteAt
	v.utf16HasByte = false

	if v.utf16HasHigh {
		if !isLowSurrogate(unit) {
			return invalidSourceTextError(v.kind, v.utf16HighAt, "unpaired UTF-16 high surrogate")
		}
		v.utf16HasHigh = false
		return nil
	}
	if isHighSurrogate(unit) {
		v.utf16HighAt = unitAt
		v.utf16HasHigh = true
		return nil
	}
	if isLowSurrogate(unit) {
		return invalidSourceTextError(v.kind, unitAt, "unpaired UTF-16 low surrogate")
	}
	return nil
}

func (v *textStreamValidator) finish() error {
	switch v.kind {
	case textEncodingUTF8:
		if v.utf8Remaining != 0 {
			return invalidSourceTextError(v.kind, v.utf8SequenceAt, "truncated UTF-8 rune")
		}
	case textEncodingUTF16LE, textEncodingUTF16BE:
		if v.utf16HasByte {
			return invalidSourceTextError(v.kind, v.utf16ByteAt, "truncated UTF-16 code unit")
		}
		if v.utf16HasHigh {
			return invalidSourceTextError(v.kind, v.utf16HighAt, "unpaired UTF-16 high surrogate")
		}
	}
	return nil
}

func invalidSourceTextError(kind textEncodingKind, offset int64, reason string) error {
	return fmt.Errorf("%w: %s at byte offset %d (%s)", ErrInvalidSourceText, reason, offset, kind)
}

func validateCompleteText(kind textEncodingKind, data []byte) error {
	validator := textStreamValidator{kind: kind}
	if err := validator.consume(data); err != nil {
		return err
	}
	return validator.finish()
}
