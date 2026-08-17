package exportx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/encodingx"
	"github.com/quarry/quarry-wails3/internal/fileio"
)

// ExportByteRangeText exports only a character-aligned, valid text range. It
// rejects unaligned byte endpoints instead of expanding them implicitly. Use
// ExportByteRange when an exact arbitrary byte fragment is intended.
func ExportByteRangeText(ctx context.Context, doc document.ReaderAtSize, sourcePath string, outputPath string, start int64, end int64, sourceEncoding string, targetEncoding string, opts Options) (Summary, error) {
	return exportByteRangeTextCore(ctx, doc, sourcePath, outputPath, start, end, sourceEncoding, targetEncoding, "byte-range-text", 0, 0, false, opts, nil)
}

// ExportVisibleRangeText exports a decoded visible byte range. Like every text
// range export, it rejects rather than expands endpoints that split an encoded
// character. Call ExportVisibleRange for an intentionally byte-exact fragment.
func ExportVisibleRangeText(ctx context.Context, doc document.ReaderAtSize, sourcePath string, outputPath string, start int64, end int64, sourceEncoding string, targetEncoding string, opts Options) (Summary, error) {
	return exportByteRangeTextCore(ctx, doc, sourcePath, outputPath, start, end, sourceEncoding, targetEncoding, "visible-range-text", 0, 0, false, opts, nil)
}

// ExportLineRangeText validates the byte offsets produced by the line mapper
// against source encoding boundaries before publishing text.
func ExportLineRangeText(ctx context.Context, doc *document.FileDocument, sourcePath string, outputPath string, startLine int64, endLine int64, sourceEncoding string, targetEncoding string, opts Options) (Summary, error) {
	if doc == nil {
		return Summary{}, errors.New("document is required")
	}
	if startLine <= 0 || endLine <= 0 {
		return Summary{}, errors.New("line numbers must be positive")
	}
	if endLine < startLine {
		return Summary{}, errors.New("end line must be greater than or equal to start line")
	}
	encoding := doc.Metadata().Encoding
	resolve := func(resolveCtx context.Context, reader document.ReaderAtSize) (int64, int64, error) {
		return exactLineRangeOffsets(resolveCtx, reader, encoding, startLine, endLine)
	}
	return exportByteRangeTextCore(ctx, doc, sourcePath, outputPath, 0, 0, sourceEncoding, targetEncoding, "line-range-text", startLine, endLine, true, opts, resolve)
}

