// Package inplace contains Quarry's dormant, fail-closed primitive for
// length-preserving source-file patches.
//
// The public application does not currently enable this path. SavePatch remains
// disabled until Quarry also has an explicit confirmation capability and a
// retained, user-restorable backup. Keeping this primitive hardened matters
// because leftover recovery evidence must never become an unchecked write
// instruction if the feature is enabled later.
package inplace

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/quarry/quarry-wails3/internal/directoryfile"
)

// Patch overwrites [Offset, Offset+len(Old)) with New. New and Old must have
// the same length. Old is the expected pre-transaction content.
type Patch struct {
	Offset int64
	Old    []byte
	New    []byte
}

var (
	ErrNotLengthPreserving = errors.New("inplace: patch is not length-preserving")
	ErrEmptyPatch          = errors.New("inplace: empty patches are not allowed")
	ErrDrift               = errors.New("inplace: file content changed under the patch")
	ErrOutOfRange          = errors.New("inplace: patch is out of range")
	ErrOverlap             = errors.New("inplace: patches overlap")
	ErrTooLarge            = errors.New("inplace: recovery transaction exceeds configured limits")
	ErrSidecarExists       = errors.New("inplace: sidecar already exists")
	ErrUnsafeSidecar       = errors.New("inplace: unsafe sidecar")
	ErrUnsafeSource        = errors.New("inplace: source is not a stable regular file")
	ErrInvalidSidecar      = errors.New("inplace: invalid sidecar")
	ErrSourceMismatch      = errors.New("inplace: recovery sidecar belongs to a different source file")
	ErrRecoveryDrift       = errors.New("inplace: source bytes do not match the recorded recovery transaction")
	ErrMutationDisabled    = errors.New("inplace: creating source-file mutation transactions is disabled; save a copy instead")
)

const (
	// The recovery parser never allocates outside these explicit bounds. The
	// payload limit is the total number of original source bytes retained in a
	// transaction; per-entry metadata is bounded independently.
	maxRecoveryPayloadBytes int64 = 64 << 20
	maxRecoveryEntries      int64 = 65_536
	maxSidecarArtifactBytes int64 = 72 << 20

	sidecarHeaderSize     int64 = 96
	sidecarEntryFixedSize int64 = 48 // offset + length + SHA-256(new bytes)
	sidecarTrailerSize    int64 = sha256.Size
	committedFlagOffset         = 8
	stateMarkerSize       int64 = 40 // state + complement + reserved + digest

	sidecarStatePending   byte = 0
	sidecarStateCommitted byte = 1

	identityKindWindows byte = 1
	identityKindUnix    byte = 2
)

var (
	sidecarMagic      = [8]byte{'Q', 'R', 'Y', 'R', 'P', 0, 0, 2}
	stateDigestDomain = []byte("Quarry in-place recovery state v2\x00")

	// Seams are variables only so focused failure-injection tests can prove
	// rollback and evidence-preservation behavior.
	writeAtSource  = exactWriteAt
	writeAtSidecar = exactWriteAt
	syncSource     = func(f *os.File) error { return f.Sync() }
	syncParent     = syncParentDirectory
)

type sourceIdentity struct {
	kind byte
	a    uint64
	b    uint64
}

type sourceDescriptor struct {
	size     int64
	identity sourceIdentity
}

type entry struct {
	offset  int64
	old     []byte
	newHash [sha256.Size]byte
}

type sidecarRecord struct {
	committed bool
	source    sourceDescriptor
	entries   []entry
	bodyHash  [sha256.Size]byte
	payload   int64
}

// RecoveryState is a bounded, read-only inspection of adjacent recovery
// evidence. SourceState is one of "original", "patched", "partial", or
// "drifted". It is advisory until Recover reacquires the source lock and
// revalidates every extent.
type RecoveryState struct {
	SidecarPath   string `json:"sidecarPath"`
	ArtifactSize  int64  `json:"artifactSize"`
	Phase         string `json:"phase"`
	SourceSize    int64  `json:"sourceSize"`
	EntryCount    int    `json:"entryCount"`
	PayloadBytes  int64  `json:"payloadBytes"`
	SourceMatches bool   `json:"sourceMatches"`
	SourceState   string `json:"sourceState"`
	CanRollback   bool   `json:"canRollback"`
	CanClear      bool   `json:"canClear"`
}

