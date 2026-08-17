package manualedit

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/quarry/quarry-wails3/internal/document"
)

var sessionIdentityAllocator struct {
	sync.Mutex
	last uint64
}

var ErrSessionIdentityExhausted = errors.New("manual-edit session identity space exhausted")

type sessionEdit struct {
	edit        Edit
	expectedOld []byte
	verified    bool
}

// Session is a single-owner mutable staging model. Callers must confine it to
// one goroutine/UI owner or provide external synchronization before mutating it.
type Session struct {
	identity     uint64
	originalSize int64
	limits       Limits
	history      []sessionEdit
	cursor       int
	revision     uint64
	table        *PieceTable
	binding      *sourceBinding
}

// NewSession creates an unbound staging model for in-memory/internal callers.
// A session used by WriteSessionToFile must instead be created with
// NewSourceBoundSession so stale offsets can never be published.
func NewSession(size int64, maxInsertedBytes int64) *Session {
	session, err := NewSessionWithLimits(size, legacyLimits(maxInsertedBytes))
	if err != nil {
		// legacyLimits always returns a complete positive policy. Keep this API
		// allocation-free on invalid sizes and fail later rather than panic.
		return &Session{originalSize: max64(size, 0), limits: DefaultLimits(), table: NewPieceTable(max64(size, 0))}
	}
	return session
}

func NewSessionWithLimits(size int64, limits Limits) (*Session, error) {
	if size < 0 {
		return nil, errors.New("source size must be non-negative")
	}
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	identity, err := nextSessionIdentity()
	if err != nil {
		return nil, err
	}
	s := &Session{identity: identity, originalSize: size, limits: limits}
	s.table = NewPieceTable(size)
	s.table.setLimits(limits)
	return s, nil
}

func nextSessionIdentity() (uint64, error) {
	sessionIdentityAllocator.Lock()
	defer sessionIdentityAllocator.Unlock()
	if sessionIdentityAllocator.last == ^uint64(0) {
		return 0, ErrSessionIdentityExhausted
	}
	sessionIdentityAllocator.last++
	return sessionIdentityAllocator.last, nil
}

// Identity is a process-local, monotonic token for this exact Session object.
// Unlike a Session pointer, it is safe to retain after releasing a registry
// lease and lets dialog approval code reject a discarded/recreated session.
func (s *Session) Identity() uint64 {
	if s == nil {
		return 0
	}
	return s.identity
}

// ApplyEdit is available only for unbound in-memory sessions. Production file
// sessions must call ApplyVerifiedEdit and retain the exact bytes that were
// observed before the edit.
func (s *Session) ApplyEdit(edit Edit) error {
	if s == nil {
		return errors.New("session is required")
	}
	if s.binding != nil {
		return ErrExpectedBytesRequired
	}
	return s.applyEdit(sessionEdit{edit: edit})
}

// ApplyVerifiedEdit stages an edit only when expectedOld exactly matches the
// current transformed bytes. The expected bytes are retained in bounded undo
// history and checked again against the source during Save Copy.
func (s *Session) ApplyVerifiedEdit(edit Edit, expectedOld []byte) error {
	return s.ApplyVerifiedEditContext(context.Background(), edit, expectedOld)
}

// ApplyVerifiedEditContext is the source-bound, cancellable form of
// ApplyVerifiedEdit. It verifies original spans against the exact source
// fingerprint captured when the session was prepared rather than trusting a
// raw retained reader whose bytes may have changed externally.
func (s *Session) ApplyVerifiedEditContext(ctx context.Context, edit Edit, expectedOld []byte) error {
	if s == nil {
		return errors.New("session is required")
	}
	if edit.Start < 0 || edit.End < edit.Start || edit.End > s.Size() || int64(len(expectedOld)) != edit.End-edit.Start {
		return fmt.Errorf("%w: edit range [%d,%d) has %d expected bytes", ErrExpectedBytesMismatch, edit.Start, edit.End, len(expectedOld))
	}
	reader := s.boundReader()
	if s.binding != nil {
		var err error
		reader, err = s.verifiedSourceReader(ctx)
		if err != nil {
			return err
		}
	}
	matches, err := s.table.rangeEquals(reader, edit.Start, edit.End, expectedOld)
	if err != nil {
		return err
	}
	if !matches {
		return ErrExpectedBytesMismatch
	}
	if s.binding != nil {
		if err := s.validateBoundSourceState(); err != nil {
			return err
		}
	}
	return s.applyEdit(sessionEdit{edit: edit, expectedOld: expectedOld, verified: true})
}

