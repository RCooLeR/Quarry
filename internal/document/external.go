package document

import (
	"errors"
	"os"
	"time"

	"github.com/quarry/quarry-wails3/internal/fileio"
	"github.com/quarry/quarry-wails3/internal/regularfile"
)

// FileState captures the on-disk state of the currently opened file.
type FileState struct {
	Size    int64
	ModTime time.Time
}

// OpenedFileInfo returns identity information for the exact retained file
// handle, not for whatever currently occupies the document's path. It is used
// to recognize hard-link, symlink, relative-path, and case aliases without
// canonicalizing the path spelling presented by the user.
func (d *FileDocument) OpenedFileInfo() (os.FileInfo, error) {
	if d == nil {
		return nil, errors.New("document is required")
	}
	d.lifecycleMu.RLock()
	defer d.lifecycleMu.RUnlock()
	if d.closed || d.file == nil {
		return nil, errDocumentClosed
	}
	return d.file.Stat()
}

// CurrentPathIdentity compares the pathname's current target with the exact
// file handle retained by this document. It detects rename-and-recreate even
// when size and timestamps happen to match.
func (d *FileDocument) CurrentPathIdentity() (FileState, bool, error) {
	if d == nil {
		return FileState{}, false, errors.New("document is required")
	}
	d.lifecycleMu.RLock()
	defer d.lifecycleMu.RUnlock()
	if d.closed || d.file == nil {
		return FileState{}, false, errDocumentClosed
	}
	if err := fileio.RequireAtomicWriteReadReady(d.path); err != nil {
		return FileState{}, false, err
	}
	openedInfo, err := d.file.Stat()
	if err != nil {
		return FileState{}, false, err
	}
	pathInfo, err := os.Stat(d.path)
	if err != nil {
		return FileState{}, false, err
	}
	if !pathInfo.Mode().IsRegular() {
		return FileState{}, false, &os.PathError{Op: "stat", Path: d.path, Err: ErrNonRegularSource}
	}
	return FileState{Size: pathInfo.Size(), ModTime: pathInfo.ModTime()}, os.SameFile(openedInfo, pathInfo), nil
}

// ExternalModification compares the original open state to the current file on disk.
type ExternalModification struct {
	Original FileState
	Current  FileState
	Metadata Metadata
}

func (s FileState) Equal(other FileState) bool {
	return s.Size == other.Size && s.ModTime.Equal(other.ModTime)
}

func (d *FileDocument) OriginalFileState() FileState {
	return FileState{Size: d.size, ModTime: d.mtime}
}

// HasMutationGeneration reports whether this retained handle has an OS
// mutation-generation token captured at open. Artifact transforms use this to
// fail closed on platforms/filesystems where size and mtime are the only
// available signals and can therefore be restored after a rewrite.
func (d *FileDocument) HasMutationGeneration() bool {
	if d == nil {
		return false
	}
	d.lifecycleMu.RLock()
	defer d.lifecycleMu.RUnlock()
	return !d.closed && d.file != nil && d.change.available && d.change.strong
}

func (d *FileDocument) CurrentFileState() (FileState, error) {
	if err := fileio.RequireAtomicWriteReadReady(d.path); err != nil {
		return FileState{}, err
	}
	st, err := os.Stat(d.path)
	if err != nil {
		return FileState{}, err
	}
	if !st.Mode().IsRegular() {
		return FileState{}, &os.PathError{Op: "stat", Path: d.path, Err: ErrNonRegularSource}
	}
	return FileState{Size: st.Size(), ModTime: st.ModTime()}, nil
}

func (d *FileDocument) InspectExternalModification() (ExternalModification, bool, error) {
	current, sameIdentity, err := d.CurrentPathIdentity()
	if err != nil {
		return ExternalModification{}, false, err
	}
	original := d.OriginalFileState()
	if sameIdentity && original.Equal(current) {
		return ExternalModification{}, false, nil
	}

	if err := fileio.RequireAtomicWriteReadReady(d.path); err != nil {
		return ExternalModification{}, true, err
	}
	reopened, err := regularfile.Open(d.path)
	if err != nil {
		return ExternalModification{}, true, err
	}
	defer reopened.Close()
	openedInfo, err := reopened.Stat()
	if err != nil {
		return ExternalModification{}, true, err
	}
	openedState := FileState{Size: openedInfo.Size(), ModTime: openedInfo.ModTime()}
	if !openedState.Equal(current) {
		return ExternalModification{}, true, errors.New("source changed while external modification metadata was opened")
	}
	sample, err := readSample(reopened, openedState.Size, openSampleSize)
	if err != nil {
		return ExternalModification{}, true, err
	}
	finalInfo, err := reopened.Stat()
	if err != nil {
		return ExternalModification{}, true, err
	}
	pathInfo, err := os.Stat(d.path)
	if err != nil {
		return ExternalModification{}, true, err
	}
	if !pathInfo.Mode().IsRegular() || !os.SameFile(openedInfo, finalInfo) ||
		!os.SameFile(finalInfo, pathInfo) || finalInfo.Size() != openedInfo.Size() ||
		!finalInfo.ModTime().Equal(openedInfo.ModTime()) || pathInfo.Size() != finalInfo.Size() ||
		!pathInfo.ModTime().Equal(finalInfo.ModTime()) {
		return ExternalModification{}, true, errors.New("source changed while external modification metadata was read")
	}

	return ExternalModification{
		Original: original,
		Current:  openedState,
		Metadata: detectMetadata(d.path, openedState.Size, sample),
	}, true, nil
}
