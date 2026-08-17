package replace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPHPSerializationDetectorRecognizesMarkersAcrossEverySeam(t *testing.T) {
	markers := []string{
		`s:15:"http://old.test";`,
		`a:2:{s:3:"url";s:15:"http://old.test";}`,
		`O:8:"stdClass":0:{}`,
		`C:4:"Demo":0:{}`,
		`E:11:"Suit:Hearts";`,
		`S:3:\"foo\";`,
		`s:999999999999999999999999999999:"x";`,
	}

	for _, marker := range markers {
		marker := marker
		t.Run(marker, func(t *testing.T) {
			for seam := 0; seam <= len(marker); seam++ {
				var detector phpSerializationDetector
				detector.Inspect([]byte(marker[:seam]))
				detector.Inspect([]byte(marker[seam:]))
				if err := detector.Err(); !errors.Is(err, ErrPossiblePHPSerializedData) {
					t.Fatalf("seam %d: error = %v, want ErrPossiblePHPSerializedData", seam, err)
				}
			}

			var bytewise phpSerializationDetector
			for i := range marker {
				bytewise.Inspect([]byte(marker[i : i+1]))
			}
			if err := bytewise.Err(); !errors.Is(err, ErrPossiblePHPSerializedData) {
				t.Fatalf("one-byte chunks: error = %v, want ErrPossiblePHPSerializedData", err)
			}
		})
	}
}

func TestPHPSerializationDetectorDoesNotFlagIncompleteMarkers(t *testing.T) {
	nonMarkers := []string{
		`ordinary SQL without serialization`,
		`s:12;`,
		`s::"value";`,
		`s:x:"value";`,
		`s:12:value;`,
		`a:3:[1,2,3]`,
		`O:4:{test}`,
		`C:4:'Demo'`,
	}
	for _, input := range nonMarkers {
		var detector phpSerializationDetector
		detector.Inspect([]byte(input))
		if err := detector.Err(); err != nil {
			t.Fatalf("input %q: unexpected detection: %v", input, err)
		}
	}
}

func TestAtomicBatchReplaceRejectsPHPSerializedSQLWithoutPublishing(t *testing.T) {
	const wordpressDump = "INSERT INTO `wp_options` VALUES (1,'siteurl','a:2:{s:3:\"url\";s:15:\"http://old.test\";s:5:\"emoji\";s:4:\"\xf0\x9f\x98\x80\";}');\n"

	tests := []struct {
		name string
		run  func(context.Context, string, string) (FileSummary, error)
	}{
		{
			name: "plain length-changing replacement",
			run: func(ctx context.Context, sourcePath, outputPath string) (FileSummary, error) {
				return replaceBatchPlainFileAtomic(ctx, sourcePath, outputPath,
					[]BatchRule{{Name: "domain", Find: []byte("old.test"), Replace: []byte("new.example")}},
					FileOptions{}, BatchOptions{ChunkSize: 5})
			},
		},
		{
			name: "plain same-length replacement",
			run: func(ctx context.Context, sourcePath, outputPath string) (FileSummary, error) {
				return replaceBatchPlainFileAtomic(ctx, sourcePath, outputPath,
					[]BatchRule{{Name: "domain", Find: []byte("old"), Replace: []byte("new")}},
					FileOptions{}, BatchOptions{ChunkSize: 7})
			},
		},
		{
			name: "plain serialization metadata replacement",
			run: func(ctx context.Context, sourcePath, outputPath string) (FileSummary, error) {
				return replaceBatchPlainFileAtomic(ctx, sourcePath, outputPath,
					[]BatchRule{{Name: "metadata", Find: []byte("s:15"), Replace: []byte("s:19")}},
					FileOptions{}, BatchOptions{ChunkSize: 3})
			},
		},
		{
			name: "regex replacement",
			run: func(ctx context.Context, sourcePath, outputPath string) (FileSummary, error) {
				return replaceBatchRegexpFileAtomic(ctx, sourcePath, outputPath,
					[]BatchRule{{Name: "domain", Find: []byte(`old[.]test`), Replace: []byte("new.example")}},
					FileOptions{}, RegexOptions{ChunkSize: 11, MaxMatchWindow: 64})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "wordpress.sql")
			outputPath := filepath.Join(dir, "replaced.sql")
			if err := os.WriteFile(sourcePath, []byte(wordpressDump), 0o600); err != nil {
				t.Fatal(err)
			}

			summary, err := test.run(context.Background(), sourcePath, outputPath)
			if !errors.Is(err, ErrPossiblePHPSerializedData) {
				t.Fatalf("error = %v, want ErrPossiblePHPSerializedData", err)
			}
			if !strings.Contains(err.Error(), "no output was published") {
				t.Fatalf("error = %q, want explicit publication status", err)
			}
			if summary.Published || summary.Complete || summary.TempPath != "" {
				t.Fatalf("summary = %#v, want unpublished incomplete result", summary)
			}
			if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("rejected output became visible: %v", statErr)
			}
			assertReplaceFileBytes(t, sourcePath, []byte(wordpressDump))
			assertNoReplaceScratch(t, dir)
		})
	}
}

