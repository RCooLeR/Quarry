package replace

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/quarry/quarry-wails3/internal/fileio"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

var ErrSwapOriginalDisabled = errors.New("replacing the opened source is disabled; choose a separate output path")

// replaceBatchPlainFileAtomic is a dormant defense-in-depth copy-only path.
// The source is retained read-only and complete output is published with the
// shared handle-bound, no-clobber transaction if this private code is tested.
func replaceBatchPlainFileAtomic(ctx context.Context, sourcePath string, outputPath string, rules []BatchRule, opts FileOptions, batchOpts BatchOptions) (summary FileSummary, retErr error) {
	if opts.SwapOriginal {
		return FileSummary{}, ErrSwapOriginalDisabled
	}
	if _, err := validateBatchRules(rules); err != nil {
		return FileSummary{OutputPath: outputPath}, err
	}
	if _, err := batchWriteBufferSize(batchOpts.WriteBufferSize); err != nil {
		return FileSummary{OutputPath: outputPath}, err
	}
	if _, err := normalizedPlainChunkSize(batchOpts.ChunkSize, 64*1024*1024, 1); err != nil {
		return FileSummary{OutputPath: outputPath}, err
	}
	return replaceBatchFileAtomic(ctx, sourcePath, outputPath, opts, func(src readStatSource, dst syncWriter, progress func(Progress)) (int64, int64, error) {
		innerProgress := batchOpts.Progress
		batchOpts.Progress = func(p Progress) {
			progress(p)
			if innerProgress != nil {
				innerProgress(p)
			}
		}
		return replaceBatchPlain(ctx, src, dst, rules, batchOpts)
	})
}

// replaceBatchRegexpFileAtomic is the dormant copy-only regex batch path.
func replaceBatchRegexpFileAtomic(ctx context.Context, sourcePath string, outputPath string, rules []BatchRule, opts FileOptions, regexOpts RegexOptions) (summary FileSummary, retErr error) {
	if opts.SwapOriginal {
		return FileSummary{}, ErrSwapOriginalDisabled
	}
	if err := validateRegexOptions(regexOpts); err != nil {
		return FileSummary{OutputPath: outputPath}, err
	}
	regexOpts = normalizeRegexOptions(regexOpts)
	if _, err := compileRegexBatchRules(rules, regexOpts.CaseInsensitive, regexOpts.MaxMatchWindow); err != nil {
		return FileSummary{OutputPath: outputPath}, err
	}
	return replaceBatchFileAtomic(ctx, sourcePath, outputPath, opts, func(src readStatSource, dst syncWriter, progress func(Progress)) (int64, int64, error) {
		innerProgress := regexOpts.Progress
		regexOpts.Progress = func(p Progress) {
			progress(p)
			if innerProgress != nil {
				innerProgress(p)
			}
		}
		return replaceBatchRegexp(ctx, src, dst, rules, regexOpts)
	})
}

type atomicBatchTransform func(src readStatSource, dst syncWriter, progress func(Progress)) (matches int64, conflicts int64, err error)

func replaceBatchFileAtomic(ctx context.Context, sourcePath string, outputPath string, opts FileOptions, transform atomicBatchTransform) (summary FileSummary, retErr error) {
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
	// Generic byte replacement cannot safely maintain the decoded byte counts
	// embedded in PHP/WordPress serialized values. Inspect the exact verified
	// source stream before each chunk reaches either the plain or regex engine.
	// Any detection aborts this private temporary output before publication.
	guardedSource := &phpSerializationGuardSource{source: src}
	matches, conflicts, err := transform(guardedSource, writer, progress)
	summary.Matches = matches
	summary.Conflicts = conflicts
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
		var publication *fileio.PublicationError
		if errors.As(err, &publication) {
			summary.Published = true
			summary.PublicationUncertain = publication.LocationUncertain
		}
		return summary, err
	}
	summary.TempPath = ""
	summary.Published = true
	return summary, nil
}

// cleanupAtomicBatchOutput clears retained-temp evidence only after cleanup is
// confirmed. If removal/close fails, callers need the exact path (when the
// platform uses a named temp) to report and inspect the retained artifact.
func cleanupAtomicBatchOutput(summary *FileSummary, cleanup func() error) error {
	if cleanup == nil {
		return errors.New("atomic replacement cleanup is required")
	}
	err := cleanup()
	if err == nil && summary != nil {
		summary.TempPath = ""
	}
	return err
}

type exactSyncWriter struct {
	dst     syncWriter
	written int64
}

func (w *exactSyncWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if n > 0 {
		if int64(n) > int64(^uint64(0)>>1)-w.written {
			return n, errors.New("replacement output byte count overflow")
		}
		w.written += int64(n)
	}
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (w *exactSyncWriter) Sync() error { return w.dst.Sync() }