// InconsistentStateError reports that a post-WAL operation failed and Quarry
// could not prove synchronous rollback. The sidecar is deliberately retained.
type InconsistentStateError struct {
	SidecarPath string
	Operation   error
	Rollback    error
}

func (e *InconsistentStateError) Error() string {
	return fmt.Sprintf("inplace: operation failed and rollback could not be proven; recovery evidence retained at %q: operation: %v; rollback: %v", e.SidecarPath, e.Operation, e.Rollback)
}

func (e *InconsistentStateError) Unwrap() error { return e.Operation }

// Apply is retained as a compatibility boundary and always fails closed.
// Quarry does not create new source-mutation transactions. Recover remains
// available only to restore legacy interrupted transactions whose evidence was
// created by an older build.
func Apply(path string, patches []Patch, sidecarPath string) error {
	_ = path
	_ = patches
	_ = sidecarPath
	return ErrMutationDisabled
}

// applyTransaction retains the former transaction implementation solely for
// package-level recovery-format tests. It is deliberately unexported and has
// no non-test caller.
func applyTransaction(path string, patches []Patch, sidecarPath string) error {
	if len(patches) == 0 {
		return nil
	}
	ordered, payloadBytes, err := validate(patches)
	if err != nil {
		return err
	}

	f, err := openRegularSource(path, true)
	if err != nil {
		return err
	}
	locked := false
	closeSource := func() error {
		var closeErr error
		if locked {
			closeErr = unlockSourceFile(f)
			locked = false
		}
		return errors.Join(closeErr, f.Close())
	}
	if err := lockSourceFile(f); err != nil {
		return errors.Join(err, f.Close())
	}
	locked = true

	info, err := f.Stat()
	if err != nil {
		return joinCleanup(err, closeSource())
	}
	if !info.Mode().IsRegular() {
		return joinCleanup(errors.New("inplace: source must be a regular file"), closeSource())
	}
	identity, err := sourceIdentityForFile(f)
	if err != nil {
		return joinCleanup(err, closeSource())
	}
	source := sourceDescriptor{size: info.Size(), identity: identity}
	if err := validatePatchesAgainstSource(f, source.size, ordered); err != nil {
		return joinCleanup(err, closeSource())
	}

	sidecar, err := createSidecar(sidecarPath, source, ordered, payloadBytes)
	if err != nil {
		return errors.Join(err, closeSource())
	}
	// Re-prove both retained handles and their path identities at the last
	// possible point before the first source write. This does not make POSIX
	// pathname unlink atomic, but it closes substitutions that happened while
	// the sidecar was written and synced.
	if err := verifySourcePath(path, f, info); err != nil {
		return errors.Join(err, closeSource(), sidecar.discard())
	}
	if err := sidecar.verifyOwnership(info); err != nil {
		return errors.Join(err, closeSource(), sidecar.discard())
	}

	operationErr := applyEntries(f, ordered)
	if operationErr == nil {
		operationErr = syncSource(f)
	}
	if operationErr == nil {
		operationErr = sidecar.markCommitted()
	}
	if operationErr != nil {
		rollbackErr := rollbackPatches(f, ordered)
		if rollbackErr == nil {
			rollbackErr = syncSource(f)
		}
		if rollbackErr != nil {
			return errors.Join(&InconsistentStateError{
				SidecarPath: sidecar.path,
				Operation:   operationErr,
				Rollback:    rollbackErr,
			}, closeSource(), sidecar.close())
		}
		if sourceCloseErr := closeSource(); sourceCloseErr != nil {
			return errors.Join(operationErr, sourceCloseErr, sidecar.close())
		}
		return errors.Join(operationErr, sidecar.remove())
	}

	// Do not remove the committed record until the source handle has unlocked
	// and closed successfully. A close failure therefore remains actionable.
	if err := closeSource(); err != nil {
		return errors.Join(err, sidecar.close())
	}
	return sidecar.remove()
}

