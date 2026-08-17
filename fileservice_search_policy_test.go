package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/regexutil"
	"github.com/quarry/quarry-wails3/internal/search"
)

func TestServiceSearchAndHarvestRejectOversizedStringsBeforeServiceAccess(t *testing.T) {
	plainAtLimit := strings.Repeat("p", search.MaxPlainPatternBytes)
	regexAtLimit := strings.Repeat("r", regexutil.MaxPatternBytes)
	if err := validateServiceSearchQuery(plainAtLimit, false); err != nil {
		t.Fatalf("plain query at limit: %v", err)
	}
	if err := validateServiceSearchQuery(regexAtLimit, true); err != nil {
		t.Fatalf("regex query at limit: %v", err)
	}

	var service *FileService
	plainOver := plainAtLimit + "x"
	if _, err := service.findNext("missing", plainOver, 0, false, true, false); !errors.Is(err, search.ErrResourceLimit) {
		t.Fatalf("oversized FindNext error = %v, want search.ErrResourceLimit", err)
	}
	if _, err := service.searchAllPage("missing", plainOver, false, true, false, 1, 0, false); !errors.Is(err, search.ErrResourceLimit) {
		t.Fatalf("oversized SearchAllPage error = %v, want search.ErrResourceLimit", err)
	}

	regexOver := regexAtLimit + "x"
	if _, err := service.findPrev("missing", regexOver, 0, true, true, false); !errors.Is(err, regexutil.ErrRegexResourceLimit) {
		t.Fatalf("oversized regex FindPrev error = %v, want regex resource error", err)
	}
	if _, err := service.searchAll("missing", regexOver, true, true, false, 1); !errors.Is(err, regexutil.ErrRegexResourceLimit) {
		t.Fatalf("oversized regex SearchAll error = %v, want regex resource error", err)
	}
	if _, err := service.HarvestMatchesViaDialog("missing", regexOver, false); !errors.Is(err, regexutil.ErrRegexResourceLimit) {
		t.Fatalf("oversized Harvest error = %v, want regex resource error", err)
	}
}

func TestPlainSearchPolicyReportsUnsupportedModes(t *testing.T) {
	tests := []struct {
		name          string
		encoding      string
		query         string
		caseSensitive bool
		wholeWord     bool
	}{
		{name: "UTF-8 non-ASCII insensitive", encoding: "UTF-8", query: "привет"},
		{name: "Windows-1251 non-ASCII insensitive", encoding: "Windows-1251", query: "привет"},
		{name: "Windows-1252 whole word", encoding: "Windows-1252", query: "resume", caseSensitive: true, wholeWord: true},
		{name: "UTF-16LE whole word", encoding: "UTF-16LE", query: "word", caseSensitive: true, wholeWord: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason := plainSearchUnsupportedReason(tt.encoding, tt.query, tt.caseSensitive, tt.wholeWord); reason == "" {
				t.Fatal("unsupported mode lacked an explicit limitation message")
			}
		})
	}
	for _, tt := range []struct {
		encoding      string
		query         string
		caseSensitive bool
		wholeWord     bool
	}{
		{encoding: "UTF-8", query: "привет", caseSensitive: true},
		{encoding: "UTF-8", query: "hello", wholeWord: true},
		{encoding: "Windows-1251", query: "hello"},
	} {
		if reason := plainSearchUnsupportedReason(tt.encoding, tt.query, tt.caseSensitive, tt.wholeWord); reason != "" {
			t.Fatalf("supported mode rejected: %s", reason)
		}
	}
}

func TestServiceRejectsNonASCIIInsensitiveSearchExplicitly(t *testing.T) {
	svc, meta := openSearchFixture(t, []byte("ПРИВЕТ"))
	hit, err := svc.findNext(meta.FileID, "привет", 0, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !hit.Unsupported || hit.Message == "" || hit.Found {
		t.Fatalf("find result=%+v, want explicit unsupported result", hit)
	}
	result, err := svc.searchAllPage(meta.FileID, "привет", false, false, false, 10, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Unsupported || result.Message == "" || len(result.Hits) != 0 {
		t.Fatalf("search-all result=%+v, want explicit unsupported result", result)
	}
}

func TestServiceUTF16PlainSearchRejectsUnalignedBytePattern(t *testing.T) {
	// BOM + two non-A code units containing an unaligned 41 00 byte seam,
	// followed by an aligned U+0041.
	data := []byte{0xff, 0xfe, 0x00, 0x41, 0x00, 0x42, 0x41, 0x00}
	path := writeTempFile(t, "utf16-search.txt", data)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := svc.CloseFile(meta.FileID); err != nil {
			t.Errorf("CloseFile: %v", err)
		}
	})

	for _, backward := range []bool{false, true} {
		start := int64(0)
		if backward {
			start = meta.Size
		}
		var hit SearchHit
		if backward {
			hit, err = svc.findPrev(meta.FileID, "A", start, false, true, false)
		} else {
			hit, err = svc.findNext(meta.FileID, "A", start, false, true, false)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !hit.Found || hit.Offset != 6 || hit.Length != 2 {
			t.Fatalf("backward=%v hit=%+v, want aligned offset 6", backward, hit)
		}
	}

	result, err := svc.searchAllPage(meta.FileID, "A", false, true, false, 10, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].Offset != 6 {
		t.Fatalf("search-all hits=%+v, want aligned offset 6", result.Hits)
	}

	unsupported, err := svc.findNext(meta.FileID, "A", 0, false, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !unsupported.Unsupported || unsupported.Message == "" {
		t.Fatalf("whole-word UTF-16 result=%+v, want explicit unsupported", unsupported)
	}

	// Ensure the source remained byte-exact throughout read-only searches.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("source changed: %x, want %x", got, data)
	}
}
