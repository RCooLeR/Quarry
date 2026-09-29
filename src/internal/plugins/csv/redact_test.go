package csv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaskValue(t *testing.T) {
	key := bytes.Repeat([]byte{0x41}, PseudonymKeyBytes)
	cases := []struct {
		mode RedactMode
		in   string
		want string
	}{
		{RedactNull, "secret", ""},
		{RedactFixed, "secret", "REDACTED"},
		{RedactEmail, "alice@example.com", "a***@example.com"},
		{RedactEmail, "é@example.com", "é***@example.com"},
		{RedactEmail, "noatsign", "n***"},
		{RedactEmail, "", ""},
	}
	for _, c := range cases {
		got, err := maskValue(c.in, c.mode, "REDACTED", nil)
		if err != nil {
			t.Fatalf("maskValue(%q,%s): %v", c.in, c.mode, err)
		}
		if got != c.want {
			t.Fatalf("maskValue(%q,%s)=%q want %q", c.in, c.mode, got, c.want)
		}
	}
	// A keyed pseudonym is stable within one operation and uses 128 output bits.
	h1, err := maskValue("alice", RedactHash, "", key)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := maskValue("alice", RedactHash, "", key)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 || len(h1) != 2*pseudonymOutputBytes {
		t.Fatalf("pseudonym not stable or collision-resistant length: %q %q", h1, h2)
	}
	bob, err := maskValue("bob", RedactHash, "", key)
	if err != nil {
		t.Fatal(err)
	}
	if bob == h1 {
		t.Fatal("pseudonym collision for different inputs")
	}
	other, err := maskValue("alice", RedactHash, "", bytes.Repeat([]byte{0x42}, PseudonymKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	if other == h1 {
		t.Fatal("different operation keys produced linkable pseudonyms")
	}
	if _, err := maskValue("secret", RedactHash, "", nil); err == nil {
		t.Fatal("hash mode without a key did not fail closed")
	}
	if _, err := maskValue("secret", RedactMode("unknown"), "", nil); err == nil {
		t.Fatal("unknown mode did not fail closed")
	}
}

func TestRedactColumnsFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.csv")
	dst := filepath.Join(dir, "out.csv")
	if err := os.WriteFile(src, []byte("id,name,email\n1,Alice,alice@x.com\n2,Bob,bob@y.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := RedactColumnsFile(context.Background(), src, dst, RedactOptions{
		Delimiter: ',',
		HasHeader: true,
		Columns:   map[int]RedactMode{1: RedactFixed, 2: RedactEmail},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum.RecordsWritten != 3 || sum.CellsMasked != 4 {
		t.Fatalf("summary = %+v", sum)
	}
	got, _ := os.ReadFile(dst)
	want := "id,name,email\n1,REDACTED,a***@x.com\n2,REDACTED,b***@y.com\n"
	if string(got) != want {
		t.Fatalf("output = %q\nwant %q", string(got), want)
	}
	if strings.Contains(string(got), "Alice") || strings.Contains(string(got), "bob@y.com") {
		t.Fatal("PII leaked into redacted output")
	}
}

func TestRedactColumnsFileKeyedPseudonymsAreOperationLocal(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.csv")
	if err := os.WriteFile(src, []byte("id,value\n1,alice\n2,alice\n3,bob\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyA := bytes.Repeat([]byte{0x11}, PseudonymKeyBytes)
	keyB := bytes.Repeat([]byte{0x22}, PseudonymKeyBytes)
	outputs := make([][]string, 0, 2)
	for i, key := range [][]byte{keyA, keyB} {
		dst := filepath.Join(dir, fmt.Sprintf("out-%d.csv", i))
		if _, err := RedactColumnsFile(context.Background(), src, dst, RedactOptions{
			Delimiter: ',', HasHeader: true, Columns: map[int]RedactMode{1: RedactHash}, PseudonymKey: key,
		}); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(readAll(t, dst)), "\n")
		outputs = append(outputs, lines)
		alice1 := strings.Split(lines[1], ",")[1]
		alice2 := strings.Split(lines[2], ",")[1]
		bob := strings.Split(lines[3], ",")[1]
		if alice1 != alice2 || alice1 == bob || len(alice1) != 2*pseudonymOutputBytes {
			t.Fatalf("operation %d pseudonyms = %#v", i, lines)
		}
	}
	if strings.Split(outputs[0][1], ",")[1] == strings.Split(outputs[1][1], ",")[1] {
		t.Fatal("different operation keys produced linkable output pseudonyms")
	}
}

func TestRedactColumnsFileHashWithoutKeyFailsBeforeOutput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.csv")
	dst := filepath.Join(dir, "out.csv")
	if err := os.WriteFile(src, []byte("id,value\n1,secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RedactColumnsFile(context.Background(), src, dst, RedactOptions{
		Delimiter: ',', HasHeader: true, Columns: map[int]RedactMode{1: RedactHash},
	}); err == nil || !strings.Contains(err.Error(), "per-operation key") {
		t.Fatalf("missing-key error = %v", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing key created output: %v", err)
	}
}

func TestRedactColumnsFileRejectsInvalidModesBeforeOutputCreation(t *testing.T) {
	modes := []RedactMode{
		"",
		"hashed",
		"tokenize",
		"Hash",
		"HASH",
		" hash",
		"hash ",
	}
	for _, mode := range modes {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "in.csv")
			dst := filepath.Join(dir, "out.csv")
			original := []byte("id,ssn\n1,123-45-6789\n")
			if err := os.WriteFile(src, original, 0o600); err != nil {
				t.Fatal(err)
			}

			sum, err := RedactColumnsFile(context.Background(), src, dst, RedactOptions{
				Delimiter: ',',
				HasHeader: true,
				Columns:   map[int]RedactMode{1: mode},
			})
			if err == nil || !strings.Contains(err.Error(), "invalid redaction mode") {
				t.Fatalf("error = %v, want invalid redaction mode", err)
			}
			if sum.RecordsRead != 0 || sum.RecordsWritten != 0 || sum.CellsMasked != 0 {
				t.Fatalf("invalid mode returned non-zero summary: %+v", sum)
			}
			if _, err := os.Stat(dst); !os.IsNotExist(err) {
				t.Fatalf("invalid mode created output: stat error = %v", err)
			}
			got, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(original) {
				t.Fatalf("source changed: got %q want %q", got, original)
			}
		})
	}
}

func TestRedactColumnsFileInvalidInputPreservesExistingDestination(t *testing.T) {
	tests := []struct {
		name      string
		opts      RedactOptions
		errorPart string
	}{
		{
			name:      "no columns",
			opts:      RedactOptions{Delimiter: ','},
			errorPart: "select at least one column",
		},
		{
			name:      "negative index",
			opts:      RedactOptions{Delimiter: ',', Columns: map[int]RedactMode{-1: RedactHash}, PseudonymKey: bytes.Repeat([]byte{1}, PseudonymKeyBytes)},
			errorPart: "is negative",
		},
		{
			name:      "out of range index",
			opts:      RedactOptions{Delimiter: ',', Columns: map[int]RedactMode{2: RedactHash}, PseudonymKey: bytes.Repeat([]byte{1}, PseudonymKeyBytes)},
			errorPart: "outside the first record",
		},
		{
			name:      "invalid delimiter",
			opts:      RedactOptions{Delimiter: '"', Columns: map[int]RedactMode{1: RedactHash}},
			errorPart: "invalid delimiter",
		},
		{
			name:      "unknown mode",
			opts:      RedactOptions{Delimiter: ',', Columns: map[int]RedactMode{1: "future-mode"}},
			errorPart: "invalid redaction mode",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "in.csv")
			dst := filepath.Join(dir, "out.csv")
			originalSource := []byte("id,secret\n1,pii\n")
			destinationSentinel := []byte("existing destination must survive")
			if err := os.WriteFile(src, originalSource, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, destinationSentinel, 0o600); err != nil {
				t.Fatal(err)
			}

			sum, err := RedactColumnsFile(context.Background(), src, dst, test.opts)
			if err == nil || !strings.Contains(err.Error(), test.errorPart) {
				t.Fatalf("error = %v, want substring %q", err, test.errorPart)
			}
			if sum.RecordsRead != 0 || sum.RecordsWritten != 0 || sum.CellsMasked != 0 {
				t.Fatalf("invalid input returned non-zero summary: %+v", sum)
			}
			gotDestination, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			if string(gotDestination) != string(destinationSentinel) {
				t.Fatalf("destination changed: got %q want %q", gotDestination, destinationSentinel)
			}
			gotSource, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			if string(gotSource) != string(originalSource) {
				t.Fatalf("source changed: got %q want %q", gotSource, originalSource)
			}
		})
	}
}

