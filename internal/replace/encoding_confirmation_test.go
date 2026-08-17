package replace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/encodingx"
)

func TestAmbiguousEncodingTransformsFailBeforeOutputCreation(t *testing.T) {
	data := []byte{
		0x4E, 0x9F, 0x4F, 0x9E, 0x50, 0x9D, 0x51, 0x9C,
		0x52, 0x9B, 0x53, 0x9A, 0x54, 0x99, 0x55, 0x98,
	}
	for _, transform := range []struct {
		name string
		run  func(string, string) error
	}{
		{name: "encoding", run: func(source, output string) error {
			_, err := convertEncodingFile(context.Background(), source, output, "UTF-8", FileOptions{})
			return err
		}},
		{name: "line endings", run: func(source, output string) error {
			_, err := convertLineEndingsFile(context.Background(), source, output, "LF", FileOptions{})
			return err
		}},
	} {
		t.Run(transform.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.bin")
			output := filepath.Join(dir, "output.txt")
			if err := os.WriteFile(source, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := transform.run(source, output); !errors.Is(err, encodingx.ErrEncodingConfirmationRequired) {
				t.Fatalf("error=%v, want ErrEncodingConfirmationRequired", err)
			}
			if got, err := os.ReadFile(source); err != nil || string(got) != string(data) {
				t.Fatalf("source=%x err=%v", got, err)
			}
			for _, artifact := range []string{output, output + ".quarry.tmp", output + ".quarry.manifest.json"} {
				if _, err := os.Lstat(artifact); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("preflight created %q: %v", artifact, err)
				}
			}
		})
	}
}
