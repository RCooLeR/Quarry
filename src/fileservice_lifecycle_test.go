package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/quarry/quarry-wails3/internal/session"
)

func waitForTransition(t *testing.T, service *FileService, fileID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		lease, err := service.reg.AcquireRead(fileID)
		if errors.Is(err, session.ErrTransitioning) {
			return
		}
		if lease != nil {
			lease.Release()
		}
		if err != nil {
			t.Fatalf("waiting for transition: %v", err)
		}
		runtime.Gosched()
	}
	t.Fatalf("file %q did not enter transition", fileID)
}

func installBlockingLeaseHook(t *testing.T, operation string, fileID string) (<-chan struct{}, func()) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	restore := installServiceLeaseHook(func(gotOperation string, gotFileID string, _ session.FileSnapshot) {
		if gotOperation != operation || gotFileID != fileID {
			return
		}
		enteredOnce.Do(func() { close(entered) })
		<-release
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		restore()
	})
	return entered, func() { releaseOnce.Do(func() { close(release) }) }
}

func waitForActiveFileJob(t *testing.T, service *FileService, fileID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		manager := service.jobs()
		manager.mu.Lock()
		active := manager.active != nil && manager.active.spec.FileID == fileID
		manager.mu.Unlock()
		if active {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %q did not register an active job", fileID)
		}
		time.Sleep(time.Millisecond)
	}
}

func lifecycleInterrupted(err error) bool {
	return errors.Is(err, ErrJobCancelled) || errors.Is(err, context.Canceled) || errors.Is(err, session.ErrTransitioning)
}

