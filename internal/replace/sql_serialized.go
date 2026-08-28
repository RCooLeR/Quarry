package replace

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/quarry/quarry-wails3/internal/asciifold"
	"github.com/quarry/quarry-wails3/internal/fileio"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

const maxSQLSerializedLiteralBytes = 32 * 1024 * 1024

var ErrUnsupportedPHPSerializedData = errors.New("replacement stopped: unsupported or malformed PHP/WordPress serialized data; no output was published")
var ErrUnsupportedSQLReplaceContext = errors.New("replacement stopped: unsupported SQL dump context detected; routine bodies, custom delimiters, and nonstandard string escape modes are not rewritten; no output was published")

// ReplaceSQLPlainFileAtomic streams a SQL dump to a separate output while
// applying a plain replacement to SQL string literal values only. Comments,
// identifiers, routine bodies, custom-delimiter scripts, and raw/opaque payload
// modes are deliberately not rewritten. SQL string literals are decoded before
// matching. If a literal is a PHP serialized value, string/class/custom byte
// lengths are recalculated before the literal is SQL-escaped again.
func ReplaceSQLPlainFileAtomic(ctx context.Context, sourcePath string, outputPath string, find []byte, replacement []byte, opts FileOptions, plainOpts BatchOptions) (summary FileSummary, retErr error) {
	if opts.SwapOriginal {
		return FileSummary{}, ErrSwapOriginalDisabled
	}
	rules := []BatchRule{{Name: "find-replace", Find: find, Replace: replacement}}
	if _, err := validateBatchRules(rules); err != nil {
		return FileSummary{OutputPath: outputPath}, err
	}
	if _, err := batchWriteBufferSize(plainOpts.WriteBufferSize); err != nil {
		return FileSummary{OutputPath: outputPath}, err
	}
	if _, err := normalizedPlainChunkSize(plainOpts.ChunkSize, 64*1024*1024, 1); err != nil {
		return FileSummary{OutputPath: outputPath}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return FileSummary{}, err
	}
	summary.OutputPath = outputPath

	source, err := sourceio.OpenContext(ctx, sourcePath, opts.ExpectedSource)
	if err != nil {
		return summary, err
	}
	src := readStatSource(source)
	defer func() {
		retErr = errors.Join(retErr, source.Close())
	}()
	before, err := src.Stat()
	if err != nil {
		return summary, err
	}
	if !before.Mode().IsRegular() {
		return summary, errors.New("source must be a regular file")
	}

	out, err := fileio.OpenAtomicOutput(outputPath, []string{sourcePath}, 0o600)
	if err != nil {
		return summary, err
	}
	summary.TempPath = out.TempPath()
	defer func() {
		retErr = errors.Join(retErr, cleanupAtomicBatchOutput(&summary, out.Cleanup))
	}()

	writer := &exactSyncWriter{dst: out}
	processed := int64(0)
	progress := func(p Progress) {
		processed = p.BytesProcessed
		if opts.Progress != nil {
			opts.Progress(p)
		}
	}
	matches, err := replaceSQLPlain(ctx, src, writer, find, replacement, plainOpts, progress)
	summary.Matches = matches
	summary.BytesWritten = writer.written
	if err != nil {
		if errors.Is(err, sourceio.ErrSourceChanged) {
			return summary, errors.Join(ErrSourceModifiedDuringOperation, err)
		}
		return summary, err
	}
	if processed != before.Size() {
		return summary, fmt.Errorf("%w: processed %d of %d source bytes", ErrSourceModifiedDuringOperation, processed, before.Size())
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	summary.Complete = true
	validateSource := func(validationCtx context.Context) error {
		err := source.ValidateContext(validationCtx)
		if errors.Is(err, sourceio.ErrSourceChanged) {
			return errors.Join(ErrSourceModifiedDuringOperation, err)
		}
		return err
	}
	if err := out.CommitContextValidated(ctx, validateSource); err != nil {
		if publication, ok := errors.AsType[*fileio.PublicationError](err); ok {
			summary.Published = true
			summary.PublicationUncertain = publication.LocationUncertain
		}
		return summary, err
	}
	summary.TempPath = ""
	summary.Published = true
	return summary, nil
}

func replaceSQLPlain(ctx context.Context, src readStatSource, dst syncWriter, find []byte, replacement []byte, opts BatchOptions, progress func(Progress)) (int64, error) {
	if len(find) == 0 {
		return 0, errors.New("empty pattern")
	}
	st, err := src.Stat()
	if err != nil {
		return 0, err
	}
	total := st.Size()
	bufferedDst := bufio.NewWriterSize(dst, firstPositive(opts.WriteBufferSize, plainReplaceWriteBufferSize))
	reader := bufio.NewReaderSize(src, firstPositive(opts.ChunkSize, 64*1024))
	outside := make([]byte, 0, 64*1024)
	contextDetector := sqlReplaceContextDetector{}
	var processed int64
	var matches int64

	flushOutside := func() error {
		if len(outside) == 0 {
			return nil
		}
		if contextDetector.Inspect(outside) {
			return ErrUnsupportedSQLReplaceContext
		}
		_, err := bufferedDst.Write(outside)
		outside = outside[:0]
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			return matches, err
		}
		b, readErr := reader.ReadByte()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if err := flushOutside(); err != nil {
					return matches, err
				}
				if progress != nil {
					progress(Progress{BytesProcessed: processed, BytesTotal: total, Matches: matches})
				}
				if err := bufferedDst.Flush(); err != nil {
					return matches, err
				}
				return matches, dst.Sync()
			}
			return matches, readErr
		}
		processed++
		if b != '\'' {
			outside = append(outside, b)
			if len(outside) >= 64*1024 {
				if err := flushOutside(); err != nil {
					return matches, err
				}
			}
			if progress != nil && processed%(4*1024*1024) == 0 {
				progress(Progress{BytesProcessed: processed, BytesTotal: total, Matches: matches})
			}
			continue
		}
		if err := flushOutside(); err != nil {
			return matches, err
		}
		literal, consumed, err := readSQLSingleQuotedLiteral(reader)
		processed += consumed
		if err != nil {
			return matches, err
		}
		next, count, err := replaceSQLLiteralDecoded(literal, find, replacement, opts)
		if err != nil {
			return matches, err
		}
		matches += int64(count)
		if _, err := bufferedDst.Write(appendSQLSingleQuotedLiteral(nil, next)); err != nil {
			return matches, err
		}
		if progress != nil {
			progress(Progress{BytesProcessed: processed, BytesTotal: total, Matches: matches})
		}
	}
}

