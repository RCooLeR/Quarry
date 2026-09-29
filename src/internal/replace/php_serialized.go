package replace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrPossiblePHPSerializedData is returned before a generic replacement can
// be published from a source that contains a recognizable PHP serialization
// marker. PHP strings and class names carry decoded byte lengths in the form
// s:N:"..." and O:N:"...". Replacing raw SQL-dump bytes cannot update those
// lengths safely because SQL escaping changes the relationship between dump
// bytes and the bytes stored by the database.
//
// Callers must use a SQL- and PHP-serialization-aware migration tool instead.
var ErrPossiblePHPSerializedData = errors.New("replacement stopped: possible PHP/WordPress serialized data detected; generic replacement can invalidate stored byte lengths; no output was published")

type phpSerializationScanState uint8

const (
	phpSerializationIdle phpSerializationScanState = iota
	phpSerializationWantColon
	phpSerializationWantDigit
	phpSerializationDigits
	phpSerializationWantValueStart
	phpSerializationEscapedQuote
)

// phpSerializationDetector recognizes high-confidence native PHP
// serialization prefixes without retaining the numeric length or any payload.
// It is intentionally conservative: once a marker is recognizable, Quarry
// cannot prove a generic raw-byte replacement is structurally safe.
//
// The detector is a constant-memory DFA so markers may cross arbitrary source
// read boundaries, including one-byte chunks and very long numeric fields.
type phpSerializationDetector struct {
	state           phpSerializationScanState
	kind            byte
	candidateOffset int64
	nextOffset      int64
	detected        bool
}

func (d *phpSerializationDetector) Inspect(p []byte) {
	if len(p) == 0 {
		return
	}
	if d.detected {
		d.nextOffset += int64(len(p))
		return
	}
	for i, b := range p {
		d.inspectByte(b, d.nextOffset+int64(i))
		if d.detected {
			break
		}
	}
	d.nextOffset += int64(len(p))
}

func (d *phpSerializationDetector) inspectByte(b byte, offset int64) {
	switch d.state {
	case phpSerializationIdle:
		d.restartAt(b, offset)
	case phpSerializationWantColon:
		if b == ':' {
			d.state = phpSerializationWantDigit
			return
		}
		d.restartAt(b, offset)
	case phpSerializationWantDigit:
		if isASCIIDigit(b) {
			d.state = phpSerializationDigits
			return
		}
		d.restartAt(b, offset)
	case phpSerializationDigits:
		if isASCIIDigit(b) {
			return
		}
		if b == ':' {
			d.state = phpSerializationWantValueStart
			return
		}
		d.restartAt(b, offset)
	case phpSerializationWantValueStart:
		if d.kind == 'a' {
			if b == '{' {
				d.detected = true
				return
			}
			d.restartAt(b, offset)
			return
		}
		if b == '"' {
			d.detected = true
			return
		}
		if b == '\\' {
			// Some SQL/JSON exporters unnecessarily escape the serialization
			// delimiter. Accept one or more backslashes before the quote so
			// the safety guard still fails closed for those dumps.
			d.state = phpSerializationEscapedQuote
			return
		}
		d.restartAt(b, offset)
	case phpSerializationEscapedQuote:
		if b == '\\' {
			return
		}
		if b == '"' {
			d.detected = true
			return
		}
		d.restartAt(b, offset)
	default:
		d.state = phpSerializationIdle
		d.restartAt(b, offset)
	}
}

func (d *phpSerializationDetector) restartAt(b byte, offset int64) {
	if isPHPSerializationLengthKind(b) {
		d.kind = b
		d.candidateOffset = offset
		d.state = phpSerializationWantColon
		return
	}
	d.kind = 0
	d.state = phpSerializationIdle
}

func (d *phpSerializationDetector) Err() error {
	if !d.detected {
		return nil
	}
	return fmt.Errorf("%w (marker %q at byte %d)", ErrPossiblePHPSerializedData, string([]byte{d.kind}), d.candidateOffset)
}

func isPHPSerializationLengthKind(b byte) bool {
	switch b {
	case 'a', 's', 'S', 'O', 'C', 'E':
		return true
	default:
		return false
	}
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// phpSerializationGuardSource inspects the exact bytes already read from the
// verified source handle. Detection returns a terminal error without handing
// the suspicious chunk to the replacement engine. Earlier output can exist
// only in the operation-owned atomic temporary file, which the caller removes.
type phpSerializationGuardSource struct {
	source   readStatSource
	detector phpSerializationDetector
}

func (s *phpSerializationGuardSource) Read(p []byte) (int, error) {
	n, readErr := s.source.Read(p)
	if n > 0 {
		s.detector.Inspect(p[:n])
		if detectErr := s.detector.Err(); detectErr != nil {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return 0, errors.Join(detectErr, readErr)
			}
			return 0, detectErr
		}
	}
	return n, readErr
}

func (s *phpSerializationGuardSource) Stat() (os.FileInfo, error) {
	return s.source.Stat()
}

// rejectPossiblePHPSerialization performs a bounded preflight for the legacy
// file-transform pipelines, which require *os.File and therefore cannot use
// phpSerializationGuardSource directly. Those pipelines are not used by the
// current application service, but the dormant legacy callers receive the same
// fail-closed preflight for recognizable native markers. Each legacy file
// pipeline also wraps its actual transform stream, closing the mutation gap
// between this scan and the second pass. The active atomic pipeline above
// avoids this extra clean-file pass by inspecting its one transform stream.
func rejectPossiblePHPSerialization(ctx context.Context, source io.ReadSeeker) error {
	if source == nil {
		return errors.New("PHP serialization preflight source is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var detector phpSerializationDetector
	buffer := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			detector.Inspect(buffer[:n])
			if detectErr := detector.Err(); detectErr != nil {
				return detectErr
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("restart source after PHP serialization preflight: %w", err)
	}
	return ctx.Err()
}