func TestRefreshCommittedCloseErrorCleansGenerationOwnedServiceState(t *testing.T) {
	t.Setenv("QUARRY_HOME", t.TempDir())
	service := NewFileService()
	path := writeTempFile(t, "refresh-committed-close-error.txt", []byte("alpha,beta\n1,2\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.CloseFile(meta.FileID) })
	before, ok := service.reg.Snapshot(meta.FileID)
	if !ok {
		t.Fatal("missing initial file generation")
	}
	if _, err := service.PrepareEditSession(meta.FileID); err != nil {
		t.Fatal(err)
	}
	if preparedEditCountForTest(service) != 1 {
		t.Fatal("prepared edit admission was not installed")
	}
	const cursorOffset = int64(7)
	service.issueCSVGridCursor(meta.FileID, before.Generation, ',', cursorOffset)
	if err := service.validateCSVGridCursor(meta.FileID, before.Generation, ',', cursorOffset); err != nil {
		t.Fatalf("issued CSV cursor was not registered: %v", err)
	}

	injected := errors.New("injected committed refresh cleanup failure")
	originalReopen := service.reopenValidated
	service.reopenValidated = func(registry *session.Registry, lease *session.Lease, validate func(*session.File) error) (*session.File, error) {
		replacement, reopenErr := originalReopen(registry, lease, validate)
		if reopenErr != nil || replacement == nil {
			return replacement, reopenErr
		}
		return replacement, &session.ReopenCommittedError{FileID: replacement.ID, Err: injected}
	}

	refreshed, err := service.RefreshFile(meta.FileID)
	if err != nil {
		t.Fatalf("committed refresh RPC error = %v; authoritative metadata must remain deliverable", err)
	}
	if refreshed.FileID != meta.FileID || refreshed.RefreshWarning == "" || !strings.Contains(refreshed.RefreshWarning, injected.Error()) {
		t.Fatalf("committed refresh metadata = %+v; want same id and cleanup warning", refreshed)
	}
	after, ok := service.reg.Snapshot(meta.FileID)
	if !ok || after.Generation != before.Generation+1 || after.Doc == before.Doc {
		t.Fatalf("refresh did not retain committed replacement: before=%+v after=%+v present=%v", before, after, ok)
	}
	if preparedEditCountForTest(service) != 0 {
		t.Fatal("committed refresh error retained old prepared-edit admission")
	}
	if err := service.validateCSVGridCursor(meta.FileID, before.Generation, ',', cursorOffset); !errors.Is(err, ErrCSVGridCursorInvalid) {
		t.Fatalf("committed refresh error retained old CSV cursor: %v", err)
	}

	service.reopenValidated = originalReopen
	if refreshed, err := service.RefreshFile(meta.FileID); err != nil {
		t.Fatalf("normal refresh after committed cleanup error: %v", err)
	} else if refreshed.FileID != meta.FileID {
		t.Fatalf("normal refresh file id = %q, want %q", refreshed.FileID, meta.FileID)
	}
}

func TestRefreshWarningIsBoundedValidUTF8(t *testing.T) {
	invalidLong := errors.New("\x00header\r\n" + strings.Repeat("ж", maxRefreshWarningBytes) + "\xff")
	warning := boundedRefreshWarning(invalidLong)
	if len(warning) > maxRefreshWarningBytes {
		t.Fatalf("warning length = %d, want <= %d", len(warning), maxRefreshWarningBytes)
	}
	if !utf8.ValidString(warning) {
		t.Fatalf("warning is not valid UTF-8: %q", warning)
	}
	for _, r := range warning {
		if unicode.IsControl(r) {
			t.Fatalf("warning contains control character %U: %q", r, warning)
		}
	}
	if !strings.HasSuffix(warning, "…") {
		t.Fatalf("truncated warning = %q, want ellipsis suffix", warning)
	}
}

func TestRefreshNonCommittedErrorStillFailsWithoutReplacingGeneration(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "refresh-uncommitted-error.txt", []byte("unchanged\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.CloseFile(meta.FileID) })
	before, ok := service.reg.Snapshot(meta.FileID)
	if !ok {
		t.Fatal("missing initial file generation")
	}

	injected := errors.New("candidate open failed before commit")
	service.reopenValidated = func(*session.Registry, *session.Lease, func(*session.File) error) (*session.File, error) {
		return nil, injected
	}
	if refreshed, err := service.RefreshFile(meta.FileID); !errors.Is(err, injected) {
		t.Fatalf("RefreshFile = %+v, %v; want uncommitted failure", refreshed, err)
	} else if refreshed != (FileMeta{}) {
		t.Fatalf("RefreshFile metadata = %+v on uncommitted failure, want zero value", refreshed)
	}
	after, ok := service.reg.Snapshot(meta.FileID)
	if !ok || after.Generation != before.Generation || after.Doc != before.Doc {
		t.Fatalf("uncommitted failure replaced generation: before=%+v after=%+v present=%v", before, after, ok)
	}
}

func TestCloseFileWaitsForReadLeaseAndRejectsNewRPC(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "close-read.txt", []byte("stable source"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lease, file, err := service.acquireReadFile(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- service.CloseFile(meta.FileID) }()
	waitForTransition(t, service, meta.FileID)
	if _, err := service.GetWindow(meta.FileID, 0, 1024); err == nil {
		t.Fatal("new RPC acquired after close began")
	}
	if _, err := service.StageEdit(meta.FileID, 0, 6, "changed"); err == nil {
		t.Fatal("staging writer acquired after clean close committed")
	}
	if got, err := file.Doc.ReadRange(0, 6); err != nil || string(got) != "stable" {
		t.Fatalf("active read lease lost its document: %q, %v", got, err)
	}
	select {
	case err := <-closeDone:
		t.Fatalf("close completed before read release: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	lease.Release()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not finish after read release")
	}
}

func TestRefreshCancelsSearchThenDrainsGenerationLease(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "refresh-search.txt", []byte("needle\nother\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, ok := service.reg.Snapshot(meta.FileID); ok {
			_ = service.CloseFile(meta.FileID)
		}
	}()
	entered, release := installBlockingLeaseHook(t, "search", meta.FileID)
	searchDone := make(chan error, 1)
	go func() {
		_, err := service.findNext(meta.FileID, "needle", 0, false, true, false)
		searchDone <- err
	}()
	<-entered
	refreshDone := make(chan error, 1)
	go func() {
		_, err := service.RefreshFile(meta.FileID)
		refreshDone <- err
	}()
	waitForTransition(t, service, meta.FileID)
	select {
	case err := <-refreshDone:
		t.Fatalf("refresh completed while search held generation: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	release()
	if err := <-searchDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("search error = %v, want lifecycle cancellation", err)
	}
	select {
	case err := <-refreshDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not finish after search released")
	}
	snapshot, ok := service.reg.Snapshot(meta.FileID)
	if !ok || snapshot.Generation != 2 {
		t.Fatalf("refresh generation = %+v, present=%v", snapshot, ok)
	}
}

func TestStageAndDiscardSerializeExclusiveEditOwnership(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "stage-discard.txt", []byte("alpha\nbeta\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)
	mustPrepareEditSession(t, service, meta.FileID)
	entered, release := installBlockingLeaseHook(t, "stage-edit", meta.FileID)
	stageDone := make(chan error, 1)
	go func() {
		_, err := service.StageEdit(meta.FileID, 0, 5, "omega")
		stageDone <- err
	}()
	<-entered
	discardDone := make(chan error, 1)
	go func() {
		_, err := service.DiscardEdits(meta.FileID)
		discardDone <- err
	}()
	select {
	case err := <-discardDone:
		t.Fatalf("discard bypassed active stage ownership: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	release()
	if err := <-stageDone; err != nil {
		t.Fatal(err)
	}
	if err := <-discardDone; err != nil {
		t.Fatal(err)
	}
	state, err := service.GetStagingState(meta.FileID)
	if err != nil || state.EditCount != 0 {
		t.Fatalf("serialized discard state = %+v, %v", state, err)
	}
}

func TestCloseWaitsForConcurrentStageThenRejectsDirtySession(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "stage-close.txt", []byte("alpha\nbeta\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = service.DiscardEdits(meta.FileID)
		_ = service.CloseFile(meta.FileID)
	})
	mustPrepareEditSession(t, service, meta.FileID)
	entered, release := installBlockingLeaseHook(t, "stage-edit", meta.FileID)
	stageDone := make(chan error, 1)
	go func() {
		_, err := service.StageEdit(meta.FileID, 0, 5, "omega")
		stageDone <- err
	}()
	<-entered

	closeDone := make(chan error, 1)
	go func() { closeDone <- service.CloseFile(meta.FileID) }()
	select {
	case err := <-closeDone:
		t.Fatalf("close bypassed active staging ownership: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	release()
	if err := <-stageDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closeDone:
		if !errors.Is(err, ErrStagedEditsPending) {
			t.Fatalf("CloseFile error = %v, want ErrStagedEditsPending", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not finish after staging released ownership")
	}
	state, err := service.GetStagingState(meta.FileID)
	if err != nil || state.EditCount != 1 {
		t.Fatalf("rejected close state = %+v, %v", state, err)
	}
}

func TestSaveCopyBlocksDiscardUntilPublished(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "save-discard.txt", []byte("alpha\nbeta\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)
	mustPrepareEditSession(t, service, meta.FileID)
	if _, err := service.StageEdit(meta.FileID, 0, 5, "omega"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "saved.txt")
	entered, release := installBlockingLeaseHook(t, "save-copy", meta.FileID)
	saveDone := make(chan error, 1)
	go func() {
		_, err := service.saveCopy(meta.FileID, dst)
		saveDone <- err
	}()
	<-entered
	discardDone := make(chan error, 1)
	go func() {
		_, err := service.DiscardEdits(meta.FileID)
		discardDone <- err
	}()
	select {
	case err := <-discardDone:
		t.Fatalf("discard bypassed active save ownership: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	release()
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}
	if err := <-discardDone; err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "omega\nbeta\n" {
		t.Fatalf("saved output = %q, %v", got, err)
	}
}

func TestSQLAnalyzeCloseCancelsThenDrainsLease(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "analyze-close.sql", []byte("CREATE TABLE t (id INT);\nINSERT INTO t VALUES (1);\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := installBlockingLeaseHook(t, "sql-analyze", meta.FileID)
	analyzeDone := make(chan error, 1)
	go func() {
		_, err := service.SqlAnalyze(meta.FileID)
		analyzeDone <- err
	}()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- service.CloseFile(meta.FileID) }()
	waitForTransition(t, service, meta.FileID)
	select {
	case err := <-closeDone:
		t.Fatalf("close completed while SQL lease was held: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	release()
	if err := <-analyzeDone; err == nil {
		t.Fatal("analysis published after close cancellation")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not finish after SQL analysis released")
	}
}

func TestCloseInterruptsPrepareEditQueuedForExclusiveLease(t *testing.T) {
	const source = "alpha\nbeta\n"
	service := NewFileService()
	path := writeTempFile(t, "prepare-queued-close.txt", []byte(source))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blocker, _, err := service.acquireReadFile(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	releaseBlocker := func() { releaseOnce.Do(blocker.Release) }
	t.Cleanup(releaseBlocker)

	prepareDone := make(chan error, 1)
	go func() {
		_, err := service.PrepareEditSession(meta.FileID)
		prepareDone <- err
	}()
	waitForActiveFileJob(t, service, meta.FileID)
	closeDone := make(chan error, 1)
	go func() { closeDone <- service.CloseFile(meta.FileID) }()
	waitForTransition(t, service, meta.FileID)
	select {
	case err := <-prepareDone:
		if !lifecycleInterrupted(err) {
			t.Fatalf("queued prepare error = %v, want lifecycle interruption", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued prepare did not stop while another reader retained its lease")
	}
	if preparedEditCountForTest(service) != 0 {
		t.Fatal("interrupted queued prepare retained an admission slot")
	}
	releaseBlocker()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not finish after the pre-existing reader released")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != source {
		t.Fatalf("source after queued prepare cancellation = %q, %v", got, err)
	}
}

func TestCloseCancelsHeldPrepareEditBeforeSessionInstall(t *testing.T) {
	const source = "alpha\nbeta\n"
	service := NewFileService()
	path := writeTempFile(t, "prepare-held-close.txt", []byte(source))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := installBlockingLeaseHook(t, "prepare-edit", meta.FileID)
	prepareDone := make(chan error, 1)
	go func() {
		_, err := service.PrepareEditSession(meta.FileID)
		prepareDone <- err
	}()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- service.CloseFile(meta.FileID) }()
	waitForTransition(t, service, meta.FileID)
	release()
	if err := <-prepareDone; !errors.Is(err, ErrJobCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("held prepare error = %v, want canceled job", err)
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not drain held prepare")
	}
	if preparedEditCountForTest(service) != 0 {
		t.Fatal("held canceled prepare retained an admission slot")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != source {
		t.Fatalf("source after held prepare cancellation = %q, %v", got, err)
	}
}

func TestRefreshInterruptsSaveCopyQueuedForExclusiveLeaseAndPreservesEdits(t *testing.T) {
	const source = "alpha\nbeta\n"
	service := NewFileService()
	path := writeTempFile(t, "save-queued-refresh.txt", []byte(source))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = service.DiscardEdits(meta.FileID)
		_ = service.CloseFile(meta.FileID)
	})
	mustPrepareEditSession(t, service, meta.FileID)
	if _, err := service.StageEdit(meta.FileID, 0, 5, "omega"); err != nil {
		t.Fatal(err)
	}
	blocker, _, err := service.acquireReadFile(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	releaseBlocker := func() { releaseOnce.Do(blocker.Release) }
	t.Cleanup(releaseBlocker)
	dst := filepath.Join(t.TempDir(), "queued-copy.txt")
	saveDone := make(chan error, 1)
	go func() {
		_, err := service.saveCopy(meta.FileID, dst)
		saveDone <- err
	}()
	waitForActiveFileJob(t, service, meta.FileID)
	refreshDone := make(chan error, 1)
	go func() {
		_, err := service.RefreshFile(meta.FileID)
		refreshDone <- err
	}()
	waitForTransition(t, service, meta.FileID)
	select {
	case err := <-saveDone:
		if !lifecycleInterrupted(err) {
			t.Fatalf("queued save error = %v, want lifecycle interruption", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued save did not stop while another reader retained its lease")
	}
	releaseBlocker()
	if err := <-refreshDone; !errors.Is(err, ErrStagedEditsPending) {
		t.Fatalf("refresh error = %v, want ErrStagedEditsPending", err)
	}
	state, err := service.GetStagingState(meta.FileID)
	if err != nil || state.EditCount != 1 {
		t.Fatalf("staging state after canceled queued save = %+v, %v", state, err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("queued canceled save destination state = %v, want absent", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != source {
		t.Fatalf("source after queued save cancellation = %q, %v", got, err)
	}
}

func TestRefreshCancelsHeldSaveCopyAndPreservesDirtySession(t *testing.T) {
	const source = "alpha\nbeta\n"
	service := NewFileService()
	path := writeTempFile(t, "save-held-refresh.txt", []byte(source))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = service.DiscardEdits(meta.FileID)
		_ = service.CloseFile(meta.FileID)
	})
	mustPrepareEditSession(t, service, meta.FileID)
	if _, err := service.StageEdit(meta.FileID, 0, 5, "omega"); err != nil {
		t.Fatal(err)
	}
	entered, release := installBlockingLeaseHook(t, "save-copy", meta.FileID)
	dst := filepath.Join(t.TempDir(), "held-copy.txt")
	saveDone := make(chan error, 1)
	go func() {
		_, err := service.saveCopy(meta.FileID, dst)
		saveDone <- err
	}()
	<-entered
	refreshDone := make(chan error, 1)
	go func() {
		_, err := service.RefreshFile(meta.FileID)
		refreshDone <- err
	}()
	waitForTransition(t, service, meta.FileID)
	release()
	if err := <-saveDone; !errors.Is(err, ErrJobCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("held save error = %v, want canceled job", err)
	}
	if err := <-refreshDone; !errors.Is(err, ErrStagedEditsPending) {
		t.Fatalf("refresh error = %v, want ErrStagedEditsPending", err)
	}
	state, err := service.GetStagingState(meta.FileID)
	if err != nil || state.EditCount != 1 {
		t.Fatalf("staging state after held save cancellation = %+v, %v", state, err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("held canceled save destination state = %v, want absent", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != source {
		t.Fatalf("source after held save cancellation = %q, %v", got, err)
	}
}
