package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
)

func TestSearchAllPageForwardBackwardPlainAndRegex(t *testing.T) {
	data := []byte("hit-01 xx hit-02 xx hit-03 xx hit-04")
	wantForward := []int64{0, 10, 20, 30}
	wantBackward := []int64{30, 20, 10, 0}

	for _, tc := range []struct {
		name  string
		query string
		regex bool
	}{
		{name: "plain", query: "hit", regex: false},
		{name: "regex", query: `hit-\d{2}`, regex: true},
	} {
		for _, backward := range []bool{false, true} {
			direction := "forward"
			want := wantForward
			startOffset := int64(0)
			if backward {
				direction = "backward"
				want = wantBackward
				startOffset = int64(len(data))
			}
			t.Run(tc.name+"/"+direction, func(t *testing.T) {
				svc, meta := openSearchFixture(t, data)

				first, err := svc.searchAllPage(meta.FileID, tc.query, tc.regex, true, false, 2, startOffset, backward)
				if err != nil {
					t.Fatal(err)
				}
				if !first.Truncated || first.Continuation == nil {
					t.Fatalf("first page = %+v, want truncation with continuation", first)
				}
				if first.Continuation.Backward != backward {
					t.Fatalf("continuation direction = %v, want %v", first.Continuation.Backward, backward)
				}

				second, err := svc.searchAllPage(meta.FileID, tc.query, tc.regex, true, false, 2, first.Continuation.StartOffset, first.Continuation.Backward)
				if err != nil {
					t.Fatal(err)
				}
				if second.Truncated || second.Continuation != nil {
					t.Fatalf("exact final page = %+v, want no truncation or continuation", second)
				}

				got := append(searchHitOffsets(first.Hits), searchHitOffsets(second.Hits)...)
				if !slices.Equal(got, want) {
					t.Fatalf("paged offsets = %v, want %v", got, want)
				}
			})
		}
	}
}

func TestSearchAllLimitsAndExactTruncation(t *testing.T) {
	t.Run("negative limit and invalid offsets fail before collection", func(t *testing.T) {
		svc, meta := openSearchFixture(t, []byte("xxx"))
		if _, err := svc.searchAllPage(meta.FileID, "x", false, true, false, -1, 0, false); err == nil {
			t.Fatal("negative hit limit was accepted")
		}
		if _, err := svc.searchAllPage(meta.FileID, "x", false, true, false, 1, -1, false); err == nil {
			t.Fatal("negative start offset was accepted")
		}
		if _, err := svc.searchAllPage(meta.FileID, "x", false, true, false, 1, meta.Size+1, false); err == nil {
			t.Fatal("start offset beyond EOF was accepted")
		}
	})

	t.Run("zero uses the bounded default", func(t *testing.T) {
		data := bytes.Repeat([]byte("x"), maxSearchAllHits+2)
		svc, meta := openSearchFixture(t, data)
		result, err := svc.searchAllPage(meta.FileID, "x", false, true, false, 0, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if result.Limit != maxSearchAllHits || len(result.Hits) != maxSearchAllHits {
			t.Fatalf("limit/hits = %d/%d, want %d/%d", result.Limit, len(result.Hits), maxSearchAllHits, maxSearchAllHits)
		}
		if !result.Truncated || result.Continuation == nil {
			t.Fatal("dense default result lacks truncation metadata")
		}
	})

	t.Run("limit above the hard maximum is rejected", func(t *testing.T) {
		svc, meta := openSearchFixture(t, []byte("xxx"))
		for _, requested := range []int{maxSearchAllHits + 1, int(^uint(0) >> 1)} {
			if _, err := svc.searchAllPage(meta.FileID, "x", false, true, false, requested, 0, false); err == nil {
				t.Fatalf("oversized hit limit %d was silently clamped", requested)
			}
		}
	})

	t.Run("exact limit is not reported truncated", func(t *testing.T) {
		svc, meta := openSearchFixture(t, []byte("x-x"))
		result, err := svc.searchAllPage(meta.FileID, "x", false, true, false, 2, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Hits) != 2 || result.Truncated || result.Continuation != nil {
			t.Fatalf("exact-limit result = %+v", result)
		}
	})

	t.Run("limit plus one is reported truncated", func(t *testing.T) {
		svc, meta := openSearchFixture(t, []byte("xxx"))
		result, err := svc.searchAllPage(meta.FileID, "x", false, true, false, 2, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Hits) != 2 || !result.Truncated || result.Continuation == nil {
			t.Fatalf("limit-plus-one result = %+v", result)
		}
	})

	t.Run("zero-width regex is rejected", func(t *testing.T) {
		svc, meta := openSearchFixture(t, []byte("aaaa"))
		if _, err := svc.searchAllPage(meta.FileID, `a*`, true, true, false, 10, 0, false); err == nil {
			t.Fatal("zero-width regex was accepted")
		}
	})
}

func TestSearchAllCancellationPlainAndRegexBothDirections(t *testing.T) {
	data := bytes.Repeat([]byte("match "), 4096)
	for _, regex := range []bool{false, true} {
		for _, backward := range []bool{false, true} {
			t.Run(testSearchMode(regex, backward), func(t *testing.T) {
				svc, meta := openSearchFixture(t, data)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				start := int64(0)
				if backward {
					start = meta.Size
				}
				result, err := svc.searchAllPageContext(ctx, meta.FileID, "match", regex, true, false, maxSearchAllHits, start, backward)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want context.Canceled", err)
				}
				if len(result.Hits) != 0 || result.PreviewBytes != 0 {
					t.Fatalf("canceled result retained data: %+v", result)
				}
			})
		}
	}
}

