// Package bridgetransport provides Quarry's Wails-independent HTTP bridge
// admission layer. It owns chunk retention and assembly, then delegates one
// bounded ordinary request to Wails for decoding, dispatch, and responses.
package bridgetransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	runtimePath = "/wails/runtime"

	chunkIDHeader    = "x-wails-chunk-id"
	chunkIndexHeader = "x-wails-chunk-index"
	chunkTotalHeader = "x-wails-chunk-total"
	clientIDHeader   = "x-wails-client-id"
	windowIDHeader   = "x-wails-window-id"
	windowNameHeader = "x-wails-window-name"
)

type limits struct {
	maxUnchunkedBody int64
	maxChunkBody     int64
	maxAssembled     int
	maxPendingBytes  int
	maxPendingIDs    int
	maxChunkTotal    int
	maxActive        int
	maxWorkingBytes  int64
	maxChunkID       int
	maxClientID      int
	maxWindowName    int
	maxRawQuery      int
	pendingTTL       time.Duration
	cleanupInterval  time.Duration
}

func productionLimits() limits {
	return limits{
		// Wails runtime beta.23 chunks JSON strings above 512 KiB. At or
		// below that threshold UTF-8 needs at most three bytes per UTF-16
		// code unit, so 2 MiB covers every conforming ordinary request.
		maxUnchunkedBody: 2 * 1024 * 1024,
		maxChunkBody:     1 * 1024 * 1024,
		maxAssembled:     64 * 1024 * 1024,
		maxPendingBytes:  64 * 1024 * 1024,
		maxPendingIDs:    32,
		maxChunkTotal:    1024,
		maxActive:        32,
		// This admission ledger includes retained chunks, three conservative copies
		// of every current read, and two copies of the final body (Quarry's
		// exact assembly plus Wails' ordinary-request buffer). It is expected-copy
		// accounting, not a measured or strictly enforced Go heap ceiling.
		maxWorkingBytes: 256 * 1024 * 1024,
		maxChunkID:      128,
		maxClientID:     128,
		maxWindowName:   256,
		maxRawQuery:     2 * 1024 * 1024,
		pendingTTL:      30 * time.Second,
		cleanupInterval: 5 * time.Second,
	}
}

type upload struct {
	total        int
	chunks       [][]byte
	received     int
	size         int
	lastActivity time.Time
	completing   bool
}

// Guard is an http.Handler admission layer. Start must be called before use.
type Guard struct {
	limits limits

	running     atomic.Bool
	lifeMu      sync.Mutex
	dispatchMu  sync.Mutex
	runCtx      context.Context
	cancel      context.CancelFunc
	everStarted bool

	mu                sync.Mutex
	pending           map[string]*upload
	pendingBytes      int
	transientReserved int64
	assemblyReserved  int64
	active            chan struct{}
	assembly          chan struct{}
}

func New() *Guard { return newGuard(productionLimits()) }

func newGuard(policy limits) *Guard {
	return &Guard{
		limits:   policy,
		pending:  make(map[string]*upload),
		active:   make(chan struct{}, policy.maxActive),
		assembly: make(chan struct{}, 1),
	}
}

func (g *Guard) Start(ctx context.Context) error {
	g.lifeMu.Lock()
	defer g.lifeMu.Unlock()
	if ctx == nil {
		return errors.New("bridge guard requires a non-nil context")
	}
	if g.everStarted {
		return errors.New("bridge guard cannot be started more than once")
	}
	g.everStarted = true
	g.runCtx, g.cancel = context.WithCancel(ctx)
	g.running.Store(true)
	go g.cleanupLoop(g.runCtx)
	return nil
}

func (g *Guard) Stop() {
	g.lifeMu.Lock()
	if !g.running.Load() {
		g.lifeMu.Unlock()
		return
	}
	g.dispatchMu.Lock()
	g.running.Store(false)
	g.dispatchMu.Unlock()
	cancel := g.cancel
	g.cancel = nil
	g.runCtx = nil
	g.lifeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	g.mu.Lock()
	clear(g.pending)
	g.pendingBytes = 0
	g.transientReserved = 0
	g.assemblyReserved = 0
	g.mu.Unlock()
}

