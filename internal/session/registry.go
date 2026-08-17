// Package session owns opened editor documents and serializes their lifecycle.
// Registry leases are the boundary that prevents Close/Reopen from closing a
// document while an RPC is still using that generation.
package session

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/manualedit"
)

var (
	ErrUnknownFile             = errors.New("session: unknown file id")
	ErrFileClosing             = errors.New("session: file is closing")
	ErrTransitioning           = errors.New("session: file lifecycle transition is in progress")
	ErrInvalidLease            = errors.New("session: invalid operation lease")
	ErrOpenFileLimit           = errors.New("session: open file limit reached")
	ErrOpenInFlightLimit       = errors.New("session: concurrent open limit reached")
	ErrFileIDSequenceExhausted = errors.New("session: file id sequence is exhausted")
	ErrFileGenerationExhausted = errors.New("session: file generation sequence is exhausted")
	ErrRegistryStopped         = errors.New("session: registry is shutting down")
	ErrReopenCommitted         = errors.New("session: reopen committed with a previous-generation cleanup failure")
)

// ReopenCommittedError reports that a replacement generation is already the
// registry's current owner, but closing the previous generation returned an
// error. Reopen callers must process the accompanying non-nil *File as committed
// before surfacing this error; rolling back is impossible because Close consumes
// the previous document handle even when the platform reports a close failure.
type ReopenCommittedError struct {
	FileID string
	Err    error
}

func (e *ReopenCommittedError) Error() string {
	if e == nil {
		return ErrReopenCommitted.Error()
	}
	if e.FileID == "" {
		return fmt.Sprintf("%v: %v", ErrReopenCommitted, e.Err)
	}
	return fmt.Sprintf("%v for %q: %v", ErrReopenCommitted, e.FileID, e.Err)
}

func (e *ReopenCommittedError) Unwrap() []error {
	if e == nil || e.Err == nil {
		return []error{ErrReopenCommitted}
	}
	return []error{ErrReopenCommitted, e.Err}
}

// DefaultMaxOpenFiles bounds retained document handles, per-file caches, and
// background indexers. Overflow is rejected instead of evicting a session,
// because eviction could discard staged edits or invalidate an active lease.
const (
	DefaultMaxOpenFiles       = 128
	DefaultMaxConcurrentOpens = 4

	// File generations cross the JSON bridge as numbers and are echoed back by
	// generation-bound CSV operations. JavaScript can compare them exactly only
	// through Number.MAX_SAFE_INTEGER.
	MaxBridgeFileGeneration uint64 = 1<<53 - 1
)

// File is one immutable document generation plus generation-owned staged
// edits. Edit and ResetEdits require an exclusive Registry lease. Doc reads
// require a read or exclusive lease for the entire higher-level operation.
type File struct {
	ID   string
	Doc  *document.FileDocument
	Path string
	Edit *manualedit.Session

	generation  uint64
	indexMu     sync.Mutex
	cancelIndex context.CancelFunc
}

// FileSnapshot identifies one exact installed document generation. A snapshot
// obtained from Lease.Snapshot remains alive until that lease is released.
// Registry.Snapshot is retained only for compatibility and does not itself
// keep the document alive.
type FileSnapshot struct {
	ID         string
	Doc        *document.FileDocument
	Path       string
	Generation uint64
}

// EditSession returns the source-bound staging session, creating it on first
// use. Capturing its full streaming source fingerprint can take O(file size)
// time but is memory-bounded. The caller must hold an exclusive Registry lease.
func (f *File) EditSession() (*manualedit.Session, error) {
	return f.EditSessionContext(context.Background())
}

// EditSessionContext creates the first source-bound edit session with a
// cancellable full-source fingerprint. The caller must hold an exclusive
// Registry lease for the duration of this call.
func (f *File) EditSessionContext(ctx context.Context) (*manualedit.Session, error) {
	return f.EditSessionContextWithProgress(ctx, nil)
}

// EditSessionContextWithProgress creates the first source-bound session while
// forwarding the source layer's bounded fingerprint progress. Existing edit
// sessions return immediately and never replay the full-source progress pass.
func (f *File) EditSessionContextWithProgress(ctx context.Context, progress func(completed, total int64)) (*manualedit.Session, error) {
	if f.Edit == nil {
		edit, err := manualedit.NewSourceBoundSessionContextWithProgress(ctx, f.Doc, f.Path, f.generation, manualedit.DefaultLimits(), progress)
		if err != nil {
			return nil, err
		}
		f.Edit = edit
	}
	return f.Edit, nil
}

