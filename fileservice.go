package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/encodingx"
	"github.com/quarry/quarry-wails3/internal/fileio"
	"github.com/quarry/quarry-wails3/internal/logger"
	"github.com/quarry/quarry-wails3/internal/regexutil"
	"github.com/quarry/quarry-wails3/internal/search"
	"github.com/quarry/quarry-wails3/internal/session"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

// searchTimeout bounds a single find call so a no-match search on a huge file
// cannot run forever. The UI shows "searching…" while it runs.
const searchTimeout = 60 * time.Second

// validateServiceSearchQuery bounds caller-controlled bridge strings before
// converting or encoding them into a second byte slice. The lower-level search
// and regexp packages enforce the same limits independently; this service
// guard prevents an oversized RPC value from multiplying memory first.
func validateServiceSearchQuery(query string, regex bool) error {
	if regex {
		if len(query) > regexutil.MaxPatternBytes {
			return fmt.Errorf("%w: pattern has %d bytes, maximum is %d", regexutil.ErrRegexResourceLimit, len(query), regexutil.MaxPatternBytes)
		}
		return nil
	}
	if len(query) > search.MaxPlainPatternBytes {
		return fmt.Errorf("%w: plain pattern has %d bytes, maximum is %d", search.ErrResourceLimit, len(query), search.MaxPlainPatternBytes)
	}
	return nil
}

const (
	defaultSearchAllHits          = 1000
	maxSearchAllHits              = 1000
	searchAllPreviewRadius        = 160
	maxSearchAllPreviewPerHit     = 512
	maxSearchAllTotalPreviewBytes = maxSearchAllHits * maxSearchAllPreviewPerHit
	harvestRegexMatchWindow       = 1 * 1024 * 1024
)

// defaultWindowBytes is the byte budget for one editor window. The editor never
// holds more than a few of these regardless of file size, so a 400 GB file and
// a 4 KB file cost the same in memory.
const defaultWindowBytes = 1 << 20 // 1 MiB

const (
	// maxRowDisplayRunes collapses a long line to this many runes (+ ⋯) so a huge
	// line (mysqldump extended INSERT) shows as one compact row, not a giant
	// CodeMirror line that breaks the viewport.
	maxRowDisplayRunes = 500
	// rowDisplayBytes is how many bytes of each line are read for display before
	// trimming to maxRowDisplayRunes (enough for 500 runes of up to 4 bytes each).
	rowDisplayBytes = 2400
	// windowLineTarget is the number of logical lines a window aims to include.
	windowLineTarget = 2000
	// maxWindowDecodedBytes bounds the Go UTF-8 string before JSON/CodeMirror
	// copies. Supported single-byte code pages need at most three UTF-8 bytes per
	// source byte, plus bounded row separators and truncation markers.
	maxWindowDecodedBytes = 4 << 20
)

// FileService exposes streaming, windowed access to very large files. It is
// bound to the frontend by Wails; the whole-file content never crosses the
// bridge — only bounded, line-aligned windows do.
type FileService struct {
	reg *session.Registry
	// reopenValidated is instance-local so committed-error fault injection does
	// not race unrelated parallel service tests. Production uses the registry
	// implementation directly.
	reopenValidated func(*session.Registry, *session.Lease, func(*session.File) error) (*session.File, error)
	// refreshFileCandidate is an instance-local test seam after the replacement
	// document has opened but before its transition-owned registry installation.
	// Production leaves it as a no-op; the authoritative validation follows.
	refreshFileCandidate func(*session.File)

	// serviceStopping is an irreversible admission gate raised before shutdown
	// cancels jobs, foreground work, SQL analysis, indexers, or document handles.
	// Central file-lease helpers consult it, which covers direct RPC work as well
	// as the long-running operations' own job/foreground registries.
	serviceMu       sync.Mutex
	serviceStopping bool
	serviceStopDone chan struct{}

	// preparedEdits is an admission ledger for source-bound manual-edit
	// sessions. The per-session engine has hard byte/history/transient limits;
	// this second bound prevents all 128 open-file slots from retaining one at
	// once. Entries are reserved before fingerprint allocation and released on
	// preparation failure, discard, refresh, or close.
	editMu        sync.Mutex
	preparedEdits map[string]struct{}
	// editWorkMu serializes the bounded but potentially allocation-heavy
	// piece-table verification/rebuild performed by StageEdit across files.
	// Lock order is editWorkMu, then a per-file exclusive session lease.
	editWorkMu sync.Mutex

	// foregroundRuns owns bounded, non-job RPC work such as interactive
	// searches. A run may be indexed under more than one source ID (schema
	// comparisons use this) so lifecycle cancellation of either source reaches
	// it before the run attempts to acquire a document lease.
	foregroundMu       sync.Mutex
	foregroundRuns     map[string]map[uint64]*foregroundRun
	foregroundBlocked  map[string]struct{}
	foregroundSeq      uint64
	foregroundRunCount int
	foregroundStopping bool

	// searchRequests gives interactive bridge searches a stable, server-owned
	// cancellation identity. IDs are monotonic and never reused, so a delayed
	// cancel for an old request cannot target newer work.
	searchRequestMu  sync.Mutex
	searchRequests   map[string]*interactiveSearchRequest
	searchRequestSeq uint64

	csvCursorMu    sync.Mutex
	csvCursors     map[csvGridCursorKey]struct{}
	csvCursorOrder []csvGridCursorKey
	csvCursorNext  int

	sqlMu      sync.Mutex
	sqlSummary map[string]sqlAnalysisEntry // cached SQL analysis bound to a source generation
	sqlRuns    map[string]sqlAnalysisRun   // in-flight analysis, canceled by lifecycle transitions
	sqlRunSeq  uint64
	sqlUseSeq  uint64

	jobOnce sync.Once
	jobMgr  *jobManager
}

// NewFileService constructs the service with an empty session registry.
func NewFileService() *FileService {
	return &FileService{
		reg:                  session.New(),
		reopenValidated:      defaultReopenUnderLeaseValidated,
		refreshFileCandidate: func(*session.File) {},
		serviceStopDone:      make(chan struct{}),
		preparedEdits:        make(map[string]struct{}),
		foregroundRuns:       make(map[string]map[uint64]*foregroundRun),
		foregroundBlocked:    make(map[string]struct{}),
		searchRequests:       make(map[string]*interactiveSearchRequest),
		csvCursors:           make(map[csvGridCursorKey]struct{}),
		sqlSummary:           make(map[string]sqlAnalysisEntry),
		sqlRuns:              make(map[string]sqlAnalysisRun),
	}
}

