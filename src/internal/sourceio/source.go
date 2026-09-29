// Package sourceio binds pathname-based streaming transforms to an expected
// retained source generation. It does not modify the source or output paths.
package sourceio

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"sync"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/regularfile"
)

const (
	fingerprintChunkBytes    = 1024 * 1024
	maxFingerprintChunkBytes = 16 * 1024 * 1024
	maxFingerprintChunks     = 256 * 1024

	// Fingerprinting a maximum-size source can visit hundreds of thousands of
	// verification chunks. Progress must stay bounded independently of source
	// size so one huge edit cannot flood the bridge or retain an unbounded event
	// queue while cancellation checks still run at every chunk boundary.
	maxFingerprintProgressCallbacks int64 = 1024
)

var (
	ErrSourceChanged             = errors.New("source pathname or retained generation changed")
	ErrMutationGenerationMissing = errors.New("exact source mutation generation is unavailable")
	ErrSourceVerificationLimit   = errors.New("source exceeds the exact verification limit")
)

// Expectation is an immutable identity/content snapshot of the FileDocument
// handle retained by the session registry. Its fields are intentionally
// private so callers cannot manufacture a weaker identity contract.
type Expectation struct {
	path         string
	info         os.FileInfo
	digest       [sha256.Size]byte
	chunkSize    int64
	chunkDigests [][sha256.Size]byte
}

// MemoryBounds reports the bytes retained by the block-digest map and the
// largest operation-owned buffer needed to verify one source block. Callers
// with their own aggregate memory policy can account for the exact bounded
// verifier cost without accessing or weakening the expectation itself.
func (e *Expectation) MemoryBounds() (retainedBytes int64, verificationBufferBytes int64) {
	if e == nil {
		return 0, 0
	}
	return int64(len(e.chunkDigests)) * sha256.Size, e.chunkSize
}

// VerificationMemoryBoundsForSize reports the block-map and scratch-buffer
// cost that ExpectDocumentContext would require for size. Callers with a
// stricter aggregate memory policy can therefore reject the operation before
// the fingerprint pass allocates either object.
func VerificationMemoryBoundsForSize(size int64) (retainedBytes int64, verificationBufferBytes int64, err error) {
	chunkSize, chunkCount, err := boundedFingerprintLayout(size)
	if err != nil {
		return 0, 0, err
	}
	return int64(chunkCount) * sha256.Size, chunkSize, nil
}

// MaximumVerificationMemoryBounds reports the absolute source-expectation
// block-map and one-pass scratch bounds supported by this package. It lets a
// higher-level session admission policy account for the worst case without
// duplicating the fingerprint layout constants.
func MaximumVerificationMemoryBounds() (retainedBytes int64, verificationBufferBytes int64) {
	return int64(maxFingerprintChunks) * sha256.Size, maxFingerprintChunkBytes
}

