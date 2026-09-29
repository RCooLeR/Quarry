package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/encodingx"
	"github.com/quarry/quarry-wails3/internal/session"
)

// windowExactLineScanBytes caps the auxiliary source I/O used only to label a
// rendered window with an exact line number. Sparse index anchors are line-
// based, so on a huge single line the nearest anchor can be hundreds of
// gigabytes behind the requested byte. The viewport remains usable by falling
// back to its explicit Approx contract once this fixed scan budget is exceeded.
const windowExactLineScanBytes int64 = 1 << 20

// GetMatchWindow resolves a raw search result to one exact CodeMirror UTF-16
// span. The compact per-hit contract avoids shipping an O(window-bytes) byte
// mapping array through JSON.
func (s *FileService) GetMatchWindow(fileID string, hitOffset int64, hitLength int, maxBytes int) (MatchWindow, error) {
	lease, f, err := s.acquireReadFile(fileID)
	if err != nil {
		return MatchWindow{}, err
	}
	defer lease.Release()
	budget, err := validateWindowBudget(maxBytes)
	if err != nil {
		return MatchWindow{}, err
	}
	if hitOffset < 0 || hitOffset > f.Doc.Size() || hitLength < 0 || int64(hitLength) > f.Doc.Size()-hitOffset {
		return MatchWindow{}, errors.New("search hit is outside the opened source")
	}
	if hitLength > budget {
		return MatchWindow{}, errors.New("search hit is too large for the bounded match window")
	}

	margin := min(256, (budget-hitLength)/2)
	hint := max(hitOffset-int64(margin), 0)
	matchEnd := hitOffset + int64(hitLength)
	if matchEnd-hint > int64(budget) {
		hint = matchEnd - int64(budget)
	}
	rendered, err := s.renderWindow(f, hint, budget, false, -1)
	if err != nil {
		return MatchWindow{}, err
	}
	for _, row := range rendered.rows {
		from, fromOK := row.utf16Position(hitOffset)
		to, toOK := row.utf16Position(matchEnd)
		if fromOK && toOK && to >= from {
			return MatchWindow{Window: rendered.window, Found: true, From: from, To: to}, nil
		}
	}
	return MatchWindow{Window: rendered.window}, nil
}

type renderedWindow struct {
	window Window
	rows   []renderedWindowRow
}

type renderedWindowRow struct {
	rawStart       int64
	rawEnd         int64
	textStartUTF16 int
	text           string
	rawBoundaries  []int
}

func (r renderedWindowRow) utf16Position(rawOffset int64) (int, bool) {
	if rawOffset < r.rawStart || rawOffset > r.rawEnd || len(r.rawBoundaries) == 0 {
		return 0, false
	}
	relative := int(rawOffset - r.rawStart)
	// A leading BOM occupies source bytes but no rendered UTF-16 units.
	if relative <= r.rawBoundaries[0] {
		return r.textStartUTF16, true
	}
	units := 0
	runeIndex := 0
	for _, decoded := range r.text {
		if decoded > 0xFFFF {
			units += 2
		} else {
			units++
		}
		runeIndex++
		if runeIndex < len(r.rawBoundaries) && r.rawBoundaries[runeIndex] == relative {
			return r.textStartUTF16 + units, true
		}
	}
	return 0, false
}

func validateWindowBudget(maxBytes int) (int, error) {
	if maxBytes < 0 {
		return 0, errors.New("window byte budget must not be negative")
	}
	if maxBytes == 0 {
		return defaultWindowBytes, nil
	}
	if maxBytes > defaultWindowBytes {
		return 0, fmt.Errorf("window byte budget %d exceeds hard limit %d", maxBytes, defaultWindowBytes)
	}
	return maxBytes, nil
}