func TestAtomicBatchReplaceAllowsSQLWithoutSerializationMarkers(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "plain.sql")
	outputPath := filepath.Join(dir, "replaced.sql")
	source := []byte("UPDATE settings SET value='http://old.test';\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := replaceBatchPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]BatchRule{{Name: "domain", Find: []byte("old.test"), Replace: []byte("new.example")}},
		FileOptions{}, BatchOptions{ChunkSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Published || !summary.Complete || summary.Matches != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	assertReplaceFileBytes(t, sourcePath, source)
	assertReplaceFileBytes(t, outputPath, []byte("UPDATE settings SET value='http://new.example';\n"))
	assertNoReplaceScratch(t, dir)
}

func TestSQLPlainReplaceRecountsPHPSerializedLengths(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "wordpress.sql")
	outputPath := filepath.Join(dir, "replaced.sql")
	source := []byte("INSERT INTO wp_options VALUES ('a:2:{s:3:\"url\";s:15:\"http://old.test\";s:5:\"emoji\";s:4:\"\xf0\x9f\x98\x80\";}');\n")
	want := []byte("INSERT INTO wp_options VALUES ('a:2:{s:3:\"url\";s:18:\"http://new.example\";s:5:\"emoji\";s:4:\"\xf0\x9f\x98\x80\";}');\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := ReplaceSQLPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]byte("old.test"), []byte("new.example"), FileOptions{}, BatchOptions{ChunkSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Published || !summary.Complete || summary.Matches != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	assertReplaceFileBytes(t, sourcePath, source)
	assertReplaceFileBytes(t, outputPath, want)
	assertNoReplaceScratch(t, dir)
}

func TestSQLPlainReplaceRecountsObjectAndCustomSerializedLengths(t *testing.T) {
	tests := []struct {
		name   string
		source []byte
		want   []byte
	}{
		{
			name:   "object",
			source: []byte("INSERT INTO wp_options VALUES ('O:8:\"WP_Error\":1:{s:7:\"message\";s:15:\"http://old.test\";}');\n"),
			want:   []byte("INSERT INTO wp_options VALUES ('O:8:\"WP_Error\":1:{s:7:\"message\";s:18:\"http://new.example\";}');\n"),
		},
		{
			name:   "custom",
			source: []byte("INSERT INTO wp_options VALUES ('C:4:\"Demo\":15:{http://old.test}');\n"),
			want:   []byte("INSERT INTO wp_options VALUES ('C:4:\"Demo\":18:{http://new.example}');\n"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "wordpress.sql")
			outputPath := filepath.Join(dir, "replaced.sql")
			if err := os.WriteFile(sourcePath, tt.source, 0o600); err != nil {
				t.Fatal(err)
			}

			summary, err := ReplaceSQLPlainFileAtomic(context.Background(), sourcePath, outputPath,
				[]byte("old.test"), []byte("new.example"), FileOptions{}, BatchOptions{ChunkSize: 3})
			if err != nil {
				t.Fatal(err)
			}
			if !summary.Published || !summary.Complete || summary.Matches != 1 {
				t.Fatalf("summary = %#v", summary)
			}
			assertReplaceFileBytes(t, sourcePath, tt.source)
			assertReplaceFileBytes(t, outputPath, tt.want)
			assertNoReplaceScratch(t, dir)
		})
	}
}

