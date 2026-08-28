package exportx

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
)

type SplitOptions struct {
	Progress      func(done int64, total int64, completedParts int)
	ComputeSHA256 bool
	ManifestPath  string
	// ValidateSource runs immediately before each part is published and again
	// before the completion manifest. A retained document validation follows it
	// at every publication point.
	ValidateSource func(context.Context) error
}

// Each path in Outputs was individually completed and published before the
// function returned. Complete becomes true only after the manifest is also
// published. On failure Quarry preserves those parts, records the reason, and
// never attempts unsafe pathname rollback.
type SplitSummary struct {
	SourcePath           string
	BasePath             string
	Mode                 string
	Outputs              []string
	OutputChecksums      []OutputChecksum
	ManifestPath         string
	BytesWritten         int64
	Parts                int
	BytesPerPart         int64
	LinesPerPart         int64
	ChecksumAlgorithm    string
	Complete             bool
	Failure              string
	PublicationUncertain bool `json:",omitempty"`
}

// SplitIncompleteError reports that a split did not publish its complete
// manifest. Outputs lists the individually committed parts that Quarry leaves
// in place for explicit user recovery; it never rolls them back by pathname.
type SplitIncompleteError struct {
	Outputs      []string
	ManifestPath string
	Err          error
}

func (e *SplitIncompleteError) Error() string {
	return fmt.Sprintf("split incomplete; %d verified part(s) preserved and manifest %q is not confirmed: %v", len(e.Outputs), e.ManifestPath, e.Err)
}

func (e *SplitIncompleteError) Unwrap() error { return e.Err }

type OutputChecksum struct {
	Path   string
	SHA256 string
}

const defaultSplitManifestSuffix = ".quarry-split-manifest.json"

// A split retains one path and, optionally, one checksum per part until the
// manifest is published. Keep that operation metadata and the number of files
// created by one request predictably bounded.
const maxSplitParts int64 = 10_000

func checkedSplitPartCount(total int64, bytesPerPart int64) (int, error) {
	if total < 0 {
		return 0, errors.New("document size must not be negative")
	}
	if bytesPerPart <= 0 {
		return 0, errors.New("bytes per part must be positive")
	}
	if total == 0 {
		return 0, nil
	}

	count := total / bytesPerPart
	if total%bytesPerPart != 0 {
		count++
	}
	if count > maxSplitParts {
		return 0, fmt.Errorf("split would create %d parts; maximum is %d", count, maxSplitParts)
	}
	return int(count), nil
}

func checkedSplitPartRange(total int64, bytesPerPart int64, partIndex int) (int64, int64, error) {
	partCount, err := checkedSplitPartCount(total, bytesPerPart)
	if err != nil {
		return 0, 0, err
	}
	if partIndex < 0 || partIndex >= partCount {
		return 0, 0, errors.New("split part index is out of range")
	}

	start := int64(partIndex) * bytesPerPart
	remaining := total - start
	partLength := min(remaining, bytesPerPart)
	return start, start + partLength, nil
}

