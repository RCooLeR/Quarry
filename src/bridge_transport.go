package main

import (
	"context"
	"net/http"

	"github.com/quarry/quarry-wails3/internal/bridgetransport"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// boundedWailsTransport leaves Wails' mature ordinary-request decoding,
// service dispatch, error marshalling, and response encoding intact. Quarry's
// Wails-independent guard owns all chunking and forwards a single bounded body,
// so the dependency's per-request limit is supplemented by Quarry's aggregate
// memory/concurrency limits and its chunk map remains unreachable.
type boundedWailsTransport struct {
	inner *application.HTTPTransport
	guard *bridgetransport.Guard
}

var _ application.Transport = (*boundedWailsTransport)(nil)
var _ application.TransportHTTPHandler = (*boundedWailsTransport)(nil)

func newBoundedWailsTransport() *boundedWailsTransport {
	return &boundedWailsTransport{
		inner: application.NewHTTPTransport(),
		guard: bridgetransport.New(),
	}
}

func (t *boundedWailsTransport) Start(ctx context.Context, processor *application.MessageProcessor) error {
	if err := t.inner.Start(ctx, processor); err != nil {
		return err
	}
	if err := t.guard.Start(ctx); err != nil {
		_ = t.inner.Stop()
		return err
	}
	return nil
}

func (t *boundedWailsTransport) JSClient() []byte { return t.inner.JSClient() }

func (t *boundedWailsTransport) Stop() error {
	t.guard.Stop()
	return t.inner.Stop()
}

func (t *boundedWailsTransport) Handler() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return t.guard.Handler(t.inner.Handler()(next))
	}
}
