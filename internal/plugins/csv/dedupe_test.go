package csv

import (
	"context"
	stdcsv "encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestDedupeCanonicalKeysAreUnambiguous(t *testing.T) {
	collisionLeft := []string{"a\x00b", "c"}
	collisionRight := []string{"a", "b\x00c"}
	for _, keyColumn := range []int{-1, 9} {
		if left, right := dedupeKey(collisionLeft, keyColumn), dedupeKey(collisionRight, keyColumn); left == right {
			t.Fatalf("key column %d retained the embedded-NUL collision: %q", keyColumn, left)
		}
	}
	if present, missing := dedupeKey([]string{"id", ""}, 1), dedupeKey([]string{"id"}, 1); present == missing {
		t.Fatalf("present empty key collided with missing key: %q", present)
	}
	if first, second := dedupeKey([]string{"1", "same"}, 1), dedupeKey([]string{"2", "same"}, 1); first != second {
		t.Fatalf("equal selected cells produced different keys: %q != %q", first, second)
	}

	values := []string{
		"", "\x00", "a\x00b", "é", "控制", "\xff", "a,b", "\"", "\r\n",
		strings.Repeat("x", 127), strings.Repeat("x", 128), strings.Repeat("x", 255),
	}
	keys := make(map[string]string, len(values)*len(values))
	for _, first := range values {
		for _, second := range values {
			record := []string{first, second}
			key := dedupeKey(record, -1)
			description := fmt.Sprintf("%q", record)
			if previous, exists := keys[key]; exists && previous != description {
				t.Fatalf("canonical key collision: %s and %s", previous, description)
			}
			keys[key] = description
		}
	}
}

func TestDedupeRowsPreservesEmbeddedNULCollisionPair(t *testing.T) {
	input := "a,b\n\"a\x00b\",c\na,\"b\x00c\"\n"
	for _, keyColumn := range []int{-1, 9} {
		t.Run(strconv.Itoa(keyColumn), func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "source.csv")
			dst := filepath.Join(dir, "output.csv")
			if err := os.WriteFile(src, []byte(input), 0o600); err != nil {
				t.Fatal(err)
			}
			summary, err := DedupeRowsFile(context.Background(), src, dst, DedupeOptions{
				Delimiter: ',', HasHeader: true, KeyColumn: keyColumn,
				MaxDistinctKeys: 4, MaxMemoryBytes: 1 << 20,
			})
			if err != nil {
				t.Fatal(err)
			}
			if summary.RecordsWritten != 3 {
				t.Fatalf("records written = %d, want header plus both distinct rows", summary.RecordsWritten)
			}
			output, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			reader := stdcsv.NewReader(strings.NewReader(string(output)))
			reader.FieldsPerRecord = -1
			records, err := reader.ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 3 || records[1][0] != "a\x00b" || records[1][1] != "c" || records[2][0] != "a" || records[2][1] != "b\x00c" {
				t.Fatalf("records = %#v", records)
			}
		})
	}
}

