package main

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/session"
)

// defaultWindowBytes is the byte budget for one editor window. The editor never
// holds more than a few of these regardless of file size, so a 400 GB file and
// a 4 KB file cost the same in memory.
const defaultWindowBytes = 1 << 20 // 1 MiB

// FileService exposes streaming, windowed access to very large files. It is
// bound to the frontend by Wails; the whole-file content never crosses the
// bridge — only bounded, line-aligned windows do.
type FileService struct {
	reg *session.Registry
}

// NewFileService constructs the service with an empty session registry.
func NewFileService() *FileService {
	return &FileService{reg: session.New()}
}

// FileMeta describes a freshly opened file.
type FileMeta struct {
	FileID   string `json:"fileId"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Encoding string `json:"encoding"`
	Detected string `json:"detected"`
	Binary   bool   `json:"binary"`
}

// Window is a bounded, line-aligned slice of the file decoded to UTF-8, plus
// the per-line global coordinates the editor gutter needs.
type Window struct {
	FileID      string  `json:"fileId"`
	StartByte   int64   `json:"startByte"`   // global byte offset of the first line
	NextByte    int64   `json:"nextByte"`    // feed to GetNextWindow to continue
	Text        string  `json:"text"`        // lines joined with "\n"
	LineOffsets []int64 `json:"lineOffsets"` // global byte offset per line
	LineNumbers []int64 `json:"lineNumbers"` // global line number per line (approx until indexed)
	AtBOF       bool    `json:"atBof"`
	AtEOF       bool    `json:"atEof"`
	Approx      bool    `json:"approx"` // line numbers are approximate (index not ready)
}

// OpenFile opens path, starts background indexing, and returns its metadata.
func (s *FileService) OpenFile(path string) (FileMeta, error) {
	f, err := s.reg.Open(path)
	if err != nil {
		return FileMeta{}, err
	}
	f.StartIndexing()
	m := f.Doc.Metadata()
	return FileMeta{
		FileID:   f.ID,
		Path:     m.Path,
		Size:     m.Size,
		Encoding: m.Encoding,
		Detected: m.FileType,
		Binary:   m.Binary,
	}, nil
}

// CloseFile releases a file's resources.
func (s *FileService) CloseFile(fileID string) error {
	return s.reg.Close(fileID)
}

// GetWindow returns a window beginning at startByte (clamped, aligned by the
// document's own line handling).
func (s *FileService) GetWindow(fileID string, startByte int64, maxBytes int) (Window, error) {
	f, ok := s.reg.Get(fileID)
	if !ok {
		return Window{}, fmt.Errorf("unknown file id %q", fileID)
	}
	return s.windowFrom(f, startByte, maxBytes)
}

// GetNextWindow continues forward from a previous window's NextByte.
func (s *FileService) GetNextWindow(fileID string, fromByte int64, maxBytes int) (Window, error) {
	return s.GetWindow(fileID, fromByte, maxBytes)
}

// GetPrevWindow returns the window immediately preceding currentStart, aligned
// to a line boundary so it reads cleanly.
func (s *FileService) GetPrevWindow(fileID string, currentStart int64, maxBytes int) (Window, error) {
	f, ok := s.reg.Get(fileID)
	if !ok {
		return Window{}, fmt.Errorf("unknown file id %q", fileID)
	}
	if maxBytes <= 0 {
		maxBytes = defaultWindowBytes
	}
	if currentStart <= 0 {
		return s.windowFrom(f, 0, maxBytes)
	}
	target := currentStart - int64(maxBytes)
	if target < 0 {
		target = 0
	}
	if target > 0 {
		// Align up to the next line boundary so the window starts cleanly.
		raw, err := f.Doc.ReadRange(target, currentStart)
		if err != nil {
			return Window{}, err
		}
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			target += int64(i) + 1
		}
	}
	span := int(currentStart - target)
	if span <= 0 {
		span = maxBytes
	}
	return s.windowFrom(f, target, span)
}

func (s *FileService) windowFrom(f *session.File, startByte int64, maxBytes int) (Window, error) {
	if maxBytes <= 0 {
		maxBytes = defaultWindowBytes
	}
	size := f.Doc.Size()
	startByte = f.Doc.ClampOffset(startByte)

	firstLine, ok := f.Doc.ApproxOffsetToLine(startByte)
	approx := true
	switch {
	case startByte == 0:
		firstLine, approx = 1, false
	case !ok || firstLine <= 0:
		firstLine = 1 // unknown until indexed; numbers shown are relative
	}

	opts := document.VisibleLineOptions{
		MaxBytes:        maxBytes,
		MaxLineBytes:    maxBytes, // no horizontal truncation; the editor wraps
		FirstLineNumber: firstLine,
	}
	page, err := f.Doc.VisiblePageFromOffset(startByte, 1<<20, opts)
	if err != nil {
		return Window{}, err
	}

	var b strings.Builder
	offsets := make([]int64, 0, len(page.Lines))
	numbers := make([]int64, 0, len(page.Lines))
	for i, ln := range page.Lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ln.Text)
		offsets = append(offsets, ln.Offset)
		numbers = append(numbers, ln.LineNumber)
	}

	return Window{
		FileID:      f.ID,
		StartByte:   page.StartOffset,
		NextByte:    page.NextOffset,
		Text:        b.String(),
		LineOffsets: offsets,
		LineNumbers: numbers,
		AtBOF:       page.StartOffset == 0,
		AtEOF:       page.NextOffset >= size,
		Approx:      approx,
	}, nil
}