func TestSearchAllEnforcesAggregatePreviewCap(t *testing.T) {
	const matchBytes = maxSearchAllPreviewPerHit
	data := bytes.Repeat([]byte("a"), matchBytes*(maxSearchAllHits+1))
	svc, meta := openSearchFixture(t, data)

	result, err := svc.searchAllPage(meta.FileID, `a{512}`, true, true, false, maxSearchAllHits, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != maxSearchAllHits || !result.Truncated || result.Continuation == nil {
		t.Fatalf("result count/truncation = %d/%v/%v", len(result.Hits), result.Truncated, result.Continuation)
	}
	if result.PreviewBytes > maxSearchAllTotalPreviewBytes {
		t.Fatalf("preview bytes = %d, hard cap = %d", result.PreviewBytes, maxSearchAllTotalPreviewBytes)
	}
	var sum int
	for _, hit := range result.Hits {
		if len(hit.Preview) > maxSearchAllPreviewPerHit {
			t.Fatalf("one preview has %d bytes, per-hit cap = %d", len(hit.Preview), maxSearchAllPreviewPerHit)
		}
		sum += len(hit.Preview)
	}
	if sum != result.PreviewBytes {
		t.Fatalf("preview byte metadata = %d, actual = %d", result.PreviewBytes, sum)
	}
	if !result.PreviewsTruncated {
		t.Fatal("expected preview truncation to be reported independently")
	}

	next, err := svc.searchAllPage(meta.FileID, `a{512}`, true, true, false, maxSearchAllHits, result.Continuation.StartOffset, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Hits) != 1 || next.Truncated {
		t.Fatalf("continuation result = %d hits, truncated=%v; want one final hit", len(next.Hits), next.Truncated)
	}
}

func openSearchFixture(t *testing.T, data []byte) (*FileService, FileMeta) {
	t.Helper()
	path := writeTempFile(t, "search-all.txt", data)
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
	return svc, meta
}

func searchHitOffsets(hits []SearchAllHit) []int64 {
	offsets := make([]int64, len(hits))
	for i, hit := range hits {
		offsets[i] = hit.Offset
	}
	return offsets
}

func testSearchMode(regex, backward bool) string {
	mode := "plain/forward"
	if regex {
		mode = "regex/forward"
	}
	if backward {
		mode = mode[:len(mode)-len("forward")] + "backward"
	}
	return mode
}
