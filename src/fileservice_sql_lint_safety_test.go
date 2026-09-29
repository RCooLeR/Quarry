package main

import (
	"strings"
	"testing"
)

func TestSQLLintDoesNotRecommendDisabledDefinerPreset(t *testing.T) {
	const source = "CREATE DEFINER=`root`@`localhost` TABLE alpha (id int);\nINSERT INTO alpha VALUES (1);\n"
	svc, meta, _ := openAnalyzedSQLGenerationFile(t, source)
	findings, err := svc.SqlLint(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings.Findings {
		if !strings.Contains(finding.Title, "DEFINER clauses") {
			continue
		}
		detail := strings.ToLower(finding.Detail)
		if strings.Contains(detail, "remove definer preset") || strings.Contains(detail, "consider the") {
			t.Fatalf("lint recommends an unavailable cleanup preset: %q", finding.Detail)
		}
		if !strings.Contains(detail, "cleanup presets are unavailable") || !strings.Contains(detail, "serialization-aware migration tool") {
			t.Fatalf("lint does not provide safe remediation guidance: %q", finding.Detail)
		}
		return
	}
	t.Fatalf("lint findings do not report the DEFINER clause: %#v", findings.Findings)
}

func TestSQLLintMetadataDetailIsSortedAndBounded(t *testing.T) {
	values := map[string]int{
		"j": 1, "i": 1, "h": 1, "g": 1, "f": 1,
		"e": 1, "d": 1, "c": 1, "b": 1, "a": 1,
	}
	if got, want := mapKeys(values), "a, b, c, d, e, f, g, h, … (+2)"; got != want {
		t.Fatalf("metadata detail = %q, want %q", got, want)
	}
}
