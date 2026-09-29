package manualedit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

var (
	ErrSessionSourceUnbound     = errors.New("manual-edit session is not bound to a source generation")
	ErrSessionGenerationChanged = errors.New("manual-edit source generation changed")
	ErrSessionSourceChanged     = errors.New("manual-edit source identity or content changed")
)

type sourceBinding struct {
	path       string
	generation uint64
	info       os.FileInfo
	reader     document.ReaderAtSize
	document   *document.FileDocument
	expected   *sourceio.Expectation
}

// NewSourceBoundSession captures the exact retained document identity, a full
// streaming SHA-256 fingerprint, and a bounded source-block digest map. The
// capture costs O(file size) time at the first edit while retaining at most the
// shared source-verification limit rather than whole-file content.
func NewSourceBoundSession(doc *document.FileDocument, path string, generation uint64, limits Limits) (*Session, error) {
	return NewSourceBoundSessionContext(context.Background(), doc, path, generation, limits)
}

// NewSourceBoundSessionContext is the cancellable source-bound constructor.
// Fingerprinting can require one complete source pass, so production callers
// that own a request/job context should prefer this form.
func NewSourceBoundSessionContext(ctx context.Context, doc *document.FileDocument, path string, generation uint64, limits Limits) (*Session, error) {
	return NewSourceBoundSessionContextWithProgress(ctx, doc, path, generation, limits, nil)
}