func defaultReopenUnderLeaseValidated(registry *session.Registry, lease *session.Lease, validate func(*session.File) error) (*session.File, error) {
	return registry.ReopenUnderLeaseValidated(lease, validate)
}

// FileMeta describes a freshly opened file.
type FileMeta struct {
	FileID                       string `json:"fileId"`
	Path                         string `json:"path"`
	Size                         int64  `json:"size"`
	Encoding                     string `json:"encoding"`
	EncodingRequiresConfirmation bool   `json:"encodingRequiresConfirmation"`
	Detected                     string `json:"detected"`
	Binary                       bool   `json:"binary"`
	Editable                     bool   `json:"editable"` // UTF-8/ASCII + LF: in-window editing allowed
	// RefreshWarning is populated only when RefreshFile committed a fresh
	// generation but cleanup of the previous generation reported an error. The
	// successful result is authoritative: returning a Go error here would make
	// Wails discard this metadata and strand the renderer on stale generation
	// state. OpenFile and ordinary refreshes leave the field empty.
	RefreshWarning string `json:"refreshWarning,omitempty"`
}

const maxRefreshWarningBytes = 2 * 1024

func boundedRefreshWarning(err error) string {
	if err == nil {
		return ""
	}
	raw := err.Error()
	buf := make([]byte, 0, min(len(raw), maxRefreshWarningBytes))
	const maxScannedBytes = 4 * maxRefreshWarningBytes
	scanned := 0
	truncated := false
	for len(raw) > 0 && scanned < maxScannedBytes {
		r, size := utf8.DecodeRuneInString(raw)
		raw = raw[size:]
		scanned += size
		if r == utf8.RuneError && size == 1 {
			r = unicode.ReplacementChar
		}
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			r = ' '
		}
		// Warnings are one line. Collapsing whitespace also prevents a hostile
		// cleanup error from consuming the entire UI budget with padding.
		if r == ' ' && (len(buf) == 0 || buf[len(buf)-1] == ' ') {
			continue
		}
		if len(buf)+utf8.RuneLen(r) > maxRefreshWarningBytes {
			truncated = true
			break
		}
		buf = utf8.AppendRune(buf, r)
	}
	if len(raw) > 0 {
		truncated = true
	}
	warning := strings.TrimSpace(string(buf))
	if warning == "" {
		warning = session.ErrReopenCommitted.Error()
	}
	if !truncated {
		return warning
	}
	const suffix = "…"
	for len(warning)+len(suffix) > maxRefreshWarningBytes {
		_, size := utf8.DecodeLastRuneInString(warning)
		warning = warning[:len(warning)-size]
	}
	return strings.TrimSpace(warning) + suffix
}

// Window is a bounded, line-aligned slice of the file decoded to UTF-8, plus
// the per-line global coordinates the editor gutter needs.
type Window struct {
	FileID             string  `json:"fileId"`
	StartByte          int64   `json:"startByte"`          // global byte offset of the first displayed row
	NextByte           int64   `json:"nextByte"`           // exact exclusive raw continuation boundary
	Text               string  `json:"text"`               // decoded rows joined with "\n"
	LineOffsets        []int64 `json:"lineOffsets"`        // exact raw start per displayed row
	LineEndOffsets     []int64 `json:"lineEndOffsets"`     // exact raw end of displayed text per row
	LineNumbers        []int64 `json:"lineNumbers"`        // global line number per row (approx until indexed)
	LineTruncated      []bool  `json:"lineTruncated"`      // row omits raw text to the right
	StartContinuesLine bool    `json:"startContinuesLine"` // first row begins inside a long logical line
	EndContinuesLine   bool    `json:"endContinuesLine"`   // NextByte is inside a logical line
	SourceBytes        int64   `json:"sourceBytes"`        // raw bytes consumed by this page
	BudgetBytes        int     `json:"budgetBytes"`        // validated caller/server raw-byte cap
	AtBOF              bool    `json:"atBof"`
	AtEOF              bool    `json:"atEof"`
	Approx             bool    `json:"approx"` // line numbers are approximate (index not ready)
}

// MatchWindow returns one decoded window plus an exact CodeMirror span. From
// and To are UTF-16 code-unit positions, not source byte offsets.
type MatchWindow struct {
	Window Window `json:"window"`
	Found  bool   `json:"found"`
	From   int    `json:"from"`
	To     int    `json:"to"`
}

// OpenViaDialog shows a native open-file dialog and opens the chosen file. A
// cancelled dialog returns an empty FileMeta (FileID == "") with a nil error.
func (s *FileService) OpenViaDialog() (FileMeta, error) {
	if err := s.ensureServiceRunning(); err != nil {
		return FileMeta{}, err
	}
	path, err := application.Get().Dialog.OpenFile().
		CanChooseFiles(true).
		SetTitle("Open file in Quarry").
		AddFilter("All files (*.*)", "*.*").
		AddFilter("Data & dumps (*.sql, *.csv, *.tsv, *.log, *.txt, *.json)", "*.sql;*.csv;*.tsv;*.log;*.txt;*.json").
		PromptForSingleSelection()
	if err != nil {
		return FileMeta{}, err
	}
	if path == "" {
		return FileMeta{}, nil // cancelled
	}
	return s.OpenFile(path)
}

// OpenFile opens path, starts background indexing, and returns its metadata.
// A leftover in-place recovery sidecar must be resolved explicitly before an
// ordinary open. OpenFile never replays or removes recovery data.
var openFilePostRegistryHook = func(*session.File, bool) {}

func (s *FileService) OpenFile(path string) (FileMeta, error) {
	if err := validateRPCPath(path); err != nil {
		return FileMeta{}, err
	}
	if err := s.ensureServiceRunning(); err != nil {
		return FileMeta{}, err
	}
	if err := requireNoPendingInPlaceRecovery(path); err != nil {
		return FileMeta{}, err
	}
	f, added, err := s.reg.OpenWithStatus(path)
	if err != nil {
		return FileMeta{}, err
	}
	openFilePostRegistryHook(f, added)
	lease, current, err := s.acquireReadFile(f.ID)
	if err != nil {
		if added {
			closeErr := s.reg.Close(f.ID)
			return FileMeta{}, errors.Join(err, closeErr)
		}
		return FileMeta{}, err
	}
	defer lease.Release()
	// The alias checked above may have no sidecar while the first-open path
	// does, and recovery evidence can appear while a new candidate is opening.
	// Always recheck the retained exact spelling before indexing or returning.
	if err := requireNoPendingInPlaceRecovery(current.Path); err != nil {
		if !added {
			return FileMeta{}, err
		}
		closeHandle, closeStartErr := s.reg.BeginClose(current.ID)
		lease.Release()
		if closeStartErr != nil {
			return FileMeta{}, errors.Join(err, closeStartErr)
		}
		return FileMeta{}, errors.Join(err, closeHandle.Finish())
	}
	if added {
		current.StartIndexing()
	}
	m := current.Doc.Metadata()
	return FileMeta{
		FileID:                       current.ID,
		Path:                         m.Path,
		Size:                         m.Size,
		Encoding:                     m.Encoding,
		EncodingRequiresConfirmation: m.EncodingRequiresConfirmation,
		Detected:                     m.FileType,
		Binary:                       m.Binary,
		Editable:                     !m.Binary && editableEncoding(m.Encoding, m.LineEnding),
	}, nil
}