func TestRedactColumnsFileRejectsEmptyInputBeforeOutputCreation(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "empty.csv")
	dst := filepath.Join(dir, "out.csv")
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := RedactColumnsFile(context.Background(), src, dst, RedactOptions{
		Delimiter: ',',
		Columns:   map[int]RedactMode{0: RedactNull},
	})
	if err == nil || !strings.Contains(err.Error(), "empty CSV") {
		t.Fatalf("error = %v, want empty CSV error", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("empty input created output: stat error = %v", err)
	}
}

func TestRedactColumnsFileCountsOnlyChangedCells(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.csv")
	dst := filepath.Join(dir, "out.csv")
	input := "id,name,note,email\n" +
		"1,REDACTED,,a***@x.com\n" +
		"2,Alice,secret,alice@x.com\n"
	if err := os.WriteFile(src, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}

	sum, err := RedactColumnsFile(context.Background(), src, dst, RedactOptions{
		Delimiter: ',',
		HasHeader: true,
		Columns: map[int]RedactMode{
			1: RedactFixed,
			2: RedactNull,
			3: RedactEmail,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum.RecordsRead != 3 || sum.RecordsWritten != 3 || sum.CellsMasked != 3 {
		t.Fatalf("summary = %+v, want 3 records read/written and 3 changed cells", sum)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	want := "id,name,note,email\n" +
		"1,REDACTED,,a***@x.com\n" +
		"2,REDACTED,,a***@x.com\n"
	if string(got) != want {
		t.Fatalf("output = %q\nwant %q", got, want)
	}
}

func TestRedactColumnsFileRejectsRaggedAndMalformedRecords(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "missing selected field", input: "id,name,secret\n1,Alice\n2,Bob,pii\n"},
		{name: "wider row", input: "id,name,secret\n1,Alice,pii,extra\n"},
		{name: "malformed quote", input: "id,name,secret\n1,Alice,\"unterminated\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "in.csv")
			dst := filepath.Join(dir, "out.csv")
			if err := os.WriteFile(src, []byte(tt.input), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := RedactColumnsFile(context.Background(), src, dst, RedactOptions{
				Delimiter: ',',
				HasHeader: true,
				Columns:   map[int]RedactMode{2: RedactFixed},
			})
			if err == nil {
				t.Fatal("malformed/ragged input succeeded")
			}
			if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed redaction left final output: %v", statErr)
			}
			got, readErr := os.ReadFile(src)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != tt.input {
				t.Fatal("failed redaction changed source")
			}
		})
	}
}

func TestRedactColumnsFileInvalidUTF8EmailCleansOutput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "legacy.csv")
	dst := filepath.Join(dir, "out.csv")
	input := append([]byte("id,email\n1,plain@example.com\n2,"), 0xe9)
	input = append(input, []byte("@example.com\n")...)
	if err := os.WriteFile(src, input, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := RedactColumnsFile(context.Background(), src, dst, RedactOptions{
		Delimiter: ',',
		HasHeader: true,
		Columns:   map[int]RedactMode{1: RedactEmail},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Fatalf("error = %v, want invalid UTF-8", err)
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed email redaction left output: %v", statErr)
	}
	got, readErr := os.ReadFile(src)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, input) {
		t.Fatal("failed email redaction changed source")
	}
}