func (s *Session) boundReader() document.ReaderAtSize {
	if s != nil && s.binding != nil {
		return s.binding.reader
	}
	return sessionTableReader{s: s}
}

// sessionTableReader is used only by unbound verified unit/internal callers.
// It intentionally cannot supply original source bytes, so a non-empty source
// range fails rather than accepting unverifiable expectations.
type sessionTableReader struct{ s *Session }

func (r sessionTableReader) ReadAt([]byte, int64) (int, error) {
	return 0, ErrExpectedBytesRequired
}
func (r sessionTableReader) Size() int64 {
	if r.s == nil {
		return 0
	}
	return r.s.originalSize
}

func (s *Session) applyEdit(edit sessionEdit) error {
	if s.revision == ^uint64(0) {
		return ErrSessionRevisionExhausted
	}
	prefix := s.cursor
	nextDepth := prefix + 1
	if nextDepth > s.limits.MaxEditCount {
		return ErrEditCountLimit
	}
	if nextDepth > s.limits.MaxHistoryDepth {
		return ErrHistoryDepthLimit
	}

	historyBytes, err := historyBytesFor(s.history[:prefix])
	if err != nil {
		return err
	}
	editBytes, err := historyEditBytes(edit)
	if err != nil {
		return err
	}
	historyBytes, err = checkedMemorySum(historyBytes, editBytes)
	if err != nil || historyBytes > s.limits.MaxHistoryBytes {
		return ErrHistoryBytesLimit
	}

	prospective := make([]sessionEdit, 0, nextDepth)
	prospective = append(prospective, s.history[:prefix]...)
	prospective = append(prospective, edit)
	if err := s.preflightRebuild(nextDepth, prospective, historyBytes); err != nil {
		return err
	}

	table, err := s.buildTable(nextDepth, prospective)
	if err != nil {
		return err
	}

	// Clone only after every cap and rebuild has succeeded. A rejected edit is
	// therefore transactional and cannot discard redo history or retain bytes.
	prospective[len(prospective)-1] = cloneSessionEdit(edit)
	s.history = prospective
	s.cursor = nextDepth
	s.table = table
	s.revision++
	return nil
}

func (s *Session) Undo() error {
	if s == nil {
		return errors.New("session is required")
	}
	if !s.CanUndo() {
		return nil
	}
	if s.revision == ^uint64(0) {
		return ErrSessionRevisionExhausted
	}
	nextCursor := s.cursor - 1
	historyBytes, err := historyBytesFor(s.history)
	if err != nil {
		return err
	}
	if err := s.preflightRebuild(nextCursor, s.history, historyBytes); err != nil {
		return err
	}
	table, err := s.buildTable(nextCursor, s.history)
	if err != nil {
		return err
	}
	s.cursor = nextCursor
	s.table = table
	s.revision++
	return nil
}

func (s *Session) Redo() error {
	if s == nil {
		return errors.New("session is required")
	}
	if !s.CanRedo() {
		return nil
	}
	if s.revision == ^uint64(0) {
		return ErrSessionRevisionExhausted
	}
	nextCursor := s.cursor + 1
	historyBytes, err := historyBytesFor(s.history)
	if err != nil {
		return err
	}
	if err := s.preflightRebuild(nextCursor, s.history, historyBytes); err != nil {
		return err
	}
	table, err := s.buildTable(nextCursor, s.history)
	if err != nil {
		return err
	}
	s.cursor = nextCursor
	s.table = table
	s.revision++
	return nil
}

func (s *Session) CanUndo() bool { return s != nil && s.cursor > 0 }

