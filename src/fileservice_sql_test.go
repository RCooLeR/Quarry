package main

import (
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

func TestFmtByteCountIsTotalAcrossInt64(t *testing.T) {
	tests := []struct {
		value int64
		want  string
	}{
		{value: -1, want: "-1 B"},
		{value: 0, want: "0 B"},
		{value: 1023, want: "1023 B"},
		{value: 1 << 10, want: "1.0 KiB"},
		{value: 1 << 20, want: "1.0 MiB"},
		{value: 1 << 30, want: "1.0 GiB"},
		{value: 1 << 40, want: "1.0 TiB"},
		{value: 1 << 50, want: "1.0 PiB"},
		{value: 1 << 60, want: "1.0 EiB"},
		{value: math.MaxInt64, want: "8.0 EiB"},
	}
	for _, tt := range tests {
		if got := fmtByteCount(tt.value); got != tt.want {
			t.Fatalf("fmtByteCount(%d) = %q, want %q", tt.value, got, tt.want)
		}
	}
}

func TestSampleInsertRows(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		maxRows int
		want    string
	}{
		{
			name:    "first two of four tuples",
			data:    "INSERT INTO `t` VALUES (1,'a'),(2,'b'),(3,'c');\nINSERT INTO `t` VALUES (4,'d');\n",
			maxRows: 2,
			want:    "INSERT INTO `t` VALUES (1,'a'),(2,'b');\n",
		},
		{
			name:    "across statements",
			data:    "INSERT INTO `t` VALUES (1,'a'),(2,'b');\nINSERT INTO `t` VALUES (3,'c'),(4,'d');\n",
			maxRows: 3,
			want:    "INSERT INTO `t` VALUES (1,'a'),(2,'b');\nINSERT INTO `t` VALUES (3,'c');\n",
		},
		{
			name:    "paren inside string is ignored",
			data:    "INSERT INTO `t` VALUES (1,'a)b'),(2,'c');\n",
			maxRows: 1,
			want:    "INSERT INTO `t` VALUES (1,'a)b');\n",
		},
		{
			name:    "escaped quote inside string",
			data:    "INSERT INTO `t` VALUES (1,'a\\'b'),(2,'c');\n",
			maxRows: 1,
			want:    "INSERT INTO `t` VALUES (1,'a\\'b');\n",
		},
		{
			name:    "fewer rows than requested keeps terminator",
			data:    "INSERT INTO `t` VALUES (1,'a');\n",
			maxRows: 10,
			want:    "INSERT INTO `t` VALUES (1,'a');\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := strings.NewReader(tc.data)
			got, err := sampleInsertRows(r, 0, int64(len(tc.data)), tc.maxRows)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", string(got), tc.want)
			}
		})
	}
}

type zeroEOFReaderAt struct{}

func (zeroEOFReaderAt) ReadAt([]byte, int64) (int, error) { return 0, io.EOF }

type fillReaderAt byte

func (r fillReaderAt) ReadAt(p []byte, _ int64) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func TestSampleInsertRowsRejectsEarlyEOFAndOversizedTuple(t *testing.T) {
	if _, err := sampleInsertRowsContext(t.Context(), zeroEOFReaderAt{}, 0, 10, 1); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("early EOF error = %v, want io.ErrUnexpectedEOF", err)
	}
	if _, err := sampleInsertRowsContext(t.Context(), fillReaderAt('x'), 0, maxFixtureSampleBytes+1, 1); err == nil || !strings.Contains(err.Error(), "memory limit") {
		t.Fatalf("oversized tuple error = %v, want memory limit", err)
	}
}
