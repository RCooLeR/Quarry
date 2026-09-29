package csv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

type ValueCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type ColumnProfile struct {
	Name           string       `json:"name"`
	SQLType        string       `json:"sqlType"`
	NonNull        int          `json:"nonNull"`
	Null           int          `json:"null"`
	Distinct       int          `json:"distinct"`
	DistinctCapped bool         `json:"distinctCapped"`
	Min            string       `json:"min"`
	Max            string       `json:"max"`
	Top            []ValueCount `json:"top"`
}

type ProfileReport struct {
	Columns         []ColumnProfile `json:"columns"`
	RecordsScanned  int             `json:"recordsScanned"`
	RaggedRows      int             `json:"raggedRows"`
	TruncatedSample bool            `json:"truncatedSample"`
	Warnings        []string        `json:"warnings"`
}

const (
	profileDistinctCap          = 50_000
	profileAggregateDistinctCap = 100_000
	profileTopValuesCap         = 10
)

type colAcc struct {
	counts  map[string]int
	capped  bool
	nonNull int
	null    int
	kind    schemaKind
	min     string
	max     string
	hasMM   bool
}

// ProfileColumns scans a bounded sample and returns per-column stats: null %,
// distinct estimate, lexicographic min/max, and top values — plus a ragged-row
// count for the dump linter.
func ProfileColumns(ctx context.Context, r io.Reader, opts SchemaOptions) (ProfileReport, error) {
	if r == nil {
		return ProfileReport{}, errors.New("reader is required")
	}
	opts, err := normalizeSchemaOptions(opts)
	if err != nil {
		return ProfileReport{}, err
	}
	sample, err := readBoundedSample(ctx, r, opts.MaxBytes)
	if err != nil {
		return ProfileReport{}, err
	}
	data := sample.Data
	partialOmitted := false
	if sample.Truncated {
		data, partialOmitted, err = CompleteRecordPrefix(ctx, data, opts.Delimiter, opts.MaxRecordBytes, true)
		if err != nil {
			return ProfileReport{}, err
		}
	}
	report := ProfileReport{TruncatedSample: sample.Truncated}
	if len(bytes.TrimSpace(data)) == 0 {
		report.Warnings = append(report.Warnings, "sample is empty")
		return report, nil
	}

	reader, err := newBoundedCSVReader(ctx, bytes.NewReader(data), csvReaderConfig{
		Delimiter: opts.Delimiter, MaxRecordBytes: opts.MaxRecordBytes,
		FieldsPerRecord: -1, LazyQuotes: false,
	})
	if err != nil {
		return report, err
	}

	nulls := makeSQLNullSet(opts.NullValues)
	var names []string
	var accs []*colAcc
	expected := 0
	distinctEntries := 0

	ensure := func(n int) {
		previousDataRows := report.RecordsScanned - 1
		if opts.HasHeader {
			previousDataRows--
		}
		for len(accs) < n {
			// A column discovered in a wider later row was absent in each
			// preceding data row; include those missing cells in its null count.
			accs = append(accs, &colAcc{counts: map[string]int{}, null: max(0, previousDataRows)})
			names = append(names, fmt.Sprintf("column_%d", len(accs)))
		}
	}

	for report.RecordsScanned < opts.MaxRows {
		if err := contextErr(ctx); err != nil {
			return report, err
		}
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return report, err
		}
		report.RecordsScanned++
		if report.RecordsScanned == 1 {
			if opts.HasHeader {
				names = normalizeSQLColumnNames(record)
				accs = make([]*colAcc, len(names))
				for i := range accs {
					accs[i] = &colAcc{counts: map[string]int{}}
				}
				expected = len(names)
				continue
			}
			ensure(len(record))
			expected = len(record)
		}
		if len(record) != expected {
			report.RaggedRows++
		}
		if len(record) > len(accs) {
			ensure(len(record))
		}
		for i := range accs {
			a := accs[i]
			if i >= len(record) {
				a.null++
				continue
			}
			value := record[i]
			if _, ok := nulls[value]; ok {
				a.null++
				continue
			}
			a.nonNull++
			a.kind = promoteSchemaKind(a.kind, classifySchemaValue(value))
			if !a.hasMM {
				a.min, a.max, a.hasMM = value, value, true
			} else {
				if value < a.min {
					a.min = value
				}
				if value > a.max {
					a.max = value
				}
			}
			if _, exists := a.counts[value]; exists {
				a.counts[value]++
			} else if len(a.counts) >= profileDistinctCap || distinctEntries >= profileAggregateDistinctCap {
				// The aggregate ceiling prevents the per-column cap from
				// multiplying by MaxCSVFieldsPerRecord. Mark only columns for
				// which a value was actually omitted as approximate.
				a.capped = true
			} else {
				a.counts[value] = 1
				distinctEntries++
			}
		}
	}
	if report.RecordsScanned == opts.MaxRows && hasRemainingCSVRecords(data, reader.InputOffset()) {
		report.TruncatedSample = true
		report.Warnings = append(report.Warnings, fmt.Sprintf("profile limited to %d records", opts.MaxRows))
	}

	report.Columns = make([]ColumnProfile, len(accs))
	for i, a := range accs {
		name := ""
		if i < len(names) {
			name = names[i]
		}
		top := topProfileValues(a.counts)
		report.Columns[i] = ColumnProfile{
			Name:           name,
			SQLType:        schemaKindSQLType(a.kind),
			NonNull:        a.nonNull,
			Null:           a.null,
			Distinct:       len(a.counts),
			DistinctCapped: a.capped,
			Min:            a.min,
			Max:            a.max,
			Top:            top,
		}
	}
	if sample.Truncated {
		report.Warnings = append(report.Warnings, fmt.Sprintf("profile from first %d bytes", opts.MaxBytes))
	}
	if partialOmitted {
		report.Warnings = append(report.Warnings, "trailing partial logical record omitted from profile")
	}
	return report, nil
}

// Keep only the returned values alive. Sorting the entire frequency map and
// returning a ten-element subslice retained every entry's string backing store
// for the lifetime of the bridge result, even though those values were hidden.
func topProfileValues(counts map[string]int) []ValueCount {
	top := make([]ValueCount, 0, min(len(counts), profileTopValuesCap))
	for value, count := range counts {
		pos := 0
		for pos < len(top) && (top[pos].Count > count || top[pos].Count == count && top[pos].Value < value) {
			pos++
		}
		if pos >= profileTopValuesCap {
			continue
		}
		if len(top) < profileTopValuesCap {
			top = append(top, ValueCount{})
		}
		copy(top[pos+1:], top[pos:len(top)-1])
		top[pos] = ValueCount{Value: value, Count: count}
	}
	// A cell is a substring of an entire parsed CSV record. Clone retained
	// top values so a short result cannot keep a huge unrelated row alive.
	for i := range top {
		top[i].Value = strings.Clone(top[i].Value)
	}
	return top
}