// ResetEdits discards staged edits. The caller must hold an exclusive lease.
func (f *File) ResetEdits() { f.Edit = nil }

type registryEntry struct {
	mu             sync.Mutex
	cond           *sync.Cond
	file           *File
	readers        int
	writer         bool
	waitingWriters int
	closing        bool
	transition     *Transition
}

func newRegistryEntry(file *File) *registryEntry {
	entry := &registryEntry{file: file}
	entry.cond = sync.NewCond(&entry.mu)
	return entry
}

// Registry tracks open files by opaque id.
//
// Lock ordering is strict:
//  1. Registry.mu
//  2. registryEntry.mu
//  3. service-owned locks such as sqlMu/jobManager.mu, only after no registry
//     or entry mutex is held
//  4. FileDocument's internal lifecycle/index locks
//
// Lease holders do not retain registryEntry.mu; they own a reader/writer count.
// Close/refresh therefore mark their transition first, release registry locks,
// cancel service work, and only then wait for leases to drain.
type Registry struct {
	mu         sync.Mutex
	files      map[string]*registryEntry
	seq        int64
	maxOpen    int
	maxOpening int
	openCount  int

	// stopping is an irreversible admission barrier. shutdownDone is created
	// while holding mu at the same instant the barrier is raised, so Open,
	// lease acquisition, and lifecycle transitions cannot slip in after the
	// shutdown snapshot has been taken.
	stopping     bool
	shutdownDone chan struct{}
	shutdownErr  error
	opening      int
	openingDone  chan struct{}

	// closing retains every detached CloseHandle until its exact document has
	// actually closed. BeginShutdown snapshots this set as well as files, so a
	// close that won the registry lock just before the shutdown barrier cannot
	// fall outside the shutdown drain.
	closing map[*CloseHandle]struct{}

	// replacedDocumentCloseResult is an instance-local fault-injection seam. The
	// real document Close always runs first; production leaves this nil.
	replacedDocumentCloseResult func(error) error
}

func New() *Registry { return NewWithLimit(DefaultMaxOpenFiles) }

// NewWithLimit constructs a registry with a positive retained-handle limit.
// It is primarily useful for focused lifecycle tests; production uses New.
func NewWithLimit(maxOpen int) *Registry {
	if maxOpen <= 0 {
		maxOpen = DefaultMaxOpenFiles
	}
	maxOpening := DefaultMaxConcurrentOpens
	if maxOpening > maxOpen {
		maxOpening = maxOpen
	}
	return &Registry{files: make(map[string]*registryEntry), maxOpen: maxOpen, maxOpening: maxOpening}
}

// Open opens path and returns the one registry File whose retained handle
// identifies that source. Alias spellings are deduplicated without replacing
// the exact path spelling from the first successful open.
func (r *Registry) Open(path string) (*File, error) {
	file, _, err := r.OpenWithStatus(path)
	return file, err
}

