package replace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/quarry/quarry-wails3/internal/fileio"
)

func TestAllLegacyFileTransformsPreserveConcurrentDestinationCreator(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("legacy publication is intentionally disabled without an atomic no-replace rename primitive")
	}
	tests := []struct {
		name string
		run  func(context.Context, string, string) (FileSummary, error)
	}{
		{
			name: "plain",
			run: func(ctx context.Context, sourcePath string, outputPath string) (FileSummary, error) {
				return replacePlainFile(ctx, sourcePath, outputPath, []byte("alpha"), []byte("omega"), FileOptions{})
			},
		},
		{
			name: "regexp",
			run: func(ctx context.Context, sourcePath string, outputPath string) (FileSummary, error) {
				return replaceRegexpFile(ctx, sourcePath, outputPath, []byte("alpha"), []byte("omega"), FileOptions{}, RegexOptions{})
			},
		},
		{
			name: "plain batch",
			run: func(ctx context.Context, sourcePath string, outputPath string) (FileSummary, error) {
				return replaceBatchPlainFile(ctx, sourcePath, outputPath, []BatchRule{{Find: []byte("alpha"), Replace: []byte("omega")}}, FileOptions{}, BatchOptions{})
			},
		},
		{
			name: "regexp batch",
			run: func(ctx context.Context, sourcePath string, outputPath string) (FileSummary, error) {
				return replaceBatchRegexpFile(ctx, sourcePath, outputPath, []BatchRule{{Find: []byte("alpha"), Replace: []byte("omega")}}, FileOptions{}, RegexOptions{})
			},
		},
		{
			name: "encoding",
			run: func(ctx context.Context, sourcePath string, outputPath string) (FileSummary, error) {
				return convertEncodingFile(ctx, sourcePath, outputPath, "UTF-8", FileOptions{})
			},
		},
		{
			name: "line endings",
			run: func(ctx context.Context, sourcePath string, outputPath string) (FileSummary, error) {
				return convertLineEndingsFile(ctx, sourcePath, outputPath, "LF", FileOptions{})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "source.txt")
			outputPath := filepath.Join(dir, "output.txt")
			if err := os.WriteFile(sourcePath, []byte("alpha\r\nbeta\r\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			originalRename := renamePath
			renamePath = func(tempPath string, finalPath string) error {
				file, err := os.OpenFile(finalPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					t.Fatalf("create concurrent destination: %v", err)
				}
				if _, err := file.WriteString("concurrent owner"); err != nil {
					_ = file.Close()
					t.Fatalf("write concurrent destination: %v", err)
				}
				if err := file.Close(); err != nil {
					t.Fatalf("close concurrent destination: %v", err)
				}
				return fileio.PublishExistingNoClobber(tempPath, finalPath)
			}
			t.Cleanup(func() { renamePath = originalRename })

			summary, err := test.run(context.Background(), sourcePath, outputPath)
			if !errors.Is(err, fileio.ErrExists) || !errors.Is(err, os.ErrExist) {
				t.Fatalf("transform error = %v, want fileio.ErrExists and os.ErrExist", err)
			}
			if summary.Published || !summary.Complete || summary.PublicationUncertain {
				t.Fatalf("summary = %#v, want complete retained temp and no publication", summary)
			}
			assertReplaceFileContent(t, sourcePath, "alpha\r\nbeta\r\n")
			assertReplaceFileContent(t, outputPath, "concurrent owner")
			if _, statErr := os.Lstat(summary.TempPath); statErr != nil {
				t.Fatalf("completed temp was not retained: %v", statErr)
			}
		})
	}
}

func TestLegacyPublicationErrorMarksVisibleOutput(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(sourcePath, []byte("alpha beta"), 0o600); err != nil {
		t.Fatal(err)
	}

	syncFailure := errors.New("simulated publication finalization failure")
	originalRename := renamePath
	renamePath = func(tempPath string, finalPath string) error {
		if err := os.Rename(tempPath, finalPath); err != nil {
			return err
		}
		return &fileio.PublicationError{FinalPath: finalPath, Err: syncFailure}
	}
	t.Cleanup(func() { renamePath = originalRename })

	summary, err := replacePlainFile(context.Background(), sourcePath, outputPath, []byte("alpha"), []byte("omega"), FileOptions{})
	if !errors.Is(err, syncFailure) {
		t.Fatalf("transform error = %v, want publication finalization failure", err)
	}
	if !summary.Complete || !summary.Published || summary.PublicationUncertain {
		t.Fatalf("summary = %#v, want completed visible output", summary)
	}
	assertReplaceFileContent(t, outputPath, "omega beta")
}