// CloseFile releases a file's resources. It refuses a session with staged
// edits; callers must explicitly save or discard them first.
func (s *FileService) CloseFile(fileID string) error {
	if err := validateRPCFileID(fileID); err != nil {
		return err
	}
	if err := s.ensureServiceRunning(); err != nil {
		return err
	}
	// Establish the lifecycle barrier before any potentially blocking state
	// check. This rejects new leases; registered work is then canceled and the
	// transition drains only leases that already existed.
	transition, err := s.reg.BeginTransition(fileID)
	if err != nil {
		return normalizeSessionLeaseError(fileID, err)
	}
	defer transition.Abort()
	s.blockForegroundRuns(fileID)
	defer s.unblockForegroundRuns(fileID)
	s.cancelJobForFile(fileID)
	s.invalidateSQLAnalysis(fileID)

	lease, err := transition.Wait()
	if err != nil {
		return normalizeSessionLeaseError(fileID, err)
	}
	current := lease.File()
	if current.Edit != nil && current.Edit.HasEdits() {
		lease.Release()
		return ErrStagedEditsPending
	}
	closeHandle, err := s.reg.BeginCloseUnderTransition(lease)
	if err != nil {
		lease.Release()
		return normalizeSessionLeaseError(fileID, err)
	}
	s.invalidateCSVGridCursors(fileID)
	// BeginCloseUnderTransition has removed the ID. Releasing the transition
	// lease hands exclusive ownership to Finish without an admission gap.
	lease.Release()
	closeErr := closeHandle.Finish()
	// BeginClose has already removed the session even when closing its retained
	// descriptor reports an error, so its aggregate edit reservation must not
	// survive as an unreachable slot.
	s.releasePreparedEdit(fileID)
	return closeErr
}

// FileSize re-stats the file on disk and returns its current size. Cheap — used
// to poll a growing file (tail/follow) without reopening it.
func (s *FileService) FileSize(fileID string) (int64, error) {
	lease, _, err := s.acquireReadFile(fileID)
	if err != nil {
		return 0, err
	}
	defer lease.Release()
	file := lease.Snapshot()
	st, sameOpenedFile, err := file.Doc.CurrentPathIdentity()
	if err != nil {
		s.cancelJobForFile(fileID)
		s.invalidateSQLAnalysisGeneration(fileID, file.Generation)
		return 0, err
	}
	generationChanged, err := documentGenerationChanged(file.Doc)
	if err != nil {
		s.cancelJobForFile(fileID)
		s.invalidateSQLAnalysisGeneration(fileID, file.Generation)
		return 0, err
	}
	if !sameOpenedFile || generationChanged || !st.Equal(file.Doc.OriginalFileState()) {
		s.cancelJobForFile(fileID)
		s.invalidateSQLAnalysisGeneration(fileID, file.Generation)
	}
	return st.Size, nil
}

type FileStateResult struct {
	Size            int64 `json:"size"`
	ModTimeNanos    int64 `json:"modTimeNanos"`
	SameOpenedFile  bool  `json:"sameOpenedFile"`
	ChangedFromOpen bool  `json:"changedFromOpen"`
}

// FileState returns the current pathname state plus whether it still resolves
// to the exact handle opened for this session.
func (s *FileService) FileState(fileID string) (FileStateResult, error) {
	lease, opened, err := s.acquireReadFile(fileID)
	if err != nil {
		return FileStateResult{}, err
	}
	defer lease.Release()
	file := lease.Snapshot()
	state, same, err := file.Doc.CurrentPathIdentity()
	if err != nil {
		s.cancelJobForFile(fileID)
		s.invalidateSQLAnalysisGeneration(fileID, file.Generation)
		return FileStateResult{}, err
	}
	original := opened.Doc.OriginalFileState()
	generationChanged, err := documentGenerationChanged(opened.Doc)
	if err != nil {
		s.cancelJobForFile(fileID)
		s.invalidateSQLAnalysisGeneration(fileID, file.Generation)
		return FileStateResult{}, err
	}
	changed := generationChanged || state.Size != original.Size || !state.ModTime.Equal(original.ModTime)
	if !same || changed {
		s.cancelJobForFile(fileID)
		s.invalidateSQLAnalysisGeneration(fileID, file.Generation)
	}
	return FileStateResult{
		Size:            state.Size,
		ModTimeNanos:    state.ModTime.UnixNano(),
		SameOpenedFile:  same,
		ChangedFromOpen: changed,
	}, nil
}

