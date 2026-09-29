package fileio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAtomicOutputPublishesCompletePrivateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.txt")
	out, err := OpenAtomicOutput(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	temp := out.TempPath()
	if _, err := out.Write([]byte("complete output")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final name became visible before commit: %v", err)
	}
	if err := out.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "complete output" {
		t.Fatalf("final = %q, err %v", got, err)
	}
	if temp != "" {
		if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temp remains after commit: %v", err)
		}
	}
}

func TestAtomicOutputRejectsNonExactFinalPath(t *testing.T) {
	dir := t.TempDir()
	path := dir + string(os.PathSeparator) + "." + string(os.PathSeparator) + "result.txt"
	_, err := OpenAtomicOutput(path, nil, 0o600)
	if !errors.Is(err, ErrInvalidExactPath) {
		t.Fatalf("error = %v, want ErrInvalidExactPath", err)
	}
	var exactPathErr *InvalidExactPathError
	if !errors.As(err, &exactPathErr) {
		t.Fatalf("error = %T, want *InvalidExactPathError", err)
	}
	if exactPathErr.Path != path || exactPathErr.CleanPath != filepath.Clean(path) {
		t.Fatalf("exact-path error = %+v", exactPathErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "result.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected path created an output: %v", statErr)
	}
}

func TestAtomicOutputPreservesLeadingFilenameSpace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, " result.txt")
	out, err := OpenAtomicOutput(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Cleanup()
	if _, err := out.Write([]byte("exact spaced output")); err != nil {
		t.Fatal(err)
	}
	if err := out.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "exact spaced output" {
		t.Fatalf("spaced output = %q, err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "result.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unspaced path unexpectedly exists: %v", err)
	}
}

func TestAtomicOutputPublicationStaysInOpenedParent(t *testing.T) {
	root := t.TempDir()
	selectedParent := filepath.Join(root, "selected")
	if err := os.Mkdir(selectedParent, 0o700); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(selectedParent, "result.txt")
	out, err := OpenAtomicOutput(finalPath, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Cleanup()
	if _, err := out.Write([]byte("owned output")); err != nil {
		t.Fatal(err)
	}

	movedParent := filepath.Join(root, "selected-original")
	if err := os.Rename(selectedParent, movedParent); err != nil {
		if runtime.GOOS != "windows" || !errors.Is(err, os.ErrPermission) {
			t.Fatalf("rename opened parent: %v", err)
		}
		// Windows may deny renaming a directory that contains the retained
		// output handle. That is a valid fail-closed containment result: the
		// pathname cannot be substituted while publication is pending.
		if err := out.Cleanup(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed substitution attempt left an output: %v", err)
		}
		if _, err := os.Stat(movedParent); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed rename unexpectedly created replacement path: %v", err)
		}
		return
	}
	if err := os.Mkdir(selectedParent, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinelPath := filepath.Join(selectedParent, "sentinel.txt")
	if err := os.WriteFile(sentinelPath, []byte("replacement directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := out.Commit(); !errors.Is(err, ErrOutputPathDrift) {
		t.Fatalf("commit error = %v, want ErrOutputPathDrift", err)
	}
	if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output escaped into replacement directory: %v", err)
	}
	anchoredPath := filepath.Join(movedParent, "result.txt")
	if _, err := os.Stat(anchoredPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("drifted handle-owned output was not rolled back: %v", err)
	}
	if got, err := os.ReadFile(sentinelPath); err != nil || string(got) != "replacement directory" {
		t.Fatalf("replacement sentinel = %q, err %v", got, err)
	}
}

func TestAtomicOutputCancellationBeforePublicationLeavesNoFinal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.txt")
	out, err := OpenAtomicOutput(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write([]byte("complete but cancelled")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := out.CommitContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("commit error = %v, want context.Canceled", err)
	}
	if err := out.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled output published final: %v", err)
	}
}

func TestAtomicOutputFinalPublicationRunsInsideContextBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bounded.txt")
	out, err := OpenAtomicOutput(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Cleanup()
	if _, err := out.Write([]byte("complete")); err != nil {
		t.Fatal(err)
	}

	calls := 0
	ctx := WithPublicationBoundary(context.Background(), func(publish func() error) error {
		calls++
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("final visible before boundary publication: %v", err)
		}
		err := publish()
		if err == nil {
			if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "complete" {
				t.Fatalf("published output inside boundary = %q, %v", got, readErr)
			}
		}
		return err
	})
	if err := out.CommitContext(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("publication boundary calls = %d, want 1", calls)
	}
}

func TestAtomicOutputPublicationBoundaryCanRejectWithoutPublishing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rejected.txt")
	out, err := OpenAtomicOutput(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Cleanup()
	if _, err := out.Write([]byte("must stay private")); err != nil {
		t.Fatal(err)
	}

	ctx := WithPublicationBoundary(context.Background(), func(func() error) error {
		return context.Canceled
	})
	if err := out.CommitContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("commit error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected boundary published final: %v", err)
	}
}

