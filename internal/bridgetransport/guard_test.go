package bridgetransport

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func smallLimits() limits {
	return limits{
		maxUnchunkedBody: 128,
		maxChunkBody:     64,
		maxAssembled:     128,
		maxPendingBytes:  128,
		maxPendingIDs:    2,
		maxChunkTotal:    8,
		maxActive:        2,
		maxWorkingBytes:  1024,
		maxChunkID:       16,
		maxClientID:      16,
		maxWindowName:    16,
		maxRawQuery:      128,
		pendingTTL:       time.Second,
		cleanupInterval:  time.Hour,
	}
}

func startedGuard(t *testing.T, policy limits) *Guard {
	t.Helper()
	g := newGuard(policy)
	if err := g.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Stop)
	return g
}

func runtimeRequest(body []byte) *http.Request {
	return httptest.NewRequest(http.MethodPost, "https://wails.localhost/wails/runtime", bytes.NewReader(body))
}

func chunkRequest(id string, index, total int, body []byte) *http.Request {
	r := runtimeRequest(body)
	r.Header.Set(chunkIDHeader, id)
	r.Header.Set(chunkIndexHeader, strconvItoa(index))
	r.Header.Set(chunkTotalHeader, strconvItoa(total))
	return r
}

func strconvItoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for value > 0 {
		i--
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[i:])
}