func readSQLSingleQuotedLiteral(r *bufio.Reader) ([]byte, int64, error) {
	var out []byte
	var consumed int64
	for {
		b, err := r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, consumed, io.ErrUnexpectedEOF
			}
			return nil, consumed, err
		}
		consumed++
		if b == '\'' {
			next, err := r.Peek(1)
			if err == nil && next[0] == '\'' {
				_, _ = r.ReadByte()
				consumed++
				out = append(out, '\'')
				continue
			}
			return out, consumed, nil
		}
		if b == '\\' {
			esc, err := r.ReadByte()
			if err != nil {
				return nil, consumed, err
			}
			consumed++
			out = append(out, decodeSQLBackslashEscape(esc))
		} else {
			out = append(out, b)
		}
		if len(out) > maxSQLSerializedLiteralBytes {
			return nil, consumed, replaceLimit("SQL string literal exceeds %d bytes", maxSQLSerializedLiteralBytes)
		}
	}
}

func decodeSQLBackslashEscape(b byte) byte {
	switch b {
	case '0':
		return 0
	case 'b':
		return '\b'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	case 'Z':
		return 26
	default:
		return b
	}
}

func appendSQLSingleQuotedLiteral(dst []byte, value []byte) []byte {
	dst = append(dst, '\'')
	for _, b := range value {
		switch b {
		case 0:
			dst = append(dst, '\\', '0')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\'':
			dst = append(dst, '\'', '\'')
		case 26:
			dst = append(dst, '\\', 'Z')
		default:
			dst = append(dst, b)
		}
	}
	dst = append(dst, '\'')
	return dst
}

func replaceSQLLiteralDecoded(value []byte, find []byte, replacement []byte, opts BatchOptions) ([]byte, int, error) {
	if bytes.Contains(bytes.ToUpper(value), []byte("NO_BACKSLASH_ESCAPES")) {
		return nil, 0, ErrUnsupportedSQLReplaceContext
	}
	if !bytesWouldReplace(value, find, opts) {
		return value, 0, nil
	}
	if !containsPHPSerialization(value) {
		if encodedPHPSerializationHazard(value, find, opts) {
			return nil, 0, ErrUnsupportedPHPSerializedData
		}
		return replaceBytesOnce(value, find, replacement, opts)
	}
	parser := phpSerializedRewriter{
		input:           value,
		find:            find,
		replacement:     replacement,
		caseInsensitive: opts.CaseInsensitive,
		wholeWord:       opts.WholeWord,
	}
	out, err := parser.parseValue()
	if err != nil {
		return nil, 0, err
	}
	if parser.pos != len(value) {
		return nil, 0, ErrUnsupportedPHPSerializedData
	}
	return out, parser.matches, nil
}