func SplitBySize(ctx context.Context, doc document.ReaderAtSize, sourcePath string, outputBasePath string, bytesPerPart int64, opts SplitOptions) (SplitSummary, error) {
	if doc == nil {
		return SplitSummary{}, errors.New("document is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if outputBasePath == "" {
		return SplitSummary{}, errors.New("output base path is required")
	}
	if err := fileio.ValidateExactOutputPath(outputBasePath); err != nil {
		return SplitSummary{}, err
	}
	if bytesPerPart <= 0 {
		return SplitSummary{}, errors.New("bytes per part must be positive")
	}

	total := doc.Size()
	partCount, err := checkedSplitPartCount(total, bytesPerPart)
	if err != nil {
		return SplitSummary{}, err
	}
	summary := SplitSummary{
		SourcePath:   sourcePath,
		BasePath:     outputBasePath,
		Mode:         "split-size",
		ManifestPath: splitManifestPathFor(outputBasePath, opts.ManifestPath),
		BytesPerPart: bytesPerPart,
	}
	if opts.ComputeSHA256 {
		summary.ChecksumAlgorithm = "sha256"
	}
	if err := ensureSplitManifestAvailable(summary.ManifestPath, sourcePath); err != nil {
		return failSplit(summary, err)
	}
	for i := 0; i < partCount; i++ {
		if err := ensureSplitManifestPartDistinct(summary.ManifestPath, partPath(outputBasePath, i+1)); err != nil {
			return failSplit(summary, err)
		}
	}
	if total == 0 {
		return finalizeSplit(ctx, doc, summary, opts.ValidateSource)
	}

	done := int64(0)
	prepared := &preparedExportSource{}
	for i := 0; i < partCount; i++ {
		start, end, err := checkedSplitPartRange(total, bytesPerPart, i)
		if err != nil {
			return failSplit(summary, err)
		}
		outputPath := partPath(outputBasePath, i+1)
		baseDone := done
		partSummary, err := exportByteRangeCore(ctx, doc, sourcePath, outputPath, start, end, "byte-range", 0, 0, false, Options{
			ComputeSHA256:  opts.ComputeSHA256,
			ValidateSource: opts.ValidateSource,
			Progress: func(written int64, partTotal int64) {
				if opts.Progress != nil {
					opts.Progress(baseDone+written, total, i)
				}
			},
		}, prepared, nil)
		if err != nil {
			if splitPartWasPublished(err, outputPath) {
				recordSplitPart(&summary, outputPath, partSummary, opts.ComputeSHA256)
			}
			return failSplit(summary, err)
		}
		recordSplitPart(&summary, outputPath, partSummary, opts.ComputeSHA256)
		done = summary.BytesWritten
		if opts.Progress != nil {
			opts.Progress(done, total, summary.Parts)
		}
	}

	return finalizeSplit(ctx, doc, summary, prepared.completionValidation)
}

func SplitByLineCount(ctx context.Context, doc *document.FileDocument, sourcePath string, outputBasePath string, linesPerPart int64, opts SplitOptions) (SplitSummary, error) {
	if doc == nil {
		return SplitSummary{}, errors.New("document is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if outputBasePath == "" {
		return SplitSummary{}, errors.New("output base path is required")
	}
	if err := fileio.ValidateExactOutputPath(outputBasePath); err != nil {
		return SplitSummary{}, err
	}
	if linesPerPart <= 0 {
		return SplitSummary{}, errors.New("lines per part must be positive")
	}

	total := doc.Size()
	summary := SplitSummary{
		SourcePath:   sourcePath,
		BasePath:     outputBasePath,
		Mode:         "split-lines",
		ManifestPath: splitManifestPathFor(outputBasePath, opts.ManifestPath),
		LinesPerPart: linesPerPart,
	}
	if opts.ComputeSHA256 {
		summary.ChecksumAlgorithm = "sha256"
	}
	if err := ensureSplitManifestAvailable(summary.ManifestPath, sourcePath); err != nil {
		return failSplit(summary, err)
	}
	if total == 0 {
		return finalizeSplit(ctx, doc, summary, opts.ValidateSource)
	}

	readSource, completionValidation, err := prepareExactExportSource(ctx, doc, opts.ValidateSource)
	if err != nil {
		return failSplit(summary, err)
	}
	ranges, err := exactLineSplitRanges(ctx, readSource, doc.Metadata().Encoding, linesPerPart, maxSplitParts)
	if err != nil {
		return failSplit(summary, err)
	}
	done := int64(0)
	prepared := &preparedExportSource{reader: readSource, completionValidation: completionValidation}
	for index, current := range ranges {
		part := index + 1
		if ctx.Err() != nil {
			return failSplit(summary, ctx.Err())
		}

		outputPath := partPath(outputBasePath, part)
		if err := ensureSplitManifestPartDistinct(summary.ManifestPath, outputPath); err != nil {
			return failSplit(summary, err)
		}
		baseDone := done
		partSummary, err := exportByteRangeCore(ctx, doc, sourcePath, outputPath, current[0], current[1], "byte-range", 0, 0, false, Options{
			ComputeSHA256:  opts.ComputeSHA256,
			ValidateSource: opts.ValidateSource,
			Progress: func(written int64, partTotal int64) {
				if opts.Progress != nil {
					opts.Progress(baseDone+written, total, part-1)
				}
			},
		}, prepared, nil)
		if err != nil {
			if splitPartWasPublished(err, outputPath) {
				recordSplitPart(&summary, outputPath, partSummary, opts.ComputeSHA256)
			}
			return failSplit(summary, err)
		}
		recordSplitPart(&summary, outputPath, partSummary, opts.ComputeSHA256)
		done = summary.BytesWritten
		if opts.Progress != nil {
			opts.Progress(done, total, summary.Parts)
		}

	}

	return finalizeSplit(ctx, doc, summary, completionValidation)
}

func splitManifestPathFor(basePath string, override string) string {
	if override != "" {
		return override
	}
	return basePath + defaultSplitManifestSuffix
}

func ensureSplitManifestAvailable(path string, protectedPaths ...string) error {
	if path == "" {
		return errors.New("split manifest path is required")
	}
	if err := fileio.ValidateExactOutputPath(path); err != nil {
		return err
	}
	for _, protectedPath := range protectedPaths {
		same, err := fileio.SamePath(path, protectedPath)
		if err != nil {
			return err
		}
		if same {
			return fmt.Errorf("%w: split manifest %s", fileio.ErrSourceAlias, path)
		}
	}
	return ensureManifestFinalAvailable("split", path)
}

func ensureSplitManifestPartDistinct(manifestPath string, partPath string) error {
	same, err := fileio.SamePath(manifestPath, partPath)
	if err != nil {
		return err
	}
	if same {
		return fmt.Errorf("%w: split manifest aliases part %s", fileio.ErrSourceAlias, partPath)
	}
	return nil
}

func writeSplitManifest(ctx context.Context, summary SplitSummary, validate func(context.Context) error) (retErr error) {
	if summary.ManifestPath == "" {
		return errors.New("split manifest path is required")
	}
	protectedPaths := make([]string, 0, len(summary.Outputs)+1)
	protectedPaths = append(protectedPaths, summary.SourcePath)
	protectedPaths = append(protectedPaths, summary.Outputs...)
	manifest, err := fileio.OpenAtomicOutput(summary.ManifestPath, protectedPaths, 0o600)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, manifest.Cleanup()) }()
	if err := writeManifestJSON(manifest, &summary); err != nil {
		return err
	}
	if validate != nil {
		return manifest.CommitContextValidated(ctx, validate)
	}
	return manifest.CommitContext(ctx)
}