func TestSQLPlainReplaceRecountsNestedSerializedStringPayload(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "nested-wordpress.sql")
	outputPath := filepath.Join(dir, "replaced.sql")
	nested := `O:4:"Demo":1:{s:3:"url";s:15:"http://old.test";}`
	source := []byte(`INSERT INTO wp_options VALUES ('a:1:{s:7:"payload";s:` + strconv.Itoa(len(nested)) + `:"` + nested + `";}');` + "\n")
	wantNested := `O:4:"Demo":1:{s:3:"url";s:18:"http://new.example";}`
	want := []byte(`INSERT INTO wp_options VALUES ('a:1:{s:7:"payload";s:` + strconv.Itoa(len(wantNested)) + `:"` + wantNested + `";}');` + "\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := ReplaceSQLPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]byte("old.test"), []byte("new.example"), FileOptions{}, BatchOptions{ChunkSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Published || !summary.Complete || summary.Matches != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	assertReplaceFileBytes(t, sourcePath, source)
	assertReplaceFileBytes(t, outputPath, want)
	assertNoReplaceScratch(t, dir)
}

func TestSQLPlainReplaceHandlesSQLBackslashEscapedSerializedLiteral(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "wordpress.sql")
	outputPath := filepath.Join(dir, "replaced.sql")
	source := []byte("INSERT INTO wp_options VALUES ('s:15:\\\"http://old.test\\\";');\n")
	want := []byte("INSERT INTO wp_options VALUES ('s:18:\"http://new.example\";');\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := ReplaceSQLPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]byte("old.test"), []byte("new.example"), FileOptions{}, BatchOptions{ChunkSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Matches != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	assertReplaceFileBytes(t, outputPath, want)
	assertNoReplaceScratch(t, dir)
}

func TestSQLPlainReplaceCaseInsensitiveInsideSerializedLiteral(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "wordpress.sql")
	outputPath := filepath.Join(dir, "replaced.sql")
	source := []byte("INSERT INTO wp_options VALUES ('s:15:\"HTTP://OLD.TEST\";');\n")
	want := []byte("INSERT INTO wp_options VALUES ('s:18:\"http://new.example\";');\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := ReplaceSQLPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]byte("http://old.test"), []byte("http://new.example"), FileOptions{}, BatchOptions{CaseInsensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Matches != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	assertReplaceFileBytes(t, outputPath, want)
	assertNoReplaceScratch(t, dir)
}

func TestSQLPlainReplaceDoesNotRewriteCommentsOrIdentifiers(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "plain.sql")
	outputPath := filepath.Join(dir, "replaced.sql")
	source := []byte("-- http://old.test in comment\nCREATE TABLE old_test (url text);\nINSERT INTO old_test VALUES ('http://old.test');\n")
	want := []byte("-- http://old.test in comment\nCREATE TABLE old_test (url text);\nINSERT INTO old_test VALUES ('http://new.example');\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := ReplaceSQLPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]byte("old.test"), []byte("new.example"), FileOptions{}, BatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Matches != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	assertReplaceFileBytes(t, outputPath, want)
	assertNoReplaceScratch(t, dir)
}

func TestSQLPlainReplaceRejectsEncodedSerializedLiteralWhenEncodedTextWouldChange(t *testing.T) {
	tests := []struct {
		name   string
		source string
		find   string
	}{
		{
			name:   "base64",
			source: "INSERT INTO wp_options VALUES ('YToxOntzOjM6InVybCI7czoxNToiaHR0cDovL29sZC50ZXN0Ijt9');\n",
			find:   "YTox",
		},
		{
			name:   "hex",
			source: "INSERT INTO wp_options VALUES ('613A313A7B733A333A2275726C223B733A31353A22687474703A2F2F6F6C642E74657374223B7D');\n",
			find:   "613A",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "encoded.sql")
			outputPath := filepath.Join(dir, "replaced.sql")
			source := []byte(test.source)
			if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
				t.Fatal(err)
			}

			summary, err := ReplaceSQLPlainFileAtomic(context.Background(), sourcePath, outputPath,
				[]byte(test.find), []byte("changed"), FileOptions{}, BatchOptions{})
			if !errors.Is(err, ErrUnsupportedPHPSerializedData) {
				t.Fatalf("error = %v, want ErrUnsupportedPHPSerializedData", err)
			}
			if summary.Published || summary.Complete {
				t.Fatalf("summary = %#v, want no publication", summary)
			}
			assertReplaceFileBytes(t, sourcePath, source)
			if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("rejected output became visible: %v", statErr)
			}
			assertNoReplaceScratch(t, dir)
		})
	}
}

