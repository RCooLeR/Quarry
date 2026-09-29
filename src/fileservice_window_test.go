package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/encodingx"
)

func TestGetWindowDecodesSupportedEncodingsWithExactRawOffsets(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
		bom      []byte
		text     string
	}{
		{name: "utf8", encoding: "UTF-8", bom: encodingx.BOMBytes("UTF-8"), text: "первая 😀\r\nsecond 漢字\r\n"},
		{name: "utf16le", encoding: "UTF-16LE", bom: encodingx.BOMBytes("UTF-16LE"), text: "первая 😀\r\nsecond 漢字\r\n"},
		{name: "utf16be", encoding: "UTF-16BE", bom: encodingx.BOMBytes("UTF-16BE"), text: "первая 😀\r\nsecond 漢字\r\n"},
		{name: "windows1251", encoding: "Windows-1251", text: "первая строка\r\nвторая строка\r\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := encodingx.EncodeString(test.encoding, test.text)
			if err != nil {
				t.Fatal(err)
			}
			raw := append(append([]byte(nil), test.bom...), encoded...)
			service, meta := openWindowFixture(t, raw)
			if meta.Encoding != test.encoding {
				t.Fatalf("detected encoding = %q, want %q", meta.Encoding, test.encoding)
			}

			secondText := strings.Index(test.text, "second")
			if secondText < 0 {
				secondText = strings.Index(test.text, "вторая")
			}
			prefix, err := encodingx.EncodeString(test.encoding, test.text[:secondText])
			if err != nil {
				t.Fatal(err)
			}
			secondOffset := int64(len(test.bom) + len(prefix))
			window, err := service.GetWindow(meta.FileID, secondOffset+2, 256)
			if err != nil {
				t.Fatal(err)
			}
			if window.Text != "second 漢字" && window.Text != "вторая строка" {
				t.Fatalf("decoded window = %q", window.Text)
			}
			if window.StartByte != secondOffset || len(window.LineOffsets) != 1 || window.LineOffsets[0] != secondOffset {
				t.Fatalf("raw offsets = start %d lines %v, want %d", window.StartByte, window.LineOffsets, secondOffset)
			}
			if window.NextByte != int64(len(raw)) || !window.AtEOF || window.SourceBytes > 256 {
				t.Fatalf("window bounds = %+v", window)
			}
		})
	}
}

func TestWindowForwardBackwardRoundTripAndLongLineProgress(t *testing.T) {
	fixtures := []struct {
		name     string
		encoding string
		text     string
	}{
		{name: "utf8", encoding: "UTF-8", text: strings.Repeat("α😀z", 80) + "\r\nnext\r\nlast"},
		{name: "utf16le", encoding: "UTF-16LE", text: strings.Repeat("α😀z", 80) + "\r\nnext\r\nlast"},
		{name: "utf16be", encoding: "UTF-16BE", text: strings.Repeat("α😀z", 80) + "\r\nnext\r\nlast"},
		{name: "windows1251", encoding: "Windows-1251", text: strings.Repeat("абв", 120) + "\r\nдальше\r\nконец"},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			encoded, err := encodingx.EncodeString(fixture.encoding, fixture.text)
			if err != nil {
				t.Fatal(err)
			}
			bom := encodingx.BOMBytes(fixture.encoding)
			raw := append(append([]byte(nil), bom...), encoded...)
			service, meta := openWindowFixture(t, raw)

			first, err := service.GetWindow(meta.FileID, 0, 64)
			if err != nil {
				t.Fatal(err)
			}
			if first.NextByte <= first.StartByte || first.SourceBytes > 64 || !first.EndContinuesLine {
				t.Fatalf("first long-line window = %+v", first)
			}
			second, err := service.GetNextWindow(meta.FileID, first.NextByte, 64)
			if err != nil {
				t.Fatal(err)
			}
			if second.StartByte != first.NextByte || second.NextByte <= second.StartByte || !second.StartContinuesLine {
				t.Fatalf("second long-line window = %+v after %+v", second, first)
			}
			previous, err := service.GetPrevWindow(meta.FileID, second.StartByte, 64)
			if err != nil {
				t.Fatal(err)
			}
			if previous.StartByte != first.StartByte || previous.NextByte != second.StartByte || previous.Text != first.Text {
				t.Fatalf("previous = %+v, want first %+v", previous, first)
			}
		})
	}
}

