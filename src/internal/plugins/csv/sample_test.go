package csv

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBoundedSampleOperationsHonorCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := strings.NewReader("id,name\n1,Ada\n")

	if _, err := InspectReaderContext(ctx, input, InspectOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("InspectReaderContext err = %v, want context.Canceled", err)
	}
	if _, err := InferSchemaContext(ctx, strings.NewReader("id,name\n1,Ada\n"), SchemaOptions{HasHeader: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("InferSchemaContext err = %v, want context.Canceled", err)
	}
	if _, err := ProfileColumns(ctx, strings.NewReader("id,name\n1,Ada\n"), SchemaOptions{HasHeader: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ProfileColumns err = %v, want context.Canceled", err)
	}
	if _, err := PreviewRowsContext(ctx, strings.NewReader("id,name\n1,Ada\n"), PreviewOptions{HasHeader: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("PreviewRowsContext err = %v, want context.Canceled", err)
	}
	if _, err := BuildColumnGuideContext(ctx, strings.NewReader("id,name\n1,Ada\n"), ColumnGuideOptions{HasHeader: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildColumnGuideContext err = %v, want context.Canceled", err)
	}
	if _, err := PreviewProjectedColumnsContext(ctx, strings.NewReader("id,name\n1,Ada\n"), ProjectPreviewOptions{Columns: []int{0}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("PreviewProjectedColumnsContext err = %v, want context.Canceled", err)
	}
	if _, err := PreviewSQLConversionContext(ctx, strings.NewReader("id,name\n1,Ada\n"), SQLPreviewOptions{
		SQLConvertOptions: SQLConvertOptions{TableName: "records", HasHeader: true},
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("PreviewSQLConversionContext err = %v, want context.Canceled", err)
	}
}

type sampleStepReader struct {
	data      []byte
	step      int
	zeroReads int
	calls     int
	maxRead   int
}

func (r *sampleStepReader) Read(p []byte) (int, error) {
	r.calls++
	if len(p) > r.maxRead {
		r.maxRead = len(p)
	}
	if r.zeroReads > 0 {
		r.zeroReads--
		return 0, nil
	}
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	step := r.step
	if step <= 0 || step > len(p) {
		step = len(p)
	}
	if step > len(r.data) {
		step = len(r.data)
	}
	copy(p, r.data[:step])
	r.data = r.data[step:]
	return step, nil
}

func TestReadBoundedSampleExactTruncationAndBOMSemantics(t *testing.T) {
	tests := []struct {
		name      string
		input     []byte
		maxBytes  int64
		want      []byte
		truncated bool
	}{
		{name: "short source", input: []byte("ab"), maxBytes: 4, want: []byte("ab")},
		{name: "exact source", input: []byte("abcd"), maxBytes: 4, want: []byte("abcd")},
		{name: "one byte over", input: []byte("abcde"), maxBytes: 4, want: []byte("abcd"), truncated: true},
		{name: "UTF-8 BOM exact payload", input: append(append([]byte{}, utf8BOM...), []byte("abcd")...), maxBytes: 4, want: []byte("abcd")},
		{name: "UTF-8 BOM over payload", input: append(append([]byte{}, utf8BOM...), []byte("abcde")...), maxBytes: 4, want: []byte("abcd"), truncated: true},
		{name: "partial UTF-8 BOM is data", input: []byte{0xEF, 0xBB}, maxBytes: 2, want: []byte{0xEF, 0xBB}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &sampleStepReader{data: append([]byte(nil), test.input...), step: 1}
			sample, err := readBoundedSample(context.Background(), reader, test.maxBytes)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(sample.Data, test.want) {
				t.Fatalf("data = % X, want % X", sample.Data, test.want)
			}
			if sample.BytesScanned != int64(len(test.want)) {
				t.Fatalf("bytes scanned = %d, want %d", sample.BytesScanned, len(test.want))
			}
			if sample.Truncated != test.truncated {
				t.Fatalf("truncated = %t, want %t", sample.Truncated, test.truncated)
			}
			if cap(sample.Data) != len(sample.Data) {
				t.Fatalf("returned sample exposes look-ahead capacity: len=%d cap=%d", len(sample.Data), cap(sample.Data))
			}
		})
	}
}

func TestAllBoundedSampleAPIsUseExactBOMAwareTruncation(t *testing.T) {
	type callAPI func(io.Reader, int64) (bool, int64, error)
	apis := []struct {
		name string
		call callAPI
	}{
		{"inspect", func(r io.Reader, maxBytes int64) (bool, int64, error) {
			report, err := InspectReaderContext(context.Background(), r, InspectOptions{MaxBytes: maxBytes, MaxRows: 4})
			return report.TruncatedSample, report.BytesScanned, err
		}},
		{"preview", func(r io.Reader, maxBytes int64) (bool, int64, error) {
			report, err := PreviewRowsContext(context.Background(), r, PreviewOptions{HasHeader: true, MaxBytes: maxBytes, MaxRows: 4})
			return report.TruncatedSample, report.BytesScanned, err
		}},
		{"schema", func(r io.Reader, maxBytes int64) (bool, int64, error) {
			report, err := InferSchemaContext(context.Background(), r, SchemaOptions{HasHeader: true, MaxBytes: maxBytes, MaxRows: 4})
			return report.TruncatedSample, report.BytesScanned, err
		}},
		{"profile", func(r io.Reader, maxBytes int64) (bool, int64, error) {
			report, err := ProfileColumns(context.Background(), r, SchemaOptions{HasHeader: true, MaxBytes: maxBytes, MaxRows: 4})
			return report.TruncatedSample, -1, err
		}},
		{"column guide", func(r io.Reader, maxBytes int64) (bool, int64, error) {
			report, err := BuildColumnGuideContext(context.Background(), r, ColumnGuideOptions{HasHeader: true, MaxBytes: maxBytes, MaxRows: 4})
			return report.Schema.TruncatedSample, report.Schema.BytesScanned, err
		}},
		{"project preview", func(r io.Reader, maxBytes int64) (bool, int64, error) {
			report, err := PreviewProjectedColumnsContext(context.Background(), r, ProjectPreviewOptions{Columns: []int{0}, MaxBytes: maxBytes, MaxRows: 4})
			return report.TruncatedSample, report.BytesScanned, err
		}},
		{"SQL preview", func(r io.Reader, maxBytes int64) (bool, int64, error) {
			report, err := PreviewSQLConversionContext(context.Background(), r, SQLPreviewOptions{
				SQLConvertOptions: SQLConvertOptions{TableName: "records", HasHeader: true},
				MaxBytes:          maxBytes,
				MaxRows:           4,
			})
			return report.TruncatedSample, report.BytesScanned, err
		}},
	}

	for _, api := range apis {
		for _, test := range []struct {
			name      string
			payload   string
			truncated bool
		}{
			{name: "exact", payload: "c\n1\n"},
			{name: "over", payload: "c\n1\nX", truncated: true},
		} {
			t.Run(api.name+"/"+test.name, func(t *testing.T) {
				input := append(append([]byte{}, utf8BOM...), []byte(test.payload)...)
				truncated, bytesScanned, err := api.call(bytes.NewReader(input), 4)
				if err != nil {
					t.Fatal(err)
				}
				if truncated != test.truncated {
					t.Fatalf("truncated = %t, want %t", truncated, test.truncated)
				}
				if bytesScanned >= 0 && bytesScanned != 4 {
					t.Fatalf("bytes scanned = %d, want 4 payload bytes", bytesScanned)
				}
			})
		}
	}
}

func TestReadBoundedSampleRejectsUTF16BOMAcrossShortReads(t *testing.T) {
	for _, bom := range [][]byte{utf16LEBOM, utf16BEBOM} {
		reader := &sampleStepReader{data: append(append([]byte{}, bom...), 'x', 0), step: 1}
		_, err := readBoundedSample(context.Background(), reader, 16)
		if !errors.Is(err, ErrUTF16Input) {
			t.Fatalf("BOM % X error = %v, want ErrUTF16Input", bom, err)
		}
	}
}

func TestReadBoundedSampleHandlesShortAndEmptyReads(t *testing.T) {
	reader := &sampleStepReader{data: []byte("a,b\n"), step: 1, zeroReads: 5}
	sample, err := readBoundedSample(context.Background(), reader, 32)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(sample.Data); got != "a,b\n" {
		t.Fatalf("data = %q, want complete short-read input", got)
	}
	if sample.Truncated {
		t.Fatal("short source was reported as truncated")
	}

	stalled := &sampleStepReader{zeroReads: maxConsecutiveEmptyReads + 1}
	if _, err := readBoundedSample(context.Background(), stalled, 32); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("stalled reader error = %v, want io.ErrNoProgress", err)
	}
}

func TestReadBoundedSampleValidatesBeforeReadOrLargeAllocation(t *testing.T) {
	for _, limit := range []int64{-1, MaxSampleBytes + 1, math.MaxInt64} {
		reader := &sampleStepReader{data: []byte("must not be read")}
		if _, err := readBoundedSample(context.Background(), reader, limit); !errors.Is(err, ErrCSVSampleLimit) {
			t.Fatalf("limit %d error = %v, want ErrCSVSampleLimit", limit, err)
		}
		if reader.calls != 0 {
			t.Fatalf("limit %d performed %d reads before validation", limit, reader.calls)
		}
	}

	// A valid hard-limit request against a tiny source starts with only the BOM
	// probe. It does not allocate or request a MaxSampleBytes-sized buffer.
	reader := &sampleStepReader{}
	sample, err := readBoundedSample(context.Background(), reader, MaxSampleBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(sample.Data) != 0 || reader.maxRead > len(utf8BOM) {
		t.Fatalf("empty hard-limit sample = len %d, max read request %d", len(sample.Data), reader.maxRead)
	}
}

func TestReadBoundedSampleCancellationBeforeRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := &sampleStepReader{data: []byte("must not be read")}
	if _, err := readBoundedSample(ctx, reader, 32); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if reader.calls != 0 {
		t.Fatalf("canceled sample performed %d reads", reader.calls)
	}
}

type blockingSampleReader struct {
	started  chan struct{}
	release  chan struct{}
	returned chan struct{}
	once     sync.Once
}

func (r *blockingSampleReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	close(r.returned)
	return 0, io.EOF
}

func TestPreviewRowsCancellationReturnsWhileReaderIsBlocked(t *testing.T) {
	reader := &blockingSampleReader{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		returned: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := PreviewRowsContext(ctx, reader, PreviewOptions{MaxBytes: 32, MaxRows: 2})
		result <- err
	}()

	select {
	case <-reader.started:
	case <-time.After(2 * time.Second):
		t.Fatal("sample reader did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not release the blocked sample caller")
	}

	// The helper does not own arbitrary readers and therefore does not close
	// them. Release the deterministic test reader so the in-flight Read and its
	// worker can exit cleanly.
	close(reader.release)
	select {
	case <-reader.returned:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked reader did not exit after release")
	}
}

func TestAllBoundedSampleAPIsRejectInvalidLimitsBeforeRead(t *testing.T) {
	type callAPI func(context.Context, io.Reader, int64, int) error
	apis := []struct {
		name string
		call callAPI
	}{
		{"inspect", func(ctx context.Context, r io.Reader, maxBytes int64, maxRows int) error {
			_, err := InspectReaderContext(ctx, r, InspectOptions{MaxBytes: maxBytes, MaxRows: maxRows})
			return err
		}},
		{"preview", func(ctx context.Context, r io.Reader, maxBytes int64, maxRows int) error {
			_, err := PreviewRowsContext(ctx, r, PreviewOptions{MaxBytes: maxBytes, MaxRows: maxRows})
			return err
		}},
		{"schema", func(ctx context.Context, r io.Reader, maxBytes int64, maxRows int) error {
			_, err := InferSchemaContext(ctx, r, SchemaOptions{MaxBytes: maxBytes, MaxRows: maxRows})
			return err
		}},
		{"profile", func(ctx context.Context, r io.Reader, maxBytes int64, maxRows int) error {
			_, err := ProfileColumns(ctx, r, SchemaOptions{MaxBytes: maxBytes, MaxRows: maxRows})
			return err
		}},
		{"column guide", func(ctx context.Context, r io.Reader, maxBytes int64, maxRows int) error {
			_, err := BuildColumnGuideContext(ctx, r, ColumnGuideOptions{MaxBytes: maxBytes, MaxRows: maxRows})
			return err
		}},
		{"project preview", func(ctx context.Context, r io.Reader, maxBytes int64, maxRows int) error {
			_, err := PreviewProjectedColumnsContext(ctx, r, ProjectPreviewOptions{Columns: []int{0}, MaxBytes: maxBytes, MaxRows: maxRows})
			return err
		}},
		{"SQL preview", func(ctx context.Context, r io.Reader, maxBytes int64, maxRows int) error {
			_, err := PreviewSQLConversionContext(ctx, r, SQLPreviewOptions{
				SQLConvertOptions: SQLConvertOptions{TableName: "records"},
				MaxBytes:          maxBytes,
				MaxRows:           maxRows,
			})
			return err
		}},
	}
	limits := []struct {
		name     string
		maxBytes int64
		maxRows  int
	}{
		{name: "negative bytes", maxBytes: -1},
		{name: "bytes over hard limit", maxBytes: MaxSampleBytes + 1},
		{name: "MaxInt64 bytes", maxBytes: math.MaxInt64},
		{name: "negative rows", maxRows: -1},
		{name: "rows over hard limit", maxRows: MaxSampleRows + 1},
		{name: "MaxInt rows", maxRows: int(^uint(0) >> 1)},
	}

	for _, api := range apis {
		for _, limit := range limits {
			t.Run(api.name+"/"+limit.name, func(t *testing.T) {
				reader := &sampleStepReader{data: []byte("must not be read")}
				err := api.call(context.Background(), reader, limit.maxBytes, limit.maxRows)
				if !errors.Is(err, ErrCSVSampleLimit) {
					t.Fatalf("error = %v, want ErrCSVSampleLimit", err)
				}
				if reader.calls != 0 {
					t.Fatalf("invalid options performed %d reads", reader.calls)
				}
			})
		}
	}
}
