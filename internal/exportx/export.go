package exportx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
)

type Options struct {
	Progress      func(done int64, total int64)
	ComputeSHA256 bool
	WriteManifest bool
	ManifestPath  string
	// ValidateSource is an optional operation-level generation check (for
	// example, a cached SQL analysis lease). It runs after output fsync and
	// immediately before publication, followed by the retained document's own
	// validation when that capability is available. When a completion manifest
	// is requested, the same checks run again immediately before the manifest is
	// published so it cannot claim success for a subsequently stale operation.
	ValidateSource func(context.Context) error
}

type Summary struct {
	SourcePath        string
	OutputPath        string
	ManifestPath      string
	StartOffset       int64
	EndOffset         int64
	BytesWritten      int64
	StartLine         int64
	EndLine           int64
	Mode              string
	UsedLineRange     bool
	SourceEncoding    string
	TargetEncoding    string
	ChecksumAlgorithm string
	SHA256            string
	// PublicationUncertain is true only when a handle-owned complete artifact
	// may remain after exact-path drift and rollback could not be confirmed.
	PublicationUncertain bool `json:",omitempty"`
}

func ExportByteRange(ctx context.Context, doc document.ReaderAtSize, sourcePath string, outputPath string, start int64, end int64, opts Options) (Summary, error) {
	return exportByteRangeCore(ctx, doc, sourcePath, outputPath, start, end, "byte-range", 0, 0, false, opts, nil, nil)
}

func ExportVisibleRange(ctx context.Context, doc document.ReaderAtSize, sourcePath string, outputPath string, start int64, end int64, opts Options) (Summary, error) {
	return exportByteRangeCore(ctx, doc, sourcePath, outputPath, start, end, "visible-range", 0, 0, false, opts, nil, nil)
}

func ExportLineRange(ctx context.Context, doc *document.FileDocument, sourcePath string, outputPath string, startLine int64, endLine int64, opts Options) (Summary, error) {
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
	return exportByteRangeCore(ctx, doc, sourcePath, outputPath, 0, 0, "line-range", startLine, endLine, true, opts, nil, resolve)
}

type exportRangeResolver func(context.Context, document.ReaderAtSize) (int64, int64, error)

func exportByteRangeCore(ctx context.Context, doc document.ReaderAtSize, sourcePath string, outputPath string, start int64, end int64, mode string, startLine int64, endLine int64, usedLineRange bool, opts Options, prepared *preparedExportSource, resolve exportRangeResolver) (_ Summary, retErr error) {
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
	summary := Summary{
		SourcePath:    sourcePath,
		OutputPath:    outputPath,
		StartOffset:   start,
		EndOffset:     end,
		StartLine:     startLine,
		EndLine:       endLine,
		Mode:          mode,
		UsedLineRange: usedLineRange,
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
	var readSource document.ReaderAtSize
	var commitValidation func(context.Context) error
	if prepared != nil && prepared.reader != nil {
		readSource = prepared.reader
		// The prepared reader has already bound every returned byte to the
		// captured expectation. Per-part publication still runs the caller's
		// generation lease and the retained document's cheap mutation check;
		// the full digest is reserved for the completion manifest.
		commitValidation = opts.ValidateSource
	} else {
		readSource, commitValidation, err = prepareExactExportSource(ctx, doc, opts.ValidateSource)
		if err != nil {
			return summary, err
		}
		if prepared != nil {
			prepared.reader = readSource
			prepared.completionValidation = commitValidation
			commitValidation = opts.ValidateSource
		}
	}
	if resolve != nil {
		start, end, err = resolve(ctx, readSource)
		if err != nil {
			return summary, err
		}
		if start < 0 || end < start || end > readSource.Size() {
			return summary, errors.New("resolved export range is outside the source")
		}
		summary.StartOffset = start
		summary.EndOffset = end
	}
	if err := validateExportSource(ctx, doc, nil); err != nil {
		return summary, err
	}

	total := end - start
	var checksum hashWriter
	if opts.ComputeSHA256 {
		checksum = sha256.New()
	}
	written, err := copyExactRawRange(ctx, readSource, start, total, dst, opts.Progress, checksum)
	summary.BytesWritten = written
	if err != nil {
		return summary, err
	}
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

func copyExactRawRange(
	ctx context.Context,
	doc document.ReaderAtSize,
	start int64,
	total int64,
	dst io.Writer,
	progress func(done int64, total int64),
	checksum hashWriter,
) (int64, error) {
	reader := io.NewSectionReader(doc, start, total)
	buf := make([]byte, 1024*1024)
	var written int64
	for written < total {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := reader.Read(buf)
		if n > 0 {
			w, writeErr := dst.Write(buf[:n])
			written += int64(w)
			if checksum != nil && w > 0 {
				_, _ = checksum.Write(buf[:w])
			}
			if progress != nil {
				progress(written, total)
			}
			if writeErr != nil {
				return written, writeErr
			}
			if w != n {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return written, readErr
		}
		if written == total {
			break
		}
		if errors.Is(readErr, io.EOF) {
			return written, io.ErrUnexpectedEOF
		}
		if n == 0 {
			return written, io.ErrNoProgress
		}
	}
	if written != total {
		return written, io.ErrUnexpectedEOF
	}
	return written, nil
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}
