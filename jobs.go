package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/quarry/quarry-wails3/internal/fileio"
	"github.com/wailsapp/wails/v3/pkg/application"
)

var (
	ErrJobAlreadyRunning    = errors.New("another background job is already running")
	ErrJobManagerStopping   = errors.New("background jobs are shutting down")
	ErrNoActiveJob          = errors.New("no background job is active")
	ErrJobIDMismatch        = errors.New("background job id does not match the active job")
	ErrJobAlreadyCommitted  = errors.New("background job has already committed its result")
	ErrJobCancelled         = errors.New("background job was cancelled")
	ErrJobSequenceExhausted = errors.New("background job id sequence is exhausted")
)

const (
	jobKindTransform          = "transform"
	jobKindSQLAnalysis        = "sql-analysis"
	jobKindSourceVerification = "source-verification"

	// JavaScript represents event numbers exactly only through 2^53-1. Clamp
	// progress payloads at that boundary rather than silently emitting rounded
	// counters to the frontend.
	maxJobProgressValue = int64(1<<53 - 1)
	maxJobNoteBytes     = 160
)

type jobSpec struct {
	Title  string
	Kind   string
	FileID string
	Total  int64
}

type activeJob struct {
	id              string
	sequence        int64
	spec            jobSpec
	ctx             context.Context
	cancel          context.CancelFunc
	completed       int64
	total           int64
	note            string
	cancelRequested bool
	committed       bool
	done            chan struct{}
	doneOnce        sync.Once
}

// jobManager owns the single service-wide long-running operation. Keeping one
// authoritative slot prevents a second scan from replacing the first scan's
// cancellation handle. Job IDs are part of cancellation requests so a delayed
// frontend click can never cancel a newer operation.
type jobManager struct {
	mu       sync.Mutex
	eventMu  sync.Mutex
	seq      int64
	active   *activeJob
	stopping bool
	emit     func(string, any)

	// afterOutputPublication is an instance-local deterministic test seam. It
	// runs under mu after the platform publisher returns but before committed is
	// recorded, exposing the exact race interval without weakening production
	// publication or relying on scheduler timing.
	afterOutputPublication func(string, error)
}

func (s *FileService) jobs() *jobManager {
	s.jobOnce.Do(func() {
		s.jobMgr = &jobManager{emit: emitEvent}
	})
	return s.jobMgr
}

func emitEvent(name string, data any) {
	app := application.Get()
	if app == nil {
		return
	}
	app.Event.EmitEvent(&application.CustomEvent{Name: name, Data: data})
}

func clampJobProgress(value int64) int64 {
	switch {
	case value < 0:
		return 0
	case value > maxJobProgressValue:
		return maxJobProgressValue
	default:
		return value
	}
}

func boundedJobNote(note string) string {
	if len(note) > maxJobNoteBytes {
		note = note[:maxJobNoteBytes]
	}
	return strings.ToValidUTF8(note, "")
}

func normalizeJobSpec(spec jobSpec) jobSpec {
	spec.Title = strings.TrimSpace(spec.Title)
	if spec.Title == "" {
		spec.Title = "Working"
	}
	spec.Kind = strings.TrimSpace(spec.Kind)
	if spec.Kind == "" {
		spec.Kind = jobKindTransform
	}
	spec.Total = clampJobProgress(spec.Total)
	return spec
}

func (m *jobManager) begin(spec jobSpec) (*activeJob, error) {
	spec = normalizeJobSpec(spec)
	ctx, cancel := context.WithCancel(context.Background())

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		cancel()
		return nil, ErrJobManagerStopping
	}
	if m.active != nil {
		cancel()
		return nil, fmt.Errorf("%w (%s)", ErrJobAlreadyRunning, m.active.spec.Title)
	}
	// Job IDs cross the RPC/event boundary together with their numeric sequence.
	// Stop at JavaScript's exact-integer ceiling so the ID string and event
	// sequence can never disagree after JSON decoding in the frontend.
	if m.seq >= maxJobProgressValue {
		cancel()
		return nil, ErrJobSequenceExhausted
	}
	m.seq++
	job := &activeJob{
		id:       fmt.Sprintf("job%d", m.seq),
		sequence: m.seq,
		spec:     spec,
		ctx:      ctx,
		cancel:   cancel,
		total:    spec.Total,
		done:     make(chan struct{}),
	}
	m.active = job
	return job, nil
}

func emitJobEventSafely(emit func(string, any), name string, data any) {
	if emit == nil {
		return
	}
	// A frontend event transport failure must not strand the active-job slot or
	// turn a completed file operation into an application panic.
	defer func() { _ = recover() }()
	emit(name, data)
}

