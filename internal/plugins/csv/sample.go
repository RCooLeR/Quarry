package csv

import (
	"context"
	"io"
)

func readBoundedSample(ctx context.Context, r io.Reader, maxBytes int64) ([]byte, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