func TestWindowAlternatingNavigationReconstructsEncodedSequence(t *testing.T) {
	fixtures := []struct {
		name     string
		encoding string
		text     string
	}{
		{name: "utf8 mixed breaks", encoding: "UTF-8", text: "α😀 one\rβ two\r\n漢 three\nfour 😀 five\r\nsix"},
		{name: "utf16le mixed breaks", encoding: "UTF-16LE", text: "α😀 one\rβ two\r\n漢 three\nfour 😀 five\r\nsix"},
		{name: "utf16be mixed breaks", encoding: "UTF-16BE", text: "α😀 one\rβ two\r\n漢 three\nfour 😀 five\r\nsix"},
		{name: "windows1251 mixed breaks", encoding: "Windows-1251", text: "один\rдва\r\nтри\nчетыре\r\nпять"},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			encoded, err := encodingx.EncodeString(fixture.encoding, fixture.text)
			if err != nil {
				t.Fatal(err)
			}
			raw := append(append([]byte(nil), encodingx.BOMBytes(fixture.encoding)...), encoded...)
			service, meta := openWindowFixture(t, raw)
			const budget = 17
			forward := make([]Window, 0, 8)
			window, err := service.GetWindow(meta.FileID, 0, budget)
			if err != nil {
				t.Fatal(err)
			}
			forward = append(forward, window)
			for !window.AtEOF {
				next, nextErr := service.GetNextWindow(meta.FileID, window.NextByte, budget)
				if nextErr != nil {
					t.Fatal(nextErr)
				}
				if next.StartByte != window.NextByte || next.NextByte <= next.StartByte {
					t.Fatalf("non-progressing forward pair: %+v then %+v", window, next)
				}
				forward = append(forward, next)
				window = next
				if len(forward) > 100 {
					t.Fatal("forward navigation did not reach EOF")
				}
			}
			for index := len(forward) - 1; index > 0; index-- {
				previous, previousErr := service.GetPrevWindow(meta.FileID, forward[index].StartByte, budget)
				if previousErr != nil {
					t.Fatal(previousErr)
				}
				want := forward[index-1]
				if previous.StartByte != want.StartByte || previous.NextByte != forward[index].StartByte || previous.Text != want.Text {
					t.Fatalf("reverse page %d = [%d,%d) %q, want [%d,%d) %q", index, previous.StartByte, previous.NextByte, previous.Text, want.StartByte, want.NextByte, want.Text)
				}
			}
		})
	}
}

func TestWindowRowTruncationNeverSplitsRuneOrSurrogate(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
		text     string
	}{
		{name: "utf8", encoding: "UTF-8", text: strings.Repeat("a", rowDisplayBytes-1) + "😀tail"},
		{name: "utf16le", encoding: "UTF-16LE", text: strings.Repeat("a", rowDisplayBytes/2-1) + "😀tail"},
		{name: "utf16be", encoding: "UTF-16BE", text: strings.Repeat("a", rowDisplayBytes/2-1) + "😀tail"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := encodingx.EncodeString(test.encoding, test.text)
			if err != nil {
				t.Fatal(err)
			}
			raw := append(append([]byte(nil), encodingx.BOMBytes(test.encoding)...), encoded...)
			service, meta := openWindowFixture(t, raw)
			window, err := service.GetWindow(meta.FileID, 0, 4096)
			if err != nil {
				t.Fatal(err)
			}
			if strings.ContainsRune(window.Text, '\uFFFD') {
				t.Fatalf("window silently inserted replacement character: %q", window.Text[len(window.Text)-20:])
			}
			if len(window.LineTruncated) != 1 || !window.LineTruncated[0] || window.LineEndOffsets[0] >= int64(len(raw)) {
				t.Fatalf("truncation metadata = %+v", window)
			}
		})
	}
}

