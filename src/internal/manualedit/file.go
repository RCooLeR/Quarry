package manualedit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
	"github.com/quarry/quarry-wails3/internal/regularfile"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

var (
	ErrSourceModifiedDuringOperation = errors.New("source file modified during operation")
	ErrSwapOriginalDisabled          = errors.New("manual-edit source replacement is disabled; save a copy instead")
	ErrIncompleteManualEditOutput    = errors.New("manual-edit output is incomplete")
)

// atomicOutput is the minimal handle-owned publication contract needed by the
// manual-edit path. Keeping the interface here also lets tests inject failures
// without weakening fileio.AtomicOutput's production implementation.
type atomicOutput interface {
	io.Writer
	CommitContextValidated(context.Context, func(context.Context) error) error
	Cleanup() error
}

var (
	openSourceFile   = regularfile.Open
	statSourcePath   = os.Stat
	openAtomicOutput = func(path string, sourcePaths []string, mode os.FileMode) (atomicOutput, error) {
		return fileio.OpenAtomicOutput(path, sourcePaths, mode)
	}
)

type FileOptions struct {
	// DeletePartialOnCancel is retained for API compatibility. Manual-edit copy
	// saves now always discard their operation-owned, unpublished scratch object;
	// they never preserve a pathname that could later be mistaken for an output.
	DeletePartialOnCancel bool
	// SwapOriginal is fail-disabled before any filesystem access. Source mutation
	// requires a separate, explicitly designed recovery transaction.
	SwapOriginal     bool
	BackupPath       string
	MaxInsertedBytes int64
	// SourceGeneration must match the immutable registry generation captured
	// when a source-bound Session was created. It is required for session saves.
	SourceGeneration uint64
	Progress         func(Progress)
}

// FileSummary distinguishes a fully streamed result from publication. On an
// ordinary error, Complete and Published are false and this operation created
// no final output (the pathname may still be owned by a racing creator). A
// fileio.PublicationError is the exceptional state in which both are true but
// publication finalization/durability returned an error.
type FileSummary struct {
	OutputPath string

	// TempPath and ManifestPath are retained for RPC/source compatibility. The
	// secure copy-only path deliberately exposes neither and leaves both empty.
	TempPath     string
	ManifestPath string

	// BackupPath and Swapped remain empty/false because SwapOriginal is disabled.
	BackupPath string
	Swapped    bool

	BytesWritten         int64
	ModifiedRange        Range
	Complete             bool
	Published            bool
	PublicationUncertain bool
}

type sourceSnapshot struct {
	info    os.FileInfo
	size    int64
	modTime time.Time
}

func ApplyFileEdit(ctx context.Context, sourcePath string, outputPath string, edit Edit, opts FileOptions) (summary FileSummary, retErr error) {
	summary = FileSummary{
		OutputPath: outputPath,
		ModifiedRange: Range{
			Start: edit.Start,
			End:   max64(edit.End, edit.Start+int64(len(edit.Text))),
		},
	}
	if opts.SwapOriginal {
		return summary, ErrSwapOriginalDisabled
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}

	src, err := openSourceFile(sourcePath)
	if err != nil {
		return summary, err
	}
	sourceOwned := true
	defer func() {
		if sourceOwned {
			retErr = errors.Join(retErr, src.Close())
		}
	}()

	st, err := src.Stat()
	if err != nil {
		return summary, err
	}
	sourceState := snapshotSource(st)
	table := NewPieceTable(st.Size())
	if opts.MaxInsertedBytes > 0 {
		table.SetMaxInsertedBytes(opts.MaxInsertedBytes)
	}
	if err := table.Replace(edit.Start, edit.End, edit.Text); err != nil {
		return summary, err
	}

	sourceDocument, err := document.OpenFile(sourcePath)
	if err != nil {
		return summary, err
	}
	sourceDocumentOwned := true
	defer func() {
		if sourceDocumentOwned {
			retErr = errors.Join(retErr, sourceDocument.Close())
		}
	}()
	expectedSource, err := sourceio.ExpectDocumentContext(ctx, sourceDocument)
	if err != nil {
		return summary, classifyDirectSourceError("capture exact manual-edit source", err)
	}
	documentInfo, err := sourceDocument.OpenedFileInfo()
	if err != nil || !os.SameFile(st, documentInfo) || !sameFileState(st, documentInfo) {
		return summary, fmt.Errorf("%w: transform handles do not identify one source generation", ErrSourceModifiedDuringOperation)
	}
	verifiedSource, err := sourceio.NewVerifiedDocumentReader(ctx, expectedSource, sourceDocument)
	if err != nil {
		return summary, classifyDirectSourceError("open exact manual-edit source reader", err)
	}
	if err := verifySourceUnchanged(sourcePath, src, sourceState); err != nil {
		return summary, err
	}

	summary, sourceOwned, retErr = writeOpenTableToFile(
		ctx,
		sourcePath,
		outputPath,
		src,
		verifiedSource,
		ErrSourceModifiedDuringOperation,
		sourceState,
		table,
		summary,
		opts,
		func(validateCtx context.Context, file *os.File) error {
			if err := expectedSource.ValidateDocumentContext(validateCtx, sourceDocument); err != nil {
				return classifyDirectSourceError("revalidate exact manual-edit source", err)
			}
			closeErr := sourceDocument.Close()
			sourceDocumentOwned = false
			return closeErr
		},
	)
	return summary, retErr
}

