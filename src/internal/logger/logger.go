package logger

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/quarry/quarry-wails3/internal/fileio"
	"github.com/quarry/quarry-wails3/internal/settings"
)

const (
	logFileName        = "quarry.log"
	rotatedLogFileName = "quarry.log.1"
	defaultMaxBytes    = 1024 * 1024
	maxRecentEntries   = 1000
	maxLogLineBytes    = 64 * 1024
	maxLevelBytes      = 32
	maxComponentBytes  = 256
	maxMessageBytes    = 16 * 1024
	maxFieldCount      = 32
	maxFieldKeyBytes   = 128
	maxFieldValueBytes = 1024
)

var (
	ErrUnsafeLogPath = errors.New("logger: unsafe log path")

	mu       sync.Mutex
	maxBytes int64 = defaultMaxBytes
)

type Entry struct {
	Time      string            `json:"time"`
	Level     string            `json:"level"`
	Component string            `json:"component"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields,omitempty"`
}

func LogPath() (string, error) {
	dir, err := settings.ConfigDir()
	if err != nil {
		return "", err
	}
	return fileio.ExactChildPath(dir, logFileName)
}

func Warn(component string, message string, fields map[string]string) {
	_ = Write("warn", component, message, fields)
}

func Info(component string, message string, fields map[string]string) {
	_ = Write("info", component, message, fields)
}

func ReadRecent(limit int) ([]Entry, error) {
	if limit <= 0 {
		return nil, nil
	}
	if limit > maxRecentEntries {
		limit = maxRecentEntries
	}

	mu.Lock()
	defer mu.Unlock()

	path, err := LogPath()
	if err != nil {
		return nil, err
	}
	f, err := openLogForRead(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, limit)
	err = readBoundedLines(f, func(line []byte) {
		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			return
		}
		entries = append(entries, entry)
		if len(entries) > limit {
			copy(entries, entries[1:])
			entries = entries[:limit]
		}
	})
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return entries, errors.Join(err, closeErr)
	}
	reverseEntries(entries)
	return entries, nil
}

// readBoundedLines skips an oversized record and continues at its next newline.
// Unlike bufio.Scanner, one hostile line therefore cannot hide later entries.
func readBoundedLines(r io.Reader, visit func([]byte)) error {
	reader := bufio.NewReaderSize(r, 32*1024)
	line := make([]byte, 0, 4*1024)
	oversized := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if !oversized {
			if len(line)+len(fragment) > maxLogLineBytes {
				line = line[:0]
				oversized = true
			} else {
				line = append(line, fragment...)
			}
		}

		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err != nil && !errors.Is(err, io.EOF):
			return err
		}

		if !oversized && len(line) > 0 {
			line = bytesWithoutLineEnding(line)
			if len(line) > 0 {
				visit(line)
			}
		}
		line = line[:0]
		oversized = false
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func bytesWithoutLineEnding(line []byte) []byte {
	line = bytesTrimSuffix(line, '\n')
	line = bytesTrimSuffix(line, '\r')
	return line
}

func bytesTrimSuffix(data []byte, suffix byte) []byte {
	if len(data) > 0 && data[len(data)-1] == suffix {
		return data[:len(data)-1]
	}
	return data
}

func reverseEntries(entries []Entry) {
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
}

func Write(level string, component string, message string, fields map[string]string) error {
	entry := Entry{
		Time:      time.Now().UTC().Format(time.RFC3339Nano),
		Level:     boundedTrimmedString(level, maxLevelBytes),
		Component: boundedTrimmedString(component, maxComponentBytes),
		Message:   boundedTrimmedString(message, maxMessageBytes),
		Fields:    boundedFields(fields),
	}
	if entry.Level == "" {
		entry.Level = "info"
	}
	if entry.Component == "" {
		entry.Component = "app"
	}
	if entry.Message == "" {
		entry.Message = "event"
	}

	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if len(line)+1 > maxLogLineBytes {
		entry.Message = boundedTrimmedString(entry.Message, 8*1024)
		entry.Fields = map[string]string{"quarry_truncated": "entry exceeded the log record limit"}
		line, err = json.Marshal(entry)
		if err != nil {
			return err
		}
		if len(line)+1 > maxLogLineBytes {
			return errors.New("log entry exceeds the safe record limit")
		}
	}
	line = append(line, '\n')

	mu.Lock()
	defer mu.Unlock()

	path, err := LogPath()
	if err != nil {
		return err
	}
	dir, _ := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := rotateIfNeeded(path, int64(len(line)+1)); err != nil {
		return err
	}

	f, err := openLogForAppend(path)
	if err != nil {
		return err
	}
	prefix, prefixErr := missingFinalNewlinePrefix(f)
	if prefixErr != nil {
		_ = f.Close()
		return prefixErr
	}
	payload := line
	if len(prefix) > 0 {
		payload = make([]byte, 0, len(prefix)+len(line))
		payload = append(payload, prefix...)
		payload = append(payload, line...)
	}
	written, writeErr := f.Write(payload)
	if writeErr == nil && written != len(payload) {
		writeErr = io.ErrShortWrite
	}
	closeErr := f.Close()
	if writeErr != nil {
		return errors.Join(writeErr, closeErr)
	}
	return closeErr
}

func rotateIfNeeded(path string, pendingBytes int64) error {
	active, err := openLogForRead(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	info, statErr := active.Stat()
	closeErr := active.Close()
	if statErr != nil || closeErr != nil {
		return errors.Join(statErr, closeErr)
	}
	if info.Size() == 0 || info.Size()+pendingBytes <= maxBytes {
		return nil
	}
	dir := filepath.Dir(path)
	rotatedPath, err := fileio.ExactChildPath(dir, rotatedLogFileName)
	if err != nil {
		return err
	}
	rotated, err := openLogForRead(rotatedPath)
	if err == nil {
		if closeErr := rotated.Close(); closeErr != nil {
			return closeErr
		}
		if err := os.Remove(rotatedPath); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(path, rotatedPath)
}

func missingFinalNewlinePrefix(file *os.File) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		return nil, nil
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], info.Size()-1); err != nil {
		return nil, err
	}
	if last[0] == '\n' {
		return nil, nil
	}
	return []byte{'\n'}, nil
}

func boundedTrimmedString(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if len(value) <= maximum {
		return value
	}
	cut := maximum
	for cut > 0 && cut < len(value) && value[cut]&0xc0 == 0x80 {
		cut--
	}
	return value[:cut]
}

func boundedFields(fields map[string]string) map[string]string {
	if len(fields) == 0 {
		return nil
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > maxFieldCount {
		keys = keys[:maxFieldCount]
	}
	bounded := make(map[string]string, len(keys)+1)
	for _, key := range keys {
		bounded[boundedTrimmedString(key, maxFieldKeyBytes)] = boundedTrimmedString(fields[key], maxFieldValueBytes)
	}
	if len(fields) > len(keys) {
		bounded["quarry_truncated"] = fmt.Sprintf("%d fields omitted", len(fields)-len(keys))
	}
	return bounded
}