// Inspect validates a recovery sidecar and examines the source read-only. It
// never writes, removes, truncates, or renames either path.
func Inspect(path string, sidecarPath string) (RecoveryState, error) {
	state := RecoveryState{SidecarPath: sidecarPath}
	sidecar, err := openOwnedSidecar(sidecarPath)
	if err != nil {
		return state, err
	}
	defer sidecar.close()
	state.ArtifactSize = sidecar.info.Size()

	record, err := readSidecar(sidecar.file, state.ArtifactSize)
	if err != nil {
		return state, err
	}
	state.SourceSize = record.source.size
	state.EntryCount = len(record.entries)
	state.PayloadBytes = record.payload
	if record.committed {
		state.Phase = "committed"
	} else {
		state.Phase = "pending"
	}

	source, err := openRegularSource(path, false)
	if err != nil {
		return state, err
	}
	defer source.Close()
	if err := validateSourceDescriptor(source, record.source); err != nil {
		return state, err
	}
	state.SourceMatches = true
	state.SourceState, err = inspectExtentState(source, record.entries)
	if err != nil {
		return state, err
	}
	state.CanRollback = !record.committed && state.SourceState != "drifted"
	state.CanClear = record.committed && verifyCommittedExtents(source, record.entries) == nil
	return state, nil
}

// Recover explicitly rolls back a pending transaction or clears a verified
// committed record. Invalid, substituted, source-mismatched, or drifted
// evidence is preserved byte-for-byte and returned as an error.
func Recover(path string, sidecarPath string) (rolledBack bool, err error) {
	sidecar, err := openOwnedSidecar(sidecarPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	record, err := readSidecar(sidecar.file, sidecar.info.Size())
	if err != nil {
		return false, errors.Join(err, sidecar.close())
	}

	// Parsing, checksum, exact-EOF, range, and overlap validation are complete
	// before the source is opened writable.
	source, err := openRegularSource(path, !record.committed)
	if err != nil {
		return false, errors.Join(err, sidecar.close())
	}
	locked := false
	closeSource := func() error {
		var closeErr error
		if locked {
			closeErr = unlockSourceFile(source)
			locked = false
		}
		return errors.Join(closeErr, source.Close())
	}
	if err := lockSourceFile(source); err != nil {
		return false, errors.Join(err, source.Close(), sidecar.close())
	}
	locked = true
	if err := validateSourceDescriptor(source, record.source); err != nil {
		return false, errors.Join(err, closeSource(), sidecar.close())
	}
	openedSourceInfo, err := source.Stat()
	if err != nil {
		return false, errors.Join(err, closeSource(), sidecar.close())
	}
	if err := verifySourcePath(path, source, openedSourceInfo); err != nil {
		return false, errors.Join(err, closeSource(), sidecar.close())
	}

	if record.committed {
		if err := verifyCommittedExtents(source, record.entries); err != nil {
			return false, errors.Join(err, closeSource(), sidecar.close())
		}
		if err := closeSource(); err != nil {
			return false, errors.Join(err, sidecar.close())
		}
		return false, sidecar.remove()
	}

	if err := rollbackEntries(source, record.entries); err != nil {
		return false, errors.Join(err, closeSource(), sidecar.close())
	}
	if err := syncSource(source); err != nil {
		return false, errors.Join(err, closeSource(), sidecar.close())
	}
	if err := verifyOriginalExtents(source, record.entries); err != nil {
		return false, errors.Join(err, closeSource(), sidecar.close())
	}
	if err := closeSource(); err != nil {
		return false, errors.Join(err, sidecar.close())
	}
	if err := sidecar.remove(); err != nil {
		return true, err
	}
	return true, nil
}

func validate(patches []Patch) ([]Patch, int64, error) {
	if int64(len(patches)) > maxRecoveryEntries {
		return nil, 0, ErrTooLarge
	}
	ordered := make([]Patch, len(patches))
	var payload int64
	for i, p := range patches {
		if len(p.New) != len(p.Old) {
			return nil, 0, ErrNotLengthPreserving
		}
		if len(p.Old) == 0 {
			return nil, 0, ErrEmptyPatch
		}
		if p.Offset < 0 || int64(len(p.Old)) > math.MaxInt64-p.Offset {
			return nil, 0, ErrOutOfRange
		}
		if int64(len(p.Old)) > maxRecoveryPayloadBytes-payload {
			return nil, 0, ErrTooLarge
		}
		payload += int64(len(p.Old))
		ordered[i] = Patch{
			Offset: p.Offset,
			Old:    append([]byte(nil), p.Old...),
			New:    append([]byte(nil), p.New...),
		}
	}
	if !validArtifactSize(int64(len(ordered)), payload) {
		return nil, 0, ErrTooLarge
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Offset < ordered[j].Offset })
	for i := 1; i < len(ordered); i++ {
		previousEnd := ordered[i-1].Offset + int64(len(ordered[i-1].Old))
		if ordered[i].Offset < previousEnd {
			return nil, 0, ErrOverlap
		}
	}
	return ordered, payload, nil
}

