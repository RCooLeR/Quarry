package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/manualedit"
	"github.com/quarry/quarry-wails3/internal/session"
)

func mustPrepareEditSession(t *testing.T, svc *FileService, fileID string) StagingState {
	t.Helper()
	state, err := svc.PrepareEditSession(fileID)
	if err != nil {
		t.Fatalf("PrepareEditSession: %v", err)
	}
	return state
}

func preparedEditCountForTest(svc *FileService) int {
	svc.editMu.Lock()
	defer svc.editMu.Unlock()
	return len(svc.preparedEdits)
}

func TestPreparedEditAggregateAccountingHasFixedCeilings(t *testing.T) {
	perSession := manualedit.DefaultRetainedMemoryUpperBound()
	aggregateRetained := int64(maxPreparedEditSessions) * perSession
	const maxReviewedAggregateRetained = int64(300 * 1024 * 1024)
	if perSession <= 0 || aggregateRetained <= 0 || aggregateRetained > maxReviewedAggregateRetained {
		t.Fatalf("prepared edit accounting = %d per session, %d aggregate; reviewed aggregate ceiling %d",
			perSession, aggregateRetained, maxReviewedAggregateRetained)
	}
	aggregateTransient := int64(maxPreparedEditSessions) * manualedit.DefaultMaxTransientBytes
	if aggregateTransient != 768*1024*1024 {
		t.Fatalf("unserialized aggregate transient multiplier = %d, want 768 MiB", aggregateTransient)
	}
	serializedStagePeak := manualedit.DefaultMaxTransientBytes + int64(maxPreparedEditSessions-1)*perSession
	const maxReviewedSerializedStagePeak = int64(420 * 1024 * 1024)
	if serializedStagePeak > maxReviewedSerializedStagePeak {
		t.Fatalf("serialized StageEdit accounting peak = %d, reviewed ceiling %d", serializedStagePeak, maxReviewedSerializedStagePeak)
	}
}

func TestStageEditSerializesHeavyWorkAcrossFiles(t *testing.T) {
	svc := NewFileService()
	firstPath := writeTempFile(t, "stage-serial-first.txt", []byte("alpha\nbeta\n"))
	secondPath := writeTempFile(t, "stage-serial-second.txt", []byte("bravo\ncharlie\n"))
	first, err := svc.OpenFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.OpenFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = svc.DiscardEdits(first.FileID)
		_, _ = svc.DiscardEdits(second.FileID)
		_ = svc.CloseFile(first.FileID)
		_ = svc.CloseFile(second.FileID)
	})
	mustPrepareEditSession(t, svc, first.FileID)
	mustPrepareEditSession(t, svc, second.FileID)

	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var firstOnce, secondOnce, releaseOnce sync.Once
	releaseFirstStage := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	restore := installServiceLeaseHook(func(operation, fileID string, _ session.FileSnapshot) {
		if operation != "stage-edit" {
			return
		}
		switch fileID {
		case first.FileID:
			firstOnce.Do(func() { close(firstEntered) })
			<-releaseFirst
		case second.FileID:
			secondOnce.Do(func() { close(secondEntered) })
		}
	})
	t.Cleanup(restore)
	// Always unblock the hook before file cleanup, including an early assertion
	// failure; otherwise cleanup itself could hide the useful failure in a lease
	// wait.
	t.Cleanup(releaseFirstStage)

	firstDone := make(chan error, 1)
	go func() {
		_, err := svc.StageEdit(first.FileID, 0, 5, "omega")
		firstDone <- err
	}()
	select {
	case <-firstEntered:
	case err := <-firstDone:
		t.Fatalf("first StageEdit returned before its lease hook: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("first StageEdit did not reach its lease hook")
	}
	if svc.editWorkMu.TryLock() {
		svc.editWorkMu.Unlock()
		t.Fatal("first StageEdit reached its lease hook without retaining global edit-work ownership")
	}
	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		_, err := svc.StageEdit(second.FileID, 0, 5, "delta")
		secondDone <- err
	}()
	<-secondStarted
	select {
	case <-secondEntered:
		t.Fatal("second StageEdit acquired its file lease while first retained global edit-work ownership")
	case err := <-secondDone:
		t.Fatalf("second StageEdit completed before first released: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	releaseFirstStage()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first StageEdit did not finish after release")
	}
	select {
	case <-secondEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("second StageEdit did not acquire after first released")
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second StageEdit did not finish after acquiring its lease")
	}
}

