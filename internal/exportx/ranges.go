package exportx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
)

// ExportByteRanges concatenates validated, ordered, non-overlapping source
// ranges into one atomically published output. It exists for noncontiguous SQL
// table blocks; bytes in the gaps are deliberately not copied.
func ExportByteRanges(ctx context.Context, doc document.ReaderAtSize, sourcePath, outputPath string, ranges [][2]int64, opts Options) (_ Summary, retErr error) {
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
	if shouldWriteExportManifest(opts) {
		return Summary{}, errors.New("multi-range export manifest is unsupported without explicit range metadata")
	}
	if len(ranges) == 0 {
		return Summary{}, errors.New("at least one byte range is required")
	}
	size := doc.Size()
	var total int64
	previousEnd := int64(-1)
	for index, current := range ranges {
		start, end := current[0], current[1]
		if start < 0 || end <= start || end > size {
			return Summary{}, fmt.Errorf("invalid byte range %d [%d,%d) for source size %d", index, start, end, size)
		}
		if previousEnd > start {
			return Summary{}, fmt.Errorf("byte range %d overlaps or is out of order", index)
		}
		length := end - start
		if total > math.MaxInt64-length {
			return Summary{}, errors.New("byte range total overflows int64")
		}
		total += length
		previousEnd = end
	}
	summary := Summary{
		SourcePath:  sourcePath,
		OutputPath:  outputPath,
		StartOffset: ranges[0][0],
		EndOffset:   ranges[len(ranges)-1][1],
		Mode:        "byte-ranges",
	}
	if opts.ComputeSHA256 {
		summary.ChecksumAlgorithm = "sha256"
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	destination, err := openCreatedOutput(outputPath, sourcePath)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, destination.Cleanup()) }()
	readSource, commitValidation, err := prepareExactExportSource(ctx, doc, opts.ValidateSource)
	if err != nil {
		return summary, err
	}
	if err := validateExportSource(ctx, doc, nil); err != nil {
		return summary, err
	}

	var checksum hashWriter
	if opts.ComputeSHA256 {
		checksum = sha256.New()
	}
	var written int64
	for _, current := range ranges {
		base := written
		part, copyErr := copyExactRawRange(ctx, readSource, current[0], current[1]-current[0], destination, func(done, _ int64) {
			if opts.Progress != nil {
				opts.Progress(base+done, total)
			}
		}, checksum)
		written += part
		summary.BytesWritten = written
		if copyErr != nil {
			return summary, copyErr
		}
	}
	if written != total {
		return summary, fmt.Errorf("multi-range export wrote %d of %d bytes", written, total)
	}
	if checksum != nil {
		summary.SHA256 = hex.EncodeToString(checksum.Sum(nil))
	}
	if err := commitExportOutput(ctx, destination, doc, commitValidation); err != nil {
		markUncertainPublication(&summary, err)
		return summary, err
	}
	return summary, nil
}
