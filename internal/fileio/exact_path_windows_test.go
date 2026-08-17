//go:build windows

package fileio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateExactOutputPathRejectsWindowsAmbiguities(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		path string
	}{
		{name: "drive relative", path: `C:relative.txt`},
		{name: "alternate data stream", path: filepath.Join(dir, "carrier.txt") + ":secret"},
		{name: "reserved device", path: filepath.Join(dir, "NUL.txt")},
		{name: "reserved device before extension space", path: filepath.Join(dir, "NUL .txt")},
		{name: "reserved numbered device before extension space", path: filepath.Join(dir, "COM1 .log")},
		{name: "reserved superscript com device", path: filepath.Join(dir, "COM¹.txt")},
		{name: "reserved superscript lpt device", path: filepath.Join(dir, "LPT².log")},
		{name: "reserved device directory", path: filepath.Join(dir, "aux", "result.txt")},
		{name: "wildcard", path: filepath.Join(dir, "result?.txt")},
		{name: "ambiguous parent", path: filepath.Join(dir, "parent ", "result.txt")},
		{name: "ambiguous trailing dot leaf", path: filepath.Join(dir, "result.txt.")},
		{name: "ambiguous trailing space leaf", path: filepath.Join(dir, "result.txt ")},
		{name: "device namespace", path: `\\.\C:\result.txt`},
		{name: "global root namespace", path: `\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy1\result.txt`},
		{name: "volume guid namespace", path: `\\?\Volume{00000000-0000-0000-0000-000000000000}\result.txt`},
		{name: "ambiguous UNC share", path: `\\server\share \result.txt`},
		{name: "ambiguous UNC server", path: `\\server.\share\result.txt`},
		{name: "invalid UNC share character", path: `\\server\sha:re\result.txt`},
		{name: "incomplete extended UNC", path: `\\?\UNC\server`},
		{name: "extended UNC share root is not a file", path: `\\?\UNC\server\share`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateExactOutputPath(test.path); !errors.Is(err, ErrInvalidExactPath) {
				t.Fatalf("error = %v, want ErrInvalidExactPath", err)
			}
		})
	}
	if err := ValidateExactOutputPath(filepath.Join(dir, "normal-output.txt")); err != nil {
		t.Fatalf("ordinary absolute path rejected: %v", err)
	}
	if err := ValidateExactOutputPath(filepath.Join(dir, " exact leading space.txt")); err != nil {
		t.Fatalf("identity-significant leading space rejected: %v", err)
	}
	if err := ValidateExactOutputPath(`\\?\C:\ordinary-long-path.txt`); err != nil {
		t.Fatalf("ordinary extended drive path rejected: %v", err)
	}
	if err := ValidateExactOutputPath(`\\server\share\ordinary.txt`); err != nil {
		t.Fatalf("ordinary UNC path rejected: %v", err)
	}
	if err := ValidateExactOutputPath(`\\?\UNC\server\share\ordinary.txt`); err != nil {
		t.Fatalf("extended UNC file path rejected: %v", err)
	}
	if err := ValidateExactDirectoryPath(`\\?\UNC\server\share`); err != nil {
		t.Fatalf("extended UNC share directory rejected: %v", err)
	}
}

func TestAtomicOutputRejectsAmbiguousTrailingSpaceLeafOnWindows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.txt ")
	out, err := OpenAtomicOutput(path, nil, 0o600)
	if out != nil || !errors.Is(err, ErrInvalidExactPath) {
		t.Fatalf("OpenAtomicOutput = %#v, %v; want nil/ErrInvalidExactPath", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "result.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trimmed neighbor changed during refusal: %v", err)
	}
}

func TestOpenAtomicOutputRejectsAlternateDataStreamWithoutTouchingCarrier(t *testing.T) {
	carrier := filepath.Join(t.TempDir(), "carrier.txt")
	if err := os.WriteFile(carrier, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := OpenAtomicOutput(carrier+":quarry", nil, 0o600)
	if out != nil || !errors.Is(err, ErrInvalidExactPath) {
		t.Fatalf("OpenAtomicOutput = %#v, %v; want nil/ErrInvalidExactPath", out, err)
	}
	if got, readErr := os.ReadFile(carrier); readErr != nil || string(got) != "source" {
		t.Fatalf("carrier changed: %q, %v", got, readErr)
	}
	if _, statErr := os.Stat(carrier + ":quarry"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("alternate data stream exists after refusal: %v", statErr)
	}
}