// OpenWithStatus is Open plus an added result. added is true only when this
// call installed a new session and the caller must start its background index.
// A redundant candidate handle is always closed before the existing File is
// returned.
func (r *Registry) OpenWithStatus(path string) (*File, bool, error) {
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return nil, false, ErrRegistryStopped
	}
	if r.opening >= r.maxOpening {
		limit := r.maxOpening
		r.mu.Unlock()
		return nil, false, fmt.Errorf("%w (%d)", ErrOpenInFlightLimit, limit)
	}
	r.opening++
	r.mu.Unlock()
	defer r.finishOpening()

	doc, err := document.OpenFile(path)
	if err != nil {
		return nil, false, err
	}
	candidateInfo, err := doc.OpenedFileInfo()
	if err != nil {
		_ = doc.Close()
		return nil, false, fmt.Errorf("session: stat opened file %q: %w", path, err)
	}

	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		closeErr := doc.Close()
		return nil, false, errors.Join(ErrRegistryStopped, closeErr)
	}
	for _, entry := range r.files {
		entry.mu.Lock()
		existing := entry.file
		available := !entry.closing && existing != nil && existing.Doc != nil
		if available {
			existingInfo, statErr := existing.Doc.OpenedFileInfo()
			if statErr != nil {
				entry.mu.Unlock()
				r.mu.Unlock()
				closeErr := doc.Close()
				return nil, false, errors.Join(
					fmt.Errorf("session: stat existing opened file %q: %w", existing.Path, statErr),
					closeErr,
				)
			}
			if os.SameFile(existingInfo, candidateInfo) {
				entry.mu.Unlock()
				r.mu.Unlock()
				if closeErr := doc.Close(); closeErr != nil {
					return nil, false, fmt.Errorf("session: close redundant handle for %q: %w", path, closeErr)
				}
				return existing, false, nil
			}
		}
		entry.mu.Unlock()
	}
	if r.openCount >= r.maxOpen {
		limit := r.maxOpen
		r.mu.Unlock()
		closeErr := doc.Close()
		return nil, false, errors.Join(
			fmt.Errorf("%w (%d)", ErrOpenFileLimit, limit),
			closeErr,
		)
	}
	// File IDs are part of the public RPC contract and accept only positive
	// signed-64-bit decimal sequences. Refuse the first ID that cannot be
	// represented by that grammar; wrapping would install an unreachable file
	// under an ID that the service's own validator rejects.
	if r.seq == math.MaxInt64 {
		r.mu.Unlock()
		closeErr := doc.Close()
		return nil, false, errors.Join(ErrFileIDSequenceExhausted, closeErr)
	}
	r.seq++
	file := &File{ID: fmt.Sprintf("f%d", r.seq), Doc: doc, Path: path, generation: 1}
	r.files[file.ID] = newRegistryEntry(file)
	r.openCount++
	r.mu.Unlock()
	return file, true, nil
}

// Lease owns one stable File generation. Release is idempotent. Callers must
// not retain File/Doc/Edit pointers after Release.
type Lease struct {
	entry      *registryEntry
	file       *File
	snapshot   FileSnapshot
	exclusive  bool
	transition *Transition
	once       sync.Once
}

func (l *Lease) File() *File {
	if l == nil {
		return nil
	}
	return l.file
}

func (l *Lease) Snapshot() FileSnapshot {
	if l == nil {
		return FileSnapshot{}
	}
	return l.snapshot
}

func (l *Lease) Exclusive() bool { return l != nil && l.exclusive }

func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		entry := l.entry
		if entry == nil {
			return
		}
		entry.mu.Lock()
		if l.exclusive {
			if entry.writer {
				entry.writer = false
			}
			if l.transition != nil && entry.transition == l.transition {
				entry.transition = nil
			}
		} else if entry.readers > 0 {
			entry.readers--
		}
		entry.cond.Broadcast()
		entry.mu.Unlock()
	})
}

func snapshotOf(file *File) FileSnapshot {
	if file == nil {
		return FileSnapshot{}
	}
	return FileSnapshot{ID: file.ID, Doc: file.Doc, Path: file.Path, Generation: file.generation}
}

func (r *Registry) lockEntry(id string) (*registryEntry, error) {
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return nil, ErrRegistryStopped
	}
	entry, ok := r.files[id]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w %q", ErrUnknownFile, id)
	}
	entry.mu.Lock()
	r.mu.Unlock()
	return entry, nil
}

// AcquireRead returns a stable document-generation lease. It is the
// compatibility wrapper for callers without a cancellation source.
func (r *Registry) AcquireRead(id string) (*Lease, error) {
	return r.AcquireReadContext(context.Background(), id)
}

// AcquireReadContext returns a stable document-generation lease. Queued
// writers have priority so long reads cannot starve edit/save operations
// indefinitely. Cancellation wakes a blocked Cond waiter without polling and
// never consumes a reader slot.
func (r *Registry) AcquireReadContext(ctx context.Context, id string) (*Lease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	entry, err := r.lockEntry(id)
	if err != nil {
		return nil, err
	}
	defer entry.mu.Unlock()
	stopWake := context.AfterFunc(ctx, func() {
		entry.mu.Lock()
		entry.cond.Broadcast()
		entry.mu.Unlock()
	})
	defer stopWake()
	for entry.writer || entry.waitingWriters > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.closing {
			return nil, fmt.Errorf("%w %q", ErrFileClosing, id)
		}
		if entry.transition != nil {
			return nil, fmt.Errorf("%w for %q", ErrTransitioning, id)
		}
		entry.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if entry.closing {
		return nil, fmt.Errorf("%w %q", ErrFileClosing, id)
	}
	if entry.transition != nil {
		return nil, fmt.Errorf("%w for %q", ErrTransitioning, id)
	}
	entry.readers++
	file := entry.file
	return &Lease{entry: entry, file: file, snapshot: snapshotOf(file)}, nil
}

