package lineindex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/quarry/quarry-wails3/internal/newlines"
)

var ErrIndexEntryLimit = errors.New("line index exceeds the bounded anchor limit")

// Entry maps a known line number to a byte offset.
type Entry struct {
	Line   int64
	Offset int64
}

// Progress describes the current index scan state.
type Progress struct {
	Lines int64
	Bytes int64
	Done  bool
}

// Snapshot is a serializable representation of an index.
type Snapshot struct {
	EveryLines int64
	Entries    []Entry
	Lines      int64
	Bytes      int64
	Done       bool
}

// Index is a sparse line index.
// Store every Nth line to support approximate and later exact navigation.
type Index struct {
	EveryLines int64

	mu              sync.RWMutex
	entries         []Entry
	priorityEntries []Entry
	lines           int64
	bytes           int64
	done            bool
}

func New(everyLines int64) *Index {
	if everyLines <= 0 {
		everyLines = defaultEveryLines
	}
	return &Index{EveryLines: everyLines}
}

// FromSnapshot restores an index snapshot.
func FromSnapshot(s Snapshot) *Index {
	idx := New(s.EveryLines)
	_ = idx.RestoreSnapshot(s)
	return idx
}

// ValidateSnapshot rejects malformed or oversized persistent anchors before
// navigation code can rely on their ordering and ranges.
func ValidateSnapshot(s Snapshot, sourceBytes int64) error {
	if s.EveryLines <= 0 {
		return errors.New("line index stride must be positive")
	}
	if s.Lines < 0 || s.Bytes < 0 || sourceBytes < 0 || s.Bytes != sourceBytes {
		return errors.New("line index totals do not match the source")
	}
	if s.Lines > s.Bytes {
		return errors.New("line index has more line breaks than source bytes")
	}
	if len(s.Entries) > MaxIndexEntries {
		return fmt.Errorf("%w: %d > %d", ErrIndexEntryLimit, len(s.Entries), MaxIndexEntries)
	}
	if len(s.Entries) == 0 {
		if s.Bytes == 0 && s.Lines == 0 {
			return nil
		}
		return errors.New("non-empty line index is missing its first anchor")
	}
	expectedEntries := int64(1) + s.Lines/s.EveryLines
	if expectedEntries > int64(MaxIndexEntries) || int64(len(s.Entries)) != expectedEntries {
		return fmt.Errorf("line index anchor count %d does not match completed totals (want %d)", len(s.Entries), expectedEntries)
	}
	if s.Entries[0] != (Entry{Line: 1, Offset: 0}) {
		return errors.New("line index must begin at line 1, byte 0")
	}
	previous := Entry{}
	for i, entry := range s.Entries {
		if entry.Line <= 0 || entry.Offset < 0 || entry.Offset > sourceBytes || entry.Line > s.Lines+1 {
			return fmt.Errorf("line index anchor %d is out of range", i)
		}
		expectedLine := int64(1) + int64(i)*s.EveryLines
		if entry.Line != expectedLine {
			return fmt.Errorf("line index anchor %d has line %d, want %d for stride %d", i, entry.Line, expectedLine, s.EveryLines)
		}
		if i > 0 && (entry.Line <= previous.Line || entry.Offset <= previous.Offset) {
			return fmt.Errorf("line index anchor %d is not strictly monotonic", i)
		}
		previous = entry
	}
	return nil
}

// RestoreSnapshot imports a validated snapshot into the existing Index object,
// keeping the document's pointer stable for concurrent readers.
func (idx *Index) RestoreSnapshot(s Snapshot) error {
	if idx == nil {
		return errors.New("line index is required")
	}
	if s.EveryLines != idx.EveryLines {
		return fmt.Errorf("line index stride changed from %d to %d", idx.EveryLines, s.EveryLines)
	}
	if err := ValidateSnapshot(s, s.Bytes); err != nil {
		return err
	}
	entries := append([]Entry(nil), s.Entries...)
	return idx.restoreSnapshotOwned(Snapshot{
		EveryLines: s.EveryLines,
		Entries:    entries,
		Lines:      s.Lines,
		Bytes:      s.Bytes,
		Done:       s.Done,
	})
}