func containsPHPSerialization(value []byte) bool {
	var detector phpSerializationDetector
	detector.Inspect(value)
	return detector.Err() != nil
}

func encodedPHPSerializationHazard(value []byte, find []byte, opts BatchOptions) bool {
	if !bytesWouldReplace(value, find, opts) {
		return false
	}
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) < 8 {
		return false
	}
	if decoded, ok := decodeLikelyHex(trimmed); ok && containsPHPSerialization(decoded) {
		return true
	}
	if decoded, ok := decodeLikelyBase64(trimmed); ok && containsPHPSerialization(decoded) {
		return true
	}
	return false
}

func decodeLikelyHex(value []byte) ([]byte, bool) {
	s := string(value)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		s = s[2:]
	}
	if len(s) < 8 || len(s)%2 != 0 {
		return nil, false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return nil, false
		}
	}
	decoded, err := hex.DecodeString(s)
	return decoded, err == nil
}

func decodeLikelyBase64(value []byte) ([]byte, bool) {
	s := string(value)
	if len(s) < 12 || len(s)%4 != 0 {
		return nil, false
	}
	for _, r := range s {
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=') {
			return nil, false
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	return decoded, err == nil
}

func bytesWouldReplace(value []byte, find []byte, opts BatchOptions) bool {
	needle := find
	if opts.CaseInsensitive {
		needle = asciifold.Fold(find)
	}
	return indexPlain(value, needle, opts.CaseInsensitive) >= 0
}

type sqlReplaceContextDetector struct {
	tail string
}

var unsupportedSQLReplaceContextMarkers = []string{
	"DELIMITER",
	"NO_BACKSLASH_ESCAPES",
	"CREATE PROCEDURE",
	"CREATE FUNCTION",
	"CREATE TRIGGER",
	"CREATE EVENT",
	"CREATE OR REPLACE PROCEDURE",
	"CREATE OR REPLACE FUNCTION",
	"CREATE OR REPLACE TRIGGER",
	"CREATE OR REPLACE EVENT",
	"CREATE DEFINER",
	"DO $$",
	"DO $",
	"COPY ",
	"\\COPY ",
	"SET TERM",
	"\nGO\n",
	"\r\nGO\r\n",
}

func (d *sqlReplaceContextDetector) Inspect(chunk []byte) bool {
	if len(chunk) == 0 {
		return false
	}
	text := strings.ToUpper(d.tail + string(chunk))
	for _, marker := range unsupportedSQLReplaceContextMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	const maxTail = 96
	if len(text) > maxTail {
		d.tail = text[len(text)-maxTail:]
	} else {
		d.tail = text
	}
	return false
}

func replaceBytesOnce(value []byte, find []byte, replacement []byte, opts BatchOptions) ([]byte, int, error) {
	var out bytes.Buffer
	needle := find
	if opts.CaseInsensitive {
		needle = asciifold.Fold(find)
	}
	_, count, err := writeReplacedPrefix(&out, value, len(value), needle, find, replacement, opts.CaseInsensitive, opts.WholeWord, 0, int64(len(value)))
	return out.Bytes(), count, err
}

type phpSerializedRewriter struct {
	input           []byte
	pos             int
	find            []byte
	replacement     []byte
	caseInsensitive bool
	wholeWord       bool
	matches         int
}

func (p *phpSerializedRewriter) parseValue() ([]byte, error) {
	if p.pos >= len(p.input) {
		return nil, ErrUnsupportedPHPSerializedData
	}
	switch p.input[p.pos] {
	case 'N':
		return p.copyUntilSemicolon()
	case 'b', 'i', 'd', 'r', 'R':
		return p.copyScalar()
	case 's', 'S', 'E':
		return p.parseByteString(p.input[p.pos])
	case 'a':
		return p.parseArray()
	case 'O':
		return p.parseObject()
	case 'C':
		return p.parseCustom()
	default:
		return nil, ErrUnsupportedPHPSerializedData
	}
}

func (p *phpSerializedRewriter) copyUntilSemicolon() ([]byte, error) {
	start := p.pos
	for p.pos < len(p.input) && p.input[p.pos] != ';' {
		p.pos++
	}
	if p.pos >= len(p.input) {
		return nil, ErrUnsupportedPHPSerializedData
	}
	p.pos++
	return append([]byte(nil), p.input[start:p.pos]...), nil
}

func (p *phpSerializedRewriter) copyScalar() ([]byte, error) {
	start := p.pos
	if p.pos+2 >= len(p.input) || p.input[p.pos+1] != ':' {
		return nil, ErrUnsupportedPHPSerializedData
	}
	p.pos += 2
	for p.pos < len(p.input) && p.input[p.pos] != ';' {
		p.pos++
	}
	if p.pos >= len(p.input) {
		return nil, ErrUnsupportedPHPSerializedData
	}
	p.pos++
	return append([]byte(nil), p.input[start:p.pos]...), nil
}

func (p *phpSerializedRewriter) parseByteString(kind byte) ([]byte, error) {
	p.pos++
	length, err := p.readLengthColon()
	if err != nil {
		return nil, err
	}
	if !p.consume('"') || length < 0 {
		return nil, ErrUnsupportedPHPSerializedData
	}
	start := p.pos
	end := start + length
	if end+2 > len(p.input) || p.input[end] != '"' || p.input[end+1] != ';' {
		var ok bool
		end, ok = findNestedPHPSerializedStringTerminator(p.input, start)
		if !ok {
			end, ok = findPHPSerializedStringTerminator(p.input, start)
		}
		if !ok {
			return nil, ErrUnsupportedPHPSerializedData
		}
	}
	value := p.input[start:end]
	p.pos = end + 2
	next, count, err := p.rewriteSerializedPayloadBytes(value)
	if err != nil {
		return nil, err
	}
	p.matches += count
	out := []byte{kind, ':'}
	out = strconv.AppendInt(out, int64(len(next)), 10)
	out = append(out, ':', '"')
	out = append(out, next...)
	out = append(out, '"', ';')
	return out, nil
}

func findNestedPHPSerializedStringTerminator(input []byte, start int) (int, bool) {
	if !startsPHPSerializedValue(input, start) {
		return 0, false
	}
	nested := phpSerializedRewriter{input: input[start:]}
	if _, err := nested.parseValue(); err != nil {
		return 0, false
	}
	end := start + nested.pos
	if end+2 <= len(input) && input[end] == '"' && input[end+1] == ';' {
		return end, true
	}
	return 0, false
}

func startsPHPSerializedValue(input []byte, start int) bool {
	if start >= len(input) {
		return false
	}
	switch input[start] {
	case 'N':
		return start+1 < len(input) && input[start+1] == ';'
	case 'b', 'i', 'd', 'r', 'R', 's', 'S', 'E', 'a', 'O', 'C':
		return start+1 < len(input) && input[start+1] == ':'
	default:
		return false
	}
}

func findPHPSerializedStringTerminator(input []byte, start int) (int, bool) {
	for pos := start; pos+1 < len(input); pos++ {
		if input[pos] != '"' || input[pos+1] != ';' {
			continue
		}
		if pos+2 >= len(input) {
			return pos, true
		}
		switch input[pos+2] {
		case '}', 'N', 'b', 'i', 'd', 'r', 'R', 's', 'S', 'E', 'a', 'O', 'C':
			return pos, true
		}
	}
	return 0, false
}

func (p *phpSerializedRewriter) parseArray() ([]byte, error) {
	p.pos++
	count, err := p.readLengthColon()
	if err != nil || !p.consume('{') {
		return nil, ErrUnsupportedPHPSerializedData
	}
	out := []byte{'a', ':'}
	out = strconv.AppendInt(out, int64(count), 10)
	out = append(out, ':', '{')
	for i := 0; i < count*2; i++ {
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		out = append(out, value...)
	}
	if !p.consume('}') {
		return nil, ErrUnsupportedPHPSerializedData
	}
	out = append(out, '}')
	return out, nil
}

func (p *phpSerializedRewriter) parseObject() ([]byte, error) {
	p.pos++
	class, err := p.readLengthDelimitedBytes()
	if err != nil {
		return nil, err
	}
	props, err := p.readLengthTerminatedColon()
	if err != nil || !p.consume('{') {
		return nil, ErrUnsupportedPHPSerializedData
	}
	nextClass, count, err := p.rewritePlainMetadataBytes(class)
	if err != nil {
		return nil, err
	}
	p.matches += count
	out := []byte{'O', ':'}
	out = strconv.AppendInt(out, int64(len(nextClass)), 10)
	out = append(out, ':', '"')
	out = append(out, nextClass...)
	out = append(out, '"', ':')
	out = strconv.AppendInt(out, int64(props), 10)
	out = append(out, ':', '{')
	for i := 0; i < props*2; i++ {
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		out = append(out, value...)
	}
	if !p.consume('}') {
		return nil, ErrUnsupportedPHPSerializedData
	}
	out = append(out, '}')
	return out, nil
}

func (p *phpSerializedRewriter) parseCustom() ([]byte, error) {
	p.pos++
	class, err := p.readLengthDelimitedBytes()
	if err != nil {
		return nil, err
	}
	payloadLength, err := p.readLengthTerminatedColon()
	if err != nil || !p.consume('{') || p.pos+payloadLength+1 > len(p.input) {
		return nil, ErrUnsupportedPHPSerializedData
	}
	payload := p.input[p.pos : p.pos+payloadLength]
	p.pos += payloadLength
	if !p.consume('}') {
		return nil, ErrUnsupportedPHPSerializedData
	}
	nextClass, classCount, err := p.rewritePlainMetadataBytes(class)
	if err != nil {
		return nil, err
	}
	nextPayload, payloadCount, err := p.rewriteSerializedPayloadBytes(payload)
	if err != nil {
		return nil, err
	}
	p.matches += classCount + payloadCount
	out := []byte{'C', ':'}
	out = strconv.AppendInt(out, int64(len(nextClass)), 10)
	out = append(out, ':', '"')
	out = append(out, nextClass...)
	out = append(out, '"', ':')
	out = strconv.AppendInt(out, int64(len(nextPayload)), 10)
	out = append(out, ':', '{')
	out = append(out, nextPayload...)
	out = append(out, '}')
	return out, nil
}

func (p *phpSerializedRewriter) rewriteSerializedPayloadBytes(value []byte) ([]byte, int, error) {
	if len(p.find) == 0 {
		return append([]byte(nil), value...), 0, nil
	}
	opts := BatchOptions{CaseInsensitive: p.caseInsensitive, WholeWord: p.wholeWord}
	if !bytesWouldReplace(value, p.find, opts) {
		return append([]byte(nil), value...), 0, nil
	}
	if !containsPHPSerialization(value) {
		return replaceBytesOnce(value, p.find, p.replacement, opts)
	}
	nested := phpSerializedRewriter{
		input:           value,
		find:            p.find,
		replacement:     p.replacement,
		caseInsensitive: p.caseInsensitive,
		wholeWord:       p.wholeWord,
	}
	out, err := nested.parseValue()
	if err != nil || nested.pos != len(value) {
		return nil, 0, ErrUnsupportedPHPSerializedData
	}
	return out, nested.matches, nil
}

func (p *phpSerializedRewriter) rewritePlainMetadataBytes(value []byte) ([]byte, int, error) {
	if len(p.find) == 0 {
		return append([]byte(nil), value...), 0, nil
	}
	return replaceBytesOnce(value, p.find, p.replacement, BatchOptions{CaseInsensitive: p.caseInsensitive, WholeWord: p.wholeWord})
}

func (p *phpSerializedRewriter) readLengthDelimitedBytes() ([]byte, error) {
	length, err := p.readLengthColon()
	if err != nil || !p.consume('"') || length < 0 {
		return nil, ErrUnsupportedPHPSerializedData
	}
	start := p.pos
	end := start + length
	if end+2 > len(p.input) || p.input[end] != '"' || p.input[end+1] != ':' {
		var ok bool
		end, ok = findPHPSerializedDelimitedBytesTerminator(p.input, start, ':')
		if !ok {
			return nil, ErrUnsupportedPHPSerializedData
		}
	}
	value := p.input[start:end]
	p.pos = end + 2
	return value, nil
}

func findPHPSerializedDelimitedBytesTerminator(input []byte, start int, delimiter byte) (int, bool) {
	for pos := start; pos+1 < len(input); pos++ {
		if input[pos] == '"' && input[pos+1] == delimiter {
			return pos, true
		}
	}
	return 0, false
}

func (p *phpSerializedRewriter) readLengthColon() (int, error) {
	if !p.consume(':') {
		return 0, ErrUnsupportedPHPSerializedData
	}
	return p.readLengthTerminatedColon()
}

func (p *phpSerializedRewriter) readLengthTerminatedColon() (int, error) {
	start := p.pos
	for p.pos < len(p.input) && p.input[p.pos] >= '0' && p.input[p.pos] <= '9' {
		p.pos++
	}
	if start == p.pos || !p.consume(':') {
		return 0, ErrUnsupportedPHPSerializedData
	}
	n, err := strconv.Atoi(string(p.input[start : p.pos-1]))
	if err != nil {
		return 0, ErrUnsupportedPHPSerializedData
	}
	return n, nil
}

func (p *phpSerializedRewriter) consume(want byte) bool {
	if p.pos >= len(p.input) || p.input[p.pos] != want {
		return false
	}
	p.pos++
	return true
}