// AcquireExclusive serializes staged-edit and save operations. It is the
// compatibility wrapper for callers without a cancellation source.
func (r *Registry) AcquireExclusive(id string) (*Lease, error) {
	return r.AcquireExclusiveContext(context.Background(), id)
}

// AcquireExclusiveContext serializes staged-edit and save operations. It does
// not mark a lifecycle transition; queued readers resume after Release.
// waitingWriters is incremented and decremented exactly once on every path so
// cancellation cannot accidentally leave reader admission blocked.
func (r *Registry) AcquireExclusiveContext(ctx context.Context, id string) (*Lease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	entry, err := r.lockEntry(id)
	if err != nil {
		return nil, err
	}
	defer entry.mu.Unlock()
	stopWake := context.AfterFunc(ctx, func() {
		entry.mu.Lock()
		entry.cond.Broadcast()
		entry.mu.Unlock()
	})
	defer stopWake()

	entry.waitingWriters++
	queued := true
	defer func() {
		if queued {
			entry.waitingWriters--
			entry.cond.Broadcast()
		}
	}()
	for entry.writer || entry.readers > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.closing || entry.transition != nil {
			if entry.closing {
				return nil, fmt.Errorf("%w %q", ErrFileClosing, id)
			}
			return nil, fmt.Errorf("%w for %q", ErrTransitioning, id)
		}
		entry.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entry.waitingWriters--
	queued = false
	if entry.closing {
		entry.cond.Broadcast()
		return nil, fmt.Errorf("%w %q", ErrFileClosing, id)
	}
	if entry.transition != nil {
		entry.cond.Broadcast()
		return nil, fmt.Errorf("%w for %q", ErrTransitioning, id)
	}
	entry.writer = true
	file := entry.file
	return &Lease{entry: entry, file: file, snapshot: snapshotOf(file), exclusive: true}, nil
}

// Transition is a two-phase refresh/reopen barrier. BeginTransition prevents
// new leases immediately; Wait is called only after service jobs are canceled.
type Transition struct {
	entry *registryEntry
	id    string
	mu    sync.Mutex
	done  bool
}

func (r *Registry) BeginTransition(id string) (*Transition, error) {
	entry, err := r.lockEntry(id)
	if err != nil {
		return nil, err
	}
	defer entry.mu.Unlock()
	if entry.closing {
		return nil, fmt.Errorf("%w %q", ErrFileClosing, id)
	}
	if entry.transition != nil {
		return nil, fmt.Errorf("%w for %q", ErrTransitioning, id)
	}
	transition := &Transition{entry: entry, id: id}
	entry.transition = transition
	entry.cond.Broadcast()
	return transition, nil
}

// Wait obtains exclusive lifecycle ownership after all pre-existing leases
// drain. A concurrent close supersedes a transition and makes Wait fail closed.
func (t *Transition) Wait() (*Lease, error) {
	if t == nil || t.entry == nil {
		return nil, ErrInvalidLease
	}
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return nil, ErrInvalidLease
	}
	t.done = true
	t.mu.Unlock()

	entry := t.entry
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.transition != t {
		return nil, ErrInvalidLease
	}
	for entry.writer || entry.readers > 0 {
		if entry.closing {
			return nil, fmt.Errorf("%w %q", ErrFileClosing, t.id)
		}
		entry.cond.Wait()
	}
	if entry.closing {
		return nil, fmt.Errorf("%w %q", ErrFileClosing, t.id)
	}
	entry.writer = true
	file := entry.file
	return &Lease{
		entry:      entry,
		file:       file,
		snapshot:   snapshotOf(file),
		exclusive:  true,
		transition: t,
	}, nil
}