func (s *FileService) renderWindow(f *session.File, requestedStart int64, maxBytes int, alignLine bool, exclusiveEnd int64) (renderedWindow, error) {
	budget, err := validateWindowBudget(maxBytes)
	if err != nil {
		return renderedWindow{}, err
	}
	if requestedStart < 0 {
		return renderedWindow{}, errors.New("window start must not be negative")
	}
	meta := f.Doc.Metadata()
	if meta.EncodingRequiresConfirmation {
		return renderedWindow{}, fmt.Errorf("%w: %s", encodingx.ErrEncodingConfirmationRequired, meta.Encoding)
	}
	size := f.Doc.Size()
	requestedStart = f.Doc.ClampOffset(requestedStart)

	start := requestedStart
	startContinues := false
	if alignLine {
		// Align ordinary lines, but do not jump far away from a requested byte on
		// a line whose omitted prefix is already beyond the renderable row cap.
		start, startContinues, err = f.Doc.AlignedWindowStart(requestedStart, int64(min(budget, rowDisplayBytes)))
	} else {
		start, err = f.Doc.AlignTextOffsetBackward(requestedStart)
		if err == nil {
			startContinues, err = f.Doc.StartsInsideLine(start)
		}
	}
	if err != nil {
		return renderedWindow{}, err
	}

	end := size
	if int64(budget) <= size-start {
		end = start + int64(budget)
	}
	if exclusiveEnd >= 0 {
		if exclusiveEnd < start || exclusiveEnd > size || exclusiveEnd-start > int64(budget) {
			return renderedWindow{}, errors.New("exclusive window end is outside the bounded source range")
		}
		end = exclusiveEnd
	}
	alignedEnd, err := f.Doc.AlignTextOffsetBackward(end)
	if err != nil {
		return renderedWindow{}, err
	}
	if alignedEnd != end && exclusiveEnd >= 0 {
		return renderedWindow{}, fmt.Errorf("%w: exclusive window end %d", document.ErrTextBoundary, end)
	}
	end = alignedEnd
	if end == start && start < size {
		return renderedWindow{}, errors.New("window byte budget is too small for one complete source character")
	}

	firstLine, approximate := windowFirstLine(f.Doc, start)
	if start == size {
		return renderedWindow{window: emptyWindow(f.ID, start, budget, startContinues, approximate)}, nil
	}

	rawBudget := int(end - start)
	if _, err := f.Doc.DecodeAlignedRange(start, end, int64(rawBudget)); err != nil {
		return renderedWindow{}, err
	}
	maxLineBytes := min(rowDisplayBytes, rawBudget)
	page, err := f.Doc.VisiblePageFromOffset(start, windowLineTarget, document.VisibleLineOptions{
		MaxBytes:        rawBudget,
		MaxLineBytes:    maxLineBytes,
		FirstLineNumber: firstLine,
	})
	if err != nil {
		return renderedWindow{}, err
	}
	if page.NextOffset > end {
		return renderedWindow{}, errors.New("decoded window exceeded its validated raw-byte budget")
	}

	result := renderedWindow{
		window: Window{
			FileID: f.ID, StartByte: page.StartOffset, NextByte: page.NextOffset,
			LineOffsets: []int64{}, LineEndOffsets: []int64{}, LineNumbers: []int64{}, LineTruncated: []bool{},
			StartContinuesLine: startContinues, BudgetBytes: budget,
			AtBOF: page.StartOffset == 0, AtEOF: page.NextOffset >= size, Approx: approximate,
		},
		rows: make([]renderedWindowRow, 0, len(page.Lines)),
	}
	var text strings.Builder
	text.Grow(min(rawBudget, maxWindowDecodedBytes))
	textUnits := 0
	lastLineNumber := int64(-1)
	for _, visual := range page.Lines {
		if len(result.rows) > 0 && visual.LineNumber == lastLineNumber {
			last := len(result.window.LineTruncated) - 1
			result.window.LineTruncated[last] = true
			continue
		}
		visual = truncateVisualLineRunes(visual, maxRowDisplayRunes)
		separatorBytes := 0
		if len(result.rows) > 0 {
			separatorBytes = 1
		}
		marker := ""
		if visual.Truncated || visual.HasRightHidden {
			marker = " \u22ef"
		}
		if text.Len()+separatorBytes+len(visual.Text)+len(marker) > maxWindowDecodedBytes {
			return renderedWindow{}, errors.New("decoded window exceeds the hard text allocation limit")
		}
		if separatorBytes != 0 {
			text.WriteByte('\n')
			textUnits++
		}
		textStart := textUnits
		text.WriteString(visual.Text)
		text.WriteString(marker)
		textUnits += utf16CodeUnits(visual.Text) + utf16CodeUnits(marker)
		result.rows = append(result.rows, renderedWindowRow{
			rawStart: visual.DisplayOffset, rawEnd: visual.DisplayEndOffset,
			textStartUTF16: textStart, text: visual.Text, rawBoundaries: visual.DisplayRuneByteOffsets,
		})
		result.window.LineOffsets = append(result.window.LineOffsets, visual.DisplayOffset)
		result.window.LineEndOffsets = append(result.window.LineEndOffsets, visual.DisplayEndOffset)
		result.window.LineNumbers = append(result.window.LineNumbers, visual.LineNumber)
		result.window.LineTruncated = append(result.window.LineTruncated, marker != "")
		lastLineNumber = visual.LineNumber
	}
	result.window.Text = text.String()
	result.window.SourceBytes = result.window.NextByte - result.window.StartByte
	if result.window.NextByte < size {
		result.window.EndContinuesLine, err = f.Doc.StartsInsideLine(result.window.NextByte)
		if err != nil {
			return renderedWindow{}, err
		}
	}
	return result, nil
}

func emptyWindow(fileID string, start int64, budget int, startContinues bool, approximate bool) Window {
	return Window{
		FileID: fileID, StartByte: start, NextByte: start,
		Text: "", LineOffsets: []int64{}, LineEndOffsets: []int64{}, LineNumbers: []int64{}, LineTruncated: []bool{},
		StartContinuesLine: startContinues, SourceBytes: 0, BudgetBytes: budget,
		AtBOF: start == 0, AtEOF: true, Approx: approximate,
	}
}

func windowFirstLine(doc *document.FileDocument, start int64) (int64, bool) {
	if start == 0 {
		return 1, false
	}
	if exact, ok, err := doc.ExactOffsetToLineWithin(start, windowExactLineScanBytes); err == nil && ok && exact > 0 {
		return exact, false
	}
	if approximate, ok := doc.ApproxOffsetToLine(start); ok && approximate > 0 {
		return approximate, true
	}
	return 1, true
}

func truncateVisualLineRunes(line document.VisualLine, maxRunes int) document.VisualLine {
	if maxRunes <= 0 || len(line.DisplayRuneByteOffsets) <= maxRunes+1 {
		return line
	}
	byteEnd := 0
	count := 0
	for index := range line.Text {
		if count == maxRunes {
			byteEnd = index
			break
		}
		count++
	}
	if byteEnd == 0 && count < maxRunes {
		return line
	}
	line.Text = line.Text[:byteEnd]
	line.DisplayRuneByteOffsets = line.DisplayRuneByteOffsets[:maxRunes+1]
	line.DisplayEndOffset = line.DisplayOffset + int64(line.DisplayRuneByteOffsets[maxRunes])
	line.Truncated = true
	line.HasRightHidden = true
	return line
}

func utf16CodeUnits(text string) int {
	units := 0
	for _, decoded := range text {
		if decoded > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return units
}