// RestoreSnapshotOwned imports a validated snapshot without copying its entry
// slice. The caller transfers ownership and must not retain or mutate Entries
// after this call. It exists for bounded streaming cache decoders, which
// already allocate a private, capped entry slice.
func (idx *Index) RestoreSnapshotOwned(s Snapshot) error {
	if idx == nil {
		return errors.New("line index is required")
	}
	if s.EveryLines != idx.EveryLines {
		return fmt.Errorf("line index stride changed from %d to %d", idx.EveryLines, s.EveryLines)
	}
	if err := ValidateSnapshot(s, s.Bytes); err != nil {
		return err
	}
	return idx.restoreSnapshotOwned(s)
}

func (idx *Index) restoreSnapshotOwned(s Snapshot) error {
	idx.mu.Lock()
	idx.entries = s.Entries
	idx.priorityEntries = nil
	idx.lines = s.Lines
	idx.bytes = s.Bytes
	idx.done = s.Done
	idx.mu.Unlock()
	return nil
}

// WithSnapshotView runs fn while holding the index read lock. The Entries
// slice in the supplied snapshot is a read-only view owned by the Index and is
// valid only for the duration of fn. This lets persistent encoders stream a
// completed index without Snapshot's full-slice copy.
func (idx *Index) WithSnapshotView(fn func(Snapshot) error) error {
	if idx == nil {
		return errors.New("line index is required")
	}
	if fn == nil {
		return errors.New("snapshot callback is required")
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return fn(Snapshot{
		EveryLines: idx.EveryLines,
		Entries:    idx.entries,
		Lines:      idx.lines,
		Bytes:      idx.bytes,
		Done:       idx.done,
	})
}

func (idx *Index) Entries() []Entry {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	out := make([]Entry, len(idx.entries))
	copy(out, idx.entries)
	return out
}

func (idx *Index) Snapshot() Snapshot {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	entries := make([]Entry, len(idx.entries))
	copy(entries, idx.entries)
	return Snapshot{
		EveryLines: idx.EveryLines,
		Entries:    entries,
		Lines:      idx.lines,
		Bytes:      idx.bytes,
		Done:       idx.done,
	}
}

func (idx *Index) Done() bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.done
}

func (idx *Index) Progress() Progress {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return Progress{Lines: idx.lines, Bytes: idx.bytes, Done: idx.done}
}

// MarkDone records a completed empty or externally scanned index state.
func (idx *Index) MarkDone(lines, bytes int64) {
	idx.setDone(lines, bytes)
}

// Build scans a reader sequentially and records sparse line offsets.
func (idx *Index) Build(ctx context.Context, r io.Reader) error {
	return idx.BuildWithEncoding(ctx, r, "UTF-8")
}

// BuildWithEncoding scans a reader sequentially and records sparse line offsets
// using encoding-aware newline detection.
func (idx *Index) BuildWithEncoding(ctx context.Context, r io.Reader, encodingName string) error {
	return idx.buildWithEncodingChunkSize(ctx, r, encodingName, 4*1024*1024)
}