// Abort releases a transition that has not been handed to a Lease.
func (t *Transition) Abort() {
	if t == nil || t.entry == nil {
		return
	}
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return
	}
	t.done = true
	t.mu.Unlock()
	entry := t.entry
	entry.mu.Lock()
	if entry.transition == t {
		entry.transition = nil
		entry.cond.Broadcast()
	}
	entry.mu.Unlock()
}

// ReopenUnderLease installs a new document generation while an exclusive
// transition lease prevents reads, edits, close-finalization, and other reopen.
// It shares ReopenUnderLeaseValidated's committed-error return contract.
func (r *Registry) ReopenUnderLease(lease *Lease) (*File, error) {
	return r.reopenUnderLeaseValidated(lease, nil)
}

// ReopenUnderLeaseValidated opens a candidate generation, invokes validate
// immediately before installation while the registry entry is still
// transition-owned, and installs only an accepted candidate. A rejected
// candidate is closed and the old generation remains registered. If the
// replacement is installed but closing the previous generation fails, this
// returns the non-nil installed replacement with *ReopenCommittedError.
func (r *Registry) ReopenUnderLeaseValidated(lease *Lease, validate func(*File) error) (*File, error) {
	return r.reopenUnderLeaseValidated(lease, validate)
}

func (r *Registry) reopenUnderLeaseValidated(lease *Lease, validate func(*File) error) (*File, error) {
	if lease == nil || !lease.exclusive || lease.transition == nil || lease.entry == nil || lease.file == nil {
		return nil, ErrInvalidLease
	}
	entry := lease.entry
	entry.mu.Lock()
	if !entry.writer || entry.transition != lease.transition || entry.file != lease.file {
		entry.mu.Unlock()
		return nil, ErrInvalidLease
	}
	if entry.closing {
		entry.mu.Unlock()
		return nil, fmt.Errorf("%w %q", ErrFileClosing, lease.file.ID)
	}
	old := entry.file
	path := old.Path
	generation := old.generation
	entry.mu.Unlock()
	if generation >= MaxBridgeFileGeneration {
		return nil, fmt.Errorf("%w for %q", ErrFileGenerationExhausted, old.ID)
	}

	doc, err := document.OpenFile(path)
	if err != nil {
		return nil, err
	}
	replacement := &File{ID: old.ID, Doc: doc, Path: path, generation: generation + 1}

	entry.mu.Lock()
	if entry.closing {
		entry.mu.Unlock()
		_ = doc.Close()
		return nil, fmt.Errorf("%w %q", ErrFileClosing, old.ID)
	}
	if !entry.writer || entry.transition != lease.transition || entry.file != old {
		entry.mu.Unlock()
		_ = doc.Close()
		return nil, ErrInvalidLease
	}
	if validate != nil {
		if err := validate(replacement); err != nil {
			entry.mu.Unlock()
			_ = doc.Close()
			return nil, err
		}
	}
	entry.file = replacement
	lease.file = replacement
	lease.snapshot = snapshotOf(replacement)
	entry.mu.Unlock()

	old.stopIndexing()
	var closeErr error
	if old.Doc != nil {
		closeErr = old.Doc.Close()
		if r.replacedDocumentCloseResult != nil {
			closeErr = r.replacedDocumentCloseResult(closeErr)
		}
	}
	replacement.StartIndexing()
	if closeErr != nil {
		return replacement, &ReopenCommittedError{FileID: replacement.ID, Err: closeErr}
	}
	return replacement, nil
}

// Reopen is the compatibility wrapper for callers that do not need a service
// cancellation phase. New service code should use BeginTransition, cancel its
// work, Wait, and ReopenUnderLease. A non-nil File remains authoritative even
// when the accompanying error is *ReopenCommittedError.
func (r *Registry) Reopen(id string) (*File, error) {
	transition, err := r.BeginTransition(id)
	if err != nil {
		return nil, err
	}
	defer transition.Abort()
	lease, err := transition.Wait()
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	return r.ReopenUnderLease(lease)
}

// CloseHandle is the second half of a close transition. BeginClose removes the
// ID and blocks new leases; Finish waits for existing leases and closes exactly
// the final installed generation.
type CloseHandle struct {
	registry *Registry
	entry    *registryEntry
	id       string
	once     sync.Once
	err      error
}