func TestPublicationBoundaryCannotBeReplacedByNestedEngine(t *testing.T) {
	outerCalls := 0
	innerCalls := 0
	publishCalls := 0
	ctx := WithPublicationBoundary(context.Background(), func(publish func() error) error {
		outerCalls++
		return publish()
	})
	ctx = WithPublicationBoundary(ctx, func(publish func() error) error {
		innerCalls++
		return publish()
	})
	if err := publishWithinBoundary(ctx, func() error {
		publishCalls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if outerCalls != 1 || innerCalls != 0 || publishCalls != 1 {
		t.Fatalf("boundary calls outer=%d inner=%d publish=%d", outerCalls, innerCalls, publishCalls)
	}
}

func TestAtomicOutputValidatedCommitRunsValidatorBeforePublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.txt")
	out, err := OpenAtomicOutput(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Cleanup()
	if _, err := out.Write([]byte("validated output")); err != nil {
		t.Fatal(err)
	}
	validated := false
	err = out.CommitContextValidated(context.Background(), func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("final path visible before validation: %v", err)
		}
		validated = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !validated {
		t.Fatal("publication completed without running validator")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "validated output" {
		t.Fatalf("final = %q, err %v", got, err)
	}
}

func TestAtomicOutputValidatedCommitFailureAndCancellationDoNotPublish(t *testing.T) {
	tests := []struct {
		name     string
		validate func(context.Context) error
		want     error
	}{
		{
			name: "validation failure",
			validate: func(context.Context) error {
				return errors.New("source generation changed")
			},
			want: errors.New("source generation changed"),
		},
		{
			name: "cancelled after validation",
			validate: func(ctx context.Context) error {
				cancel, ok := ctx.Value(validatedCommitCancelKey{}).(context.CancelFunc)
				if !ok {
					return errors.New("cancel function missing")
				}
				cancel()
				return nil
			},
			want: context.Canceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.txt")
			out, err := OpenAtomicOutput(path, nil, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := out.Write([]byte("must remain private")); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			ctx = context.WithValue(ctx, validatedCommitCancelKey{}, context.CancelFunc(cancel))
			err = out.CommitContextValidated(ctx, test.validate)
			if test.want == context.Canceled {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want context.Canceled", err)
				}
			} else if err == nil || err.Error() != test.want.Error() {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if err := out.Cleanup(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected validated commit published final: %v", err)
			}
		})
	}
}

type validatedCommitCancelKey struct{}

func TestAtomicOutputRejectsSourceAliases(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(source, []byte("source bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path func(t *testing.T) string
	}{
		{name: "same path", path: func(*testing.T) string { return source }},
		{name: "hard link", path: func(t *testing.T) string {
			path := filepath.Join(dir, "hard-link.txt")
			if err := os.Link(source, path); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			return path
		}},
		{name: "symlink", path: func(t *testing.T) string {
			path := filepath.Join(dir, "symlink.txt")
			if err := os.Symlink(source, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return path
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := OpenAtomicOutput(tt.path(t), []string{source}, 0o600)
			if !errors.Is(err, ErrSourceAlias) {
				t.Fatalf("error = %v, want ErrSourceAlias", err)
			}
			got, readErr := os.ReadFile(source)
			if readErr != nil || string(got) != "source bytes" {
				t.Fatalf("source = %q, err %v", got, readErr)
			}
		})
	}
}

func TestAtomicOutputPreservesExistingAndRacedDestinations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.txt")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAtomicOutput(path, nil, 0o600); !errors.Is(err, ErrExists) {
		t.Fatalf("existing output error = %v, want ErrExists", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	out, err := OpenAtomicOutput(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Cleanup()
	if _, err := out.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("raced"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := out.Commit(); !errors.Is(err, ErrExists) {
		t.Fatalf("raced output error = %v, want ErrExists", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "raced" {
		t.Fatalf("raced destination = %q, err %v", got, err)
	}
}

func TestAtomicOutputCleanupRemovesOnlyOwnedTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.txt")
	out, err := OpenAtomicOutput(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	temp := out.TempPath()
	if temp == "" {
		if err := out.Cleanup(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("anonymous cleanup published final: %v", err)
		}
		return
	}
	moved := temp + ".moved"
	if err := out.file.Close(); err != nil {
		t.Fatal(err)
	}
	out.file = nil
	if err := os.Rename(temp, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := out.Cleanup(); err == nil {
		t.Fatal("cleanup accepted a substituted temp path")
	}
	if got, err := os.ReadFile(temp); err != nil || string(got) != "sentinel" {
		t.Fatalf("sentinel = %q, err %v", got, err)
	}
	if err := os.Remove(temp); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(moved); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicOutputLostHandleNeverPublishesOrDeletesByPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.txt")
	out, err := OpenAtomicOutput(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	temp := out.TempPath()
	if _, err := out.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	if err := out.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Commit(); err == nil {
		t.Fatal("commit succeeded with a closed temp handle")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed commit published final: %v", err)
	}
	// Once the retained handle has been lost, cleanup must preserve the path
	// rather than risk deleting a substituted entry by name.
	if err := out.Cleanup(); err == nil {
		t.Fatal("cleanup silently accepted a lost ownership handle")
	}
	if temp == "" {
		return
	}
	if got, err := os.ReadFile(temp); err != nil || string(got) != "partial" {
		t.Fatalf("preserved temp = %q, err %v", got, err)
	}
	if err := os.Remove(temp); err != nil {
		t.Fatal(err)
	}
}