func validArtifactSize(count int64, payload int64) bool {
	if count <= 0 || count > maxRecoveryEntries || payload <= 0 || payload > maxRecoveryPayloadBytes {
		return false
	}
	if count > (math.MaxInt64-sidecarHeaderSize-sidecarTrailerSize-payload)/sidecarEntryFixedSize {
		return false
	}
	size := sidecarHeaderSize + count*sidecarEntryFixedSize + payload + sidecarTrailerSize
	return size <= maxSidecarArtifactBytes
}

func validatePatchesAgainstSource(f *os.File, sourceSize int64, patches []Patch) error {
	buffer := make([]byte, 0)
	for _, patch := range patches {
		if patch.Offset > sourceSize-int64(len(patch.Old)) {
			return ErrOutOfRange
		}
		if cap(buffer) < len(patch.Old) {
			buffer = make([]byte, len(patch.Old))
		}
		current := buffer[:len(patch.Old)]
		if err := exactReadAt(f, current, patch.Offset); err != nil {
			return err
		}
		if !bytes.Equal(current, patch.Old) {
			return ErrDrift
		}
	}
	return nil
}

func applyEntries(f *os.File, patches []Patch) error {
	buffer := make([]byte, 0)
	for _, patch := range patches {
		if cap(buffer) < len(patch.Old) {
			buffer = make([]byte, len(patch.Old))
		}
		current := buffer[:len(patch.Old)]
		if err := exactReadAt(f, current, patch.Offset); err != nil {
			return fmt.Errorf("inplace: revalidate at %d: %w", patch.Offset, err)
		}
		if !bytes.Equal(current, patch.Old) {
			return fmt.Errorf("%w at offset %d", ErrDrift, patch.Offset)
		}
		if err := writeAtSource(f, patch.New, patch.Offset); err != nil {
			return fmt.Errorf("inplace: write at %d: %w", patch.Offset, err)
		}
	}
	return nil
}

func rollbackPatches(f *os.File, patches []Patch) error {
	entries := make([]entry, len(patches))
	for i, patch := range patches {
		entries[i] = entry{offset: patch.Offset, old: patch.Old, newHash: sha256.Sum256(patch.New)}
	}
	return rollbackEntries(f, entries)
}

func rollbackEntries(f *os.File, entries []entry) error {
	needsWrite := make([]bool, len(entries))
	buffer := make([]byte, 0)
	for i, recoveryEntry := range entries {
		if cap(buffer) < len(recoveryEntry.old) {
			buffer = make([]byte, len(recoveryEntry.old))
		}
		current := buffer[:len(recoveryEntry.old)]
		if err := exactReadAt(f, current, recoveryEntry.offset); err != nil {
			return err
		}
		if bytes.Equal(current, recoveryEntry.old) {
			continue
		}
		if sha256.Sum256(current) != recoveryEntry.newHash {
			return fmt.Errorf("%w at offset %d", ErrRecoveryDrift, recoveryEntry.offset)
		}
		needsWrite[i] = true
	}
	for i, recoveryEntry := range entries {
		if !needsWrite[i] {
			continue
		}
		current := buffer[:len(recoveryEntry.old)]
		if err := exactReadAt(f, current, recoveryEntry.offset); err != nil {
			return err
		}
		if bytes.Equal(current, recoveryEntry.old) {
			continue
		}
		if sha256.Sum256(current) != recoveryEntry.newHash {
			return fmt.Errorf("%w during rollback at offset %d", ErrRecoveryDrift, recoveryEntry.offset)
		}
		if err := writeAtSource(f, recoveryEntry.old, recoveryEntry.offset); err != nil {
			return fmt.Errorf("inplace: rollback write at %d: %w", recoveryEntry.offset, err)
		}
	}
	return nil
}