func jobEventData(job *activeJob) map[string]any {
	return map[string]any{
		"id":        job.id,
		"sequence":  job.sequence,
		"title":     job.spec.Title,
		"kind":      job.spec.Kind,
		"fileId":    job.spec.FileID,
		"completed": job.completed,
		"total":     job.total,
		// Keep records during the event-schema transition for existing clients.
		"records": job.completed,
		"note":    job.note,
	}
}

func (m *jobManager) startEvent(job *activeJob) {
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	m.mu.Lock()
	emit := m.emit
	payload := jobEventData(job)
	m.mu.Unlock()
	emitJobEventSafely(emit, "quarry:job-start", payload)
}

func (m *jobManager) report(jobID string, completed, total int64, note string) {
	completed = clampJobProgress(completed)
	total = clampJobProgress(total)
	note = boundedJobNote(note)

	// Progress and terminal delivery share eventMu so even a producer that
	// reports from a helper goroutine cannot emit after job-end.
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	m.mu.Lock()
	job := m.active
	if job == nil || job.id != jobID || job.cancelRequested || job.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	if total > 0 {
		if job.total == 0 {
			job.total = total
		} else if total < job.total {
			// Never make a determinate progress bar move backwards because a
			// producer reported an inconsistent total.
			total = job.total
		} else {
			job.total = total
		}
	}
	if completed < job.completed {
		completed = job.completed
	}
	if job.total > 0 && completed > job.total {
		completed = job.total
	}
	if completed == job.completed && note == job.note && total == 0 {
		m.mu.Unlock()
		return
	}
	job.completed = completed
	job.note = note
	payload := jobEventData(job)
	emit := m.emit
	m.mu.Unlock()
	emitJobEventSafely(emit, "quarry:job-progress", payload)
}

func (m *jobManager) finish(job *activeJob, status string) {
	defer job.doneOnce.Do(func() { close(job.done) })
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	job.cancel()

	m.mu.Lock()
	if m.active == job {
		m.active = nil
	}
	payload := jobEventData(job)
	payload["status"] = status
	emit := m.emit
	m.mu.Unlock()
	emitJobEventSafely(emit, "quarry:job-end", payload)
}

type serviceJobContextKey struct{}

func serviceJobID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(serviceJobContextKey{}).(string)
	return id, ok && id != ""
}

// commit publishes a result while holding the same ownership lock used by
// CancelJob. Whichever wins that lock defines the outcome: cancellation first
// prevents publication; publication first makes a later user cancel stale.
func (m *jobManager) commit(jobID string, publish func() bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.active
	if job == nil || job.id != jobID || job.cancelRequested || job.ctx.Err() != nil {
		return false
	}
	if !publish() {
		return false
	}
	job.committed = true
	return true
}

// publishOutput enters the final AtomicOutput publication point while holding
// the same ownership lock used by user, lifecycle, and shutdown cancellation.
// A successful publication, or PublicationError evidence that an artifact may
// already exist, makes cancellation stale for the remainder of the job. This
// is deliberately sticky across multi-output jobs: after the first irreversible
// output, callers must receive the operation's success/partial-failure result,
// never a misleading clean-cancellation result.
func (m *jobManager) publishOutput(jobID string, publish func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.active
	if job == nil || job.id != jobID || job.cancelRequested || job.ctx.Err() != nil {
		return errors.Join(ErrJobCancelled, context.Canceled)
	}
	err := publish()
	if m.afterOutputPublication != nil {
		m.afterOutputPublication(jobID, err)
	}
	var publication *fileio.PublicationError
	if err == nil || errors.As(err, &publication) {
		job.committed = true
	}
	return err
}

func (m *jobManager) cancelID(jobID string) error {
	m.mu.Lock()
	job := m.active
	if job == nil {
		m.mu.Unlock()
		return ErrNoActiveJob
	}
	if job.id != jobID {
		m.mu.Unlock()
		return fmt.Errorf("%w: got %q", ErrJobIDMismatch, jobID)
	}
	if job.committed {
		m.mu.Unlock()
		return ErrJobAlreadyCommitted
	}
	job.cancelRequested = true
	cancel := job.cancel
	m.mu.Unlock()
	cancel()
	return nil
}

// cancelFile is used by source lifecycle transitions. Once any irreversible
// result is committed, lifecycle cancellation becomes stale just like a user
// cancel; the transition waits for the job's lease and receives no false
// assurance that all output was removed.
func (m *jobManager) cancelFile(fileID string) bool {
	m.mu.Lock()
	job := m.active
	if job == nil || job.spec.FileID == "" || job.spec.FileID != fileID {
		m.mu.Unlock()
		return false
	}
	if job.committed {
		m.mu.Unlock()
		return false
	}
	job.cancelRequested = true
	cancel := job.cancel
	m.mu.Unlock()
	cancel()
	return true
}