func exportByteRangeTextCore(ctx context.Context, doc document.ReaderAtSize, sourcePath string, outputPath string, start int64, end int64, sourceEncoding string, targetEncoding string, mode string, startLine int64, endLine int64, usedLineRange bool, opts Options, resolve exportRangeResolver) (_ Summary, retErr error) {
	if doc == nil {
		return Summary{}, errors.New("document is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if outputPath == "" {
		return Summary{}, errors.New("output path is required")
	}
	if err := fileio.ValidateExactOutputPath(outputPath); err != nil {
		return Summary{}, err
	}
	size := doc.Size()
	if resolve == nil {
		if start < 0 {
			start = 0
		}
		if end <= 0 || end > size {
			end = size
		}
		if end < start {
			return Summary{}, errors.New("end offset must be greater than or equal to start offset")
		}
	}
	sourceEncoding = strings.TrimSpace(sourceEncoding)
	if sourceEncoding == "" {
		sourceEncoding = "UTF-8"
	}
	targetEncoding = strings.TrimSpace(targetEncoding)
	if targetEncoding == "" {
		targetEncoding = sourceEncoding
	}

	sourceKind, err := parseTextEncoding(sourceEncoding)
	if err != nil {
		return Summary{}, err
	}
	if _, err := parseTextEncoding(targetEncoding); err != nil {
		return Summary{}, err
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	summary := Summary{
		SourcePath:     sourcePath,
		OutputPath:     outputPath,
		StartOffset:    start,
		EndOffset:      end,
		StartLine:      startLine,
		EndLine:        endLine,
		Mode:           mode,
		UsedLineRange:  usedLineRange,
		SourceEncoding: sourceEncoding,
		TargetEncoding: targetEncoding,
	}
	if opts.ComputeSHA256 {
		summary.ChecksumAlgorithm = "sha256"
	}
	if shouldWriteExportManifest(opts) {
		summary.ManifestPath = exportManifestPathFor(outputPath, opts.ManifestPath)
		if err := ensureExportManifestAvailable(summary.ManifestPath, sourcePath, outputPath); err != nil {
			return summary, err
		}
	}

	if err := ctx.Err(); err != nil {
		return summary, err
	}
	dst, err := openCreatedOutput(outputPath, sourcePath)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, dst.Cleanup()) }()
	readSource, commitValidation, err := prepareExactExportSource(ctx, doc, opts.ValidateSource)
	if err != nil {
		return summary, err
	}
	if resolve != nil {
		start, end, err = resolve(ctx, readSource)
		if err != nil {
			return summary, err
		}
		if start < 0 || end < start || end > readSource.Size() {
			return summary, errors.New("resolved export range is outside the source")
		}
	}
	start, end, err = prepareTextRange(readSource, start, end, sourceKind)
	if err != nil {
		return summary, err
	}
	summary.StartOffset = start
	summary.EndOffset = end
	total := end - start
	if total < 0 {
		total = 0
	}
	if err := validateExportSource(ctx, doc, nil); err != nil {
		return summary, err
	}

	reader := io.NewSectionReader(readSource, start, total)
	progressReader := &progressRangeReader{
		reader: reader,
		total:  total,
		report: opts.Progress,
	}
	validatedReader := newValidatingTextReader(progressReader, sourceKind, start)
	decodedReader, err := encodingx.NewDecoderReader(sourceEncoding, validatedReader)
	if err != nil {
		return summary, err
	}

	var checksum hashWriter
	if opts.ComputeSHA256 {
		checksum = sha256.New()
	}
	countedDst := &countingWriter{w: dst, checksum: checksum}
	if bom := targetBOM(targetEncoding); len(bom) > 0 {
		if n, err := countedDst.Write(bom); err != nil {
			return summary, err
		} else if n != len(bom) {
			return summary, io.ErrShortWrite
		}
	}
	encodedWriter, err := encodingx.NewEncoderWriter(targetEncoding, countedDst)
	if err != nil {
		return summary, err
	}

	if err := copyTextRange(ctx, decodedReader, encodedWriter); err != nil {
		return summary, err
	}
	if progressReader.done != total {
		return summary, io.ErrUnexpectedEOF
	}
	if closer, ok := encodedWriter.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			return summary, err
		}
	}
	summary.BytesWritten = countedDst.written
	if checksum != nil {
		summary.SHA256 = hex.EncodeToString(checksum.Sum(nil))
	}
	if err := commitExportOutput(ctx, dst, doc, commitValidation); err != nil {
		markUncertainPublication(&summary, err)
		return summary, err
	}
	if shouldWriteExportManifest(opts) {
		if err := writeExportManifest(ctx, summary, func(validateCtx context.Context) error {
			return validateExportSource(validateCtx, doc, commitValidation)
		}); err != nil {
			return summary, exportManifestPublicationError(summary, err)
		}
	}
	return summary, nil
}

type progressRangeReader struct {
	reader io.Reader
	total  int64
	done   int64
	report func(done int64, total int64)
}

func (p *progressRangeReader) Read(buf []byte) (int, error) {
	n, err := p.reader.Read(buf)
	if n > 0 {
		p.done += int64(n)
		if p.report != nil {
			p.report(p.done, p.total)
		}
	}
	return n, err
}

type countingWriter struct {
	w        io.Writer
	checksum hashWriter
	written  int64
}

func (c *countingWriter) Write(buf []byte) (int, error) {
	n, err := c.w.Write(buf)
	c.written += int64(n)
	if c.checksum != nil && n > 0 {
		_, _ = c.checksum.Write(buf[:n])
	}
	return n, err
}

func copyTextRange(ctx context.Context, src io.Reader, dst io.Writer) error {
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			written, err := dst.Write(buf[:n])
			if err != nil {
				return err
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}

func targetBOM(encodingName string) []byte {
	switch strings.ToUpper(strings.TrimSpace(encodingName)) {
	case "UTF-16LE", "UTF-16BE":
		return encodingx.BOMBytes(encodingName)
	default:
		return nil
	}
}
