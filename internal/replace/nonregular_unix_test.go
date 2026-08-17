//go:build !windows

package replace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/regularfile"
	"golang.org/x/sys/unix"
)

func TestReplacePlainFileRejectsFIFOWithoutCreatingArtifacts(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.fifo")
	output := filepath.Join(dir, "output.txt")
	if err := unix.Mkfifo(source, 0o600); err != nil {
		t.Fatal(err)
	}

	type result struct {
		summary FileSummary
		err     error
	}
	done := make(chan result, 1)
	go func() {
		summary, err := replacePlainFile(context.Background(), source, output, []byte("a"), []byte("b"), FileOptions{ChunkSize: 64 * 1024})
		done <- result{summary: summary, err: err}
	}()
	select {
	case result := <-done:
		if !errors.Is(result.err, regularfile.ErrNotRegular) {
			t.Fatalf("ReplacePlainFile(FIFO) error = %v, want regularfile.ErrNotRegular", result.err)
		}
		for _, path := range []string{output, result.summary.TempPath, result.summary.ManifestPath} {
			if path == "" {
				continue
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected FIFO created artifact %q: %v", path, err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReplacePlainFile blocked waiting for a FIFO writer")
	}
}

func TestPathBasedReplaceHelpersRejectFIFOWithoutWaitingForWriter(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.fifo")
	if err := unix.Mkfifo(source, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{
			name: "rule file",
			call: func() error {
				_, _, err := LoadBatchRuleFile(source)
				return err
			},
			want: regularfile.ErrNotRegular,
		},
		{
			name: "space estimate",
			call: func() error {
				_, err := CheckPlainReplaceSpace(context.Background(), source, filepath.Join(dir, "output.txt"), []byte("a"), []byte("b"), SpaceOptions{ChunkSize: 64 * 1024})
				return err
			},
			want: regularfile.ErrNotRegular,
		},
		{
			name: "recovery manifest",
			call: func() error {
				_, err := LoadManifest(source)
				return err
			},
			want: ErrInvalidRecoveryManifest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- test.call() }()
			select {
			case err := <-done:
				if !errors.Is(err, test.want) {
					t.Fatalf("error = %v, want %v", err, test.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("path-based read blocked waiting for a FIFO writer")
			}
		})
	}
}

func TestFindRecoveryStatesRejectsFIFODirectoryWithoutWaitingForWriter(t *testing.T) {
	dir := t.TempDir()
	fifoDir := filepath.Join(dir, "recovery.fifo")
	if err := unix.Mkfifo(fifoDir, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := FindRecoveryStates(filepath.Join(fifoDir, "source.txt"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FindRecoveryStates accepted a FIFO as its scan directory")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FindRecoveryStates blocked waiting for a FIFO writer")
	}
}