func TestOrdinaryBodyIsBoundedAndAllowlistedBeforeDelegate(t *testing.T) {
	g := startedGuard(t, smallLimits())
	var calls atomic.Int32
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"object":0,"method":0,"args":["ok"]}` {
			t.Errorf("forwarded body = %q", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h := g.Handler(next)

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, runtimeRequest([]byte(`{"object":0,"method":0,"args":["ok"]}`)))
	if recorder.Code != http.StatusNoContent || calls.Load() != 1 {
		t.Fatalf("allowed status/calls = %d/%d", recorder.Code, calls.Load())
	}

	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, runtimeRequest([]byte(`{"object":5,"method":0}`)))
	if recorder.Code != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("disabled status/calls = %d/%d", recorder.Code, calls.Load())
	}

	oversized := runtimeRequest(bytes.Repeat([]byte{'x'}, int(smallLimits().maxUnchunkedBody+1)))
	oversized.ContentLength = -1 // exercise the streaming limit, not only the header check
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, oversized)
	if recorder.Code != http.StatusRequestEntityTooLarge || calls.Load() != 1 {
		t.Fatalf("oversized status/calls = %d/%d", recorder.Code, calls.Load())
	}
}

func TestChunkAssemblyIsExactAndNeverUsesDelegateChunkStore(t *testing.T) {
	g := startedGuard(t, smallLimits())
	payload := []byte(`{"object":0,"method":0,"args":["serialized"]}`)
	cut := 17
	var calls int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get(chunkIDHeader) != "" || r.Header.Get(chunkIndexHeader) != "" || r.Header.Get(chunkTotalHeader) != "" {
			t.Error("chunk headers reached Wails delegate")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, payload) {
			t.Errorf("body = %q, want %q", body, payload)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	h := g.Handler(next)

	first := httptest.NewRecorder()
	h.ServeHTTP(first, chunkRequest("upload_1", 1, 2, payload[cut:]))
	if first.Code != http.StatusOK || calls != 0 {
		t.Fatalf("partial status/calls = %d/%d", first.Code, calls)
	}
	last := httptest.NewRecorder()
	h.ServeHTTP(last, chunkRequest("upload_1", 0, 2, payload[:cut]))
	if last.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("final status/calls = %d/%d", last.Code, calls)
	}
	if len(g.pending) != 0 || g.pendingBytes != 0 || g.assemblyReserved != 0 || g.transientReserved != 0 {
		t.Fatalf("ledger leaked: ids=%d pending=%d assembly=%d transient=%d", len(g.pending), g.pendingBytes, g.assemblyReserved, g.transientReserved)
	}
}

func TestDuplicateChunkReplacesAccountingAndMalformedTotalsClearState(t *testing.T) {
	g := startedGuard(t, smallLimits())
	if complete, err := g.store("same", 2, 0, []byte("old")); err != nil || complete {
		t.Fatalf("first = %v, %v", complete, err)
	}
	if complete, err := g.store("same", 2, 0, []byte("replacement")); err != nil || complete {
		t.Fatalf("duplicate = %v, %v", complete, err)
	}
	if g.pendingBytes != len("replacement") || g.pending["same"].received != 1 || g.pending["same"].size != len("replacement") {
		t.Fatalf("duplicate accounting = bytes %d, received %d, size %d", g.pendingBytes, g.pending["same"].received, g.pending["same"].size)
	}
	if err := g.preflight("same", 3); err == nil {
		t.Fatal("inconsistent total accepted")
	}
	if len(g.pending) != 0 || g.pendingBytes != 0 {
		t.Fatalf("inconsistent upload leaked: %#v/%d", g.pending, g.pendingBytes)
	}
}

func TestCompletedUploadIsImmutableAgainstPreflightRace(t *testing.T) {
	g := startedGuard(t, smallLimits())
	if complete, err := g.store("race", 2, 0, []byte("left")); err != nil || complete {
		t.Fatalf("first chunk = %v, %v", complete, err)
	}
	// Model a duplicate handler that passes preflight, then stalls in its body
	// read while another handler supplies the final chunk.
	if err := g.preflight("race", 2); err != nil {
		t.Fatal(err)
	}
	if complete, err := g.store("race", 2, 1, []byte("right")); err != nil || !complete {
		t.Fatalf("final chunk = %v, %v", complete, err)
	}
	wantPending := len("leftright")
	wantAssembly := int64(2 * wantPending)
	if _, err := g.store("race", 2, 0, []byte("late")); err == nil {
		t.Fatal("late duplicate changed a completed upload")
	}
	if g.pending["race"] == nil || !g.pending["race"].completing || g.pendingBytes != wantPending || g.assemblyReserved != wantAssembly {
		t.Fatalf("completed upload changed: upload=%#v pending=%d assembly=%d", g.pending["race"], g.pendingBytes, g.assemblyReserved)
	}
	u, ok := g.claim("race")
	if !ok {
		t.Fatal("completed upload became unclaimable")
	}
	g.finish("race", u)
	if len(g.pending) != 0 || g.pendingBytes != 0 || g.assemblyReserved != 0 {
		t.Fatalf("finish leaked: ids=%d pending=%d assembly=%d", len(g.pending), g.pendingBytes, g.assemblyReserved)
	}
}

func TestPendingIDByteAndWorkingCapsFailWithoutLeaks(t *testing.T) {
	policy := smallLimits()
	policy.maxPendingIDs = 1
	policy.maxPendingBytes = 5
	g := startedGuard(t, policy)
	if _, err := g.store("one", 2, 0, []byte("1234")); err != nil {
		t.Fatal(err)
	}
	if err := g.preflight("two", 2); err == nil {
		t.Fatal("distinct ID cap accepted")
	}
	if _, err := g.store("one", 2, 1, []byte("67")); err == nil {
		t.Fatal("aggregate byte cap accepted")
	}
	if len(g.pending) != 0 || g.pendingBytes != 0 {
		t.Fatalf("failed upload leaked: %d/%d", len(g.pending), g.pendingBytes)
	}

	policy = smallLimits()
	policy.maxWorkingBytes = 10
	g2 := startedGuard(t, policy)
	recorder := httptest.NewRecorder()
	g2.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("delegate called") })).ServeHTTP(
		recorder, runtimeRequest([]byte(`{"object":0,"method":0}`)))
	if recorder.Code != http.StatusTooManyRequests || g2.transientReserved != 0 {
		t.Fatalf("working cap status/ledger = %d/%d", recorder.Code, g2.transientReserved)
	}
}

func TestTTLAndStopReleaseIncompleteUploads(t *testing.T) {
	g := startedGuard(t, smallLimits())
	if _, err := g.store("old", 2, 0, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	g.pending["old"].lastActivity = time.Unix(10, 0)
	g.cleanup(time.Unix(12, 0))
	if len(g.pending) != 0 || g.pendingBytes != 0 {
		t.Fatal("expired upload retained")
	}
	if _, err := g.store("stop", 2, 0, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	g.Stop()
	if len(g.pending) != 0 || g.pendingBytes != 0 || g.running.Load() {
		t.Fatal("stop retained bridge state")
	}
}

func TestNilStartContextFailsWithoutBurningLifecycle(t *testing.T) {
	g := newGuard(smallLimits())
	//lint:ignore SA1012 This is the boundary regression: Start must reject nil without panicking or consuming its lifecycle.
	if err := g.Start(nil); err == nil {
		t.Fatal("nil Start context accepted")
	}
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("valid Start after nil rejection: %v", err)
	}
	g.Stop()
}

func TestConcurrentRequestLimitRejectsBeforeDelegate(t *testing.T) {
	policy := smallLimits()
	policy.maxActive = 1
	g := startedGuard(t, policy)
	entered := make(chan struct{})
	release := make(chan struct{})
	h := g.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), runtimeRequest([]byte(`{"object":0,"method":0}`)))
		close(done)
	}()
	<-entered
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, runtimeRequest([]byte(`{"object":0,"method":0}`)))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("concurrent status = %d", recorder.Code)
	}
	close(release)
	<-done
}

type blockingReader struct {
	started chan struct{}
	release chan struct{}
	data    []byte
	done    bool
}

func (r *blockingReader) Read(buffer []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	close(r.started)
	<-r.release
	r.done = true
	n := copy(buffer, r.data)
	return n, io.EOF
}

func TestLateDuplicateReadFailureCannotDeleteSharedUploadState(t *testing.T) {
	for _, completed := range []bool{false, true} {
		for _, failure := range []string{"oversize", "cancelled"} {
			name := failure + "-incomplete"
			if completed {
				name = failure + "-complete"
			}
			t.Run(name, func(t *testing.T) {
				g := startedGuard(t, smallLimits())
				total := 3
				if completed {
					total = 2
				}
				if done, err := g.store("race", total, 0, []byte("left")); err != nil || done {
					t.Fatalf("first chunk = %v, %v", done, err)
				}
				data := []byte("late")
				if failure == "oversize" {
					data = bytes.Repeat([]byte{'x'}, int(g.limits.maxChunkBody+1))
				}
				reader := &blockingReader{started: make(chan struct{}), release: make(chan struct{}), data: data}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				request := httptest.NewRequest(http.MethodPost, "https://wails.localhost/wails/runtime", reader).WithContext(ctx)
				request.ContentLength = -1
				request.Header.Set(chunkIDHeader, "race")
				request.Header.Set(chunkIndexHeader, "0")
				request.Header.Set(chunkTotalHeader, strconvItoa(total))
				recorder := httptest.NewRecorder()
				done := make(chan struct{})
				var delegated atomic.Bool
				go func() {
					g.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { delegated.Store(true) })).ServeHTTP(recorder, request)
					close(done)
				}()
				<-reader.started // duplicate passed preflight and is inside readBody
				becameComplete, err := g.store("race", total, 1, []byte("right"))
				if err != nil || becameComplete != completed {
					t.Fatalf("concurrent progress = %v, %v", becameComplete, err)
				}
				if failure == "cancelled" {
					cancel()
				}
				close(reader.release)
				<-done
				if delegated.Load() {
					t.Fatal("failed duplicate reached delegate")
				}
				u := g.pending["race"]
				if u == nil || u.received != 2 || u.size != len("leftright") || u.completing != completed {
					t.Fatalf("shared upload changed: %#v", u)
				}
				if completed {
					claimed, ok := g.claim("race")
					if !ok {
						t.Fatal("completed upload became unclaimable")
					}
					g.finish("race", claimed)
				} else {
					g.discard("race")
				}
			})
		}
	}
}

func TestStopDuringBodyReadCannotRecreateStateOrDispatch(t *testing.T) {
	g := startedGuard(t, smallLimits())
	reader := &blockingReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
		data:    []byte(`{"object":0,"method":0}`),
	}
	request := httptest.NewRequest(http.MethodPost, "https://wails.localhost/wails/runtime", reader)
	request.ContentLength = -1
	var calls atomic.Int32
	h := g.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(recorder, request)
		close(done)
	}()
	<-reader.started
	g.Stop()
	close(reader.release)
	<-done
	if recorder.Code != http.StatusServiceUnavailable || calls.Load() != 0 {
		t.Fatalf("shutdown status/calls = %d/%d", recorder.Code, calls.Load())
	}
	if len(g.pending) != 0 || g.pendingBytes != 0 || g.transientReserved != 0 || g.assemblyReserved != 0 {
		t.Fatalf("shutdown leaked state: ids=%d pending=%d transient=%d assembly=%d", len(g.pending), g.pendingBytes, g.transientReserved, g.assemblyReserved)
	}
	if err := g.Start(context.Background()); err == nil {
		t.Fatal("stopped guard restarted and could mix lifecycle generations")
	}
}

func TestStopDoesNotDrainContextIgnoringDelegateAndClosesAdmission(t *testing.T) {
	g := startedGuard(t, smallLimits())
	entered := make(chan struct{})
	release := make(chan struct{})
	h := g.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release // model a native call that ignores request cancellation
		w.WriteHeader(http.StatusNoContent)
	}))
	firstDone := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), runtimeRequest([]byte(`{"object":0,"method":0}`)))
		close(firstDone)
	}()
	<-entered
	stopDone := make(chan struct{})
	go func() { g.Stop(); close(stopDone) }()
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop waited for a context-ignoring delegated call")
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, runtimeRequest([]byte(`{"object":0,"method":0}`)))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("post-stop request status = %d", recorder.Code)
	}
	close(release)
	<-firstDone
}

func TestProductionOrdinaryLimitCoversRuntimeThresholdUTF8Expansion(t *testing.T) {
	const runtimeChunkThresholdCodeUnits = 512 * 1024
	if productionLimits().maxUnchunkedBody < 3*runtimeChunkThresholdCodeUnits {
		t.Fatalf("ordinary cap %d does not cover worst-case UTF-8 expansion", productionLimits().maxUnchunkedBody)
	}
}

func TestFrontendRuntimeSurfaceMatchesObjectAllowlist(t *testing.T) {
	root := filepath.Join("..", "..", "frontend")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == "node_modules" || entry.Name() == "dist") {
			return filepath.SkipDir
		}
		if entry.IsDir() || (!strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".tsx")) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		if strings.Contains(text, "Events.Emit(") {
			t.Errorf("%s adds Events.Emit; update the bridge object allowlist intentionally", path)
		}
		for _, line := range strings.Split(text, "\n") {
			if !strings.Contains(line, `from "@wailsio/runtime"`) && !strings.Contains(line, `from '@wailsio/runtime'`) {
				continue
			}
			open, close := strings.IndexByte(line, '{'), strings.IndexByte(line, '}')
			if open < 0 || close <= open {
				t.Errorf("%s uses a non-named Wails runtime import; update the bridge allowlist intentionally", path)
				continue
			}
			for _, imported := range strings.Split(line[open+1:close], ",") {
				name := strings.TrimSpace(strings.SplitN(strings.TrimSpace(imported), " as ", 2)[0])
				switch name {
				case "Events", "Call", "CancellablePromise", "Create":
				default:
					t.Errorf("%s imports disabled Wails runtime surface %s", path, name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