func (s *Session) CanRedo() bool { return s != nil && s.cursor < len(s.history) }

func (s *Session) EditCount() int {
	if s == nil {
		return 0
	}
	return s.cursor
}

// Revision is a monotonic identity for the staged state. It changes after
// every successful Apply, Undo, or Redo, including transitions that happen to
// produce the same edit count or byte length.
func (s *Session) Revision() uint64 {
	if s == nil {
		return 0
	}
	return s.revision
}

func (s *Session) ModifiedRanges() []Range {
	if s == nil || s.table == nil {
		return nil
	}
	return s.table.ModifiedRanges()
}

func (s *Session) ActiveEdits() []Edit {
	if s == nil || s.cursor == 0 {
		return nil
	}
	out := make([]Edit, 0, s.cursor)
	for i := 0; i < s.cursor; i++ {
		out = append(out, cloneEdit(s.history[i].edit))
	}
	return out
}

// SourceMappedActiveEdits returns active edits anchored to original source offsets for viewport rendering.
func (s *Session) SourceMappedActiveEdits() []Edit {
	if s == nil || s.cursor == 0 {
		return nil
	}
	table := NewPieceTable(s.originalSize)
	table.setLimits(s.limits)
	out := make([]Edit, 0, s.cursor)
	for i := 0; i < s.cursor; i++ {
		edit := cloneEdit(s.history[i].edit)
		sourceRange := table.sourceRangeForTransformedRange(edit.Start, edit.End)
		out = append(out, Edit{Start: sourceRange.Start, End: sourceRange.End, Text: append([]byte(nil), edit.Text...)})
		if err := table.Replace(edit.Start, edit.End, edit.Text); err != nil {
			return nil
		}
	}
	return out
}

// SourceMappedModifiedRanges returns source-offset ranges suitable for overview and navigation markers.
func (s *Session) SourceMappedModifiedRanges() []Range {
	edits := s.SourceMappedActiveEdits()
	if len(edits) == 0 {
		return nil
	}
	table := &PieceTable{}
	for _, edit := range edits {
		table.recordModifiedRange(edit.Start, edit.End)
	}
	return table.ModifiedRanges()
}

// SourceRangeToTransformed maps an original source byte range into the current
// transformed session coordinate space. Callers that stage source-anchored
// patches after earlier edits must use this before ApplyEdit.
func (s *Session) SourceRangeToTransformed(start int64, end int64) (Range, bool) {
	if s == nil || s.table == nil {
		return Range{}, false
	}
	return s.table.sourceRangeToTransformedRange(start, end)
}

// TransformedRangeToSource maps a range in the edited view to the conservative
// source span that corresponds to it. Ranges wholly or partly inside inserted
// text include the adjacent replaced-source gap; pure insertions map to a
// zero-width source span. Callers must bound the returned source span before
// reading it.
func (s *Session) TransformedRangeToSource(start int64, end int64) (Range, bool) {
	if s == nil || s.table == nil {
		return Range{}, false
	}
	return s.table.transformedRangeToSourceRange(start, end)
}

func (s *Session) Size() int64 {
	if s == nil || s.table == nil {
		return 0
	}
	return s.table.Size()
}

// OriginalSize is the source document's byte size (before edits).
func (s *Session) OriginalSize() int64 {
	if s == nil {
		return 0
	}
	return s.originalSize
}

// ReadRange returns the transformed (edited) bytes in [start, end), reading
// unedited spans from src. This compatibility helper is for unbound/internal
// sessions; source-bound production sessions use ReadRangeContext so original
// spans are checked against the captured source fingerprint.
func (s *Session) ReadRange(src document.ReaderAtSize, start int64, end int64) ([]byte, error) {
	if s == nil || s.table == nil {
		return nil, errors.New("session is required")
	}
	return s.table.ReadRange(src, start, end)
}

func (s *Session) HasEdits() bool { return s != nil && s.cursor > 0 }