func TestSQLPlainReplaceRejectsUnsupportedSQLContextsWithoutPublishing(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{
			name:   "custom delimiter routine",
			source: "DELIMITER $$\nCREATE PROCEDURE p()\nBEGIN\nSELECT 'http://old.test';\nEND$$\nDELIMITER ;\n",
		},
		{
			name:   "no backslash escapes",
			source: "SET sql_mode='NO_BACKSLASH_ESCAPES';\nINSERT INTO t VALUES ('http://old.test');\n",
		},
		{
			name:   "postgres copy payload",
			source: "COPY imported(value) FROM STDIN;\nhttp://old.test\n\\.\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "unsupported.sql")
			outputPath := filepath.Join(dir, "replaced.sql")
			source := []byte(test.source)
			if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
				t.Fatal(err)
			}

			summary, err := ReplaceSQLPlainFileAtomic(context.Background(), sourcePath, outputPath,
				[]byte("old.test"), []byte("new.example"), FileOptions{}, BatchOptions{})
			if !errors.Is(err, ErrUnsupportedSQLReplaceContext) {
				t.Fatalf("error = %v, want ErrUnsupportedSQLReplaceContext", err)
			}
			if summary.Published || summary.Complete {
				t.Fatalf("summary = %#v, want no publication", summary)
			}
			assertReplaceFileBytes(t, sourcePath, source)
			if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("rejected output became visible: %v", statErr)
			}
			assertNoReplaceScratch(t, dir)
		})
	}
}

func TestSQLPlainReplaceRejectsMalformedSerializedLiteralWithoutPublishing(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "bad.sql")
	outputPath := filepath.Join(dir, "replaced.sql")
	source := []byte("INSERT INTO wp_options VALUES ('s:99:\"http://old.test');\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := ReplaceSQLPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]byte("old.test"), []byte("new.example"), FileOptions{}, BatchOptions{ChunkSize: 4})
	if !errors.Is(err, ErrUnsupportedPHPSerializedData) {
		t.Fatalf("error = %v, want ErrUnsupportedPHPSerializedData", err)
	}
	if summary.Published || summary.Complete {
		t.Fatalf("summary = %#v, want no publication", summary)
	}
	assertReplaceFileBytes(t, sourcePath, source)
	if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected output became visible: %v", statErr)
	}
	assertNoReplaceScratch(t, dir)
}

func TestSQLPlainReplaceRepairsStaleSerializedStringLength(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "stale-length.sql")
	outputPath := filepath.Join(dir, "replaced.sql")
	source := []byte("INSERT INTO wp_options VALUES ('s:99:\"http://old.test\";');\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := ReplaceSQLPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]byte("old.test"), []byte("new.example"), FileOptions{}, BatchOptions{ChunkSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Matches != 1 || !summary.Published || !summary.Complete {
		t.Fatalf("summary = %#v, want one published replacement", summary)
	}
	assertReplaceFileBytes(t, sourcePath, source)
	assertReplaceFileBytes(t, outputPath, []byte("INSERT INTO wp_options VALUES ('s:18:\"http://new.example\";');\n"))
}

func TestSQLPlainReplaceCopiesUnrelatedMalformedSerializedLiteral(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "unrelated-bad.sql")
	outputPath := filepath.Join(dir, "replaced.sql")
	source := []byte("INSERT INTO wp_options VALUES ('s:99:\"http://stale.example\";','http://old.test');\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := ReplaceSQLPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]byte("old.test"), []byte("new.example"), FileOptions{}, BatchOptions{ChunkSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Matches != 1 || !summary.Published || !summary.Complete {
		t.Fatalf("summary = %#v, want one published replacement", summary)
	}
	assertReplaceFileBytes(t, sourcePath, source)
	assertReplaceFileBytes(t, outputPath, []byte("INSERT INTO wp_options VALUES ('s:99:\"http://stale.example\";','http://new.example');\n"))
}

func TestAtomicBatchReplaceCleansPriorWorkWhenSerializationMarkerAppearsLater(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "late-marker.sql")
	outputPath := filepath.Join(dir, "replaced.sql")
	source := []byte("UPDATE settings SET value='old.test';\n" + strings.Repeat("-", 128) + "\na:0:{}\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := replaceBatchPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]BatchRule{{Name: "domain", Find: []byte("old.test"), Replace: []byte("new.example")}},
		FileOptions{}, BatchOptions{ChunkSize: 8})
	if !errors.Is(err, ErrPossiblePHPSerializedData) {
		t.Fatalf("error = %v, want ErrPossiblePHPSerializedData", err)
	}
	if summary.Matches != 1 || summary.Published || summary.Complete {
		t.Fatalf("summary = %#v, want one discarded match and no publication", summary)
	}
	assertReplaceFileBytes(t, sourcePath, source)
	if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected output became visible: %v", statErr)
	}
	assertNoReplaceScratch(t, dir)
}