// Handler protects /wails/runtime and passes every other route through.
func (g *Guard) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != runtimePath {
			next.ServeHTTP(w, r)
			return
		}
		g.handleRuntime(w, r, next)
	})
}

func (g *Guard) handleRuntime(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "runtime calls require POST", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel, ok := g.requestContext(r.Context())
	if !ok {
		http.Error(w, "runtime transport is not running", http.StatusServiceUnavailable)
		return
	}
	defer cancel()
	r = r.WithContext(ctx)

	select {
	case g.active <- struct{}{}:
		defer func() { <-g.active }()
	default:
		http.Error(w, "runtime request concurrency limit reached", http.StatusTooManyRequests)
		return
	}

	if len(r.URL.RawQuery) > g.limits.maxRawQuery ||
		!singleHeader(r.Header, clientIDHeader, g.limits.maxClientID) ||
		!singleHeader(r.Header, windowNameHeader, g.limits.maxWindowName) ||
		!singleHeader(r.Header, windowIDHeader, 32) {
		http.Error(w, "runtime routing metadata is invalid or too large", http.StatusRequestEntityTooLarge)
		return
	}

	chunkID := r.Header.Get(chunkIDHeader)
	if chunkID == "" {
		if r.Header.Get(chunkIndexHeader) != "" || r.Header.Get(chunkTotalHeader) != "" {
			http.Error(w, "chunk metadata requires a chunk ID", http.StatusUnprocessableEntity)
			return
		}
		g.handleOrdinary(w, r, next)
		return
	}
	g.handleChunk(w, r, next, chunkID)
}