func (s *Session) buildTable(cursor int, history []sessionEdit) (*PieceTable, error) {
	table := NewPieceTable(s.originalSize)
	table.setLimits(s.limits)
	for i := 0; i < cursor; i++ {
		if err := table.Replace(history[i].edit.Start, history[i].edit.End, history[i].edit.Text); err != nil {
			return nil, err
		}
	}
	return table, nil
}

func (s *Session) preflightRebuild(cursor int, history []sessionEdit, retainedHistoryBytes int64) error {
	transient, err := s.estimatedTransientBytes(cursor, history, retainedHistoryBytes)
	if err != nil || transient > s.limits.MaxTransientBytes {
		return ErrTransientMemoryLimit
	}
	return nil
}

func (s *Session) estimatedTransientBytes(cursor int, history []sessionEdit, retainedHistoryBytes int64) (int64, error) {
	if cursor < 0 || cursor > len(history) {
		return 0, errors.New("invalid edit history cursor")
	}
	currentTableBytes, err := s.table.residentBytes()
	if err != nil {
		return 0, err
	}
	var activeTextBytes int64
	for i := 0; i < cursor; i++ {
		activeTextBytes, err = checkedMemorySum(activeTextBytes, int64(len(history[i].edit.Text)))
		if err != nil {
			return 0, ErrTransientMemoryLimit
		}
	}
	worstPieces := int64(1)
	if cursor > 0 {
		if int64(cursor) > (int64(maxInt())-1)/2 {
			return 0, ErrTransientMemoryLimit
		}
		worstPieces += int64(cursor) * 2
	}
	newTableBytes, err := checkedMemorySum(
		activeTextBytes,
		worstPieces*pieceAccountingBytes,
		int64(cursor)*rangeAccountingBytes,
	)
	if err != nil {
		return 0, ErrTransientMemoryLimit
	}
	// PieceTable.Replace is transactional: the old table, candidate piece
	// slice, normalized slice, and compacted added buffer overlap briefly.
	// Four times the final conservative table estimate bounds that rebuild peak.
	if newTableBytes > int64(maxInt64())/4 {
		return 0, ErrTransientMemoryLimit
	}
	rebuildPeakBytes := newTableBytes * 4
	currentHistoryBytes, err := historyBytesFor(s.history)
	if err != nil {
		return 0, ErrTransientMemoryLimit
	}
	transient, err := checkedMemorySum(
		currentTableBytes,
		currentHistoryBytes,
		retainedHistoryBytes,
		rebuildPeakBytes,
		int64(len(history))*sliceAccountingBytes,
		sourceCompareBufferBytes,
		sourceWriteBufferBytes,
	)
	if err != nil {
		return 0, ErrTransientMemoryLimit
	}
	if s.binding != nil && s.binding.expected != nil {
		retainedBytes, verificationBufferBytes := s.binding.expected.MemoryBounds()
		transient, err = checkedMemorySum(transient, retainedBytes, verificationBufferBytes)
	}
	if err != nil {
		return 0, ErrTransientMemoryLimit
	}
	return transient, nil
}

func maxInt64() int64 { return int64(^uint64(0) >> 1) }

func historyEditBytes(edit sessionEdit) (int64, error) {
	return checkedMemorySum(historyAccountingBytes, int64(len(edit.edit.Text)), int64(len(edit.expectedOld)))
}

func historyBytesFor(history []sessionEdit) (int64, error) {
	var total int64
	for _, edit := range history {
		bytes, err := historyEditBytes(edit)
		if err != nil {
			return 0, ErrHistoryBytesLimit
		}
		total, err = checkedMemorySum(total, bytes)
		if err != nil {
			return 0, ErrHistoryBytesLimit
		}
	}
	return total, nil
}

func cloneSessionEdit(edit sessionEdit) sessionEdit {
	return sessionEdit{
		edit:        cloneEdit(edit.edit),
		expectedOld: append([]byte(nil), edit.expectedOld...),
		verified:    edit.verified,
	}
}

func cloneEdit(edit Edit) Edit {
	out := edit
	if edit.Text != nil {
		out.Text = append([]byte(nil), edit.Text...)
	}
	return out
}