func TestStageEditRequiresExplicitPreparedSession(t *testing.T) {
	const source = "alpha\nbeta\n"
	path := writeTempFile(t, "unprepared.txt", []byte(source))
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)

	if _, err := svc.StageEdit(meta.FileID, 0, 5, "omega"); !errors.Is(err, ErrEditSessionNotPrepared) {
		t.Fatalf("StageEdit error = %v, want ErrEditSessionNotPrepared", err)
	}
	lease, file, err := svc.acquireReadFile(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if file.Edit != nil {
		lease.Release()
		t.Fatal("unprepared StageEdit allocated an edit session")
	}
	lease.Release()
	if got, err := os.ReadFile(path); err != nil || string(got) != source {
		t.Fatalf("source after rejected StageEdit = %q, %v", got, err)
	}
}

func TestPrepareEditSessionCancellationDoesNotInstallOrReserve(t *testing.T) {
	data := bytes.Repeat([]byte("editable row\n"), 300_000)
	path := writeTempFile(t, "cancel-prepare.txt", data)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = svc.DiscardEdits(meta.FileID)
		_ = svc.CloseFile(meta.FileID)
	})

	var once sync.Once
	var cancelErr error
	manager := svc.jobs()
	manager.mu.Lock()
	manager.emit = func(name string, payload any) {
		if name != "quarry:job-progress" {
			return
		}
		data, _ := payload.(map[string]any)
		completed, _ := data["completed"].(int64)
		jobID, _ := data["id"].(string)
		if completed > 0 {
			once.Do(func() { cancelErr = svc.CancelJob(jobID) })
		}
	}
	manager.mu.Unlock()

	_, err = svc.PrepareEditSession(meta.FileID)
	if !errors.Is(err, ErrJobCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("PrepareEditSession error = %v, want cancelled job", err)
	}
	if cancelErr != nil {
		t.Fatalf("CancelJob: %v", cancelErr)
	}
	if preparedEditCountForTest(svc) != 0 {
		t.Fatal("cancelled preparation retained an aggregate reservation")
	}
	lease, file, err := svc.acquireReadFile(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if file.Edit != nil {
		lease.Release()
		t.Fatal("cancelled preparation installed an edit session")
	}
	lease.Release()
	manager.mu.Lock()
	active := manager.active
	manager.mu.Unlock()
	if active != nil {
		t.Fatal("cancelled preparation retained job ownership")
	}
	if got, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(got, data) {
		t.Fatalf("cancelled preparation changed source: bytes=%d err=%v", len(got), readErr)
	}

	// The canceled attempt must release enough state for an immediate retry.
	mustPrepareEditSession(t, svc, meta.FileID)
	if preparedEditCountForTest(svc) != 1 {
		t.Fatal("successful retry did not retain exactly one reservation")
	}
}

