package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSaveCopyIsCancellableJobAndKeepsStagedEdits(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.txt")
	destination := filepath.Join(dir, "copy.txt")
	data := bytes.Repeat([]byte("important row\n"), 600_000)
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })
	mustPrepareEditSession(t, svc, meta.FileID)
	if _, err := svc.StageEdit(meta.FileID, 0, int64(len("important")), "essential"); err != nil {
		t.Fatal(err)
	}

	var once sync.Once
	cancelResult := make(chan error, 1)
	manager := svc.jobs()
	manager.mu.Lock()
	manager.emit = func(name string, payload any) {
		if name != "quarry:job-progress" {
			return
		}
		data, _ := payload.(map[string]any)
		jobID, _ := data["id"].(string)
		once.Do(func() { cancelResult <- svc.CancelJob(jobID) })
	}
	manager.mu.Unlock()

	_, err = svc.saveCopy(meta.FileID, destination)
	if !errors.Is(err, ErrJobCancelled) {
		t.Fatalf("SaveCopy error = %v, want ErrJobCancelled", err)
	}
	if cancelErr := <-cancelResult; cancelErr != nil {
		t.Fatalf("CancelJob error = %v", cancelErr)
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancelled save published a destination: %v", statErr)
	}
	staging, err := svc.GetStagingState(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if staging.EditCount == 0 {
		t.Fatal("cancelled save discarded staged edits")
	}
	manager.mu.Lock()
	active := manager.active
	manager.mu.Unlock()
	if active != nil {
		t.Fatal("cancelled save retained active job ownership")
	}
}

func TestSaveCopyPublicationWinsConcurrentCancel(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.txt")
	destination := filepath.Join(dir, "copy.txt")
	if err := os.WriteFile(source, []byte("important row\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })
	mustPrepareEditSession(t, svc, meta.FileID)
	if _, err := svc.StageEdit(meta.FileID, 0, int64(len("important")), "essential"); err != nil {
		t.Fatal(err)
	}

	releasePublication := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePublication) }) }
	t.Cleanup(release)
	type publicationOutcome struct {
		jobID string
		err   error
	}
	publicationReached := make(chan publicationOutcome, 1)
	manager := svc.jobs()
	manager.mu.Lock()
	manager.afterOutputPublication = func(jobID string, err error) {
		publicationReached <- publicationOutcome{jobID: jobID, err: err}
		<-releasePublication
	}
	manager.mu.Unlock()

	type saveOutcome struct {
		result SaveResult
		err    error
	}
	saveDone := make(chan saveOutcome, 1)
	go func() {
		result, err := svc.saveCopy(meta.FileID, destination)
		saveDone <- saveOutcome{result: result, err: err}
	}()
	var publication publicationOutcome
	select {
	case publication = <-publicationReached:
	case <-time.After(5 * time.Second):
		t.Fatal("Save Copy did not reach final publication")
	}
	if publication.err != nil {
		release()
		t.Fatalf("platform publication error = %v", publication.err)
	}
	if got, err := os.ReadFile(destination); err != nil || string(got) != "essential row\n" {
		release()
		t.Fatalf("visible copy = %q, %v", got, err)
	}

	cancelStarted := make(chan struct{})
	cancelDone := make(chan error, 1)
	go func() {
		close(cancelStarted)
		cancelDone <- svc.CancelJob(publication.jobID)
	}()
	<-cancelStarted
	select {
	case err := <-cancelDone:
		release()
		t.Fatalf("CancelJob returned inside the locked publication boundary: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	release()
	if err := <-cancelDone; !errors.Is(err, ErrJobAlreadyCommitted) && !errors.Is(err, ErrNoActiveJob) {
		t.Fatalf("CancelJob after publication = %v, want a stale/non-cancel result", err)
	}
	select {
	case outcome := <-saveDone:
		if outcome.err != nil {
			t.Fatalf("Save Copy error = %v", outcome.err)
		}
		if outcome.result.OutputPath != destination || outcome.result.BytesWritten != int64(len("essential row\n")) {
			t.Fatalf("Save Copy result = %+v", outcome.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Save Copy did not finish after publication")
	}
}