func TestGetWindowLongLineGotoUsesExplicitContinuationNearRequestedByte(t *testing.T) {
	raw := []byte(strings.Repeat("x", 5000) + "TARGET tail")
	service, meta := openWindowFixture(t, raw)
	window, err := service.GetWindow(meta.FileID, 5000, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !window.StartContinuesLine || window.StartByte != 5000 || !strings.HasPrefix(window.Text, "TARGET") {
		t.Fatalf("long-line goto window = %+v", window)
	}
}

func TestGetWindowHugeSingleLineBoundsExactLineLookupAndMarksApproximate(t *testing.T) {
	raw := bytes.Repeat([]byte{'x'}, int(windowExactLineScanBytes)+8192)
	service, meta := openWindowFixture(t, raw)

	deadline := time.Now().Add(5 * time.Second)
	for {
		lease, file, err := service.acquireReadFile(meta.FileID)
		if err != nil {
			t.Fatal(err)
		}
		done := file.Doc.IndexProgress().Done
		lease.Release()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("line index did not complete for bounded exact-line regression")
		}
		time.Sleep(5 * time.Millisecond)
	}

	near, err := service.GetWindow(meta.FileID, 128, 64)
	if err != nil {
		t.Fatal(err)
	}
	if near.StartByte != 128 || near.Approx || len(near.LineNumbers) != 1 || near.LineNumbers[0] != 1 {
		t.Fatalf("within-budget exact window = %+v", near)
	}

	farStart := windowExactLineScanBytes + 4096
	far, err := service.GetWindow(meta.FileID, farStart, 64)
	if err != nil {
		t.Fatal(err)
	}
	if far.StartByte != farStart || !far.StartContinuesLine || !far.Approx || len(far.LineNumbers) != 1 || far.LineNumbers[0] != 1 {
		t.Fatalf("over-budget approximate window = %+v", far)
	}
}

func TestGetWindowStrictBudgetAndStaleSource(t *testing.T) {
	utf16Bytes, err := encodingx.EncodeString("UTF-16LE", "😀 data")
	if err != nil {
		t.Fatal(err)
	}
	raw := append(encodingx.BOMBytes("UTF-16LE"), utf16Bytes...)
	service, meta := openWindowFixture(t, raw)
	if _, err := service.GetWindow(meta.FileID, 0, -1); err == nil {
		t.Fatal("negative budget accepted")
	}
	if _, err := service.GetWindow(meta.FileID, 0, defaultWindowBytes+1); err == nil {
		t.Fatal("over-limit budget accepted")
	}
	if _, err := service.GetWindow(meta.FileID, 0, 1); err == nil {
		t.Fatal("budget too small for UTF-16 character accepted")
	}

	file, ok := service.reg.Get(meta.FileID)
	if !ok {
		t.Fatal("open session disappeared")
	}
	changed := append([]byte(nil), raw...)
	changed[len(changed)-1] ^= 1
	if err := os.WriteFile(file.Path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(3 * time.Second)
	if err := os.Chtimes(file.Path, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetWindow(meta.FileID, 0, 64); !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("stale read error = %v, want ErrSourceChanged", err)
	}
}

func TestAmbiguousEncodingBlocksDecodedWindowAndSearch(t *testing.T) {
	raw := []byte{
		0x4E, 0x9F, 0x4F, 0x9E, 0x50, 0x9D, 0x51, 0x9C,
		0x52, 0x9B, 0x53, 0x9A, 0x54, 0x99, 0x55, 0x98,
	}
	service, meta := openWindowFixture(t, raw)
	if !meta.EncodingRequiresConfirmation || meta.Editable {
		t.Fatalf("ambiguous metadata = %+v", meta)
	}
	if _, err := service.GetWindow(meta.FileID, 0, 64); !errors.Is(err, encodingx.ErrEncodingConfirmationRequired) {
		t.Fatalf("window error = %v, want confirmation required", err)
	}
	if _, err := service.GetMatchWindow(meta.FileID, 0, 2, 64); !errors.Is(err, encodingx.ErrEncodingConfirmationRequired) {
		t.Fatalf("match-window error = %v, want confirmation required", err)
	}
	searchResult, err := service.searchAll(meta.FileID, "x", false, true, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !searchResult.Unsupported || !strings.Contains(searchResult.Message, "Choose the source encoding") {
		t.Fatalf("ambiguous search result = %+v", searchResult)
	}
}

func TestGetMatchWindowMapsRawBytesToJavaScriptUTF16Units(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
		text     string
		query    string
	}{
		{name: "utf8 emoji cjk combining", encoding: "UTF-8", text: "😀漢e\u0301 target and target", query: "target"},
		{name: "utf16le", encoding: "UTF-16LE", text: "😀Ж target and target", query: "target"},
		{name: "utf16be", encoding: "UTF-16BE", text: "😀Ж target and target", query: "target"},
		{name: "windows1251", encoding: "Windows-1251", text: "Привет цель и цель", query: "цель"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := encodingx.EncodeString(test.encoding, test.text)
			if err != nil {
				t.Fatal(err)
			}
			bom := encodingx.BOMBytes(test.encoding)
			raw := append(append([]byte(nil), bom...), encoded...)
			service, meta := openWindowFixture(t, raw)

			lastTextIndex := strings.LastIndex(test.text, test.query)
			prefix, err := encodingx.EncodeString(test.encoding, test.text[:lastTextIndex])
			if err != nil {
				t.Fatal(err)
			}
			queryBytes, err := encodingx.EncodeString(test.encoding, test.query)
			if err != nil {
				t.Fatal(err)
			}
			match, err := service.GetMatchWindow(meta.FileID, int64(len(bom)+len(prefix)), len(queryBytes), 1024)
			if err != nil {
				t.Fatal(err)
			}
			if !match.Found {
				t.Fatalf("match mapping not found: %+v", match)
			}
			units := utf16.Encode([]rune(match.Window.Text))
			if match.From < 0 || match.To > len(units) || string(utf16.Decode(units[match.From:match.To])) != test.query {
				t.Fatalf("mapped span [%d,%d) in %q", match.From, match.To, match.Window.Text)
			}
			firstUnit := strings.Index(match.Window.Text, test.query)
			if firstUnit >= 0 && match.From <= firstUnit {
				t.Fatalf("mapped repeated hit to first occurrence: from=%d first byte index=%d", match.From, firstUnit)
			}
		})
	}
}