// documentGenerationChanged converts the retained handle's authoritative
// size/mtime/mutation-generation comparison into the polling API's changed
// signal. A detected rewrite is normal follow-mode state, not a polling
// failure; unrelated descriptor/stat errors remain errors so callers do not
// mistake an unreadable source for an unchanged one.
func documentGenerationChanged(doc *document.FileDocument) (bool, error) {
	if doc == nil {
		return false, errors.New("document is required")
	}
	if err := doc.ValidateUnchanged(); err != nil {
		if errors.Is(err, document.ErrSourceChanged) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// RefreshFile reloads the file from disk under the same id (fresh size + line
// index), for following a growing file or picking up external changes. Staged
// edits are never discarded here; callers must explicitly save or discard them
// before refreshing.
func (s *FileService) RefreshFile(fileID string) (FileMeta, error) {
	if err := validateRPCFileID(fileID); err != nil {
		return FileMeta{}, err
	}
	if err := s.ensureServiceRunning(); err != nil {
		return FileMeta{}, err
	}
	transition, err := s.reg.BeginTransition(fileID)
	if err != nil {
		return FileMeta{}, normalizeSessionLeaseError(fileID, err)
	}
	defer transition.Abort()
	s.blockForegroundRuns(fileID)
	defer s.unblockForegroundRuns(fileID)
	// The transition rejects new leases before cancellation. Existing
	// foreground/SQL/job readers can now unwind and let Wait acquire exclusive
	// ownership. Dirty-state and recovery checks happen only after that drain,
	// so a refusal is reversible but never blocks cancellation from being sent.
	s.cancelJobForFile(fileID)
	s.invalidateSQLAnalysis(fileID)
	lease, err := transition.Wait()
	if err != nil {
		return FileMeta{}, normalizeSessionLeaseError(fileID, err)
	}
	defer lease.Release()
	current := lease.File()
	if err := requireNoPendingInPlaceRecovery(current.Path); err != nil {
		return FileMeta{}, err
	}
	if current.Edit != nil && current.Edit.HasEdits() {
		return FileMeta{}, ErrStagedEditsPending
	}
	reopenValidated := s.reopenValidated
	if reopenValidated == nil {
		reopenValidated = defaultReopenUnderLeaseValidated
	}
	f, err := reopenValidated(s.reg, lease, func(candidate *session.File) error {
		if s.refreshFileCandidate != nil {
			s.refreshFileCandidate(candidate)
		}
		if err := requireNoPendingInPlaceRecovery(candidate.Path); err != nil {
			return err
		}
		// document.OpenFile proves pathname identity while opening, but an
		// external rotator can exchange that pathname before the candidate is
		// installed. Revalidate last, after the recovery-sidecar check, so a
		// refresh never deliberately commits an already-detached handle.
		return validateDocumentSourceForOutput(candidate.Doc)
	})
	// A non-nil replacement means registry ownership already changed even when
	// closing the previous generation reported ReopenCommittedError. Complete
	// every generation-owned auxiliary cleanup before surfacing that error.
	if f != nil {
		s.releasePreparedEdit(fileID)
		s.invalidateCSVGridCursors(fileID)
		s.invalidateSQLAnalysis(fileID)
	}
	if err != nil {
		// Wails rejects the promise and discards the accompanying Go result when
		// a method returns a non-nil error. Once reopen is committed, returning
		// that error would therefore leave the renderer's metadata and derived
		// panels bound to the previous generation, especially after a same-size
		// rewrite that no later poll can distinguish. Preserve the registry's
		// typed committed-error contract, but translate it at this RPC boundary
		// into authoritative metadata plus a bounded warning.
		var committed *session.ReopenCommittedError
		if f == nil || !errors.As(err, &committed) {
			return FileMeta{}, normalizeSessionLeaseError(fileID, err)
		}
		warning := boundedRefreshWarning(err)
		logger.Warn("fileservice.refresh", "source refresh committed with previous-generation cleanup failure", map[string]string{
			"file_id": fileID,
			"error":   warning,
		})
		m := f.Doc.Metadata()
		return FileMeta{
			FileID:                       f.ID,
			Path:                         m.Path,
			Size:                         m.Size,
			Encoding:                     m.Encoding,
			EncodingRequiresConfirmation: m.EncodingRequiresConfirmation,
			Detected:                     m.FileType,
			Binary:                       m.Binary,
			Editable:                     !m.Binary && editableEncoding(m.Encoding, m.LineEnding),
			RefreshWarning:               warning,
		}, nil
	}
	if f == nil {
		return FileMeta{}, errors.New("session reopen returned no file")
	}
	// ReopenUnderLease installs a fresh generation with no manual-edit session.
	m := f.Doc.Metadata()
	return FileMeta{
		FileID:                       f.ID,
		Path:                         m.Path,
		Size:                         m.Size,
		Encoding:                     m.Encoding,
		EncodingRequiresConfirmation: m.EncodingRequiresConfirmation,
		Detected:                     m.FileType,
		Binary:                       m.Binary,
		Editable:                     !m.Binary && editableEncoding(m.Encoding, m.LineEnding),
	}, nil
}

// SearchHit is one match (or a not-found / timed-out / unsupported result).
type SearchHit struct {
	Found    bool  `json:"found"`
	Offset   int64 `json:"offset"`
	Length   int   `json:"length"`
	Line     int64 `json:"line"`     // approximate until the index is built
	TimedOut bool  `json:"timedOut"` // search hit the time budget before finishing
	// Unsupported is set when the query can't be searched in the file's encoding
	// (e.g. regex over a UTF-16/Windows-125x file, or a term with characters not
	// representable in that encoding) — so the UI can say so instead of "no matches".
	Unsupported bool   `json:"unsupported"`
	Message     string `json:"message"`
}

// findNext is the internal compatibility helper for focused service tests.
// Bridge callers must reserve a stable ID and use FindNextRequest.
func (s *FileService) findNext(fileID, query string, fromByte int64, regex, caseSensitive, wholeWord bool) (SearchHit, error) {
	return s.find(fileID, query, fromByte, false, regex, caseSensitive, wholeWord)
}

// findPrev is the internal compatibility helper for focused service tests.
// Bridge callers must reserve a stable ID and use FindPrevRequest.
func (s *FileService) findPrev(fileID, query string, beforeByte int64, regex, caseSensitive, wholeWord bool) (SearchHit, error) {
	return s.find(fileID, query, beforeByte, true, regex, caseSensitive, wholeWord)
}

func (s *FileService) find(fileID, query string, start int64, backward, regex, caseSensitive, wholeWord bool) (SearchHit, error) {
	timeoutCtx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()
	return s.findContext(timeoutCtx, fileID, query, start, backward, regex, caseSensitive, wholeWord)
}

func (s *FileService) findContext(parent context.Context, fileID, query string, start int64, backward, regex, caseSensitive, wholeWord bool) (SearchHit, error) {
	if err := validateServiceSearchQuery(query, regex); err != nil {
		return SearchHit{}, err
	}
	ctx, finish, runErr := s.beginForegroundRun(parent, fileID)
	if runErr != nil {
		return SearchHit{}, runErr
	}
	defer finish()
	return s.findRegisteredContext(ctx, fileID, query, start, backward, regex, caseSensitive, wholeWord)
}

// findRegisteredContext runs inside an already-registered foreground owner.
// Interactive request-ID RPCs use this path so lifecycle and user
// cancellation share exactly one owner and completion signal.
func (s *FileService) findRegisteredContext(ctx context.Context, fileID, query string, start int64, backward, regex, caseSensitive, wholeWord bool) (result SearchHit, retErr error) {
	lease, f, leaseErr := s.acquireReadFileContext(ctx, fileID)
	if leaseErr != nil {
		if errors.Is(leaseErr, context.DeadlineExceeded) {
			return SearchHit{TimedOut: true}, nil
		}
		return SearchHit{}, leaseErr
	}
	defer lease.Release()
	notifyServiceLease("search", fileID, lease)
	if err := f.Doc.ValidateUnchanged(); err != nil {
		return SearchHit{}, err
	}
	defer func() {
		if retErr != nil {
			return
		}
		if err := f.Doc.ValidateUnchanged(); err != nil {
			result = SearchHit{}
			retErr = err
		}
	}()
	if strings.TrimSpace(query) == "" {
		return SearchHit{}, nil
	}
	// The search engine scans the file's raw bytes, so the query must be encoded
	// into the document's encoding first — otherwise a UTF-8 query never matches a
	// UTF-16 / Windows-125x dump and the user gets a false "no matches".
	docMeta := f.Doc.Metadata()
	if docMeta.EncodingRequiresConfirmation {
		return SearchHit{Unsupported: true, Message: "Choose the source encoding before searching this file."}, nil
	}
	enc := docMeta.Encoding
	isUTF8 := enc == "" || strings.EqualFold(enc, "UTF-8")
	var pattern []byte
	if regex {
		if !isUTF8 {
			return SearchHit{Unsupported: true, Message: "Regex search isn't supported on " + enc + " files. Use plain text search, or convert the file to UTF-8."}, nil
		}
		pattern = []byte(query)
	} else if reason := plainSearchUnsupportedReason(enc, query, caseSensitive, wholeWord); reason != "" {
		return SearchHit{Unsupported: true, Message: reason}, nil
	} else if isUTF8 {
		pattern = []byte(query)
	} else {
		encoded, encErr := encodingx.EncodeString(enc, query)
		if encErr != nil {
			return SearchHit{Unsupported: true, Message: "This term can't be searched in a " + enc + " file."}, nil
		}
		pattern = encoded
	}

	var results []search.Result
	var err error
	if regex {
		results, err = search.CollectRegexp(ctx, validatedSearchDocument{doc: f.Doc}, pattern, search.RegexOptions{
			StartOffset:     start,
			MaxHits:         1,
			Backward:        backward,
			CaseInsensitive: !caseSensitive,
		}, 0)
	} else {
		results, err = search.CollectPlain(ctx, validatedSearchDocument{doc: f.Doc}, pattern, search.PlainOptions{
			StartOffset:     start,
			MaxHits:         1,
			Backward:        backward,
			CaseInsensitive: !caseSensitive,
			WholeWord:       wholeWord,
			ByteAlignment:   plainSearchByteAlignment(enc),
		}, 0)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return SearchHit{TimedOut: true}, nil
		}
		return SearchHit{}, err
	}
	if len(results) == 0 {
		return SearchHit{Found: false}, nil
	}
	m := results[0]
	line, _ := f.Doc.ApproxOffsetToLine(m.Offset)
	return SearchHit{Found: true, Offset: m.Offset, Length: m.Length, Line: line}, nil
}

// validatedSearchDocument makes every search chunk generation-checked while
// retaining the search package's memory-bounded ReaderAtSize contract.
type validatedSearchDocument struct {
	doc *document.FileDocument
}

func (r validatedSearchDocument) Size() int64 { return r.doc.Size() }

func (r validatedSearchDocument) ReadAt(p []byte, off int64) (int, error) {
	return r.doc.ReadAtValidated(p, off)
}

// SearchAllHit is one match in a whole-file search, with a preview line.
type SearchAllHit struct {
	Offset           int64  `json:"offset"`
	Length           int    `json:"length"`
	Line             int64  `json:"line"`
	Preview          string `json:"preview"`
	PreviewTruncated bool   `json:"previewTruncated"`
}

// SearchContinuation is an exact exclusive boundary for the next page. Pass
// both fields back to SearchAllPage with the same query options.
type SearchContinuation struct {
	StartOffset int64 `json:"startOffset"`
	Backward    bool  `json:"backward"`
}

// SearchAllResult is a service-bounded page of whole-file matches.
type SearchAllResult struct {
	Hits              []SearchAllHit      `json:"hits"`
	Truncated         bool                `json:"truncated"`
	Continuation      *SearchContinuation `json:"continuation,omitempty"`
	PreviewsTruncated bool                `json:"previewsTruncated"`
	PreviewBytes      int                 `json:"previewBytes"`
	Limit             int                 `json:"limit"`
	TimedOut          bool                `json:"timedOut"`
	Unsupported       bool                `json:"unsupported"`
	Message           string              `json:"message"`
}

// searchAll is the internal first-page helper. Bridge callers use
// SearchAllRequest so every execution has an exact cancellation owner.
func (s *FileService) searchAll(fileID, query string, regex, caseSensitive, wholeWord bool, maxHits int) (SearchAllResult, error) {
	return s.searchAllPageWithTimeout(fileID, query, regex, caseSensitive, wholeWord, maxHits, 0, false)
}

// SearchAllPage collects one bounded page beginning at startOffset. Forward
// pages include matches at or after startOffset. Backward pages include matches
// strictly before startOffset and return them in descending offset order. For a
// first backward page, pass the file size returned by OpenFile.
//
// A zero maxHits selects the default. Negative values are invalid, and values
// above the hard service limit are clamped before any search allocation.
func (s *FileService) searchAllPage(fileID, query string, regex, caseSensitive, wholeWord bool, maxHits int, startOffset int64, backward bool) (SearchAllResult, error) {
	return s.searchAllPageWithTimeout(fileID, query, regex, caseSensitive, wholeWord, maxHits, startOffset, backward)
}

func (s *FileService) searchAllPageWithTimeout(fileID, query string, regex, caseSensitive, wholeWord bool, maxHits int, startOffset int64, backward bool) (SearchAllResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()
	result, err := s.searchAllPageContext(ctx, fileID, query, regex, caseSensitive, wholeWord, maxHits, startOffset, backward)
	if errors.Is(err, context.DeadlineExceeded) {
		return SearchAllResult{TimedOut: true, Limit: result.Limit}, nil
	}
	return result, err
}

func (s *FileService) searchAllPageContext(ctx context.Context, fileID, query string, regex, caseSensitive, wholeWord bool, maxHits int, startOffset int64, backward bool) (SearchAllResult, error) {
	if err := validateServiceSearchQuery(query, regex); err != nil {
		return SearchAllResult{}, err
	}
	ctx, finish, runErr := s.beginForegroundRun(ctx, fileID)
	if runErr != nil {
		return SearchAllResult{}, runErr
	}
	defer finish()
	return s.searchAllPageRegisteredContext(ctx, fileID, query, regex, caseSensitive, wholeWord, maxHits, startOffset, backward)
}

// searchAllPageRegisteredContext runs inside an already-registered foreground
// owner. See findRegisteredContext.
func (s *FileService) searchAllPageRegisteredContext(ctx context.Context, fileID, query string, regex, caseSensitive, wholeWord bool, maxHits int, startOffset int64, backward bool) (result SearchAllResult, retErr error) {
	lease, f, leaseErr := s.acquireReadFileContext(ctx, fileID)
	if leaseErr != nil {
		return SearchAllResult{}, leaseErr
	}
	defer lease.Release()
	notifyServiceLease("search-all", fileID, lease)
	limit, err := validateSearchAllPage(maxHits, startOffset, f.Doc.Size())
	if err != nil {
		return SearchAllResult{}, err
	}
	result = SearchAllResult{Limit: limit}
	if err := f.Doc.ValidateUnchanged(); err != nil {
		return SearchAllResult{}, err
	}
	defer func() {
		if retErr != nil {
			return
		}
		if err := f.Doc.ValidateUnchanged(); err != nil {
			result = SearchAllResult{}
			retErr = err
		}
	}()
	if strings.TrimSpace(query) == "" {
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if backward && startOffset == 0 {
		return result, nil
	}
	docMeta := f.Doc.Metadata()
	if docMeta.EncodingRequiresConfirmation {
		result.Unsupported = true
		result.Message = "Choose the source encoding before searching this file."
		return result, nil
	}
	enc := docMeta.Encoding
	isUTF8 := enc == "" || strings.EqualFold(enc, "UTF-8")
	var pattern []byte
	if regex {
		if !isUTF8 {
			result.Unsupported = true
			result.Message = "Regex search isn't supported on " + enc + " files."
			return result, nil
		}
		pattern = []byte(query)
	} else if reason := plainSearchUnsupportedReason(enc, query, caseSensitive, wholeWord); reason != "" {
		result.Unsupported = true
		result.Message = reason
		return result, nil
	} else if isUTF8 {
		pattern = []byte(query)
	} else {
		encoded, encErr := encodingx.EncodeString(enc, query)
		if encErr != nil {
			result.Unsupported = true
			result.Message = "This term can't be searched in a " + enc + " file."
			return result, nil
		}
		pattern = encoded
	}

	probeLimit := limit + 1
	matches := make([]search.Match, 0, probeLimit)
	emit := func(m search.Match) error {
		matches = append(matches, m)
		return nil
	}
	if regex {
		re, compileErr := regexutil.Compile(pattern, !caseSensitive)
		if compileErr != nil {
			return result, compileErr
		}
		find := search.FindRegexp
		if backward {
			find = search.FindRegexpBackward
		}
		err = find(ctx, validatedSearchDocument{doc: f.Doc}, re, search.RegexOptions{
			StartOffset: startOffset,
			Backward:    backward,
			MaxHits:     probeLimit,
		}, emit)
	} else {
		find := search.FindPlain
		if backward {
			find = search.FindPlainBackward
		}
		err = find(ctx, validatedSearchDocument{doc: f.Doc}, pattern, search.PlainOptions{
			StartOffset:     startOffset,
			Backward:        backward,
			MaxHits:         probeLimit,
			CaseInsensitive: !caseSensitive,
			WholeWord:       wholeWord,
			ByteAlignment:   plainSearchByteAlignment(enc),
		}, emit)
	}
	if err != nil {
		return result, err
	}

	result.Truncated = len(matches) > limit
	if result.Truncated {
		matches = matches[:limit]
	}
	result.Hits = make([]SearchAllHit, 0, len(matches))
	previewRemaining := maxSearchAllTotalPreviewBytes
	for _, m := range matches {
		if err := ctx.Err(); err != nil {
			return SearchAllResult{Limit: limit}, err
		}
		line, _ := f.Doc.ApproxOffsetToLine(m.Offset)
		previewLimit := min(previewRemaining, maxSearchAllPreviewPerHit)
		preview, previewTruncated, previewErr := boundedSearchPreview(ctx, f.Doc, m, previewLimit)
		if previewErr != nil {
			return SearchAllResult{Limit: limit}, previewErr
		}
		previewRemaining -= len(preview)
		result.PreviewBytes += len(preview)
		result.PreviewsTruncated = result.PreviewsTruncated || previewTruncated
		result.Hits = append(result.Hits, SearchAllHit{
			Offset:           m.Offset,
			Length:           m.Length,
			Line:             line,
			Preview:          preview,
			PreviewTruncated: previewTruncated,
		})
	}
	if result.Truncated && len(matches) > 0 {
		last := matches[len(matches)-1]
		nextOffset := last.Offset
		if !backward {
			advance := int64(1)
			if regex && last.Length > 0 {
				advance = int64(last.Length)
			}
			nextOffset += advance
			if nextOffset > f.Doc.Size() {
				nextOffset = f.Doc.Size()
			}
		}
		result.Continuation = &SearchContinuation{StartOffset: nextOffset, Backward: backward}
	}
	return result, nil
}

func validateSearchAllPage(maxHits int, startOffset int64, size int64) (int, error) {
	if maxHits < 0 {
		return 0, errors.New("search hit limit must not be negative")
	}
	if startOffset < 0 || startOffset > size {
		return 0, fmt.Errorf("search start offset %d is outside [0, %d]", startOffset, size)
	}
	if maxHits == 0 {
		return defaultSearchAllHits, nil
	}
	if maxHits > maxSearchAllHits {
		return 0, fmt.Errorf("search hit limit %d exceeds hard maximum %d", maxHits, maxSearchAllHits)
	}
	return maxHits, nil
}

func boundedSearchPreview(ctx context.Context, doc *document.FileDocument, match search.Match, maxBytes int) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if maxBytes <= 0 {
		return "", true, nil
	}
	meta := doc.Metadata()
	if meta.EncodingRequiresConfirmation {
		return "", false, fmt.Errorf("%w: %s", encodingx.ErrEncodingConfirmationRequired, meta.Encoding)
	}
	size := doc.Size()
	if match.Offset < 0 || match.Offset > size || match.Length < 0 {
		return "", false, errors.New("search returned an invalid match range")
	}
	if int64(match.Length) > size-match.Offset {
		return "", false, errors.New("search returned a match beyond the source size")
	}
	matchEnd := match.Offset + int64(match.Length)
	desiredStart := max(int64(0), match.Offset-searchAllPreviewRadius)
	desiredEnd := size
	if matchEnd <= size-searchAllPreviewRadius {
		desiredEnd = matchEnd + searchAllPreviewRadius
	}

	readStart := desiredStart
	readEnd := desiredEnd
	if readEnd-readStart > int64(maxBytes) {
		matchBytes := min(int64(maxBytes), matchEnd-match.Offset)
		before := (int64(maxBytes) - matchBytes) / 2
		readStart = max(int64(0), match.Offset-before)
		readEnd = min(size, readStart+int64(maxBytes))
		if readEnd-readStart < int64(maxBytes) {
			readStart = max(int64(0), readEnd-int64(maxBytes))
		}
	}
	alignedStart, err := doc.AlignTextOffsetForward(readStart)
	if err != nil {
		return "", false, err
	}
	readStart = alignedStart
	alignedEnd, err := doc.AlignTextOffsetBackward(readEnd)
	if err != nil {
		return "", false, err
	}
	readEnd = max(alignedEnd, readStart)
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	decoded, err := doc.DecodeAlignedRange(readStart, readEnd, int64(maxBytes))
	if err != nil {
		return "", false, err
	}
	preview, cleanedTruncated := cleanBoundedDecodedSearchPreview(decoded, maxBytes)
	rangeTruncated := readStart > desiredStart || readEnd < desiredEnd
	return preview, rangeTruncated || cleanedTruncated, nil
}

func cleanBoundedDecodedSearchPreview(decoded string, maxBytes int) (string, bool) {
	preview := strings.Join(strings.Fields(decoded), " ")
	if len(preview) <= maxBytes {
		return preview, false
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(preview[end]) {
		end--
	}
	return preview[:end], true
}

// HarvestMatchesViaDialog runs a regex over the whole file and writes every
// match, one per line, to a chosen file — e.g. extract every email or id.
var harvestSaveDialog = saveDialog

func (s *FileService) HarvestMatchesViaDialog(fileID, pattern string, caseInsensitive bool) (TransformResult, error) {
	if err := validateServiceSearchQuery(pattern, true); err != nil {
		return TransformResult{}, err
	}
	lease, f, finishPreflight, err := s.acquireReadFilePreflight(fileID)
	if err != nil {
		return TransformResult{}, err
	}
	defer finishPreflight()
	if strings.TrimSpace(pattern) == "" {
		return TransformResult{}, errors.New("enter a regex pattern")
	}
	enc := f.Doc.Metadata().Encoding
	if !(enc == "" || strings.EqualFold(enc, "UTF-8")) {
		return TransformResult{}, fmt.Errorf("regex harvest needs a UTF-8 file (this file is %s)", enc)
	}
	generation := lease.Snapshot().Generation
	finishPreflight()
	re, _, err := regexutil.CompileBounded([]byte(pattern), caseInsensitive, harvestRegexMatchWindow)
	if err != nil {
		return TransformResult{}, err
	}
	dst, err := harvestSaveDialog("Save extracted matches as", "matches.txt")
	if err != nil || strings.TrimSpace(dst) == "" {
		return TransformResult{}, err
	}
	return s.withFileJob(fileID, "Harvest matches", func(ctx context.Context, progress func(int64, string)) (TransformResult, error) {
		operationLease, current, err := s.acquireReadFileContext(ctx, fileID)
		if err != nil {
			return TransformResult{}, err
		}
		defer operationLease.Release()
		if operationLease.Snapshot().Generation != generation {
			return TransformResult{}, fmt.Errorf("%w: file was refreshed while the harvest dialog was open", sourceio.ErrSourceChanged)
		}
		return s.harvestMatchesToPath(ctx, current, re, caseInsensitive, dst, progress)
	})
}

func (s *FileService) harvestMatchesToPath(ctx context.Context, f *session.File, re *regexp.Regexp, caseInsensitive bool, dst string, progress func(int64, string)) (result TransformResult, retErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := regexutil.ValidateCompiledBounded(re, harvestRegexMatchWindow); err != nil {
		return TransformResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return TransformResult{}, err
	}
	out, err := fileio.OpenAtomicOutput(dst, s.reg.Paths(), 0o600)
	if err != nil {
		return TransformResult{}, err
	}
	defer func() {
		if err := out.Cleanup(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	expected, err := sourceio.ExpectDocumentContext(ctx, f.Doc)
	if err != nil {
		return TransformResult{}, err
	}
	verified, err := sourceio.NewVerifiedDocumentReader(ctx, expected, f.Doc)
	if err != nil {
		return TransformResult{}, err
	}

	bw := bufio.NewWriterSize(out, 1<<20)
	var count int64
	err = search.FindRegexp(ctx, verified, re, search.RegexOptions{CaseInsensitive: caseInsensitive, MaxMatchWindow: harvestRegexMatchWindow}, func(m search.Match) error {
		b := make([]byte, m.Length)
		n, rerr := verified.ReadAt(b, m.Offset)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return rerr
		}
		if n != len(b) {
			return io.ErrUnexpectedEOF
		}
		if _, werr := bw.Write(flattenHarvestMatchNewlines(b)); werr != nil {
			return werr
		}
		count++
		if progress != nil && count%1000 == 0 {
			progress(count, "matches written")
		}
		return bw.WriteByte('\n')
	})
	if err != nil {
		return TransformResult{}, err
	}
	if err := bw.Flush(); err != nil {
		return TransformResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return TransformResult{}, err
	}
	result = TransformResult{OutputPath: dst, RecordsWritten: count, Note: fmt.Sprintf("%d matches", count)}
	if err := out.CommitContextValidated(ctx, func(validateCtx context.Context) error {
		return expected.ValidateDocumentContext(validateCtx, f.Doc)
	}); err != nil {
		return transformResultAfterPublication(result, err)
	}
	return result, nil
}

func flattenHarvestMatchNewlines(match []byte) []byte {
	write := 0
	for read := 0; read < len(match); read++ {
		switch match[read] {
		case '\r':
			match[write] = ' '
			write++
			if read+1 < len(match) && match[read+1] == '\n' {
				read++
			}
		case '\n':
			match[write] = ' '
			write++
		default:
			match[write] = match[read]
			write++
		}
	}
	return match[:write]
}

const (
	resolveLineExactScanBytes int64 = 8 << 20
	resolveLineTimeout              = 5 * time.Second
)

// LineResolution distinguishes a real offset zero, an approximate known line
// start, a line proven absent, an incomplete index, and a bounded-scan fallback.
// When Exact is false and Found is true, ResolvedLine is the actual line at
// Offset; it must not be mistaken for proof that the requested line was found.
type LineResolution struct {
	Offset        int64 `json:"offset"`
	ResolvedLine  int64 `json:"resolvedLine"`
	Exact         bool  `json:"exact"`
	Found         bool  `json:"found"`
	IndexComplete bool  `json:"indexComplete"`
	Limited       bool  `json:"limited"`
}

// ResolveLine maps a positive 1-based line number under a hard exact-scan byte
// budget and a lifecycle-cancellable timeout. Source read failures remain
// errors; a budget/deadline fallback is returned explicitly and never reported
// as proof that the requested line is absent.
func (s *FileService) ResolveLine(fileID string, line int64) (LineResolution, error) {
	if line < 1 {
		return LineResolution{}, errors.New("line number must be positive")
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveLineTimeout)
	defer cancel()
	return s.resolveLineContext(ctx, fileID, line)
}

func (s *FileService) resolveLineContext(parent context.Context, fileID string, line int64) (resolution LineResolution, retErr error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, finish, err := s.beginForegroundRun(parent, fileID)
	if err != nil {
		return LineResolution{}, err
	}
	defer finish()

	lease, f, err := s.acquireReadFileContext(ctx, fileID)
	if err != nil {
		return LineResolution{}, err
	}
	defer lease.Release()
	if err := f.Doc.ValidateUnchanged(); err != nil {
		return LineResolution{}, err
	}
	defer func() {
		if retErr != nil {
			return
		}
		if err := f.Doc.ValidateUnchanged(); err != nil {
			resolution = LineResolution{}
			retErr = err
		}
	}()

	lookup, lookupErr := f.Doc.LookupLineStart(ctx, line, resolveLineExactScanBytes)
	resolution = lineResolutionFromLookup(lookup, f.Doc.IndexProgress().Done)
	if lookupErr == nil {
		return resolution, nil
	}
	// The RPC's own deadline is a latency safety limit, so preserve the honest
	// known-line fallback. Lifecycle cancellation (close/refresh/shutdown) remains
	// an error so stale navigation cannot be applied to a transitioning session.
	if errors.Is(parent.Err(), context.DeadlineExceeded) &&
		(errors.Is(lookupErr, context.Canceled) || errors.Is(lookupErr, context.DeadlineExceeded)) {
		resolution.Limited = true
		return resolution, nil
	}
	return LineResolution{}, lookupErr
}

func lineResolutionFromLookup(lookup document.LineStartLookupResult, indexComplete bool) LineResolution {
	resolution := LineResolution{
		Offset:        lookup.Offset,
		ResolvedLine:  lookup.Line,
		Found:         lookup.HasPosition,
		IndexComplete: indexComplete,
	}
	switch lookup.Status {
	case document.LineStartLookupExact:
		resolution.Exact = true
		resolution.Found = true
	case document.LineStartLookupAbsent:
		return LineResolution{IndexComplete: true}
	case document.LineStartLookupPending:
		resolution.IndexComplete = false
	case document.LineStartLookupLimited:
		resolution.Limited = true
	}
	return resolution
}

// GetWindow returns a window beginning at startByte (clamped, aligned by the
// document's own line handling).
func (s *FileService) GetWindow(fileID string, startByte int64, maxBytes int) (Window, error) {
	lease, f, err := s.acquireReadFile(fileID)
	if err != nil {
		return Window{}, err
	}
	defer lease.Release()
	rendered, err := s.renderWindow(f, startByte, maxBytes, true, -1)
	return rendered.window, err
}

// GetNextWindow continues forward from a previous window's NextByte.
func (s *FileService) GetNextWindow(fileID string, fromByte int64, maxBytes int) (Window, error) {
	lease, f, err := s.acquireReadFile(fileID)
	if err != nil {
		return Window{}, err
	}
	defer lease.Release()
	rendered, err := s.renderWindow(f, fromByte, maxBytes, false, -1)
	return rendered.window, err
}

// GetPrevWindow returns the window immediately preceding currentStart, aligned
// to a line boundary so it reads cleanly.
func (s *FileService) GetPrevWindow(fileID string, currentStart int64, maxBytes int) (Window, error) {
	lease, f, err := s.acquireReadFile(fileID)
	if err != nil {
		return Window{}, err
	}
	defer lease.Release()
	return s.getPrevWindowForFile(f, currentStart, maxBytes)
}

func (s *FileService) getPrevWindowForFile(f *session.File, currentStart int64, maxBytes int) (Window, error) {
	budget, err := validateWindowBudget(maxBytes)
	if err != nil {
		return Window{}, err
	}
	if currentStart < 0 {
		return Window{}, errors.New("previous-window start must not be negative")
	}
	currentStart = f.Doc.ClampOffset(currentStart)
	if currentStart <= 0 {
		rendered, renderErr := s.renderWindow(f, 0, budget, true, -1)
		return rendered.window, renderErr
	}
	boundary, err := f.Doc.AlignTextOffsetBackward(currentStart)
	if err != nil {
		return Window{}, err
	}
	if boundary != currentStart {
		return Window{}, fmt.Errorf("%w: previous-window end %d", document.ErrTextBoundary, currentStart)
	}
	start, err := f.Doc.PreviousWindowStart(currentStart, int64(budget), windowLineTarget)
	if err != nil {
		return Window{}, err
	}
	rendered, err := s.renderWindow(f, start, budget, false, currentStart)
	if err != nil {
		return Window{}, err
	}
	if rendered.window.NextByte != currentStart {
		return Window{}, fmt.Errorf("previous window is not adjacent: next byte %d, expected %d", rendered.window.NextByte, currentStart)
	}
	return rendered.window, nil
}

// GetTailWindow returns a bounded non-empty window ending at the current EOF.
// An empty source correctly returns an empty BOF/EOF window.
func (s *FileService) GetTailWindow(fileID string, maxBytes int) (Window, error) {
	lease, f, err := s.acquireReadFile(fileID)
	if err != nil {
		return Window{}, err
	}
	defer lease.Release()
	budget, err := validateWindowBudget(maxBytes)
	if err != nil {
		return Window{}, err
	}
	size := f.Doc.Size()
	if size == 0 {
		rendered, renderErr := s.renderWindow(f, 0, budget, true, -1)
		return rendered.window, renderErr
	}
	return s.getPrevWindowForFile(f, size, budget)
}
