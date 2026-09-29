package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/session"
)

func waitForServiceStopping(t *testing.T, service *FileService) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		service.serviceMu.Lock()
		stopping := service.serviceStopping
		service.serviceMu.Unlock()
		if stopping {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("service did not raise its shutdown admission gate")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestServiceShutdownBlocksAdmissionDuringDrainAndClosesDocument(t *testing.T) {
	const source = "alpha\nbeta\n"
	service := NewFileService()
	path := writeTempFile(t, "service-shutdown.txt", []byte(source))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blocker, file, err := service.acquireReadFile(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(blocker.Release) }
	t.Cleanup(release)

	shutdownDone := make(chan struct{})
	go func() {
		service.shutdown()
		close(shutdownDone)
	}()
	waitForServiceStopping(t, service)

	latePath := filepath.Join(t.TempDir(), "late-open.txt")
	if err := os.WriteFile(latePath, []byte("late\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.OpenFile(latePath); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("OpenFile during shutdown error = %v, want ErrServiceStopped", err)
	}
	if _, err := service.FileSize(meta.FileID); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("direct FileSize during shutdown error = %v, want ErrServiceStopped", err)
	}
	if _, err := service.InspectInPlaceRecovery(path); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("direct recovery inspection during shutdown error = %v, want ErrServiceStopped", err)
	}
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before the retained document lease drained")
	case <-time.After(25 * time.Millisecond):
	}

	release()
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish after the retained lease released")
	}
	if _, err := service.GetWindow(meta.FileID, 0, 1024); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("direct RPC after shutdown error = %v, want ErrServiceStopped", err)
	}
	if _, err := file.Doc.ReadRange(0, 1); err == nil {
		t.Fatal("document handle remained readable after service shutdown")
	}
	if paths := service.reg.Paths(); len(paths) != 0 {
		t.Fatalf("registry retained paths after shutdown: %v", paths)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != source {
		t.Fatalf("source after service shutdown = %q, %v", got, err)
	}
	if _, err := service.OpenFile(path); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("OpenFile after shutdown error = %v, want ErrServiceStopped", err)
	}
}

func TestQueuedDialogPreflightsAreCancelledByShutdownBeforeDialog(t *testing.T) {
	tests := []struct {
		name string
		run  func(*FileService, string) error
	}{
		{
			name: "save-copy",
			run: func(service *FileService, fileID string) error {
				_, err := service.SaveCopyViaDialog(fileID)
				return err
			},
		},
		{
			name: "CSV-transform",
			run: func(service *FileService, fileID string) error {
				_, err := service.CsvFilterViaDialog(fileID, 1, ",", true, 0, "eq", "alpha", false)
				return err
			},
		},
		{
			name: "harvest",
			run: func(service *FileService, fileID string) error {
				_, err := service.HarvestMatchesViaDialog(fileID, "alpha", false)
				return err
			},
		},
		{
			name: "SQL-summary",
			run: func(service *FileService, fileID string) error {
				_, err := service.SqlExtractTableViaDialog(fileID, "t")
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := NewFileService()
			path := writeTempFile(t, "queued-preflight.txt", []byte("alpha,beta\n1,2\nCREATE TABLE t (id INT);\n"))
			meta, err := service.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			blocker, _, err := service.acquireExclusiveFile(meta.FileID)
			if err != nil {
				t.Fatal(err)
			}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(blocker.Release) }
			t.Cleanup(release)

			var dialogCalls atomic.Int32
			oldSaveCopy := saveCopySaveDialog
			oldCSV := csvSaveDialog
			oldHarvest := harvestSaveDialog
			oldSQL := sqlSaveDialog
			saveCopySaveDialog = func(string, string) (string, error) { dialogCalls.Add(1); return "", nil }
			csvSaveDialog = func(string, string) (string, error) { dialogCalls.Add(1); return "", nil }
			harvestSaveDialog = func(string, string) (string, error) { dialogCalls.Add(1); return "", nil }
			sqlSaveDialog = func(string, string) (string, error) { dialogCalls.Add(1); return "", nil }
			t.Cleanup(func() {
				saveCopySaveDialog = oldSaveCopy
				csvSaveDialog = oldCSV
				harvestSaveDialog = oldHarvest
				sqlSaveDialog = oldSQL
			})

			operationDone := make(chan error, 1)
			go func() { operationDone <- test.run(service, meta.FileID) }()
			waitForForegroundRunCount(t, service, meta.FileID, 1)

			shutdownDone := make(chan struct{})
			go func() {
				service.shutdown()
				close(shutdownDone)
			}()
			select {
			case err := <-operationDone:
				if !errors.Is(err, context.Canceled) && !errors.Is(err, session.ErrFileClosing) && !errors.Is(err, session.ErrRegistryStopped) {
					t.Fatalf("queued preflight error = %v, want lifecycle cancellation", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("queued preflight did not stop after shutdown")
			}
			if got := dialogCalls.Load(); got != 0 {
				t.Fatalf("save dialog calls = %d, want zero", got)
			}
			select {
			case <-shutdownDone:
				t.Fatal("shutdown returned before the pre-existing exclusive lease released")
			case <-time.After(20 * time.Millisecond):
			}
			release()
			select {
			case <-shutdownDone:
			case <-time.After(2 * time.Second):
				t.Fatal("shutdown did not drain after queued preflight cancellation")
			}
			assertNoForegroundRuns(t, service)
		})
	}
}

func TestCloseCancelsQueuedDialogPreflightBeforeDialog(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "close-preflight.txt", []byte("alpha\nbeta\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blocker, _, err := service.acquireExclusiveFile(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(blocker.Release) }
	t.Cleanup(release)

	var dialogCalls atomic.Int32
	oldHarvest := harvestSaveDialog
	harvestSaveDialog = func(string, string) (string, error) {
		dialogCalls.Add(1)
		return "", nil
	}
	t.Cleanup(func() { harvestSaveDialog = oldHarvest })

	operationDone := make(chan error, 1)
	go func() {
		_, err := service.HarvestMatchesViaDialog(meta.FileID, "alpha", false)
		operationDone <- err
	}()
	waitForForegroundRunCount(t, service, meta.FileID, 1)

	closeDone := make(chan error, 1)
	go func() { closeDone <- service.CloseFile(meta.FileID) }()
	waitForTransition(t, service, meta.FileID)
	select {
	case err := <-operationDone:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, session.ErrTransitioning) {
			t.Fatalf("queued preflight error = %v, want lifecycle cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not cancel queued preflight")
	}
	if got := dialogCalls.Load(); got != 0 {
		t.Fatalf("save dialog calls = %d, want zero", got)
	}
	release()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not drain after queued preflight cancellation")
	}
}