func (g *Guard) handleOrdinary(w http.ResponseWriter, r *http.Request, next http.Handler) {
	release, ok := g.reserveTransient(3 * g.limits.maxUnchunkedBody)
	if !ok {
		http.Error(w, "runtime transport memory limit reached", http.StatusTooManyRequests)
		return
	}
	defer release()
	body, err := readBody(r, g.limits.maxUnchunkedBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if r.Context().Err() != nil || !g.running.Load() {
		http.Error(w, "runtime transport stopped before dispatch", http.StatusServiceUnavailable)
		return
	}
	if err := validateAllowedCall(body, r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	g.forward(w, requestWithBody(r, body), next)
}

func (g *Guard) handleChunk(w http.ResponseWriter, r *http.Request, next http.Handler, id string) {
	total, index, err := g.validateChunkHeaders(r, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := g.preflight(id, total); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	release, ok := g.reserveTransient(3 * g.limits.maxChunkBody)
	if !ok {
		http.Error(w, "runtime transport memory limit reached", http.StatusTooManyRequests)
		return
	}
	defer release()
	body, err := readBody(r, g.limits.maxChunkBody)
	if err != nil {
		// Preflight does not reserve ownership of an upload. Another handler
		// may have advanced it while this body was being read, so a local read
		// failure must leave shared state to its bounded TTL cleanup.
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if r.Context().Err() != nil || !g.running.Load() {
		http.Error(w, "runtime transport stopped before chunk admission", http.StatusServiceUnavailable)
		return
	}
	complete, err := g.store(id, total, index, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	if !complete {
		w.WriteHeader(http.StatusOK)
		return
	}

	select {
	case g.assembly <- struct{}{}:
		defer func() { <-g.assembly }()
	case <-r.Context().Done():
		g.discard(id)
		http.Error(w, "runtime call cancelled while waiting for assembly", http.StatusRequestTimeout)
		return
	}

	u, ok := g.claim(id)
	if !ok {
		http.Error(w, "completed runtime upload is unavailable", http.StatusUnprocessableEntity)
		return
	}
	defer g.finish(id, u)
	assembled := make([]byte, u.size)
	offset := 0
	for _, chunk := range u.chunks {
		offset += copy(assembled[offset:], chunk)
	}
	if err := validateAllowedCall(assembled, r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	forward := requestWithBody(r, assembled)
	forward.Header.Del(chunkIDHeader)
	forward.Header.Del(chunkIndexHeader)
	forward.Header.Del(chunkTotalHeader)
	g.forward(w, forward, next)
}

func (g *Guard) forward(w http.ResponseWriter, r *http.Request, next http.Handler) {
	// The gate serializes dispatch admission with Stop, but is deliberately
	// released before Wails runs the call. AppLifecycle.ApproveClose can invoke
	// application shutdown from inside an RPC, and native calls need not honor
	// context cancellation; draining here could therefore deadlock forever.
	g.dispatchMu.Lock()
	if !g.running.Load() || r.Context().Err() != nil {
		g.dispatchMu.Unlock()
		http.Error(w, "runtime transport stopped before dispatch", http.StatusServiceUnavailable)
		return
	}
	g.dispatchMu.Unlock()
	next.ServeHTTP(w, r)
}

func (g *Guard) requestContext(parent context.Context) (context.Context, context.CancelFunc, bool) {
	g.lifeMu.Lock()
	defer g.lifeMu.Unlock()
	if !g.running.Load() || g.runCtx == nil {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(g.runCtx, cancel)
	return ctx, func() { stop(); cancel() }, true
}

func (g *Guard) reserveTransient(bytes int64) (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.running.Load() {
		return nil, false
	}
	if int64(g.pendingBytes)+g.transientReserved+g.assemblyReserved+bytes > g.limits.maxWorkingBytes {
		return nil, false
	}
	g.transientReserved += bytes
	return func() {
		g.mu.Lock()
		g.transientReserved -= bytes
		if g.transientReserved < 0 {
			g.transientReserved = 0
		}
		g.mu.Unlock()
	}, true
}

func (g *Guard) validateChunkHeaders(r *http.Request, id string) (int, int, error) {
	if !validID(id, g.limits.maxChunkID) || len(r.Header.Values(chunkIDHeader)) != 1 ||
		len(r.Header.Values(chunkIndexHeader)) != 1 || len(r.Header.Values(chunkTotalHeader)) != 1 {
		return 0, 0, errors.New("invalid chunk ID or metadata headers")
	}
	total, err := strconv.Atoi(r.Header.Get(chunkTotalHeader))
	if err != nil || total <= 0 || total > g.limits.maxChunkTotal {
		return 0, 0, errors.New("invalid chunk total")
	}
	index, err := strconv.Atoi(r.Header.Get(chunkIndexHeader))
	if err != nil || index < 0 || index >= total {
		return 0, 0, errors.New("invalid chunk index")
	}
	return total, index, nil
}

func (g *Guard) preflight(id string, total int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.running.Load() {
		return errors.New("runtime transport is not running")
	}
	if u := g.pending[id]; u != nil {
		if u.completing {
			return errors.New("chunk upload is already complete")
		}
		if u.total != total {
			g.discardLocked(id, u)
			return errors.New("inconsistent chunk total")
		}
		return nil
	}
	if len(g.pending) >= g.limits.maxPendingIDs {
		return errors.New("pending chunk ID limit reached")
	}
	return nil
}

func (g *Guard) store(id string, total, index int, body []byte) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.running.Load() {
		return false, errors.New("runtime transport is not running")
	}
	u := g.pending[id]
	if u == nil {
		if len(g.pending) >= g.limits.maxPendingIDs {
			return false, errors.New("pending chunk ID limit reached")
		}
		u = &upload{total: total, chunks: make([][]byte, total), lastActivity: time.Now()}
		g.pending[id] = u
	}
	// Completion is immutable. A concurrent duplicate may have passed
	// preflight before another request supplied the final chunk; it must not
	// delete the body or its assembly reservation while the final handler is
	// waiting to claim it.
	if u.completing {
		return false, errors.New("chunk upload is already complete")
	}
	if u.total != total {
		g.discardLocked(id, u)
		return false, errors.New("invalid chunk upload state")
	}
	old := len(u.chunks[index])
	nextSize := u.size - old + len(body)
	nextPending := g.pendingBytes - old + len(body)
	if nextSize > g.limits.maxAssembled || nextPending > g.limits.maxPendingBytes {
		g.discardLocked(id, u)
		return false, errors.New("pending or assembled chunk byte limit reached")
	}
	if u.chunks[index] == nil {
		u.received++
	}
	u.chunks[index] = body
	u.size = nextSize
	u.lastActivity = time.Now()
	g.pendingBytes = nextPending
	if u.received != u.total {
		return false, nil
	}
	assemblyBytes := int64(2 * u.size)
	if int64(g.pendingBytes)+g.transientReserved+g.assemblyReserved+assemblyBytes > g.limits.maxWorkingBytes {
		g.discardLocked(id, u)
		return false, errors.New("runtime assembly memory limit reached")
	}
	u.completing = true
	g.assemblyReserved += assemblyBytes
	return true, nil
}

func (g *Guard) claim(id string) (*upload, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	u := g.pending[id]
	return u, u != nil && u.completing && u.received == u.total
}

func (g *Guard) finish(id string, u *upload) {
	g.mu.Lock()
	if g.pending[id] == u {
		g.discardLocked(id, u)
	}
	g.assemblyReserved -= int64(2 * u.size)
	if g.assemblyReserved < 0 {
		g.assemblyReserved = 0
	}
	g.mu.Unlock()
}

func (g *Guard) discard(id string) {
	g.mu.Lock()
	if u := g.pending[id]; u != nil {
		if u.completing {
			g.assemblyReserved -= int64(2 * u.size)
		}
		g.discardLocked(id, u)
	}
	g.mu.Unlock()
}

func (g *Guard) discardLocked(id string, u *upload) {
	delete(g.pending, id)
	g.pendingBytes -= u.size
	if g.pendingBytes < 0 {
		g.pendingBytes = 0
	}
}

func (g *Guard) cleanup(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, u := range g.pending {
		if !u.completing && now.Sub(u.lastActivity) > g.limits.pendingTTL {
			g.discardLocked(id, u)
		}
	}
}

func (g *Guard) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(g.limits.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			g.cleanup(now)
		}
	}
}

type callHeader struct {
	Object *int `json:"object"`
	Method *int `json:"method"`
}

func validateAllowedCall(body []byte, r *http.Request) error {
	var call callHeader
	if len(body) > 0 {
		if err := json.Unmarshal(body, &call); err != nil {
			return fmt.Errorf("invalid runtime request JSON: %w", err)
		}
	} else {
		query := r.URL.Query()
		if value := query.Get("object"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return errors.New("invalid runtime object")
			}
			call.Object = &parsed
		}
		if value := query.Get("method"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return errors.New("invalid runtime method")
			}
			call.Method = &parsed
		}
	}
	if call.Object == nil || call.Method == nil {
		return errors.New("runtime object and method are required")
	}
	// Quarry uses generated binding calls and their cancellation path only.
	if (*call.Object == 0 || *call.Object == 10) && *call.Method == 0 {
		return nil
	}
	return fmt.Errorf("runtime object %d method %d is not enabled by Quarry", *call.Object, *call.Method)
}

func requestWithBody(r *http.Request, body []byte) *http.Request {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	clone.Body = io.NopCloser(bytes.NewReader(body))
	clone.ContentLength = int64(len(body))
	return clone
}

func readBody(r *http.Request, max int64) ([]byte, error) {
	if r.ContentLength > max {
		return nil, fmt.Errorf("runtime request body exceeds %d bytes", max)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("read runtime request: %w", err)
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("runtime request body exceeds %d bytes", max)
	}
	return body, nil
}

func singleHeader(h http.Header, name string, max int) bool {
	values := h.Values(name)
	return len(values) <= 1 && (len(values) == 0 || len(values[0]) <= max)
}

func validID(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("_-", char) {
			continue
		}
		return false
	}
	return true
}