func validateSourceDescriptor(f *os.File, expected sourceDescriptor) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != expected.size {
		return ErrSourceMismatch
	}
	identity, err := sourceIdentityForFile(f)
	if err != nil {
		return err
	}
	if identity != expected.identity {
		return ErrSourceMismatch
	}
	return nil
}

func openRegularSource(path string, writable bool) (*os.File, error) {
	// The precheck avoids opening FIFOs/devices on platforms where even a
	// no-follow open could have special behavior. The retained-handle and
	// post-open SameFile checks reject substitutions around the open.
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, ErrUnsafeSource
	}
	f, err := openSourceNoFollow(path, writable)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if err := verifySourcePath(path, f, opened); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func verifySourcePath(path string, f *os.File, opened os.FileInfo) error {
	if f == nil || opened == nil || !opened.Mode().IsRegular() {
		return ErrUnsafeSource
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsafeSource, err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return ErrUnsafeSource
	}
	return nil
}

func inspectExtentState(f *os.File, entries []entry) (string, error) {
	hasOriginal := false
	hasPatched := false
	buffer := make([]byte, 0)
	for _, recoveryEntry := range entries {
		if cap(buffer) < len(recoveryEntry.old) {
			buffer = make([]byte, len(recoveryEntry.old))
		}
		current := buffer[:len(recoveryEntry.old)]
		if err := exactReadAt(f, current, recoveryEntry.offset); err != nil {
			return "", err
		}
		oldMatch := bytes.Equal(current, recoveryEntry.old)
		newMatch := sha256.Sum256(current) == recoveryEntry.newHash
		switch {
		case oldMatch && newMatch:
			// A no-op patch is valid in either phase and does not make a mixed
			// transaction partial by itself.
		case oldMatch:
			hasOriginal = true
		case newMatch:
			hasPatched = true
		default:
			return "drifted", nil
		}
	}
	switch {
	case hasOriginal && hasPatched:
		return "partial", nil
	case hasPatched:
		return "patched", nil
	default:
		return "original", nil
	}
}

func verifyCommittedExtents(f *os.File, entries []entry) error {
	buffer := make([]byte, 0)
	for _, recoveryEntry := range entries {
		if cap(buffer) < len(recoveryEntry.old) {
			buffer = make([]byte, len(recoveryEntry.old))
		}
		current := buffer[:len(recoveryEntry.old)]
		if err := exactReadAt(f, current, recoveryEntry.offset); err != nil {
			return err
		}
		if sha256.Sum256(current) != recoveryEntry.newHash {
			return fmt.Errorf("%w at committed offset %d", ErrRecoveryDrift, recoveryEntry.offset)
		}
	}
	return nil
}

func verifyOriginalExtents(f *os.File, entries []entry) error {
	buffer := make([]byte, 0)
	for _, recoveryEntry := range entries {
		if cap(buffer) < len(recoveryEntry.old) {
			buffer = make([]byte, len(recoveryEntry.old))
		}
		current := buffer[:len(recoveryEntry.old)]
		if err := exactReadAt(f, current, recoveryEntry.offset); err != nil {
			return err
		}
		if !bytes.Equal(current, recoveryEntry.old) {
			return fmt.Errorf("%w after rollback at offset %d", ErrRecoveryDrift, recoveryEntry.offset)
		}
	}
	return nil
}

type ownedSidecar struct {
	path     string
	file     *os.File
	info     os.FileInfo
	bodyHash [sha256.Size]byte
}