func TestGetMatchWindowMapsZeroLengthBoundaryAfterEmoji(t *testing.T) {
	raw := []byte("a😀b")
	service, meta := openWindowFixture(t, raw)
	match, err := service.GetMatchWindow(meta.FileID, int64(len([]byte("a😀"))), 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	if !match.Found || match.From != 3 || match.To != 3 {
		t.Fatalf("zero-length mapping = %+v", match)
	}
}

func TestSearchAllPreviewsDecodeInDocumentEncoding(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
		text     string
		query    string
	}{
		{name: "utf8", encoding: "UTF-8", text: strings.Repeat("я", 90) + " 😀 контекст цель хвост", query: "цель"},
		{name: "utf16le", encoding: "UTF-16LE", text: strings.Repeat("я", 90) + " 😀 контекст цель хвост", query: "цель"},
		{name: "utf16be", encoding: "UTF-16BE", text: strings.Repeat("я", 90) + " 😀 контекст цель хвост", query: "цель"},
		{name: "windows1251", encoding: "Windows-1251", text: strings.Repeat("я", 90) + " контекст цель хвост", query: "цель"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := encodingx.EncodeString(test.encoding, test.text)
			if err != nil {
				t.Fatal(err)
			}
			bom := encodingx.BOMBytes(test.encoding)
			raw := append(append([]byte(nil), bom...), encoded...)
			service, meta := openWindowFixture(t, raw)
			result, err := service.searchAll(meta.FileID, test.query, false, true, false, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Hits) != 1 {
				t.Fatalf("hits = %+v", result)
			}
			hit := result.Hits[0]
			if !strings.Contains(hit.Preview, "контекст цель хвост") || strings.ContainsRune(hit.Preview, '\uFFFD') {
				t.Fatalf("decoded preview = %q", hit.Preview)
			}
			textIndex := strings.Index(test.text, test.query)
			prefix, err := encodingx.EncodeString(test.encoding, test.text[:textIndex])
			if err != nil {
				t.Fatal(err)
			}
			if hit.Offset != int64(len(bom)+len(prefix)) {
				t.Fatalf("raw hit offset = %d, want %d", hit.Offset, len(bom)+len(prefix))
			}
		})
	}
}

func openWindowFixture(t *testing.T, raw []byte) (*FileService, FileMeta) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "window.txt")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewFileService()
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.CloseFile(meta.FileID) })
	return service, meta
}

func TestPreviousWindowLineLimitRoundTrip(t *testing.T) {
	raw := bytes.Repeat([]byte("x\n"), windowLineTarget+50)
	service, meta := openWindowFixture(t, raw)
	first, err := service.GetWindow(meta.FileID, 0, defaultWindowBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.LineOffsets) != windowLineTarget {
		t.Fatalf("rows = %d, want %d", len(first.LineOffsets), windowLineTarget)
	}
	second, err := service.GetNextWindow(meta.FileID, first.NextByte, defaultWindowBytes)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := service.GetPrevWindow(meta.FileID, second.StartByte, defaultWindowBytes)
	if err != nil {
		t.Fatal(err)
	}
	if previous.StartByte != first.StartByte || previous.NextByte != second.StartByte {
		t.Fatalf("line-limited previous = [%d,%d), want [%d,%d)", previous.StartByte, previous.NextByte, first.StartByte, second.StartByte)
	}
}