func TestDedupeSetComparesFullKeysOnHashCollision(t *testing.T) {
	set, err := newDedupeSet(4, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	const forcedHash = 42
	first := dedupeKey([]string{"first"}, 0)
	second := dedupeKey([]string{"second"}, 0)
	if added, err := set.addHashed(first, forcedHash, 1); err != nil || !added {
		t.Fatalf("first add = %t, %v", added, err)
	}
	if added, err := set.addHashed(second, forcedHash, 2); err != nil || !added {
		t.Fatalf("colliding distinct add = %t, %v", added, err)
	}
	if added, err := set.addHashed(first, forcedHash, 3); err != nil || added {
		t.Fatalf("duplicate add = %t, %v", added, err)
	}
	if set.distinctKeys != 2 {
		t.Fatalf("distinct keys = %d, want 2", set.distinctKeys)
	}
}

func TestDedupeSetRefusesBeforeStateGrowth(t *testing.T) {
	t.Run("distinct keys", func(t *testing.T) {
		set, err := newDedupeSet(1, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if added, err := set.add(dedupeKey([]string{"first"}, 0), 1); err != nil || !added {
			t.Fatalf("first add = %t, %v", added, err)
		}
		beforeKeys, beforeMemory := set.distinctKeys, set.memoryBytes
		if _, err := set.add(dedupeKey([]string{"second"}, 0), 2); !errors.Is(err, ErrDedupeBudgetExceeded) {
			t.Fatalf("error = %v, want ErrDedupeBudgetExceeded", err)
		}
		if set.distinctKeys != beforeKeys || set.memoryBytes != beforeMemory {
			t.Fatalf("set grew on refusal: keys %d -> %d, memory %d -> %d", beforeKeys, set.distinctKeys, beforeMemory, set.memoryBytes)
		}
	})

	t.Run("memory", func(t *testing.T) {
		first := dedupeKey([]string{"first"}, 0)
		budget := dedupeSetBaseMemoryBytes(4) + dedupeKeyMemoryBytes(len(first))
		set, err := newDedupeSet(4, budget)
		if err != nil {
			t.Fatal(err)
		}
		if added, err := set.add(first, 1); err != nil || !added {
			t.Fatalf("first add = %t, %v", added, err)
		}
		beforeKeys, beforeMemory := set.distinctKeys, set.memoryBytes
		if _, err := set.add(dedupeKey([]string{strings.Repeat("x", 512)}, 0), 2); !errors.Is(err, ErrDedupeBudgetExceeded) {
			t.Fatalf("error = %v, want ErrDedupeBudgetExceeded", err)
		}
		if set.distinctKeys != beforeKeys || set.memoryBytes != beforeMemory {
			t.Fatalf("set grew on refusal: keys %d -> %d, memory %d -> %d", beforeKeys, set.distinctKeys, beforeMemory, set.memoryBytes)
		}
	})
}

func TestDedupeRowsFailsClosedAtDistinctKeyBudget(t *testing.T) {
	const limit = 10_000
	var input strings.Builder
	input.WriteString("key\n")
	for i := 0; i < limit+100; i++ {
		input.WriteString(strconv.Itoa(i))
		input.WriteByte('\n')
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "source.csv")
	dst := filepath.Join(dir, "output.csv")
	if err := os.WriteFile(src, []byte(input.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := DedupeRowsFile(context.Background(), src, dst, DedupeOptions{
		Delimiter: ',', HasHeader: true, KeyColumn: 0,
		MaxDistinctKeys: limit, MaxMemoryBytes: 4 << 20,
	})
	if !errors.Is(err, ErrDedupeBudgetExceeded) {
		t.Fatalf("error = %v, want ErrDedupeBudgetExceeded", err)
	}
	var budgetErr *DedupeBudgetError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("error type = %T, want *DedupeBudgetError", err)
	}
	if budgetErr.Kind != DedupeBudgetDistinctKeys || budgetErr.Limit != limit || budgetErr.Used != limit || budgetErr.Required != limit+1 || budgetErr.DistinctKeys != limit {
		t.Fatalf("budget error = %+v", budgetErr)
	}
	if budgetErr.Record != int64(limit+2) || summary.RecordsRead != int64(limit+2) {
		t.Fatalf("record/error summary = %d/%+v", summary.RecordsRead, budgetErr)
	}
	assertDedupeFailureUnpublished(t, dir, dst)
}

func TestDedupeRowsFailsClosedAtMemoryBudget(t *testing.T) {
	const maxDistinctKeys = 4
	firstKey := dedupeKey([]string{"small"}, 0)
	memoryBudget := dedupeSetBaseMemoryBytes(maxDistinctKeys) + dedupeKeyMemoryBytes(len(firstKey))
	dir := t.TempDir()
	src := filepath.Join(dir, "source.csv")
	dst := filepath.Join(dir, "output.csv")
	input := "key\nsmall\n" + strings.Repeat("x", 512) + "\n"
	if err := os.WriteFile(src, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := DedupeRowsFile(context.Background(), src, dst, DedupeOptions{
		Delimiter: ',', HasHeader: true, KeyColumn: 0,
		MaxDistinctKeys: maxDistinctKeys, MaxMemoryBytes: memoryBudget,
	})
	if !errors.Is(err, ErrDedupeBudgetExceeded) {
		t.Fatalf("error = %v, want ErrDedupeBudgetExceeded", err)
	}
	var budgetErr *DedupeBudgetError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("error type = %T, want *DedupeBudgetError", err)
	}
	if budgetErr.Kind != DedupeBudgetMemoryBytes || budgetErr.Record != 3 || budgetErr.Used != memoryBudget || budgetErr.Required <= memoryBudget || budgetErr.DistinctKeys != 1 {
		t.Fatalf("budget error = %+v", budgetErr)
	}
	assertDedupeFailureUnpublished(t, dir, dst)
}

func TestDedupeDuplicateKeysDoNotConsumeBudget(t *testing.T) {
	const repeated = "same"
	key := dedupeKey([]string{repeated}, 0)
	memoryBudget := dedupeSetBaseMemoryBytes(1) + dedupeKeyMemoryBytes(len(key))
	var input strings.Builder
	input.WriteString("key\n")
	for i := 0; i < 1000; i++ {
		input.WriteString(repeated)
		input.WriteByte('\n')
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "source.csv")
	dst := filepath.Join(dir, "output.csv")
	if err := os.WriteFile(src, []byte(input.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := DedupeRowsFile(context.Background(), src, dst, DedupeOptions{
		Delimiter: ',', HasHeader: true, KeyColumn: 0,
		MaxDistinctKeys: 1, MaxMemoryBytes: memoryBudget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.RecordsRead != 1001 || summary.RecordsWritten != 2 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestDedupeOptionsAreValidatedBeforeSourceOrOutput(t *testing.T) {
	tests := []struct {
		name string
		opts DedupeOptions
	}{
		{"negative distinct", DedupeOptions{Delimiter: ',', MaxDistinctKeys: -1}},
		{"excessive distinct", DedupeOptions{Delimiter: ',', MaxDistinctKeys: MaxDedupeDistinctKeys + 1}},
		{"negative memory", DedupeOptions{Delimiter: ',', MaxMemoryBytes: -1}},
		{"excessive memory", DedupeOptions{Delimiter: ',', MaxMemoryBytes: MaxDedupeMemoryBytes + 1}},
		{"table exceeds memory", DedupeOptions{Delimiter: ',', MaxDistinctKeys: 64, MaxMemoryBytes: 1}},
		{"negative record cap", DedupeOptions{Delimiter: ',', MaxRecordBytes: -1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "output.csv")
			_, err := DedupeRowsFile(context.Background(), filepath.Join(dir, "missing.csv"), dst, test.opts)
			if err == nil {
				t.Fatal("invalid options were accepted")
			}
			if errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source was opened before option validation: %v", err)
			}
			if _, statErr := os.Lstat(dst); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid options created output: %v", statErr)
			}
		})
	}
}

func TestDedupeCancellationRemovesPartialOutput(t *testing.T) {
	var input strings.Builder
	input.WriteString("key\n")
	for i := 0; i < 50_100; i++ {
		input.WriteString(strconv.Itoa(i))
		input.WriteByte('\n')
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "source.csv")
	dst := filepath.Join(dir, "output.csv")
	if err := os.WriteFile(src, []byte(input.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := DedupeRowsFile(ctx, src, dst, DedupeOptions{
		Delimiter: ',', HasHeader: true, KeyColumn: 0,
		MaxDistinctKeys: 60_000, MaxMemoryBytes: 16 << 20,
		Progress: func(records int64) {
			cancel()
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	assertDedupeFailureUnpublished(t, dir, dst)
}

func TestDedupeKeyEncodingHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fields := make([]string, 10_000)
	if _, err := dedupeKeyContext(ctx, fields, -1); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestDedupeSetConcurrentIndependentInstances(t *testing.T) {
	const workers = 8
	const keys = 1000
	var wait sync.WaitGroup
	errs := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			set, err := newDedupeSet(keys, 1<<20)
			if err != nil {
				errs <- err
				return
			}
			for i := 0; i < keys; i++ {
				key := dedupeKey([]string{strconv.Itoa(worker), strconv.Itoa(i)}, -1)
				added, err := set.add(key, int64(i+1))
				if err != nil || !added {
					errs <- fmt.Errorf("worker %d key %d add=%t: %w", worker, i, added, err)
					return
				}
				duplicate, err := set.add(key, int64(i+1))
				if err != nil || duplicate {
					errs <- fmt.Errorf("worker %d key %d duplicate=%t: %w", worker, i, duplicate, err)
					return
				}
			}
			if set.distinctKeys != keys || set.memoryBytes > set.maxMemoryBytes {
				errs <- fmt.Errorf("worker %d set state: keys=%d memory=%d/%d", worker, set.distinctKeys, set.memoryBytes, set.maxMemoryBytes)
			}
		}(worker)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func assertDedupeFailureUnpublished(t *testing.T, dir, destination string) {
	t.Helper()
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed dedupe published a final output: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "source.csv" {
		t.Fatalf("failed dedupe left temporary artifacts: %v", dedupeEntryNames(entries))
	}
}

func dedupeEntryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}