func createSidecar(path string, source sourceDescriptor, patches []Patch, payloadBytes int64) (_ *ownedSidecar, retErr error) {
	f, err := openExclusiveSidecar(path)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: %s", ErrSidecarExists, path)
		}
		return nil, err
	}
	sidecar := &ownedSidecar{path: path, file: f}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, sidecar.discard())
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	sidecar.info = info
	if err := sidecar.verifyOwnership(nil); err != nil {
		return nil, err
	}
	sidecarIdentity, err := sourceIdentityForFile(f)
	if err != nil {
		return nil, err
	}
	if sidecarIdentity == source.identity {
		return nil, fmt.Errorf("%w: source and sidecar identities match", ErrUnsafeSidecar)
	}

	header := encodeSidecarHeader(source, int64(len(patches)), payloadBytes)
	bodyHash := hashSidecarBody(header[:], patches)
	sidecar.bodyHash = bodyHash
	writeStateMarker(header[:], sidecarStatePending, bodyHash)
	w := bufio.NewWriterSize(f, 64<<10)
	if _, err := w.Write(header[:]); err != nil {
		return nil, err
	}
	for _, patch := range patches {
		var entryHeader [sidecarEntryFixedSize]byte
		binary.LittleEndian.PutUint64(entryHeader[0:8], uint64(patch.Offset))
		binary.LittleEndian.PutUint64(entryHeader[8:16], uint64(len(patch.Old)))
		newHash := sha256.Sum256(patch.New)
		copy(entryHeader[16:48], newHash[:])
		if _, err := w.Write(entryHeader[:]); err != nil {
			return nil, err
		}
		if _, err := w.Write(patch.Old); err != nil {
			return nil, err
		}
	}
	if _, err := w.Write(bodyHash[:]); err != nil {
		return nil, err
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if err := syncParent(path); err != nil {
		return nil, fmt.Errorf("inplace: sync sidecar directory: %w", err)
	}
	return sidecar, nil
}

func openOwnedSidecar(path string) (*ownedSidecar, error) {
	// Reject FIFOs, devices, directories, and links before open so a crafted
	// recovery pathname cannot block this process or trigger device I/O. The
	// post-open SameFile check below closes ordinary substitution races.
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, ErrUnsafeSidecar
	}
	f, err := openRecoverySidecar(path)
	if err != nil {
		return nil, err
	}
	sidecar := &ownedSidecar{path: path, file: f}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	sidecar.info = info
	if err := sidecar.verifyOwnership(nil); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return sidecar, nil
}

func (s *ownedSidecar) markCommitted() error {
	if s == nil || s.file == nil {
		return ErrUnsafeSidecar
	}
	marker := make([]byte, stateMarkerSize)
	marker[0] = sidecarStateCommitted
	marker[1] = ^sidecarStateCommitted
	digest := stateDigest(sidecarStateCommitted, s.bodyHash)
	copy(marker[8:], digest[:])
	if err := writeAtSidecar(s.file, marker, committedFlagOffset); err != nil {
		return err
	}
	return s.file.Sync()
}

