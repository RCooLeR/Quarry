// Command assetpolicy reapplies Quarry-specific policy to generated build
// assets. Wails owns the generated templates; this command keeps local release
// requirements explicit and fail-closed after an upstream refresh.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const maxPlistSize = 1 << 20

var (
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	minimumPattern = regexp.MustCompile(`(?s)(<key>\s*LSMinimumSystemVersion\s*</key>\s*<string>)([^<]*)(</string>)`)
)

type pendingUpdate struct {
	path     string
	original []byte
	updated  []byte
	mode     os.FileMode
}

func applyMinimum(data []byte, minimum string) ([]byte, error) {
	if !versionPattern.MatchString(minimum) {
		return nil, fmt.Errorf("macOS minimum %q must be a canonical three-part version", minimum)
	}
	if len(data) > maxPlistSize {
		return nil, fmt.Errorf("plist is larger than the %d-byte build-metadata limit", maxPlistSize)
	}

	matches := minimumPattern.FindAllSubmatchIndex(data, -1)
	if len(matches) != 1 {
		return nil, fmt.Errorf("plist must contain exactly one LSMinimumSystemVersion string; found %d", len(matches))
	}
	valueStart, valueEnd := matches[0][4], matches[0][5]
	if strings.TrimSpace(string(data[valueStart:valueEnd])) == minimum {
		return data, nil
	}

	result := make([]byte, 0, len(data)-(valueEnd-valueStart)+len(minimum))
	result = append(result, data[:valueStart]...)
	result = append(result, minimum...)
	result = append(result, data[valueEnd:]...)
	return result, nil
}

func readBounded(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPlistSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPlistSize {
		return nil, fmt.Errorf("plist is larger than the %d-byte build-metadata limit", maxPlistSize)
	}
	return data, nil
}

func enforceMinimum(paths []string, minimum string) error {
	if len(paths) == 0 {
		return errors.New("at least one plist path is required")
	}

	updates := make([]pendingUpdate, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		if info.Size() > maxPlistSize {
			return fmt.Errorf("inspect %s: plist is larger than the %d-byte build-metadata limit", path, maxPlistSize)
		}
		data, err := readBounded(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		updated, err := applyMinimum(data, minimum)
		if err != nil {
			return fmt.Errorf("validate %s: %w", path, err)
		}
		updates = append(updates, pendingUpdate{path: path, original: data, updated: updated, mode: info.Mode()})
	}

	// Validate every target before changing any of them. The upstream generator
	// can then be rerun safely if a later filesystem write fails.
	for _, update := range updates {
		current, err := readBounded(update.path)
		if err != nil {
			return fmt.Errorf("re-read %s: %w", update.path, err)
		}
		if bytes.Equal(current, update.updated) {
			continue
		}
		if !bytes.Equal(current, update.original) {
			return fmt.Errorf("%s changed after validation", update.path)
		}
		if err := os.WriteFile(update.path, update.updated, update.mode.Perm()); err != nil {
			return fmt.Errorf("write %s: %w", update.path, err)
		}
	}
	return nil
}

func run(args []string) error {
	set := flag.NewFlagSet("assetpolicy", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	minimum := set.String("macos-minimum", "", "required macOS minimum system version")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *minimum == "" {
		return errors.New("-macos-minimum is required")
	}
	return enforceMinimum(set.Args(), *minimum)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "assetpolicy:", err)
		os.Exit(2)
	}
}