func partPath(basePath string, part int) string {
	dir, base := filepath.Split(basePath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	label := fmt.Sprintf("%s.part%04d", name, part)
	if ext != "" {
		label += ext
	}
	return dir + label
}

func recordSplitPart(summary *SplitSummary, outputPath string, partSummary Summary, includeChecksum bool) {
	summary.Outputs = append(summary.Outputs, outputPath)
	if includeChecksum {
		summary.OutputChecksums = append(summary.OutputChecksums, OutputChecksum{Path: outputPath, SHA256: partSummary.SHA256})
	}
	summary.BytesWritten += partSummary.BytesWritten
	summary.Parts = len(summary.Outputs)
}

func splitPartWasPublished(err error, outputPath string) bool {
	publication, ok := errors.AsType[*fileio.PublicationError](err)
	return ok && !publication.LocationUncertain && publication.FinalPath == outputPath
}

func failSplit(summary SplitSummary, err error) (SplitSummary, error) {
	summary.Complete = false
	summary.Failure = err.Error()
	if publication, ok := errors.AsType[*fileio.PublicationError](err); ok && publication.LocationUncertain {
		summary.PublicationUncertain = true
	}
	summary.Parts = len(summary.Outputs)
	preserved := append([]string(nil), summary.Outputs...)
	return summary, &SplitIncompleteError{Outputs: preserved, ManifestPath: summary.ManifestPath, Err: err}
}

func finalizeSplit(ctx context.Context, doc document.ReaderAtSize, summary SplitSummary, validate func(context.Context) error) (SplitSummary, error) {
	complete := summary
	complete.Complete = true
	complete.Failure = ""
	if err := writeSplitManifest(ctx, complete, func(validateCtx context.Context) error {
		return validateExportSource(validateCtx, doc, validate)
	}); err != nil {
		return failSplit(summary, err)
	}
	return complete, nil
}