func (s *ownedSidecar) verifyOwnership(sourceInfo os.FileInfo) error {
	if s == nil || s.file == nil || s.info == nil || !s.info.Mode().IsRegular() {
		return ErrUnsafeSidecar
	}
	pathInfo, err := os.Lstat(s.path)
	if err != nil {
		return fmt.Errorf("%w: stat artifact: %v", ErrUnsafeSidecar, err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || !os.SameFile(s.info, pathInfo) {
		return ErrUnsafeSidecar
	}
	if sourceInfo != nil && os.SameFile(sourceInfo, s.info) {
		return fmt.Errorf("%w: source and sidecar are the same file", ErrUnsafeSidecar)
	}
	return nil
}

func (s *ownedSidecar) discard() error {
	if s == nil || s.file == nil {
		return nil
	}
	if err := s.verifyOwnership(nil); err != nil {
		return errors.Join(err, s.close())
	}
	removeErr := os.Remove(s.path)
	if removeErr == nil {
		removeErr = syncParent(s.path)
	}
	return errors.Join(removeErr, s.close())
}

func (s *ownedSidecar) remove() error { return s.discard() }

func (s *ownedSidecar) close() error {
	if s == nil || s.file == nil {
		return nil
	}
	f := s.file
	s.file = nil
	return f.Close()
}

func encodeSidecarHeader(source sourceDescriptor, count int64, payload int64) [sidecarHeaderSize]byte {
	var header [sidecarHeaderSize]byte
	copy(header[0:8], sidecarMagic[:])
	binary.LittleEndian.PutUint64(header[48:56], uint64(source.size))
	header[56] = source.identity.kind
	binary.LittleEndian.PutUint64(header[64:72], source.identity.a)
	binary.LittleEndian.PutUint64(header[72:80], source.identity.b)
	binary.LittleEndian.PutUint64(header[80:88], uint64(count))
	binary.LittleEndian.PutUint64(header[88:96], uint64(payload))
	return header
}

func hashSidecarBody(header []byte, patches []Patch) [sha256.Size]byte {
	h := sha256.New()
	_, _ = h.Write(header[0:8])
	_, _ = h.Write(header[48:])
	for _, patch := range patches {
		var entryHeader [sidecarEntryFixedSize]byte
		binary.LittleEndian.PutUint64(entryHeader[0:8], uint64(patch.Offset))
		binary.LittleEndian.PutUint64(entryHeader[8:16], uint64(len(patch.Old)))
		newHash := sha256.Sum256(patch.New)
		copy(entryHeader[16:48], newHash[:])
		_, _ = h.Write(entryHeader[:])
		_, _ = h.Write(patch.Old)
	}
	var result [sha256.Size]byte
	copy(result[:], h.Sum(nil))
	return result
}

func writeStateMarker(header []byte, state byte, bodyHash [sha256.Size]byte) {
	header[8] = state
	header[9] = ^state
	digest := stateDigest(state, bodyHash)
	copy(header[16:48], digest[:])
}

func stateDigest(state byte, bodyHash [sha256.Size]byte) [sha256.Size]byte {
	h := sha256.New()
	_, _ = h.Write(stateDigestDomain)
	_, _ = h.Write(bodyHash[:])
	_, _ = h.Write([]byte{state, ^state})
	var result [sha256.Size]byte
	copy(result[:], h.Sum(nil))
	return result
}

func readSidecar(r io.Reader, artifactSize int64) (*sidecarRecord, error) {
	if artifactSize < sidecarHeaderSize+sidecarEntryFixedSize+1+sidecarTrailerSize || artifactSize > maxSidecarArtifactBytes {
		return nil, fmt.Errorf("%w: artifact size %d is outside bounds", ErrInvalidSidecar, artifactSize)
	}
	var header [sidecarHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrInvalidSidecar, err)
	}
	if !bytes.Equal(header[0:8], sidecarMagic[:]) {
		return nil, fmt.Errorf("%w: bad magic or unsupported version", ErrInvalidSidecar)
	}
	if !allZero(header[10:16]) || !allZero(header[57:64]) {
		return nil, fmt.Errorf("%w: non-zero reserved fields", ErrInvalidSidecar)
	}
	state := header[8]
	if (state != sidecarStatePending && state != sidecarStateCommitted) || header[9] != ^state {
		return nil, fmt.Errorf("%w: bad transaction state", ErrInvalidSidecar)
	}
	sourceSize := int64(binary.LittleEndian.Uint64(header[48:56]))
	count := int64(binary.LittleEndian.Uint64(header[80:88]))
	payload := int64(binary.LittleEndian.Uint64(header[88:96]))
	if sourceSize < 0 || !validArtifactSize(count, payload) {
		return nil, fmt.Errorf("%w: invalid source size, count, or payload", ErrInvalidSidecar)
	}
	expectedSize := sidecarHeaderSize + count*sidecarEntryFixedSize + payload + sidecarTrailerSize
	if expectedSize != artifactSize {
		return nil, fmt.Errorf("%w: expected exact size %d, got %d", ErrInvalidSidecar, expectedSize, artifactSize)
	}
	identity := sourceIdentity{
		kind: header[56],
		a:    binary.LittleEndian.Uint64(header[64:72]),
		b:    binary.LittleEndian.Uint64(header[72:80]),
	}
	if identity.kind != identityKindWindows && identity.kind != identityKindUnix {
		return nil, fmt.Errorf("%w: unsupported source identity", ErrInvalidSidecar)
	}

	h := sha256.New()
	_, _ = h.Write(header[0:8])
	_, _ = h.Write(header[48:])
	entries := make([]entry, 0, int(count))
	remainingPayload := payload
	var previousEnd int64
	for i := int64(0); i < count; i++ {
		var entryHeader [sidecarEntryFixedSize]byte
		if _, err := io.ReadFull(r, entryHeader[:]); err != nil {
			return nil, fmt.Errorf("%w: entry %d header: %v", ErrInvalidSidecar, i, err)
		}
		_, _ = h.Write(entryHeader[:])
		offset := int64(binary.LittleEndian.Uint64(entryHeader[0:8]))
		length := int64(binary.LittleEndian.Uint64(entryHeader[8:16]))
		if offset < 0 || length <= 0 || length > remainingPayload || offset > sourceSize-length {
			return nil, fmt.Errorf("%w: entry %d range", ErrInvalidSidecar, i)
		}
		if i > 0 && offset < previousEnd {
			return nil, fmt.Errorf("%w: entry %d overlaps or is unordered", ErrInvalidSidecar, i)
		}
		previousEnd = offset + length
		old := make([]byte, int(length))
		if _, err := io.ReadFull(r, old); err != nil {
			return nil, fmt.Errorf("%w: entry %d payload: %v", ErrInvalidSidecar, i, err)
		}
		_, _ = h.Write(old)
		var newHash [sha256.Size]byte
		copy(newHash[:], entryHeader[16:48])
		entries = append(entries, entry{offset: offset, old: old, newHash: newHash})
		remainingPayload -= length
	}
	if remainingPayload != 0 {
		return nil, fmt.Errorf("%w: payload total mismatch", ErrInvalidSidecar)
	}
	var storedBodyHash [sha256.Size]byte
	if _, err := io.ReadFull(r, storedBodyHash[:]); err != nil {
		return nil, fmt.Errorf("%w: checksum: %v", ErrInvalidSidecar, err)
	}
	computedBodyHash := hashSum(h)
	if computedBodyHash != storedBodyHash {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrInvalidSidecar)
	}
	expectedStateDigest := stateDigest(state, computedBodyHash)
	if !bytes.Equal(header[16:48], expectedStateDigest[:]) {
		return nil, fmt.Errorf("%w: state checksum mismatch", ErrInvalidSidecar)
	}
	var trailing [1]byte
	if n, err := r.Read(trailing[:]); n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return nil, fmt.Errorf("%w: trailing data", ErrInvalidSidecar)
	}
	return &sidecarRecord{
		committed: state == sidecarStateCommitted,
		source: sourceDescriptor{
			size:     sourceSize,
			identity: identity,
		},
		entries:  entries,
		bodyHash: computedBodyHash,
		payload:  payload,
	}, nil
}

func hashSum(h hash.Hash) [sha256.Size]byte {
	var result [sha256.Size]byte
	copy(result[:], h.Sum(nil))
	return result
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

func exactReadAt(f *os.File, data []byte, offset int64) error {
	n, err := f.ReadAt(data, offset)
	if n != len(data) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func exactWriteAt(f *os.File, data []byte, offset int64) error {
	n, err := f.WriteAt(data, offset)
	if n != len(data) && err == nil {
		err = io.ErrShortWrite
	}
	return err
}

func joinCleanup(primary error, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	return errors.Join(primary, cleanup)
}

func syncParentDirectory(path string) error {
	dirPath, _ := filepath.Split(path)
	if dirPath == "" {
		dirPath = "."
	}
	dir, err := directoryfile.OpenForSync(dirPath)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if runtime.GOOS == "windows" && err != nil {
		// Windows does not expose a portable directory-fsync contract through
		// os.File. File data and the exact sidecar handle are still flushed; the
		// public in-place feature remains disabled, so this best effort cannot be
		// mistaken for the final cross-platform durability guarantee.
		err = nil
	}
	return errors.Join(err, closeErr)
}