func TestCancelJobInterruptsPrepareQueuedForExclusiveLease(t *testing.T) {
	const source = "alpha\nbeta\n"
	path := writeTempFile(t, "cancel-queued-prepare.txt", []byte(source))
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blocker, _, err := svc.acquireReadFile(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	releaseBlocker := func() { releaseOnce.Do(blocker.Release) }
	t.Cleanup(func() {
		releaseBlocker()
		_, _ = svc.DiscardEdits(meta.FileID)
		_ = svc.CloseFile(meta.FileID)
	})
	prepareDone := make(chan error, 1)
	go func() {
		_, err := svc.PrepareEditSession(meta.FileID)
		prepareDone <- err
	}()
	waitForActiveFileJob(t, svc, meta.FileID)
	if err := svc.CancelJob(activeJobIDForTest(t, svc)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-prepareDone:
		if !errors.Is(err, ErrJobCancelled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("queued prepare error = %v, want canceled job", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CancelJob did not interrupt the exclusive lease wait")
	}
	if preparedEditCountForTest(svc) != 0 {
		t.Fatal("queued canceled prepare retained an admission slot")
	}
	lease, file, err := svc.acquireReadFile(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if file.Edit != nil {
		lease.Release()
		t.Fatal("queued canceled prepare installed an edit session")
	}
	lease.Release()
	if got, err := os.ReadFile(path); err != nil || string(got) != source {
		t.Fatalf("source after queued CancelJob = %q, %v", got, err)
	}
}

func TestPrepareEditSessionProgressIsByteBoundedAndIdempotent(t *testing.T) {
	data := bytes.Repeat([]byte("row\n"), 800_000)
	path := writeTempFile(t, "progress-prepare.txt", data)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = svc.DiscardEdits(meta.FileID)
		_ = svc.CloseFile(meta.FileID)
	})

	var events []recordedJobEvent
	manager := svc.jobs()
	manager.mu.Lock()
	manager.emit = func(name string, payload any) {
		entry, _ := payload.(map[string]any)
		events = append(events, recordedJobEvent{name: name, data: entry})
	}
	manager.mu.Unlock()
	mustPrepareEditSession(t, svc, meta.FileID)

	startCount, endCount, progressCount := 0, 0, 0
	previous := int64(-1)
	for _, event := range events {
		switch event.name {
		case "quarry:job-start":
			startCount++
			if event.data["kind"] != jobKindSourceVerification || event.data["total"] != int64(0) {
				t.Fatalf("start event = %#v", event.data)
			}
		case "quarry:job-progress":
			progressCount++
			completed, _ := event.data["completed"].(int64)
			total, _ := event.data["total"].(int64)
			if completed < previous || completed < 0 || completed > int64(len(data)) || total != int64(len(data)) {
				t.Fatalf("invalid progress after %d: %#v", previous, event.data)
			}
			previous = completed
		case "quarry:job-end":
			endCount++
			if event.data["status"] != "completed" || event.data["completed"] != int64(len(data)) {
				t.Fatalf("end event = %#v", event.data)
			}
		}
	}
	if startCount != 1 || endCount != 1 || progressCount == 0 || progressCount > 1025 {
		t.Fatalf("events start=%d progress=%d end=%d", startCount, progressCount, endCount)
	}

	events = nil
	mustPrepareEditSession(t, svc, meta.FileID)
	if len(events) != 2 || events[0].name != "quarry:job-start" || events[1].name != "quarry:job-end" {
		t.Fatalf("idempotent preparation events = %#v, want start/end without a fingerprint pass", events)
	}
}

func TestPreparedEditAdmissionIsReleasedByLifecycle(t *testing.T) {
	path := writeTempFile(t, "prepare-lifecycle.txt", []byte("alpha\nbeta\n"))
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mustPrepareEditSession(t, svc, meta.FileID)
	if state, err := svc.ReleaseCleanEditSession(meta.FileID); err != nil || state.EditCount != 0 {
		t.Fatalf("ReleaseCleanEditSession = %+v, %v", state, err)
	}
	if preparedEditCountForTest(svc) != 0 {
		t.Fatal("clean release retained edit-session admission")
	}

	mustPrepareEditSession(t, svc, meta.FileID)
	if _, err := svc.StageEdit(meta.FileID, 0, 5, "omega"); err != nil {
		t.Fatal(err)
	}
	before, err := svc.GetStagingState(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if state, err := svc.ReleaseCleanEditSession(meta.FileID); !errors.Is(err, ErrStagedEditsPending) || state.EditCount != before.EditCount {
		t.Fatalf("dirty ReleaseCleanEditSession = %+v, %v; want preserved %+v", state, err, before)
	}
	if preparedEditCountForTest(svc) != 1 {
		t.Fatal("dirty clean-release refusal dropped admission")
	}
	if _, err := svc.DiscardEdits(meta.FileID); err != nil {
		t.Fatal(err)
	}
	if preparedEditCountForTest(svc) != 0 {
		t.Fatal("discard retained edit-session admission")
	}

	mustPrepareEditSession(t, svc, meta.FileID)
	if _, err := svc.RefreshFile(meta.FileID); err != nil {
		t.Fatal(err)
	}
	if preparedEditCountForTest(svc) != 0 {
		t.Fatal("refresh retained edit-session admission")
	}

	mustPrepareEditSession(t, svc, meta.FileID)
	if err := svc.CloseFile(meta.FileID); err != nil {
		t.Fatal(err)
	}
	if preparedEditCountForTest(svc) != 0 {
		t.Fatal("close retained edit-session admission")
	}
}

func TestPreparedEditAdmissionCapRejectsBeforeFingerprint(t *testing.T) {
	dir := t.TempDir()
	svc := NewFileService()
	fileIDs := make([]string, 0, maxPreparedEditSessions+1)
	for i := 0; i <= maxPreparedEditSessions; i++ {
		path := filepath.Join(dir, "source-"+string(rune('a'+i))+".txt")
		if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		meta, err := svc.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fileIDs = append(fileIDs, meta.FileID)
	}
	t.Cleanup(func() {
		for _, fileID := range fileIDs {
			_, _ = svc.DiscardEdits(fileID)
			_ = svc.CloseFile(fileID)
		}
	})
	for _, fileID := range fileIDs[:maxPreparedEditSessions] {
		mustPrepareEditSession(t, svc, fileID)
	}
	if _, err := svc.PrepareEditSession(fileIDs[maxPreparedEditSessions]); !errors.Is(err, ErrPreparedEditSessionLimit) {
		t.Fatalf("overflow PrepareEditSession error = %v, want ErrPreparedEditSessionLimit", err)
	}
	lease, overflow, err := svc.acquireReadFile(fileIDs[maxPreparedEditSessions])
	if err != nil {
		t.Fatal(err)
	}
	if overflow.Edit != nil {
		lease.Release()
		t.Fatal("admission rejection allocated a source fingerprint")
	}
	lease.Release()

	if _, err := svc.DiscardEdits(fileIDs[0]); err != nil {
		t.Fatal(err)
	}
	mustPrepareEditSession(t, svc, fileIDs[maxPreparedEditSessions])
}