// ValidateDocumentContext proves that doc still represents the exact identity
// and bytes captured by this expectation. It is intended for retained-handle
// transforms (for example range exports) that do not stream through Handle but
// still need a full, cancellable content check in the pre-publication window.
func (e *Expectation) ValidateDocumentContext(ctx context.Context, doc *document.FileDocument) error {
	if e == nil || doc == nil {
		return errors.New("source expectation and document are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if doc.Path() != e.path {
		return fmt.Errorf("%w: retained document path %q differs from expected path %q", ErrSourceChanged, doc.Path(), e.path)
	}
	if !doc.HasMutationGeneration() {
		return errors.Join(ErrSourceChanged, ErrMutationGenerationMissing)
	}
	if err := doc.ValidateUnchanged(); err != nil {
		return fmt.Errorf("%w: retained document validation failed: %w", ErrSourceChanged, err)
	}
	before, err := doc.OpenedFileInfo()
	if err != nil {
		return fmt.Errorf("%w: retained document identity is unavailable: %w", ErrSourceChanged, err)
	}
	if !os.SameFile(e.info, before) || !sameState(e.info, before) {
		return fmt.Errorf("%w: retained document is not the expected source generation", ErrSourceChanged)
	}
	state, same, err := doc.CurrentPathIdentity()
	if err != nil {
		return fmt.Errorf("%w: current pathname identity is unavailable: %w", ErrSourceChanged, err)
	}
	if !same || state.Size != before.Size() || !state.ModTime.Equal(before.ModTime()) {
		return fmt.Errorf("%w: current pathname no longer identifies the retained source generation", ErrSourceChanged)
	}

	digest, err := fingerprintReaderAt(ctx, doc, before.Size())
	if err != nil {
		return classifyFingerprintError("revalidate retained source", err)
	}
	if digest != e.digest {
		return fmt.Errorf("%w: retained source content changed before publication", ErrSourceChanged)
	}
	if err := doc.ValidateUnchanged(); err != nil {
		return fmt.Errorf("%w: retained document changed while it was revalidated: %w", ErrSourceChanged, err)
	}
	after, err := doc.OpenedFileInfo()
	if err != nil {
		return fmt.Errorf("%w: retained document identity is unavailable after revalidation: %w", ErrSourceChanged, err)
	}
	state, same, err = doc.CurrentPathIdentity()
	if err != nil {
		return fmt.Errorf("%w: current pathname identity is unavailable after revalidation: %w", ErrSourceChanged, err)
	}
	if !os.SameFile(before, after) || !sameState(before, after) || !same || state.Size != after.Size() || !state.ModTime.Equal(after.ModTime()) {
		return fmt.Errorf("%w: retained source changed during final publication validation", ErrSourceChanged)
	}
	return ctx.Err()
}

// ExpectDocument captures an exact, constant-memory fingerprint of the
// retained document. Artifact-producing service jobs should use
// ExpectDocumentContext so cancellation can stop this full sequential pass.
func ExpectDocument(doc *document.FileDocument) (*Expectation, error) {
	return ExpectDocumentContext(context.Background(), doc)
}

// ExpectDocumentContext captures the retained handle identity and a full
// SHA-256 content fingerprint only while its pathname still resolves to the
// same unchanged regular file.
func ExpectDocumentContext(ctx context.Context, doc *document.FileDocument) (*Expectation, error) {
	return ExpectDocumentContextWithProgress(ctx, doc, nil)
}

// ExpectDocumentContextWithProgress is ExpectDocumentContext plus a bounded
// progress callback. The callback receives exact byte counts, is invoked at
// most maxFingerprintProgressCallbacks times, and runs synchronously between
// chunk reads. Cancelling its context from the callback stops before the next
// source chunk and prevents an expectation from being returned.
func ExpectDocumentContextWithProgress(ctx context.Context, doc *document.FileDocument, progress func(completed, total int64)) (*Expectation, error) {
	if doc == nil {
		return nil, errors.New("source document is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !doc.HasMutationGeneration() {
		return nil, errors.Join(ErrSourceChanged, ErrMutationGenerationMissing)
	}
	if err := doc.ValidateUnchanged(); err != nil {
		return nil, fmt.Errorf("%w: retained document validation failed: %w", ErrSourceChanged, err)
	}
	before, err := doc.OpenedFileInfo()
	if err != nil {
		return nil, fmt.Errorf("%w: retained document identity is unavailable: %w", ErrSourceChanged, err)
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("source must be a regular file")
	}
	state, same, err := doc.CurrentPathIdentity()
	if err != nil {
		return nil, fmt.Errorf("%w: current pathname identity is unavailable: %w", ErrSourceChanged, err)
	}
	original := doc.OriginalFileState()
	if !same || !state.Equal(original) || before.Size() != original.Size || !before.ModTime().Equal(original.ModTime) {
		return nil, fmt.Errorf("%w: retained document no longer matches %q", ErrSourceChanged, doc.Path())
	}

	digest, chunkSize, chunkDigests, err := fingerprintReaderAtWithChunksProgress(ctx, doc, before.Size(), progress)
	if err != nil {
		return nil, classifyFingerprintError("fingerprint retained source", err)
	}
	if err := doc.ValidateUnchanged(); err != nil {
		return nil, fmt.Errorf("%w: retained document changed while it was fingerprinted: %w", ErrSourceChanged, err)
	}
	after, err := doc.OpenedFileInfo()
	if err != nil {
		return nil, fmt.Errorf("%w: retained document identity is unavailable after fingerprinting: %w", ErrSourceChanged, err)
	}
	state, same, err = doc.CurrentPathIdentity()
	if err != nil {
		return nil, fmt.Errorf("%w: current pathname identity is unavailable after fingerprinting: %w", ErrSourceChanged, err)
	}
	if !os.SameFile(before, after) || !sameState(before, after) || !same || !state.Equal(original) {
		return nil, fmt.Errorf("%w: retained source changed while its generation was captured", ErrSourceChanged)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &Expectation{
		path:         doc.Path(),
		info:         after,
		digest:       digest,
		chunkSize:    chunkSize,
		chunkDigests: chunkDigests,
	}, nil
}

// Handle owns the exact read-only file descriptor used by a transform. Reads
// made through Handle are hashed in stream order; publication validation fails
// unless the transform consumed one complete pass matching the expectation.
type Handle struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	base   os.FileInfo
	digest [sha256.Size]byte

	streamHash     hash.Hash
	streamBytes    int64
	streamStarted  bool
	streamVerified bool
	streamInvalid  bool
	streamErr      error

	position           int64
	chunkSize          int64
	chunkDigests       [][sha256.Size]byte
	verifiedChunkIndex int64
	verifiedChunk      []byte
}

// Open is the non-cancellable compatibility wrapper around OpenContext.
func Open(path string, expected *Expectation) (*Handle, error) {
	return OpenContext(context.Background(), path, expected)
}

// OpenContext opens path once, proves that descriptor contains the exact
// expected bytes when an expectation is supplied, and proves the pathname
// still resolves to that descriptor. With a nil expectation it captures an
// exact baseline and enforces the same stream/publication validation.
func OpenContext(ctx context.Context, path string, expected *Expectation) (*Handle, error) {
	if path == "" {
		return nil, errors.New("source path is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if expected != nil && path != expected.path {
		return nil, fmt.Errorf("%w: requested path %q differs from retained path %q", ErrSourceChanged, path, expected.path)
	}
	file, err := regularfile.Open(path)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*Handle, error) {
		return nil, errors.Join(err, file.Close())
	}
	before, err := file.Stat()
	if err != nil {
		return closeOnError(err)
	}
	if !before.Mode().IsRegular() {
		return closeOnError(errors.New("source must be a regular file"))
	}
	if expected != nil && (!os.SameFile(expected.info, before) || !sameState(expected.info, before)) {
		return closeOnError(fmt.Errorf("%w: opened path is not the retained source generation", ErrSourceChanged))
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		return closeOnError(fmt.Errorf("%w: source pathname cannot be validated: %v", ErrSourceChanged, err))
	}
	if !os.SameFile(before, pathInfo) || !sameState(before, pathInfo) {
		return closeOnError(fmt.Errorf("%w: source pathname does not identify the opened descriptor", ErrSourceChanged))
	}

	var digest [sha256.Size]byte
	var chunkSize int64
	var chunkDigests [][sha256.Size]byte
	if expected != nil {
		// The transform's complete streamed pass is hashed against this exact
		// expectation before publication. Avoid a redundant pre-transform full
		// scan; a mismatch can only leave the operation-owned temporary output.
		digest = expected.digest
		chunkSize = expected.chunkSize
		chunkDigests = expected.chunkDigests
	} else {
		digest, chunkSize, chunkDigests, err = fingerprintReaderAtWithChunks(ctx, file, before.Size())
		if err != nil {
			return closeOnError(classifyFingerprintError("fingerprint opened source", err))
		}
	}
	after, err := file.Stat()
	if err != nil {
		return closeOnError(err)
	}
	pathInfo, err = os.Stat(path)
	if err != nil {
		return closeOnError(fmt.Errorf("%w: source pathname cannot be revalidated: %v", ErrSourceChanged, err))
	}
	if !os.SameFile(before, after) || !sameState(before, after) || !os.SameFile(before, pathInfo) || !sameState(before, pathInfo) {
		return closeOnError(fmt.Errorf("%w: source changed while the transform handle was opened", ErrSourceChanged))
	}

	handle := &Handle{
		file:               file,
		path:               path,
		base:               after,
		digest:             digest,
		chunkSize:          chunkSize,
		chunkDigests:       chunkDigests,
		verifiedChunkIndex: -1,
	}
	handle.resetStreamLocked()
	return handle, nil
}

// Read streams from the retained descriptor and fingerprints the exact bytes
// consumed by the transform. A mixed-generation pass fails as soon as a full
// source-length pass has been observed.
func (h *Handle) Read(p []byte) (int, error) {
	if h == nil {
		return 0, errors.New("source handle is required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		return 0, os.ErrClosed
	}
	if h.streamErr != nil {
		return 0, h.streamErr
	}
	if len(p) == 0 {
		return 0, nil
	}
	if h.position >= h.base.Size() {
		if h.streamBytes != h.base.Size() {
			h.streamErr = fmt.Errorf("%w: transform reached EOF after %d of %d source bytes", ErrSourceChanged, h.streamBytes, h.base.Size())
			return 0, h.streamErr
		}
		return 0, io.EOF
	}

	want := int64(len(p))
	if remaining := h.base.Size() - h.position; want > remaining {
		want = remaining
	}
	n := 0
	for int64(n) < want {
		chunkIndex := h.position / h.chunkSize
		if err := h.loadVerifiedChunkLocked(chunkIndex); err != nil {
			h.streamErr = err
			return n, err
		}
		chunkStart := chunkIndex * h.chunkSize
		inside := h.position - chunkStart
		available := int64(len(h.verifiedChunk)) - inside
		need := want - int64(n)
		if available > need {
			available = need
		}
		copied := copy(p[n:], h.verifiedChunk[int(inside):int(inside+available)])
		if copied == 0 {
			h.streamErr = io.ErrNoProgress
			return n, h.streamErr
		}
		n += copied
		h.position += int64(copied)
	}
	if n > 0 {
		h.streamStarted = true
		if h.streamInvalid || h.streamBytes > h.base.Size()-int64(n) {
			h.streamErr = fmt.Errorf("%w: transform read did not form one exact source pass", ErrSourceChanged)
			return n, h.streamErr
		}
		_, _ = h.streamHash.Write(p[:n])
		h.streamBytes += int64(n)
		if h.streamBytes == h.base.Size() {
			var got [sha256.Size]byte
			copy(got[:], h.streamHash.Sum(nil))
			if got != h.digest {
				h.streamErr = fmt.Errorf("%w: bytes consumed by the transform differ from the retained generation", ErrSourceChanged)
				return n, h.streamErr
			}
			h.streamVerified = true
		}
	}
	return n, nil
}

// Seek supports transforms that perform a bounded preview and then restart at
// byte zero. Any non-zero seek invalidates exact stream verification.
func (h *Handle) Seek(offset int64, whence int) (int64, error) {
	if h == nil {
		return 0, errors.New("source handle is required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		return 0, os.ErrClosed
	}
	if h.streamErr != nil {
		return h.position, h.streamErr
	}
	var base int64
	switch whence {
	case io.SeekStart:
		base = 0
	case io.SeekCurrent:
		base = h.position
	case io.SeekEnd:
		base = h.base.Size()
	default:
		return h.position, errors.New("invalid seek whence")
	}
	if offset > 0 && base > math.MaxInt64-offset || offset < 0 && base < math.MinInt64-offset {
		return h.position, errors.New("source seek offset overflows int64")
	}
	position := base + offset
	if position < 0 {
		return h.position, errors.New("negative source seek position")
	}
	h.position = position
	if position == 0 {
		h.resetStreamLocked()
	} else {
		h.streamInvalid = true
		h.streamVerified = false
	}
	return position, nil
}

func (h *Handle) loadVerifiedChunkLocked(chunkIndex int64) error {
	if h.chunkSize <= 0 || chunkIndex < 0 || chunkIndex >= int64(len(h.chunkDigests)) {
		return fmt.Errorf("%w: source verification block %d is unavailable", ErrSourceChanged, chunkIndex)
	}
	if h.verifiedChunkIndex == chunkIndex {
		return nil
	}
	chunkStart := chunkIndex * h.chunkSize
	chunkEnd := chunkStart + h.chunkSize
	if chunkEnd > h.base.Size() {
		chunkEnd = h.base.Size()
	}
	chunkLength := chunkEnd - chunkStart
	if int64(cap(h.verifiedChunk)) < chunkLength {
		h.verifiedChunk = make([]byte, int(h.chunkSize))
	}
	h.verifiedChunk = h.verifiedChunk[:int(chunkLength)]
	n, err := h.file.ReadAt(h.verifiedChunk, chunkStart)
	if n != len(h.verifiedChunk) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return fmt.Errorf("%w: source verification block %d read %d of %d bytes: %v", ErrSourceChanged, chunkIndex, n, len(h.verifiedChunk), err)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: source verification block %d read failed: %v", ErrSourceChanged, chunkIndex, err)
	}
	if got := sha256.Sum256(h.verifiedChunk); got != h.chunkDigests[chunkIndex] {
		return fmt.Errorf("%w: source block %d differs from the captured generation", ErrSourceChanged, chunkIndex)
	}
	h.verifiedChunkIndex = chunkIndex
	return nil
}

// Stat returns state for the exact retained transform descriptor.
func (h *Handle) Stat() (os.FileInfo, error) {
	if h == nil {
		return nil, errors.New("source handle is required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		return nil, os.ErrClosed
	}
	return h.file.Stat()
}

// Validate is the non-cancellable compatibility wrapper around
// ValidateContext.
func (h *Handle) Validate() error {
	return h.ValidateContext(context.Background())
}

// ValidateContext proves that the transform consumed the exact retained
// generation, then performs a second full digest and pathname/descriptor check.
// Call immediately before atomic publication while the descriptor is open.
func (h *Handle) ValidateContext(ctx context.Context) error {
	if h == nil {
		return errors.New("source handle is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil || h.base == nil {
		return errors.New("source handle is required")
	}
	if h.streamErr != nil {
		return h.streamErr
	}
	if h.base.Size() == 0 && !h.streamStarted {
		h.streamVerified = true
	}
	if h.streamInvalid || !h.streamVerified || h.streamBytes != h.base.Size() {
		return fmt.Errorf("%w: transform did not consume one complete verified source pass", ErrSourceChanged)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := h.file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(h.base, current) || !sameState(h.base, current) {
		return fmt.Errorf("%w: retained source descriptor changed during streaming", ErrSourceChanged)
	}
	pathInfo, err := os.Stat(h.path)
	if err != nil {
		return fmt.Errorf("%w: source pathname cannot be revalidated: %v", ErrSourceChanged, err)
	}
	if !os.SameFile(h.base, pathInfo) || !sameState(h.base, pathInfo) {
		return fmt.Errorf("%w: source pathname no longer identifies the streamed generation", ErrSourceChanged)
	}

	digest, err := fingerprintReaderAt(ctx, h.file, h.base.Size())
	if err != nil {
		return classifyFingerprintError("revalidate source content", err)
	}
	if digest != h.digest {
		return fmt.Errorf("%w: source content changed before publication", ErrSourceChanged)
	}
	current, err = h.file.Stat()
	if err != nil {
		return err
	}
	pathInfo, pathErr := os.Stat(h.path)
	if pathErr != nil {
		return fmt.Errorf("%w: source pathname cannot be revalidated after fingerprinting: %v", ErrSourceChanged, pathErr)
	}
	if !os.SameFile(h.base, current) || !sameState(h.base, current) || !os.SameFile(h.base, pathInfo) || !sameState(h.base, pathInfo) {
		return fmt.Errorf("%w: source changed during final publication validation", ErrSourceChanged)
	}
	return nil
}

func (h *Handle) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		return nil
	}
	err := h.file.Close()
	h.file = nil
	return err
}

func (h *Handle) resetStreamLocked() {
	h.streamHash = sha256.New()
	h.streamBytes = 0
	h.streamStarted = false
	h.streamVerified = false
	h.streamInvalid = false
	h.streamErr = nil
	if h.base != nil && h.base.Size() == 0 {
		var empty [sha256.Size]byte
		copy(empty[:], h.streamHash.Sum(nil))
		h.streamVerified = empty == h.digest
	}
}

func fingerprintReaderAt(ctx context.Context, reader io.ReaderAt, size int64) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	if reader == nil {
		return out, errors.New("fingerprint reader is required")
	}
	if size < 0 {
		return out, errors.New("fingerprint size is negative")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	digest := sha256.New()
	buffer := make([]byte, fingerprintChunkBytes)
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		want := int64(len(buffer))
		if remaining := size - offset; want > remaining {
			want = remaining
		}
		n, err := reader.ReadAt(buffer[:want], offset)
		if n > 0 {
			_, _ = digest.Write(buffer[:n])
			offset += int64(n)
		}
		if n != int(want) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return out, fmt.Errorf("fingerprint read at %d: got %d of %d bytes: %w", offset-int64(n), n, want, err)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return out, err
		}
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	copy(out[:], digest.Sum(nil))
	return out, nil
}

// fingerprintReaderAtWithChunks records a bounded block map as well as the
// whole-source digest. The map lets selective/random-access transforms prove
// that the exact bytes they consumed belonged to this expectation, including
// when an external writer changes and restores bytes between the initial and
// final whole-file passes.
func fingerprintReaderAtWithChunks(ctx context.Context, reader io.ReaderAt, size int64) ([sha256.Size]byte, int64, [][sha256.Size]byte, error) {
	return fingerprintReaderAtWithChunksProgress(ctx, reader, size, nil)
}

type fingerprintProgressReporter struct {
	callback      func(completed, total int64)
	total         int64
	step          int64
	lastBucket    int64
	lastCompleted int64
	reportedEmpty bool
}

func newFingerprintProgressReporter(total int64, callback func(completed, total int64)) fingerprintProgressReporter {
	reporter := fingerprintProgressReporter{
		callback:      callback,
		total:         total,
		lastCompleted: -1,
	}
	if total > 0 {
		reporter.step = 1 + (total-1)/maxFingerprintProgressCallbacks
	}
	return reporter
}

func (r *fingerprintProgressReporter) report(completed int64) {
	if r == nil || r.callback == nil {
		return
	}
	if r.total <= 0 {
		if !r.reportedEmpty {
			r.reportedEmpty = true
			r.lastCompleted = 0
			r.callback(0, 0)
		}
		return
	}
	if completed < 0 {
		completed = 0
	}
	if completed > r.total {
		completed = r.total
	}
	bucket := completed / r.step
	if completed < r.total && bucket <= r.lastBucket {
		return
	}
	if completed == r.lastCompleted {
		return
	}
	r.lastBucket = bucket
	r.lastCompleted = completed
	r.callback(completed, r.total)
}

func fingerprintReaderAtWithChunksProgress(ctx context.Context, reader io.ReaderAt, size int64, progress func(completed, total int64)) ([sha256.Size]byte, int64, [][sha256.Size]byte, error) {
	var out [sha256.Size]byte
	if reader == nil {
		return out, 0, nil, errors.New("fingerprint reader is required")
	}
	if size < 0 {
		return out, 0, nil, errors.New("fingerprint size is negative")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	chunkSize, chunkCount, err := boundedFingerprintLayout(size)
	if err != nil {
		return out, 0, nil, err
	}
	reporter := newFingerprintProgressReporter(size, progress)
	if size == 0 {
		reporter.report(0)
	}
	chunks := make([][sha256.Size]byte, 0, chunkCount)
	digest := sha256.New()
	buffer := make([]byte, int(chunkSize))
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return out, 0, nil, err
		}
		want := chunkSize
		if remaining := size - offset; want > remaining {
			want = remaining
		}
		n, readErr := reader.ReadAt(buffer[:int(want)], offset)
		if n > 0 {
			_, _ = digest.Write(buffer[:n])
		}
		if n != int(want) {
			if readErr == nil {
				readErr = io.ErrUnexpectedEOF
			}
			return out, 0, nil, fmt.Errorf("fingerprint read at %d: got %d of %d bytes: %w", offset, n, want, readErr)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return out, 0, nil, readErr
		}
		chunks = append(chunks, sha256.Sum256(buffer[:n]))
		offset += int64(n)
		reporter.report(offset)
	}
	if err := ctx.Err(); err != nil {
		return out, 0, nil, err
	}
	copy(out[:], digest.Sum(nil))
	return out, chunkSize, chunks, nil
}

func boundedFingerprintLayout(size int64) (chunkSize int64, chunkCount int, err error) {
	if size < 0 {
		return 0, 0, errors.New("fingerprint size is negative")
	}
	maximumSize := int64(maxFingerprintChunks) * maxFingerprintChunkBytes
	if size > maximumSize {
		return 0, 0, fmt.Errorf("%w: %d bytes (maximum %d)", ErrSourceVerificationLimit, size, maximumSize)
	}
	chunkSize = fingerprintChunkBytes
	if size > int64(maxFingerprintChunks)*chunkSize {
		minimum := 1 + (size-1)/int64(maxFingerprintChunks)
		chunkSize = (1 + (minimum-1)/fingerprintChunkBytes) * fingerprintChunkBytes
	}
	if chunkSize > maxFingerprintChunkBytes {
		return 0, 0, fmt.Errorf("%w: computed chunk size %d", ErrSourceVerificationLimit, chunkSize)
	}
	if size == 0 {
		return chunkSize, 0, nil
	}
	count := 1 + (size-1)/chunkSize
	if count <= 0 || count > maxFingerprintChunks {
		return 0, 0, fmt.Errorf("%w: invalid chunk count %d", ErrSourceVerificationLimit, count)
	}
	return chunkSize, int(count), nil
}

func classifyFingerprintError(operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrSourceChanged, operation, err)
}

func sameState(left, right os.FileInfo) bool {
	return left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}
