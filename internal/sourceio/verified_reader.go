package sourceio

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/quarry/quarry-wails3/internal/document"
)

// VerifiedDocumentReader is a bounded-memory ReaderAt view of one exact source
// expectation. Each newly encountered block is read into an operation-owned
// buffer and checked against the expectation before any bytes are returned to a
// transform. The most recently verified block remains cached in that same
// buffer so repeated or overlapping reads do not reread or rehash it. A
// temporary external rewrite therefore cannot contaminate an artifact and be
// hidden merely by restoring the source before final publication validation.
type VerifiedDocumentReader struct {
	ctx      context.Context
	expected *Expectation
	doc      *document.FileDocument

	readAt func([]byte, int64) (int, error)
	sum256 func([]byte) [sha256.Size]byte

	mu          sync.Mutex
	buffer      []byte
	cachedChunk int64
	cacheValid  bool
}

// NewVerifiedDocumentReader binds random-access transform reads to expected.
// It performs only identity/generation checks here; the exact block hashes were
// captured by ExpectDocumentContext and are checked as bytes are consumed.
func NewVerifiedDocumentReader(ctx context.Context, expected *Expectation, doc *document.FileDocument) (*VerifiedDocumentReader, error) {
	if expected == nil || doc == nil {
		return nil, errors.New("source expectation and document are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if doc.Path() != expected.path || doc.Size() != expected.info.Size() {
		return nil, fmt.Errorf("%w: retained document does not match the verified reader expectation", ErrSourceChanged)
	}
	if expected.chunkSize <= 0 {
		return nil, errors.New("source expectation has no block verification layout")
	}
	wantChunks := 0
	if doc.Size() > 0 {
		wantChunks = int(1 + (doc.Size()-1)/expected.chunkSize)
	}
	if wantChunks != len(expected.chunkDigests) {
		return nil, errors.New("source expectation block map is incomplete")
	}
	if err := doc.ValidateUnchanged(); err != nil {
		return nil, fmt.Errorf("%w: retained document validation failed: %w", ErrSourceChanged, err)
	}
	return &VerifiedDocumentReader{
		ctx:      ctx,
		expected: expected,
		doc:      doc,
		readAt:   doc.ReadAt,
		sum256:   sha256.Sum256,
	}, nil
}

func (r *VerifiedDocumentReader) Size() int64 {
	if r == nil || r.expected == nil || r.expected.info == nil {
		return 0
	}
	return r.expected.info.Size()
}

func (r *VerifiedDocumentReader) ReadAt(dst []byte, offset int64) (int, error) {
	if r == nil || r.expected == nil || r.doc == nil {
		return 0, errors.New("verified source reader is required")
	}
	if offset < 0 {
		return 0, errors.New("negative source read offset")
	}
	if len(dst) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}

	size := r.Size()
	if offset >= size {
		return 0, io.EOF
	}
	want := int64(len(dst))
	truncated := false
	if available := size - offset; want > available {
		want = available
		truncated = true
	}
	if int64(cap(r.buffer)) < r.expected.chunkSize {
		r.buffer = make([]byte, int(r.expected.chunkSize))
	}
	r.buffer = r.buffer[:int(r.expected.chunkSize)]

	written := 0
	end := offset + want
	for current := offset; current < end; {
		if err := r.ctx.Err(); err != nil {
			return written, err
		}
		chunkIndex := current / r.expected.chunkSize
		chunkStart := chunkIndex * r.expected.chunkSize
		chunkEnd := chunkStart + r.expected.chunkSize
		if chunkEnd > size {
			chunkEnd = size
		}
		chunkLength := chunkEnd - chunkStart
		buffer := r.buffer[:int(chunkLength)]
		if !r.cacheValid || r.cachedChunk != chunkIndex {
			r.cacheValid = false
			readAt := r.readAt
			if readAt == nil {
				readAt = r.doc.ReadAt
			}
			n, readErr := readAt(buffer, chunkStart)
			if n != len(buffer) {
				if readErr == nil {
					readErr = io.ErrUnexpectedEOF
				}
				return written, fmt.Errorf("%w: verified block %d read %d of %d bytes: %v", ErrSourceChanged, chunkIndex, n, len(buffer), readErr)
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return written, fmt.Errorf("%w: verified block %d read failed: %v", ErrSourceChanged, chunkIndex, readErr)
			}
			sum256 := r.sum256
			if sum256 == nil {
				sum256 = sha256.Sum256
			}
			if got := sum256(buffer); got != r.expected.chunkDigests[chunkIndex] {
				return written, fmt.Errorf("%w: source block %d differs from the captured generation", ErrSourceChanged, chunkIndex)
			}
			r.cachedChunk = chunkIndex
			r.cacheValid = true
		}

		copyStart := current - chunkStart
		copyEnd := chunkLength
		if end < chunkEnd {
			copyEnd = end - chunkStart
		}
		copied := copy(dst[written:], buffer[int(copyStart):int(copyEnd)])
		written += copied
		current += int64(copied)
		if copied == 0 {
			return written, io.ErrNoProgress
		}
	}
	if err := r.ctx.Err(); err != nil {
		return written, err
	}
	if truncated {
		return written, io.EOF
	}
	return written, nil
}
