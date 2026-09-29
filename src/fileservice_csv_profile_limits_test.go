package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCsvProfileReportsByteAndRecordLimits(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		truncated   bool
	}{
		{"complete", "value\none\ntwo\n", false},
		{"record-limit", "value\n" + strings.Repeat("row\n", 250), true},
		{"source-byte-limit", "value\n" + strings.Repeat(strings.Repeat("x", 64*1024)+"\n", 40), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.csv")
			if err := os.WriteFile(path, []byte(tc.input), 0o600); err != nil {
				t.Fatal(err)
			}
			svc := NewFileService()
			meta, err := svc.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer svc.CloseFile(meta.FileID)
			report, err := svc.CsvProfile(meta.FileID, ",", true)
			if err != nil {
				t.Fatal(err)
			}
			if report.Truncated != tc.truncated {
				t.Fatalf("Truncated = %t, want %t (records %d)", report.Truncated, tc.truncated, report.RecordsScanned)
			}
		})
	}
}
