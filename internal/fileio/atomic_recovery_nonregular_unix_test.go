//go:build !windows

package fileio

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestAtomicRecoveryReadersRejectFIFOWithoutWaitingForWriter(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "artifact.fifo")
	if err := unix.Mkfifo(artifact, 0o600); err != nil {
		t.Fatal(err)
	}
	assertAtomicRecoveryErrorWithin(t, func() error {
		_, err := fingerprintAtomicArtifact(artifact)
		return err
	}, nil)
	assertAtomicRecoveryErrorWithin(t, func() error {
		return syncDirectoryPath(artifact)
	}, nil)

	destination := filepath.Join(dir, "settings.json")
	journal := destination + atomicWriteJournalSuffix
	if err := unix.Mkfifo(journal, 0o600); err != nil {
		t.Fatal(err)
	}
	assertAtomicRecoveryErrorWithin(t, func() error {
		_, present, err := loadAtomicWriteJournal(destination)
		if !present {
			return errors.New("FIFO journal was reported absent")
		}
		return err
	}, ErrAtomicRecoveryJournal)
}

func assertAtomicRecoveryErrorWithin(t *testing.T, call func() error, want error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("non-regular recovery path was accepted")
		}
		if want != nil && !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("atomic recovery reader blocked waiting for a FIFO writer")
	}
}