func WriteSessionToFile(ctx context.Context, sourcePath string, outputPath string, session *Session, opts FileOptions) (FileSummary, error) {
	summary := FileSummary{OutputPath: outputPath}
	if opts.SwapOriginal {
		return summary, ErrSwapOriginalDisabled
	}
	if session == nil || !session.HasEdits() {
		return summary, errors.New("session has no staged edits")
	}
	summary.ModifiedRange = combinedRange(session.SourceMappedModifiedRanges())
	return writeSessionToFile(ctx, sourcePath, outputPath, session, summary, opts)
}

func writeSessionToFile(ctx context.Context, sourcePath string, outputPath string, session *Session, summary FileSummary, opts FileOptions) (result FileSummary, retErr error) {
	if opts.SwapOriginal {
		return summary, ErrSwapOriginalDisabled
	}
	if session == nil || session.table == nil {
		return summary, errors.New("session is required")
	}
	if session.binding == nil {
		return summary, ErrSessionSourceUnbound
	}
	if opts.SourceGeneration == 0 || opts.SourceGeneration != session.binding.generation {
		return summary, ErrSessionGenerationChanged
	}
	if sourcePath != session.binding.path {
		return summary, ErrSessionSourceChanged
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}

	src, err := openSourceFile(sourcePath)
	if err != nil {
		return summary, err
	}
	sourceOwned := true
	defer func() {
		if sourceOwned {
			retErr = errors.Join(retErr, src.Close())
		}
	}()

	st, err := src.Stat()
	if err != nil {
		return summary, err
	}
	// Piece offsets are relative to the source size captured when staging began.
	// Reject stale offsets before allocating any output object.
	if st.Size() != session.table.OriginalSize() {
		return summary, fmt.Errorf("source size changed from %d to %d since staging: %w",
			session.table.OriginalSize(), st.Size(), ErrSourceModifiedDuringOperation)
	}
	if err := session.validateOpenedSource(ctx, sourcePath, opts.SourceGeneration, src); err != nil {
		return summary, err
	}
	verifiedSource, err := session.verifiedSourceReader(ctx)
	if err != nil {
		return summary, err
	}
	if err := session.verifyExpectedSource(verifiedSource); err != nil {
		if errors.Is(err, sourceio.ErrSourceChanged) {
			return summary, fmt.Errorf("%w: verify staged source bytes: %w", ErrSessionSourceChanged, err)
		}
		return summary, err
	}

	result, sourceOwned, retErr = writeOpenTableToFile(
		ctx,
		sourcePath,
		outputPath,
		src,
		verifiedSource,
		ErrSessionSourceChanged,
		snapshotSource(st),
		session.table,
		summary,
		opts,
		func(validateCtx context.Context, file *os.File) error {
			return session.validateOpenedSource(validateCtx, sourcePath, opts.SourceGeneration, file)
		},
	)
	return result, retErr
}

