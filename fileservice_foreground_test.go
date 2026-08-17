package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/session"
)

func waitForForegroundRunCount(t *testing.T, service *FileService, fileID string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		service.foregroundMu.Lock()
		got := len(service.foregroundRuns[fileID])
		service.foregroundMu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("foreground run count for %q = %d, want %d", fileID, got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func assertNoForegroundRuns(t *testing.T, service *FileService) {
	t.Helper()
	service.foregroundMu.Lock()
	defer service.foregroundMu.Unlock()
	if service.foregroundRunCount != 0 || len(service.foregroundRuns) != 0 {
		t.Fatalf("retained foreground runs: total=%d buckets=%d", service.foregroundRunCount, len(service.foregroundRuns))
	}
}

func TestForegroundRunRegistryIsBoundedAndMultiFileCleanupIsExact(t *testing.T) {
	service := NewFileService()
	finishes := make([]func(), 0, maxForegroundRunsPerFile)
	for i := 0; i < maxForegroundRunsPerFile; i++ {
		_, finish, err := service.beginForegroundRun(context.Background(), "f1", "f2", "f1")
		if err != nil {
			t.Fatalf("register run %d: %v", i, err)
		}
		finishes = append(finishes, finish)
	}
	if _, _, err := service.beginForegroundRun(context.Background(), "f1"); !errors.Is(err, ErrForegroundRunLimit) {
		t.Fatalf("overflow error = %v, want ErrForegroundRunLimit", err)
	}
	waitForForegroundRunCount(t, service, "f1", maxForegroundRunsPerFile)
	waitForForegroundRunCount(t, service, "f2", maxForegroundRunsPerFile)
	for _, finish := range finishes {
		finish()
		finish()
	}
	assertNoForegroundRuns(t, service)
}

func TestForegroundRunRegistryLazilyInitializesZeroValueService(t *testing.T) {
	service := &FileService{}
	_, finish, err := service.beginForegroundRun(context.Background(), "f1")
	if err != nil {
		t.Fatal(err)
	}
	finish()
	assertNoForegroundRuns(t, service)
}

func TestForegroundLifecycleGateRejectsRegistrationUntilReopened(t *testing.T) {
	service := NewFileService()
	service.blockForegroundRuns("f1")
	if _, _, err := service.beginForegroundRun(context.Background(), "f1"); !errors.Is(err, session.ErrTransitioning) {
		t.Fatalf("blocked registration error = %v, want ErrTransitioning", err)
	}
	service.unblockForegroundRuns("f1")
	_, finish, err := service.beginForegroundRun(context.Background(), "f1")
	if err != nil {
		t.Fatalf("registration after unblock: %v", err)
	}
	finish()
	assertNoForegroundRuns(t, service)
}

func TestCloseCancelsHeldFindAndCleansForegroundRegistry(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "close-search.txt", []byte("needle\nother\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := service.BeginSearchRequest(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := installBlockingLeaseHook(t, "search", meta.FileID)
	searchDone := make(chan error, 1)
	go func() {
		_, err := service.FindNextRequest(requestID, "needle", 0, false, true, false)
		searchDone <- err
	}()
	<-entered
	waitForForegroundRunCount(t, service, meta.FileID, 1)
	closeDone := make(chan error, 1)
	go func() { closeDone <- service.CloseFile(meta.FileID) }()
	waitForTransition(t, service, meta.FileID)
	release()
	select {
	case err := <-searchDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("find error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("find did not stop after close cancellation")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not drain canceled find")
	}
	assertNoForegroundRuns(t, service)
}

func TestRefreshCancelsHeldSearchPageAndReopensForegroundGate(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "refresh-search-page.txt", []byte("needle\nother\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)
	entered, release := installBlockingLeaseHook(t, "search-all", meta.FileID)
	searchDone := make(chan error, 1)
	go func() {
		_, err := service.searchAllPage(meta.FileID, "needle", false, true, false, 10, 0, false)
		searchDone <- err
	}()
	<-entered
	refreshDone := make(chan error, 1)
	go func() {
		_, err := service.RefreshFile(meta.FileID)
		refreshDone <- err
	}()
	waitForTransition(t, service, meta.FileID)
	release()
	if err := <-searchDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("search-page error = %v, want context.Canceled", err)
	}
	select {
	case err := <-refreshDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not drain canceled search page")
	}
	assertNoForegroundRuns(t, service)
	result, err := service.searchAllPage(meta.FileID, "needle", false, true, false, 10, 0, false)
	if err != nil || len(result.Hits) != 1 {
		t.Fatalf("search after refresh = %+v, %v", result, err)
	}
}

func TestShutdownCancelsHeldFindAndRejectsNewForegroundRuns(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "shutdown-search.txt", []byte("needle\nother\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := service.BeginSearchRequest(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := installBlockingLeaseHook(t, "search", meta.FileID)
	searchDone := make(chan error, 1)
	go func() {
		_, err := service.FindNextRequest(requestID, "needle", 0, false, true, false)
		searchDone <- err
	}()
	<-entered
	shutdownDone := make(chan struct{})
	go func() {
		service.shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before held foreground run cleaned up")
	case <-time.After(25 * time.Millisecond):
	}
	service.searchRequestMu.Lock()
	retainedWhileRunning := len(service.searchRequests)
	service.searchRequestMu.Unlock()
	if retainedWhileRunning != 0 {
		t.Fatalf("shutdown retained %d public search request IDs while cancellation drained", retainedWhileRunning)
	}
	if canceled, err := service.CancelSearch(requestID); err != nil || canceled {
		t.Fatalf("cancel after service stop = %v, %v; want false, nil", canceled, err)
	}
	release()
	if err := <-searchDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("find error = %v, want context.Canceled", err)
	}
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not join canceled foreground run")
	}
	assertNoForegroundRuns(t, service)
	service.searchRequestMu.Lock()
	retainedSearchRequests := len(service.searchRequests)
	service.searchRequestMu.Unlock()
	if retainedSearchRequests != 0 {
		t.Fatalf("post-shutdown retained search requests = %d", retainedSearchRequests)
	}
	if _, err := service.BeginSearchRequest(meta.FileID); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("post-shutdown search error = %v, want ErrServiceStopped", err)
	}
	if err := service.CloseFile(meta.FileID); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("post-shutdown close error = %v, want ErrServiceStopped", err)
	}
}