func TestLegacyFileReplaceAPIsRejectPHPSerializationBeforeCreatingArtifacts(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, string, string) (FileSummary, error)
	}{
		{
			name: "plain",
			run: func(ctx context.Context, sourcePath, outputPath string) (FileSummary, error) {
				return replacePlainFile(ctx, sourcePath, outputPath, []byte("old.test"), []byte("new.example"), FileOptions{})
			},
		},
		{
			name: "regex",
			run: func(ctx context.Context, sourcePath, outputPath string) (FileSummary, error) {
				return replaceRegexpFile(ctx, sourcePath, outputPath, []byte(`old[.]test`), []byte("new.example"), FileOptions{}, RegexOptions{MaxMatchWindow: 64})
			},
		},
		{
			name: "batch plain",
			run: func(ctx context.Context, sourcePath, outputPath string) (FileSummary, error) {
				return replaceBatchPlainFile(ctx, sourcePath, outputPath,
					[]BatchRule{{Name: "domain", Find: []byte("old.test"), Replace: []byte("new.example")}}, FileOptions{}, BatchOptions{})
			},
		},
		{
			name: "batch regex",
			run: func(ctx context.Context, sourcePath, outputPath string) (FileSummary, error) {
				return replaceBatchRegexpFile(ctx, sourcePath, outputPath,
					[]BatchRule{{Name: "domain", Find: []byte(`old[.]test`), Replace: []byte("new.example")}}, FileOptions{}, RegexOptions{MaxMatchWindow: 64})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "wordpress.sql")
			outputPath := filepath.Join(dir, "replaced.sql")
			source := []byte("INSERT INTO wp_options VALUES ('a:1:{s:3:\"url\";s:15:\"http://old.test\";}');\n")
			if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
				t.Fatal(err)
			}

			summary, err := test.run(context.Background(), sourcePath, outputPath)
			if !errors.Is(err, ErrPossiblePHPSerializedData) {
				t.Fatalf("error = %v, want ErrPossiblePHPSerializedData", err)
			}
			if summary.Published || summary.Complete {
				t.Fatalf("summary = %#v, want no publication", summary)
			}
			assertReplaceFileBytes(t, sourcePath, source)
			if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("rejected output became visible: %v", statErr)
			}
			assertNoReplaceScratch(t, dir)
		})
	}
}

func TestLegacyFileReplaceGuardsTransformAfterCleanPreflight(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	outputPath := filepath.Join(dir, "output.sql")
	initial := []byte("UPDATE settings SET value='old.test';\n")
	serialized := []byte("INSERT INTO wp_options VALUES ('a:0:{}');\n")
	if err := os.WriteFile(sourcePath, initial, 0o600); err != nil {
		t.Fatal(err)
	}

	originalOpenExclusive := openExclusive
	var mutationErr error
	openExclusive = func(path string) (syncWriteCloser, error) {
		mutationErr = os.WriteFile(sourcePath, serialized, 0o600)
		return originalOpenExclusive(path)
	}
	t.Cleanup(func() { openExclusive = originalOpenExclusive })

	summary, err := replacePlainFile(context.Background(), sourcePath, outputPath, []byte("old.test"), []byte("new.example"), FileOptions{})
	if mutationErr != nil {
		t.Fatalf("mutate after preflight: %v", mutationErr)
	}
	if !errors.Is(err, ErrPossiblePHPSerializedData) {
		t.Fatalf("error = %v, want transform-stream serialization rejection", err)
	}
	if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("serialized replacement output became visible: %v", statErr)
	}
	assertReplaceFileBytes(t, sourcePath, serialized)
	// Legacy pipelines intentionally retain failed temp/manifest evidence. The
	// safety assertion is that neither artifact was promoted to output.
	if summary.TempPath == "" || summary.ManifestPath == "" {
		t.Fatalf("legacy failure lost retained artifact evidence: %#v", summary)
	}
}