// newCloseHandleLocked transfers one registry entry into explicit close
// ownership. r.mu must be held. The handle stays in r.closing until Finish has
// closed the retained document and released its aggregate open-file slot.
func (r *Registry) newCloseHandleLocked(entry *registryEntry, id string) *CloseHandle {
	handle := &CloseHandle{registry: r, entry: entry, id: id}
	if r.closing == nil {
		r.closing = make(map[*CloseHandle]struct{})
	}
	r.closing[handle] = struct{}{}
	return handle
}

func (r *Registry) BeginClose(id string) (*CloseHandle, error) {
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return nil, ErrRegistryStopped
	}
	entry, ok := r.files[id]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w %q", ErrUnknownFile, id)
	}
	entry.mu.Lock()
	entry.closing = true
	delete(r.files, id)
	entry.cond.Broadcast()
	entry.mu.Unlock()
	handle := r.newCloseHandleLocked(entry, id)
	r.mu.Unlock()
	return handle, nil
}

// BeginCloseUnderTransition converts an exclusive transition lease into a
// close handle without reopening the lease-admission race. The caller may
// inspect generation-owned state while it holds lease and decline the close
// simply by releasing it. Once this method succeeds, the ID is removed and
// the caller must release lease before calling Finish so the close can drain.
func (r *Registry) BeginCloseUnderTransition(lease *Lease) (*CloseHandle, error) {
	if lease == nil || !lease.exclusive || lease.transition == nil || lease.entry == nil || lease.file == nil {
		return nil, ErrInvalidLease
	}
	id := lease.file.ID
	entry := lease.entry

	r.mu.Lock()
	current, ok := r.files[id]
	if !ok || current != entry {
		r.mu.Unlock()
		return nil, ErrInvalidLease
	}
	entry.mu.Lock()
	if entry.closing || !entry.writer || entry.transition != lease.transition || entry.file != lease.file {
		entry.mu.Unlock()
		r.mu.Unlock()
		return nil, ErrInvalidLease
	}
	entry.closing = true
	delete(r.files, id)
	entry.cond.Broadcast()
	entry.mu.Unlock()
	handle := r.newCloseHandleLocked(entry, id)
	r.mu.Unlock()
	return handle, nil
}

func (h *CloseHandle) Finish() error {
	if h == nil || h.entry == nil {
		return ErrInvalidLease
	}
	h.once.Do(func() {
		defer h.releaseOpenSlot()
		entry := h.entry
		entry.mu.Lock()
		for entry.writer || entry.readers > 0 {
			entry.cond.Wait()
		}
		file := entry.file
		entry.file = nil
		entry.mu.Unlock()
		if file == nil {
			return
		}
		file.stopIndexing()
		if file.Doc != nil {
			h.err = file.Doc.Close()
		}
	})
	return h.err
}

func (h *CloseHandle) releaseOpenSlot() {
	if h.registry == nil {
		return
	}
	h.registry.mu.Lock()
	if h.registry.openCount > 0 {
		h.registry.openCount--
	}
	delete(h.registry.closing, h)
	h.registry.mu.Unlock()
}

func (r *Registry) Close(id string) error {
	handle, err := r.BeginClose(id)
	if err != nil {
		return err
	}
	return handle.Finish()
}

// BeginShutdown atomically raises the irreversible registry admission barrier,
// removes every file ID, marks every entry closing, and cancels every file's
// indexer. Existing leases remain valid until released; one bounded worker per
// retained file then drains its leases and closes the exact opened document
// handle. The returned channel closes after all handles are closed. It is safe
// to call repeatedly.
//
// No source pathname is opened writable, renamed, removed, or otherwise
// mutated by this lifecycle path.
func (r *Registry) BeginShutdown() <-chan struct{} {
	r.mu.Lock()
	if r.stopping {
		done := r.shutdownDone
		r.mu.Unlock()
		return done
	}
	r.stopping = true
	r.shutdownDone = make(chan struct{})
	done := r.shutdownDone
	openDone := make(chan struct{})
	if r.opening == 0 {
		close(openDone)
	} else {
		r.openingDone = openDone
	}
	for id, entry := range r.files {
		r.newCloseHandleLocked(entry, id)
		delete(r.files, id)
	}
	// Include entries detached by a concurrent BeginClose that completed before
	// the shutdown barrier. No later close can register after stopping is set.
	handles := make([]*CloseHandle, 0, len(r.closing))
	files := make([]*File, 0, len(r.closing))
	for handle := range r.closing {
		entry := handle.entry
		entry.mu.Lock()
		entry.closing = true
		if entry.file != nil {
			files = append(files, entry.file)
		}
		entry.cond.Broadcast()
		entry.mu.Unlock()
		handles = append(handles, handle)
	}
	r.mu.Unlock()

	// Cancellation is synchronous and precedes the drain workers so an active
	// index build receives its stop signal even when another lease is retained.
	for _, file := range files {
		file.stopIndexing()
	}

	go func() {
		var wg sync.WaitGroup
		errCh := make(chan error, len(handles))
		for _, handle := range handles {
			handle := handle
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := handle.Finish(); err != nil {
					errCh <- err
				}
			}()
		}
		wg.Wait()
		<-openDone
		close(errCh)
		var shutdownErr error
		for err := range errCh {
			shutdownErr = errors.Join(shutdownErr, err)
		}
		r.mu.Lock()
		r.shutdownErr = shutdownErr
		close(done)
		r.mu.Unlock()
	}()
	return done
}