func (idx *Index) buildWithEncodingChunkSize(ctx context.Context, r io.Reader, encodingName string, chunkSize int) error {
	if chunkSize <= 0 {
		return errors.New("line-index chunk size must be positive")
	}
	buf := make([]byte, chunkSize)
	var offset int64
	var line int64
	scanner := newlines.New(encodingName)

	if !idx.tryAddExact(Entry{Line: 1, Offset: 0}) {
		return ErrIndexEntryLimit
	}
	var scanErr error
	emit := func(br newlines.Break) bool {
		line++
		if line%idx.EveryLines == 0 {
			if !idx.tryAddExact(Entry{Line: line + 1, Offset: br.End}) {
				scanErr = ErrIndexEntryLimit
				return false
			}
		}
		return true
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := r.Read(buf)
		if n > 0 {
			if !scanner.Scan(buf[:n], offset, emit) {
				return scanErr
			}
			offset += int64(n)
			idx.setProgress(line, offset)
		}

		if errors.Is(err, io.EOF) {
			if !scanner.Finish(emit) {
				return scanErr
			}
			idx.setDone(line, offset)
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// AddPriorityEntry records an approximate sparse anchor near the active viewport.
func (idx *Index) AddPriorityEntry(e Entry) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if len(idx.priorityEntries) >= maxPriorityIndexEntries {
		return
	}
	insertEntry(&idx.priorityEntries, e)
}

// ApproxLineToOffset returns the nearest known entry <= requested line.
func (idx *Index) ApproxLineToOffset(line int64) (Entry, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	best, ok := floorEntryByLine(idx.entries, line)
	priorityBest, priorityOK := floorEntryByLine(idx.priorityEntries, line)
	switch {
	case !ok && !priorityOK:
		return Entry{}, false
	case !ok:
		return priorityBest, true
	case !priorityOK:
		return best, true
	case priorityBest.Line > best.Line:
		return priorityBest, true
	case priorityBest.Line == best.Line && priorityBest.Offset > best.Offset:
		return priorityBest, true
	default:
		return best, true
	}
}

// ApproxOffsetToLine estimates the line number at or before an offset.
func (idx *Index) ApproxOffsetToLine(offset int64) (Entry, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	best, ok := floorEntryByOffset(idx.entries, offset)
	priorityBest, priorityOK := floorEntryByOffset(idx.priorityEntries, offset)
	switch {
	case !ok && !priorityOK:
		return Entry{}, false
	case !ok:
		best = priorityBest
	case priorityOK && priorityBest.Offset > best.Offset:
		best = priorityBest
	}

	estimated := best.Line
	if idx.bytes > 0 && idx.lines > 0 && offset > best.Offset {
		avgBytesPerLine := float64(idx.bytes) / float64(idx.lines+1)
		if avgBytesPerLine > 0 {
			estimated += int64(float64(offset-best.Offset) / avgBytesPerLine)
		}
	}
	if estimated < best.Line {
		estimated = best.Line
	}
	return Entry{Line: estimated, Offset: offset}, true
}

// FloorOffsetEntry returns the nearest exact sparse anchor at or before offset.
func (idx *Index) FloorOffsetEntry(offset int64) (Entry, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	return floorEntryByOffset(idx.entries, offset)
}

func (idx *Index) tryAddExact(e Entry) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if len(idx.entries) >= MaxIndexEntries {
		return false
	}
	insertEntry(&idx.entries, e)
	return true
}

func (idx *Index) setProgress(lines, bytes int64) {
	idx.mu.Lock()
	idx.lines = lines
	idx.bytes = bytes
	idx.mu.Unlock()
}

func (idx *Index) setDone(lines, bytes int64) {
	idx.mu.Lock()
	idx.lines = lines
	idx.bytes = bytes
	idx.done = true
	idx.priorityEntries = nil
	idx.mu.Unlock()
}

func floorEntryByLine(entries []Entry, line int64) (Entry, bool) {
	if len(entries) == 0 {
		return Entry{}, false
	}
	i := sort.Search(len(entries), func(i int) bool {
		return entries[i].Line > line
	})
	if i == 0 {
		return Entry{}, false
	}
	return entries[i-1], true
}

func floorEntryByOffset(entries []Entry, offset int64) (Entry, bool) {
	if len(entries) == 0 {
		return Entry{}, false
	}
	if offset <= 0 {
		return entries[0], true
	}
	i := sort.Search(len(entries), func(i int) bool {
		return entries[i].Offset > offset
	})
	if i == 0 {
		return entries[0], true
	}
	return entries[i-1], true
}

func insertEntry(dst *[]Entry, e Entry) {
	entries := *dst
	i := sort.Search(len(entries), func(i int) bool {
		if entries[i].Line != e.Line {
			return entries[i].Line >= e.Line
		}
		return entries[i].Offset >= e.Offset
	})
	if i < len(entries) && entries[i] == e {
		return
	}

	entries = append(entries, Entry{})
	copy(entries[i+1:], entries[i:])
	entries[i] = e
	*dst = entries
}