func (m *jobManager) shutdown() <-chan struct{} {
	m.mu.Lock()
	m.stopping = true
	job := m.active
	shouldCancel := job != nil && !job.committed
	if shouldCancel {
		job.cancelRequested = true
	}
	m.mu.Unlock()
	if shouldCancel {
		job.cancel()
	}
	if job != nil {
		return job.done
	}
	done := make(chan struct{})
	close(done)
	return done
}

func jobStatus(err error, panicked bool) string {
	switch {
	case panicked:
		return "panicked"
	case errors.Is(err, context.Canceled), errors.Is(err, ErrJobCancelled):
		return "cancelled"
	case err != nil:
		return "failed"
	default:
		return "completed"
	}
}

// runServiceJob provides one panic-safe lifecycle for every long service
// operation. The result type stays internal to Go; only bounded progress events
// and an opaque job ID cross the frontend bridge.
func runServiceJob[T any](s *FileService, spec jobSpec, fn func(context.Context, func(int64, int64, string)) (T, error)) (result T, retErr error) {
	// A file-less service job is valid. When a job is source-bound, require the
	// exact generated ID grammar before dereferencing the service, allocating a
	// job, or emitting any event. Never normalize an opaque identifier.
	if spec.FileID != "" {
		if err := validateRPCFileID(spec.FileID); err != nil {
			return result, err
		}
	}
	if err := validateRPCEnum(spec.Kind); err != nil {
		return result, err
	}
	if err := s.ensureServiceRunning(); err != nil {
		return result, err
	}
	job, err := s.jobs().begin(spec)
	if err != nil {
		return result, err
	}
	ctx := context.WithValue(job.ctx, serviceJobContextKey{}, job.id)
	ctx = fileio.WithPublicationBoundary(ctx, func(publish func() error) error {
		return s.jobs().publishOutput(job.id, publish)
	})
	panicked := false
	defer func() {
		if recovered := recover(); recovered != nil {
			panicked = true
			var zero T
			result = zero
			retErr = fmt.Errorf("background job %q panicked: %v", job.spec.Title, recovered)
		}
		if errors.Is(retErr, context.Canceled) {
			var zero T
			result = zero
			retErr = errors.Join(ErrJobCancelled, context.Canceled)
		}
		s.jobs().finish(job, jobStatus(retErr, panicked))
	}()

	s.jobs().startEvent(job)
	return fn(ctx, func(completed, total int64, note string) {
		s.jobs().report(job.id, completed, total, note)
	})
}

// withFileJob binds cancellation and terminal ownership to one open file ID.
// The operation callback is responsible for acquiring its document lease after
// the job slot is established; CloseFile marks closing, cancels this job, and
// then waits for that lease to drain.
func (s *FileService) withFileJob(fileID string, title string, fn func(context.Context, func(int64, string)) (TransformResult, error)) (TransformResult, error) {
	return runServiceJob(s, jobSpec{Title: title, Kind: jobKindTransform, FileID: fileID}, func(ctx context.Context, progress func(int64, int64, string)) (TransformResult, error) {
		return fn(ctx, func(records int64, note string) {
			progress(records, 0, note)
		})
	})
}

// CancelJob cancels exactly the active job represented by jobID. A stale ID is
// rejected rather than being allowed to cancel a newer scan.
func (s *FileService) CancelJob(jobID string) error {
	if err := validateRPCJobID(jobID); err != nil {
		return err
	}
	return s.jobs().cancelID(jobID)
}

func (s *FileService) cancelJobForFile(fileID string) {
	s.jobs().cancelFile(fileID)
}

// shutdown cancels both registered UI jobs and direct in-flight SQL analysis
// runs. It is intentionally unexported so it is not added to the RPC surface.
func (s *FileService) shutdown() {
	leader, shutdownDone := s.stopServiceAdmission()
	if !leader {
		<-shutdownDone
		return
	}
	defer s.finishServiceShutdown()

	jobDone := s.jobs().shutdown()
	s.stopSearchRequests()
	foregroundDone := s.stopForegroundRuns()

	s.sqlMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.sqlRuns))
	for fileID, run := range s.sqlRuns {
		delete(s.sqlRuns, fileID)
		if run.Cancel != nil {
			cancels = append(cancels, run.Cancel)
		}
	}
	clear(s.sqlSummary)
	s.sqlMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	registryDone := s.reg.BeginShutdown()

	// Cooperative jobs and foreground scans normally unwind immediately after
	// cancellation. Wait only to give their deferred cleanup/event paths a
	// chance to finish; an uninterruptible OS call must never hang application
	// shutdown indefinitely.
	done := make([]<-chan struct{}, 0, 2+len(foregroundDone))
	done = append(done, jobDone)
	done = append(done, foregroundDone...)
	done = append(done, registryDone)
	waitForServiceCleanup(done, 2*time.Second)
}

func waitForServiceCleanup(done []<-chan struct{}, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for _, signal := range done {
		if signal == nil {
			continue
		}
		select {
		case <-signal:
		case <-timer.C:
			return
		}
	}
}