func (r *Registry) finishOpening() {
	r.mu.Lock()
	if r.opening > 0 {
		r.opening--
	}
	if r.stopping && r.opening == 0 && r.openingDone != nil {
		close(r.openingDone)
		r.openingDone = nil
	}
	r.mu.Unlock()
}

// Shutdown begins an irreversible registry shutdown and waits only as long as
// ctx permits. Handle-drain workers continue after a deadline so a temporarily
// retained cooperative lease is still closed when it eventually releases.
func (r *Registry) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := r.BeginShutdown()
	select {
	case <-done:
		r.mu.Lock()
		err := r.shutdownErr
		r.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CloseAll is an explicit alias for Shutdown for callers that own a bounded
// lifecycle context but do not otherwise use shutdown terminology.
func (r *Registry) CloseAll(ctx context.Context) error { return r.Shutdown(ctx) }

// Get is a compatibility lookup. It does not keep the returned document alive;
// service code must use AcquireRead/AcquireExclusive instead.
func (r *Registry) Get(id string) (*File, bool) {
	r.mu.Lock()
	entry, ok := r.files[id]
	if !ok {
		r.mu.Unlock()
		return nil, false
	}
	entry.mu.Lock()
	r.mu.Unlock()
	file := entry.file
	available := !entry.closing && file != nil
	entry.mu.Unlock()
	return file, available
}

// Snapshot is a compatibility lookup and does not retain the generation.
func (r *Registry) Snapshot(id string) (FileSnapshot, bool) {
	file, ok := r.Get(id)
	if !ok {
		return FileSnapshot{}, false
	}
	return snapshotOf(file), true
}

// IsCurrent compares a snapshot with the currently installed generation. A
// caller that needs it to stay current must also hold its originating lease.
func (r *Registry) IsCurrent(snapshot FileSnapshot) bool {
	r.mu.Lock()
	entry, ok := r.files[snapshot.ID]
	if !ok {
		r.mu.Unlock()
		return false
	}
	entry.mu.Lock()
	r.mu.Unlock()
	file := entry.file
	current := !entry.closing && file != nil && file.Doc == snapshot.Doc && file.Path == snapshot.Path && file.generation == snapshot.Generation
	entry.mu.Unlock()
	return current
}

func (r *Registry) Paths() []string {
	r.mu.Lock()
	entries := make([]*registryEntry, 0, len(r.files))
	for _, entry := range r.files {
		entries = append(entries, entry)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry.mu.Lock()
		if !entry.closing && entry.file != nil {
			paths = append(paths, entry.file.Path)
		}
		entry.mu.Unlock()
	}
	r.mu.Unlock()
	return paths
}

func (f *File) StartIndexing() {
	ctx, cancel := context.WithCancel(context.Background())
	f.indexMu.Lock()
	previous := f.cancelIndex
	f.cancelIndex = cancel
	doc := f.Doc
	f.indexMu.Unlock()
	if previous != nil {
		previous()
	}
	go func() { _ = doc.StartIndexing(ctx) }()
}

func (f *File) stopIndexing() {
	if f == nil {
		return
	}
	f.indexMu.Lock()
	cancel := f.cancelIndex
	f.cancelIndex = nil
	f.indexMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