// writeOpenTableToFile owns src on entry and returns sourceOwned=false once it
// has closed it. It never publishes until the exact expected byte count is
// written and the opened source plus its pathname still match the snapshot.
func writeOpenTableToFile(
	ctx context.Context,
	sourcePath string,
	outputPath string,
	src *os.File,
	sourceReader document.ReaderAtSize,
	sourceReadChangedError error,
	sourceState sourceSnapshot,
	table *PieceTable,
	summary FileSummary,
	opts FileOptions,
	postSourceVerify func(context.Context, *os.File) error,
) (result FileSummary, sourceOwned bool, retErr error) {
	result = summary
	sourceOwned = true
	if sourceReader == nil || sourceReader.Size() != sourceState.size {
		return result, sourceOwned, fmt.Errorf("%w: exact source reader size does not match the opened source", sourceReadChangedError)
	}

	out, err := openAtomicOutput(outputPath, []string{sourcePath}, 0o600)
	if err != nil {
		return result, sourceOwned, err
	}
	defer func() {
		retErr = errors.Join(retErr, out.Cleanup())
	}()

	written, err := table.WriteTo(
		ctx,
		sourceReader,
		atomicSyncWriter{atomicOutput: out},
		WriteOptions{Progress: opts.Progress},
	)
	result.BytesWritten = written
	if err != nil {
		if sourceReadChangedError != nil && errors.Is(err, sourceio.ErrSourceChanged) {
			return result, sourceOwned, fmt.Errorf("%w: exact source-span read failed: %w", sourceReadChangedError, err)
		}
		return result, sourceOwned, err
	}
	if written != table.Size() {
		return result, sourceOwned, fmt.Errorf("%w: wrote %d of %d bytes", ErrIncompleteManualEditOutput, written, table.Size())
	}
	if err := verifySourceUnchanged(sourcePath, src, sourceState); err != nil {
		return result, sourceOwned, err
	}

	// Sync the complete output first, then repeat every source check while the
	// exact source descriptor remains open. Closing inside the validator keeps
	// source-close failures pre-publication and leaves only the unavoidable
	// cross-object validator-to-rename interval.
	if err := out.CommitContextValidated(ctx, func(validateCtx context.Context) error {
		if err := verifySourceUnchanged(sourcePath, src, sourceState); err != nil {
			return err
		}
		if postSourceVerify != nil {
			if err := postSourceVerify(validateCtx, src); err != nil {
				return err
			}
		}
		closeErr := src.Close()
		sourceOwned = false
		return closeErr
	}); err != nil {
		if publicationErr, ok := errors.AsType[*fileio.PublicationError](err); ok {
			result.Complete = true
			result.Published = true
			result.PublicationUncertain = publicationErr.LocationUncertain
		}
		return result, sourceOwned, err
	}
	result.Complete = true
	result.Published = true
	return result, sourceOwned, nil
}

// atomicSyncWriter defers the real data sync to AtomicOutput.CommitContext.
// PieceTable's final Sync call is intentionally a no-op here: publication owns
// the one authoritative sync/close/error-propagation sequence.
type atomicSyncWriter struct {
	atomicOutput
}

func (atomicSyncWriter) Sync() error { return nil }

func snapshotSource(info os.FileInfo) sourceSnapshot {
	return sourceSnapshot{
		info:    info,
		size:    info.Size(),
		modTime: info.ModTime(),
	}
}

func verifySourceUnchanged(path string, src *os.File, before sourceSnapshot) error {
	if src == nil || before.info == nil {
		return ErrSourceModifiedDuringOperation
	}
	handleInfo, err := src.Stat()
	if err != nil {
		return errors.Join(ErrSourceModifiedDuringOperation, err)
	}
	if !os.SameFile(before.info, handleInfo) || !sameSourceMetadata(before, handleInfo) {
		return ErrSourceModifiedDuringOperation
	}

	pathInfo, err := statSourcePath(path)
	if err != nil {
		return errors.Join(ErrSourceModifiedDuringOperation, err)
	}
	if !os.SameFile(handleInfo, pathInfo) || !sameSourceMetadata(before, pathInfo) {
		return ErrSourceModifiedDuringOperation
	}
	return nil
}

func sameSourceMetadata(before sourceSnapshot, after os.FileInfo) bool {
	return after.Size() == before.size && after.ModTime().Equal(before.modTime)
}

func classifyDirectSourceError(operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrSourceModifiedDuringOperation, operation, err)
}

func combinedRange(ranges []Range) Range {
	if len(ranges) == 0 {
		return Range{}
	}
	combined := ranges[0]
	for _, current := range ranges[1:] {
		if current.Start < combined.Start {
			combined.Start = current.Start
		}
		if current.End > combined.End {
			combined.End = current.End
		}
	}
	return combined
}