// NewSourceBoundSessionContextWithProgress is the cancellable constructor plus
// bounded exact-source fingerprint progress. The source layer caps callback
// frequency independently of file size, so service callers may forward these
// updates across an event bridge without creating an unbounded event stream.
func NewSourceBoundSessionContextWithProgress(ctx context.Context, doc *document.FileDocument, path string, generation uint64, limits Limits, progress func(completed, total int64)) (*Session, error) {
	if doc == nil {
		return nil, errors.New("source document is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if generation == 0 {
		return nil, ErrSessionGenerationChanged
	}
	if path == "" || path != doc.Path() {
		return nil, fmt.Errorf("%w: requested path does not match the retained document", ErrSessionSourceChanged)
	}
	session, err := NewSessionWithLimits(doc.Size(), limits)
	if err != nil {
		return nil, err
	}
	retainedBytes, verificationBufferBytes, err := sourceio.VerificationMemoryBoundsForSize(doc.Size())
	if err != nil {
		return nil, classifySessionSourceError("preflight exact retained source", err)
	}
	fixedVerificationBytes, err := checkedMemorySum(
		sourceCompareBufferBytes,
		sourceWriteBufferBytes,
		retainedBytes,
		verificationBufferBytes,
	)
	if err != nil || fixedVerificationBytes > limits.MaxTransientBytes {
		return nil, ErrTransientMemoryLimit
	}
	binding, err := captureSourceBinding(ctx, doc, path, generation, progress)
	if err != nil {
		return nil, err
	}
	// Defend against future layout changes making the preflight estimate stale.
	actualRetainedBytes, actualVerificationBufferBytes := binding.expected.MemoryBounds()
	actualFixedBytes, actualErr := checkedMemorySum(
		sourceCompareBufferBytes,
		sourceWriteBufferBytes,
		actualRetainedBytes,
		actualVerificationBufferBytes,
	)
	if actualErr != nil || actualFixedBytes > limits.MaxTransientBytes {
		return nil, ErrTransientMemoryLimit
	}
	session.binding = binding
	return session, nil
}

func captureSourceBinding(ctx context.Context, doc *document.FileDocument, path string, generation uint64, progress func(completed, total int64)) (*sourceBinding, error) {
	expected, err := sourceio.ExpectDocumentContextWithProgress(ctx, doc, progress)
	if err != nil {
		return nil, classifySessionSourceError("capture exact retained source", err)
	}
	info, err := doc.OpenedFileInfo()
	if err != nil {
		return nil, fmt.Errorf("%w: retained source identity is unavailable: %w", ErrSessionSourceChanged, err)
	}
	return &sourceBinding{
		path:       path,
		generation: generation,
		info:       info,
		reader:     doc,
		document:   doc,
		expected:   expected,
	}, nil
}

func (s *Session) validateOpenedSource(ctx context.Context, path string, generation uint64, file *os.File) error {
	if s == nil || s.binding == nil {
		return ErrSessionSourceUnbound
	}
	if generation == 0 || generation != s.binding.generation {
		return ErrSessionGenerationChanged
	}
	if path != s.binding.path || file == nil {
		return ErrSessionSourceChanged
	}
	before, err := file.Stat()
	if err != nil {
		return errors.Join(ErrSessionSourceChanged, err)
	}
	if !before.Mode().IsRegular() || !os.SameFile(s.binding.info, before) || !sameFileState(s.binding.info, before) {
		return ErrSessionSourceChanged
	}
	if err := validateOpenedPath(path, before); err != nil {
		return err
	}
	if s.binding.expected == nil || s.binding.document == nil {
		return ErrSessionSourceUnbound
	}
	if err := s.binding.expected.ValidateDocumentContext(ctx, s.binding.document); err != nil {
		return classifySessionSourceError("revalidate exact retained source", err)
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !sameFileState(before, after) {
		return ErrSessionSourceChanged
	}
	return validateOpenedPath(path, after)
}

func (s *Session) verifiedSourceReader(ctx context.Context) (document.ReaderAtSize, error) {
	if s == nil || s.binding == nil || s.binding.expected == nil || s.binding.document == nil {
		return nil, ErrSessionSourceUnbound
	}
	reader, err := sourceio.NewVerifiedDocumentReader(ctx, s.binding.expected, s.binding.document)
	if err != nil {
		return nil, classifySessionSourceError("open exact source reader", err)
	}
	return reader, nil
}

// ReadRangeContext returns a bounded transformed range while authenticating
// every original source block against the fingerprint captured at prepare
// time. The source identity is checked both before and after the read so even
// an all-inserted range cannot silently hide an external generation change.
func (s *Session) ReadRangeContext(ctx context.Context, start, end, maxBytes int64) ([]byte, error) {
	if s == nil || s.table == nil {
		return nil, errors.New("session is required")
	}
	if err := validateBoundedReadRange(start, end, s.Size(), maxBytes); err != nil {
		return nil, err
	}
	if err := s.validateBoundSourceState(); err != nil {
		return nil, err
	}
	reader, err := s.verifiedSourceReader(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.table.ReadRange(reader, start, end)
	if err != nil {
		return nil, classifySessionSourceError("read exact edited view", err)
	}
	if err := s.validateBoundSourceState(); err != nil {
		return nil, err
	}
	return result, nil
}

// ReadSourceRangeContext returns a bounded range from the original prepared
// source. It is used for diff previews whose coordinates are source-anchored.
func (s *Session) ReadSourceRangeContext(ctx context.Context, start, end, maxBytes int64) ([]byte, error) {
	if s == nil {
		return nil, errors.New("session is required")
	}
	if err := validateBoundedReadRange(start, end, s.originalSize, maxBytes); err != nil {
		return nil, err
	}
	if err := s.validateBoundSourceState(); err != nil {
		return nil, err
	}
	reader, err := s.verifiedSourceReader(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]byte, end-start)
	n, readErr := reader.ReadAt(result, start)
	if n != len(result) {
		if readErr == nil {
			readErr = io.ErrUnexpectedEOF
		}
		return nil, classifySessionSourceError("read exact original view", readErr)
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, classifySessionSourceError("read exact original view", readErr)
	}
	if err := s.validateBoundSourceState(); err != nil {
		return nil, err
	}
	return result, nil
}

// ReadSourcePreviewRangesContext reads inspection-only source previews through
// the retained document without replaying the multi-megabyte cryptographic
// verification block for every tiny range. The entire bounded batch is
// bracketed by the strong OS mutation-generation check captured when editing
// was prepared; any source change makes the whole result unusable.
//
// This method must not be used for staging, save, or artifact publication.
// Those paths require ReadSourceRangeContext/verifiedSourceReader so every
// consumed source block is authenticated against the prepared fingerprint.
func (s *Session) ReadSourcePreviewRangesContext(
	ctx context.Context,
	ranges []Range,
	maxRanges int,
	maxRangeBytes int64,
	maxTotalBytes int64,
) (results [][]byte, retErr error) {
	if s == nil || s.binding == nil || s.binding.reader == nil {
		return nil, ErrSessionSourceUnbound
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxRanges <= 0 || maxRanges > s.limits.MaxEditCount || len(ranges) > maxRanges {
		return nil, fmt.Errorf("source preview range count %d exceeds bounded limit %d", len(ranges), maxRanges)
	}
	if maxRangeBytes <= 0 || maxTotalBytes <= 0 || maxTotalBytes > int64(maxInt()) ||
		maxRangeBytes > s.limits.MaxTransientBytes || maxTotalBytes > s.limits.MaxTransientBytes {
		return nil, errors.New("source preview byte limits must be positive and within the session transient-memory policy")
	}

	var totalBytes int64
	for _, sourceRange := range ranges {
		if err := validateBoundedReadRange(sourceRange.Start, sourceRange.End, s.originalSize, maxRangeBytes); err != nil {
			return nil, err
		}
		var err error
		totalBytes, err = checkedMemorySum(totalBytes, sourceRange.End-sourceRange.Start)
		if err != nil || totalBytes > maxTotalBytes {
			return nil, fmt.Errorf("source preview bytes exceed bounded aggregate limit %d", maxTotalBytes)
		}
	}
	if err := s.validateBoundSourceState(); err != nil {
		return nil, err
	}
	defer func() {
		if sourceErr := s.validateBoundSourceState(); sourceErr != nil {
			results = nil
			retErr = errors.Join(retErr, sourceErr)
		}
	}()

	results = make([][]byte, len(ranges))
	for index, sourceRange := range ranges {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		length := sourceRange.End - sourceRange.Start
		if length == 0 {
			results[index] = []byte{}
			continue
		}
		preview := make([]byte, int(length))
		n, readErr := s.binding.reader.ReadAt(preview, sourceRange.Start)
		if n != len(preview) {
			if readErr == nil {
				readErr = io.ErrUnexpectedEOF
			}
			return nil, classifySessionSourceError("read bounded source previews", readErr)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, classifySessionSourceError("read bounded source previews", readErr)
		}
		results[index] = preview
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// ValidateSourceContext performs the lightweight identity/generation half of
// source-bound reading. Callers use it after ancillary operations such as line
// lookup so a successful response always belongs to the prepared generation.
func (s *Session) ValidateSourceContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.validateBoundSourceState()
}

func validateBoundedReadRange(start, end, size, maxBytes int64) error {
	if start < 0 || end < start || end > size {
		return fmt.Errorf("invalid read range [%d,%d) for size %d", start, end, size)
	}
	if maxBytes <= 0 || end-start > maxBytes || end-start > int64(maxInt()) {
		return fmt.Errorf("read range %d exceeds bounded limit %d", end-start, maxBytes)
	}
	return nil
}

// validateBoundSourceState is deliberately cheaper than a full-source hash:
// prepared sessions are admitted only when a strong OS mutation generation is
// available, and individual blocks are fingerprinted as they are consumed.
func (s *Session) validateBoundSourceState() error {
	if s == nil || s.binding == nil || s.binding.document == nil || s.binding.info == nil || s.binding.expected == nil {
		return ErrSessionSourceUnbound
	}
	doc := s.binding.document
	if !doc.HasMutationGeneration() {
		return errors.Join(ErrSessionSourceChanged, sourceio.ErrMutationGenerationMissing)
	}
	if err := doc.ValidateUnchanged(); err != nil {
		return classifySessionSourceError("validate retained edited-view source", err)
	}
	opened, err := doc.OpenedFileInfo()
	if err != nil || !os.SameFile(s.binding.info, opened) || !sameFileState(s.binding.info, opened) {
		if err == nil {
			err = ErrSessionSourceChanged
		}
		return errors.Join(ErrSessionSourceChanged, err)
	}
	state, same, err := doc.CurrentPathIdentity()
	if err != nil || !same || state.Size != opened.Size() || !state.ModTime.Equal(opened.ModTime()) {
		if err == nil {
			err = ErrSessionSourceChanged
		}
		return errors.Join(ErrSessionSourceChanged, err)
	}
	return nil
}

func validateOpenedPath(path string, opened os.FileInfo) error {
	pathInfo, err := statSourcePath(path)
	if err != nil {
		return errors.Join(ErrSessionSourceChanged, err)
	}
	if !os.SameFile(opened, pathInfo) || !sameFileState(opened, pathInfo) {
		return ErrSessionSourceChanged
	}
	return nil
}

func (s *Session) verifyExpectedSource(src document.ReaderAtSize) error {
	if s == nil || s.binding == nil {
		return ErrSessionSourceUnbound
	}
	if src == nil {
		return errors.New("source document is required")
	}
	table := NewPieceTable(s.originalSize)
	table.setLimits(s.limits)
	for idx := 0; idx < s.cursor; idx++ {
		staged := s.history[idx]
		if !staged.verified || int64(len(staged.expectedOld)) != staged.edit.End-staged.edit.Start {
			return ErrExpectedBytesRequired
		}
		matches, err := table.rangeEquals(src, staged.edit.Start, staged.edit.End, staged.expectedOld)
		if err != nil {
			return err
		}
		if !matches {
			return fmt.Errorf("%w at staged edit %d", ErrExpectedBytesMismatch, idx+1)
		}
		if err := table.Replace(staged.edit.Start, staged.edit.End, staged.edit.Text); err != nil {
			return err
		}
	}
	return nil
}

func sameFileState(left, right os.FileInfo) bool {
	return left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

func classifySessionSourceError(operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrSessionSourceChanged, operation, err)
}
