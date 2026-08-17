package logger

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/quarry/quarry-wails3/internal/settings"
)

func TestWriteCreatesJSONLUnderQuarryHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, home)

	if err := Write("warn", "replace.recovery", "skipped manifest", map[string]string{
		"path":  "output.sql.quarry.manifest.json",
		"error": "invalid character",
	}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(home, logFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	var entry Entry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Level != "warn" || entry.Component != "replace.recovery" || entry.Message != "skipped manifest" {
		t.Fatalf("entry = %#v", entry)
	}
	if entry.Fields["path"] == "" || entry.Fields["error"] == "" {
		t.Fatalf("fields = %#v", entry.Fields)
	}
}

func TestWriteRotatesExistingLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, home)
	oldMax := maxBytes
	maxBytes = 8
	defer func() {
		maxBytes = oldMax
	}()

	path := filepath.Join(home, logFileName)
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write("info", "test", "after rotation", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, rotatedLogFileName)); err != nil {
		t.Fatalf("rotated log missing: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "after rotation") {
		t.Fatalf("active log = %q, want new entry", data)
	}
}

func TestReadRecentReturnsNewestEntriesFirstAndSkipsMalformedLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, home)

	if err := Write("info", "first", "oldest", nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, logFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not-json}\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Write("warn", "second", "newer", nil); err != nil {
		t.Fatal(err)
	}
	if err := Write("info", "third", "newest", nil); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadRecent(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2: %#v", len(entries), entries)
	}
	if entries[0].Component != "third" || entries[1].Component != "second" {
		t.Fatalf("entries = %#v, want newest valid entries first", entries)
	}
}

func TestReadRecentMissingLogReturnsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, home)

	entries, err := ReadRecent(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %#v, want none for missing log", entries)
	}
}

func TestReadRecentSkipsOversizedRecordAndKeepsLaterEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, home)
	path := filepath.Join(home, logFileName)
	oversized := strings.Repeat("x", maxLogLineBytes+4096) + "\n"
	valid := `{"time":"now","level":"info","component":"after","message":"visible"}` + "\n"
	if err := os.WriteFile(path, []byte(oversized+valid), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadRecent(math.MaxInt)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Component != "after" {
		t.Fatalf("entries = %#v, want only the valid record after the oversized line", entries)
	}
}

func TestWriteBoundsFieldsAndKeepsRecordReadable(t *testing.T) {
	home := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, home)
	fields := make(map[string]string)
	for i := 0; i < maxFieldCount+10; i++ {
		fields[strings.Repeat("k", maxFieldKeyBytes+10)+string(rune('A'+i))] = strings.Repeat("v", maxFieldValueBytes+100)
	}
	if err := Write("info", strings.Repeat("c", maxComponentBytes+100), strings.Repeat("m", maxMessageBytes*8), fields); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, logFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > maxLogLineBytes {
		t.Fatalf("encoded record length = %d, maximum = %d", len(data), maxLogLineBytes)
	}
	entries, err := ReadRecent(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %#v, want one bounded record", entries)
	}
}

func TestWriteSeparatesAValidEntryFromPartialFinalLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, home)
	path := filepath.Join(home, logFileName)
	if err := os.WriteFile(path, []byte(`{"partial":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write("info", "after-partial", "still readable", nil); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadRecent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Component != "after-partial" {
		t.Fatalf("entries = %#v, want the entry after malformed partial data", entries)
	}
}

func TestConcurrentReadWriteAndRotation(t *testing.T) {
	home := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, home)
	oldMax := maxBytes
	maxBytes = 256
	defer func() { maxBytes = oldMax }()

	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if worker%2 == 0 {
					_ = Write("info", "concurrent", strings.Repeat("x", 32), nil)
				} else {
					_, _ = ReadRecent(math.MaxInt)
				}
			}
		}(worker)
	}
	wg.Wait()
	if _, err := ReadRecent(10); err != nil {
		t.Fatal(err)
	}
}

func TestWriteAndReadRefuseSymlinkLogWithoutChangingTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, home)
	sentinel := filepath.Join(t.TempDir(), "sentinel.txt")
	const original = "do not change"
	if err := os.WriteFile(sentinel, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, logFileName)
	if err := os.Symlink(sentinel, logPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := Write("warn", "alias", "must not append", nil); !errors.Is(err, ErrUnsafeLogPath) {
		t.Fatalf("Write error = %v, want ErrUnsafeLogPath", err)
	}
	if _, err := ReadRecent(1); !errors.Is(err, ErrUnsafeLogPath) {
		t.Fatalf("ReadRecent error = %v, want ErrUnsafeLogPath", err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("symlink target changed to %q", data)
	}
}

func TestRotationRefusesUnsafeRotatedPathWithoutChangingEitherTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, home)
	oldMax := maxBytes
	maxBytes = 1
	defer func() { maxBytes = oldMax }()

	activePath := filepath.Join(home, logFileName)
	const active = "active log\n"
	if err := os.WriteFile(activePath, []byte(active), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel.txt")
	const original = "do not remove or change"
	if err := os.WriteFile(sentinel, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, filepath.Join(home, rotatedLogFileName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := Write("info", "rotation", "must fail closed", nil); !errors.Is(err, ErrUnsafeLogPath) {
		t.Fatalf("Write error = %v, want ErrUnsafeLogPath", err)
	}
	if data, err := os.ReadFile(activePath); err != nil || string(data) != active {
		t.Fatalf("active log = %q, %v; want unchanged", data, err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != original {
		t.Fatalf("rotated symlink target = %q, %v; want unchanged", data, err)
	}
}

func TestWriteAndReadRefuseHardLinkedLogWithoutChangingTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, home)
	oldMax := maxBytes
	maxBytes = 1
	defer func() { maxBytes = oldMax }()
	sentinel := filepath.Join(t.TempDir(), "sentinel.txt")
	const original = "do not change"
	if err := os.WriteFile(sentinel, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(sentinel, filepath.Join(home, logFileName)); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	if err := Write("warn", "alias", "must not append", nil); !errors.Is(err, ErrUnsafeLogPath) {
		t.Fatalf("Write error = %v, want ErrUnsafeLogPath", err)
	}
	if _, err := ReadRecent(1); !errors.Is(err, ErrUnsafeLogPath) {
		t.Fatalf("ReadRecent error = %v, want ErrUnsafeLogPath", err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("hard-link target changed to %q", data)
	}
}

func TestRotationRefusesHardLinkedRotatedPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, home)
	oldMax := maxBytes
	maxBytes = 1
	defer func() { maxBytes = oldMax }()
	activePath := filepath.Join(home, logFileName)
	if err := os.WriteFile(activePath, []byte("active log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel.txt")
	const original = "do not unlink"
	if err := os.WriteFile(sentinel, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	rotatedPath := filepath.Join(home, rotatedLogFileName)
	if err := os.Link(sentinel, rotatedPath); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	if err := Write("info", "rotation", "must fail closed", nil); !errors.Is(err, ErrUnsafeLogPath) {
		t.Fatalf("Write error = %v, want ErrUnsafeLogPath", err)
	}
	if data, err := os.ReadFile(activePath); err != nil || string(data) != "active log\n" {
		t.Fatalf("active log = %q, %v; want unchanged", data, err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != original {
		t.Fatalf("rotated hard-link target = %q, %v; want unchanged", data, err)
	}
	if _, err := os.Lstat(rotatedPath); err != nil {
		t.Fatalf("rotated hard-link path was removed: %v", err)
	}
}
