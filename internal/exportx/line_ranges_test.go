package exportx

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

type exactLineRangeReader struct {
	*bytes.Reader
	size int64
}

func newExactLineRangeReader(data []byte) exactLineRangeReader {
	return exactLineRangeReader{Reader: bytes.NewReader(data), size: int64(len(data))}
}

func (r exactLineRangeReader) Size() int64 { return r.size }

func TestExactLineRangeOffsetsUsesLogicalMixedNewlines(t *testing.T) {
	data := []byte("one\r\ntwo\rthree\nfour")
	reader := newExactLineRangeReader(data)
	start, end, err := exactLineRangeOffsets(context.Background(), reader, "UTF-8", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data[start:end]); got != "two\rthree\n" {
		t.Fatalf("resolved bytes = %q at [%d,%d)", got, start, end)
	}
}

func TestExactLineRangeOffsetsUsesUTF16Boundaries(t *testing.T) {
	data := []byte{'a', 0, '\r', 0, '\n', 0, 'b', 0, '\n', 0, 'c', 0}
	reader := newExactLineRangeReader(data)
	start, end, err := exactLineRangeOffsets(context.Background(), reader, "UTF-16LE", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if start != 6 || end != 10 || !bytes.Equal(data[start:end], []byte{'b', 0, '\n', 0}) {
		t.Fatalf("UTF-16 range = [%d,%d) %v", start, end, data[start:end])
	}
}

func TestExactLineRangeOffsetsRejectsAbsentAndCanceledRanges(t *testing.T) {
	reader := newExactLineRangeReader([]byte("one\n"))
	if _, _, err := exactLineRangeOffsets(context.Background(), reader, "UTF-8", 2, 2); err == nil {
		t.Fatal("terminal empty line was accepted as source content")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := exactLineRangeOffsets(ctx, reader, "UTF-8", 1, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled range error = %v", err)
	}
}

func TestExactLineSplitRangesPreservesLogicalTerminators(t *testing.T) {
	data := []byte("one\r\ntwo\rthree\nfour")
	ranges, err := exactLineSplitRanges(context.Background(), newExactLineRangeReader(data), "UTF-8", 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 2 || string(data[ranges[0][0]:ranges[0][1]]) != "one\r\ntwo\r" || string(data[ranges[1][0]:ranges[1][1]]) != "three\nfour" {
		t.Fatalf("split ranges = %v", ranges)
	}
	if _, err := exactLineSplitRanges(context.Background(), newExactLineRangeReader([]byte("a\nb\n")), "UTF-8", 1, 1); err == nil {
		t.Fatal("excessive split part count was accepted")
	}
}
