package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSearchRequestIDsAreValidatedBeforeServiceOrMapAccess(t *testing.T) {
	var service *FileService
	invalid := strings.Repeat("x", 1024*1024)
	if _, err := service.FindNextRequest(invalid, "needle", 0, false, true, false); !errors.Is(err, ErrInvalidSearchRequestID) {
		t.Fatalf("FindNextRequest error = %v, want ErrInvalidSearchRequestID", err)
	}
	if _, err := service.SearchAllRequest(invalid, "needle", false, true, false, 10); !errors.Is(err, ErrInvalidSearchRequestID) {
		t.Fatalf("SearchAllRequest error = %v, want ErrInvalidSearchRequestID", err)
	}
	if _, err := service.CancelSearch(invalid); !errors.Is(err, ErrInvalidSearchRequestID) {
		t.Fatalf("CancelSearch error = %v, want ErrInvalidSearchRequestID", err)
	}
	if _, err := service.BeginSearchRequest("not-a-file-id"); !errors.Is(err, ErrInvalidFileID) {
		t.Fatalf("BeginSearchRequest error = %v, want ErrInvalidFileID", err)
	}
}

func TestUnknownSearchReservationsDoNotConsumeForegroundCapacity(t *testing.T) {
	service := NewFileService()
	attemptUnknown := func(fileID string) {
		t.Helper()
		requestID, err := service.BeginSearchRequest(fileID)
		if requestID != "" {
			t.Fatalf("unknown file %q returned request id %q", fileID, requestID)
		}
		if err == nil || !strings.Contains(err.Error(), "unknown file id") {
			t.Fatalf("unknown file %q error = %v, want unknown file id", fileID, err)
		}
		if errors.Is(err, ErrForegroundRunLimit) {
			t.Fatalf("unknown file %q consumed foreground capacity: %v", fileID, err)
		}
	}

	// Repeating one valid-shaped missing ID used to fill its per-file cap.
	for i := 0; i <= maxForegroundRunsPerFile; i++ {
		attemptUnknown("f999")
	}
	// Distinct valid-shaped missing IDs used to fill the global cap.
	for i := 0; i <= maxForegroundRunsTotal; i++ {
		attemptUnknown(fmt.Sprintf("f%d", 1000+i))
	}

	assertNoForegroundRuns(t, service)
	service.searchRequestMu.Lock()
	retained, sequence := len(service.searchRequests), service.searchRequestSeq
	service.searchRequestMu.Unlock()
	if retained != 0 || sequence != 0 {
		t.Fatalf("unknown reservations changed search ownership: retained=%d sequence=%d", retained, sequence)
	}

	path := writeTempFile(t, "search-after-unknown-reservations.txt", []byte("needle\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.CloseFile(meta.FileID); err != nil {
			t.Errorf("CloseFile: %v", err)
		}
	})
	requestID, err := service.BeginSearchRequest(meta.FileID)
	if err != nil || requestID != "search1" {
		t.Fatalf("real reservation after unknown attempts = %q, %v; want search1, nil", requestID, err)
	}
	if canceled, err := service.CancelSearch(requestID); err != nil || !canceled {
		t.Fatalf("cancel real reservation = %v, %v; want true, nil", canceled, err)
	}
	assertNoForegroundRuns(t, service)
}

func TestExportedSearchSurfaceRequiresStableRequestOwnership(t *testing.T) {
	serviceType := reflect.TypeOf((*FileService)(nil))
	allowed := map[string]struct{}{
		"BeginSearchRequest":   {},
		"CancelSearch":         {},
		"FindNextRequest":      {},
		"FindPrevRequest":      {},
		"SearchAllRequest":     {},
		"SearchAllPageRequest": {},
	}
	for i := 0; i < serviceType.NumMethod(); i++ {
		name := serviceType.Method(i).Name
		if !strings.Contains(name, "Search") && !strings.HasPrefix(name, "Find") {
			continue
		}
		if _, ok := allowed[name]; !ok {
			t.Fatalf("exported search method %s bypasses stable request ownership", name)
		}
		delete(allowed, name)
	}
	if len(allowed) != 0 {
		t.Fatalf("missing request-owned search methods: %v", allowed)
	}
}

func TestCancelReservedSearchIsExactAndNeverReused(t *testing.T) {
	service, meta := openSearchFixture(t, []byte("needle\n"))
	first, err := service.BeginSearchRequest(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if first != "search1" {
		t.Fatalf("first request id = %q, want search1", first)
	}
	canceled, err := service.CancelSearch(first)
	if err != nil || !canceled {
		t.Fatalf("first cancel = %v, %v; want true, nil", canceled, err)
	}
	if canceled, err = service.CancelSearch(first); err != nil || canceled {
		t.Fatalf("duplicate cancel = %v, %v; want false, nil", canceled, err)
	}
	assertNoForegroundRuns(t, service)

	second, err := service.BeginSearchRequest(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if second != "search2" || second == first {
		t.Fatalf("second request id = %q after %q", second, first)
	}
	if canceled, err = service.CancelSearch(first); err != nil || canceled {
		t.Fatalf("stale cancel = %v, %v; want false, nil", canceled, err)
	}
	result, err := service.SearchAllRequest(second, "needle", false, true, false, 10)
	if err != nil || len(result.Hits) != 1 {
		t.Fatalf("new request after stale cancel = %+v, %v", result, err)
	}
	assertNoForegroundRuns(t, service)
}

func TestSearchRequestSequenceStopsAtValidatedStringLimit(t *testing.T) {
	service, meta := openSearchFixture(t, []byte("needle\n"))
	service.searchRequestMu.Lock()
	service.searchRequestSeq = math.MaxInt64 - 1
	service.searchRequestMu.Unlock()

	lastID, err := service.BeginSearchRequest(meta.FileID)
	if err != nil || lastID != "search9223372036854775807" {
		t.Fatalf("last addressable search request = %q, err=%v", lastID, err)
	}
	if canceled, err := service.CancelSearch(lastID); err != nil || !canceled {
		t.Fatalf("cancel last addressable request = %v, err=%v", canceled, err)
	}

	if requestID, err := service.BeginSearchRequest(meta.FileID); requestID != "" || !errors.Is(err, ErrSearchRequestSequenceExhausted) {
		t.Fatalf("request after sequence exhaustion = %q, err=%v", requestID, err)
	}
	service.searchRequestMu.Lock()
	sequence, retained := service.searchRequestSeq, len(service.searchRequests)
	service.searchRequestMu.Unlock()
	if sequence != math.MaxInt64 || retained != 0 {
		t.Fatalf("search state after exhaustion: sequence=%d retained=%d", sequence, retained)
	}
	assertNoForegroundRuns(t, service)
}

func TestSearchAllPageRequestUsesOneReservationPerPage(t *testing.T) {
	service, meta := openSearchFixture(t, []byte("x-x-x"))
	firstID, err := service.BeginSearchRequest(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.SearchAllPageRequest(firstID, "x", false, true, false, 2, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Hits) != 2 || !first.Truncated || first.Continuation == nil {
		t.Fatalf("first page = %+v", first)
	}
	if _, err := service.SearchAllPageRequest(firstID, "x", false, true, false, 2, first.Continuation.StartOffset, false); !errors.Is(err, ErrSearchRequestNotFound) {
		t.Fatalf("reused page request error = %v, want ErrSearchRequestNotFound", err)
	}
	secondID, err := service.BeginSearchRequest(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.SearchAllPageRequest(secondID, "x", false, true, false, 2, first.Continuation.StartOffset, false)
	if err != nil || len(second.Hits) != 1 || second.Truncated {
		t.Fatalf("second page = %+v, %v", second, err)
	}
}

func TestCancelRunningSearchStopsExactRequestAndFinishesOnce(t *testing.T) {
	service, meta := openSearchFixture(t, []byte("needle\nother\n"))
	requestID, err := service.BeginSearchRequest(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := installBlockingLeaseHook(t, "search", meta.FileID)
	done := make(chan error, 1)
	go func() {
		_, err := service.FindNextRequest(requestID, "needle", 0, false, true, false)
		done <- err
	}()
	<-entered

	if _, err := service.FindPrevRequest(requestID, "needle", meta.Size, false, true, false); !errors.Is(err, ErrSearchRequestAlreadyStarted) {
		t.Fatalf("duplicate execution error = %v, want ErrSearchRequestAlreadyStarted", err)
	}
	canceled, err := service.CancelSearch(requestID)
	if err != nil || !canceled {
		t.Fatalf("cancel = %v, %v; want true, nil", canceled, err)
	}
	if canceled, err = service.CancelSearch(requestID); err != nil || canceled {
		t.Fatalf("duplicate running cancel = %v, %v; want false, nil", canceled, err)
	}
	release()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("running search error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled search did not return")
	}
	if canceled, err = service.CancelSearch(requestID); err != nil || canceled {
		t.Fatalf("post-completion cancel = %v, %v; want false, nil", canceled, err)
	}
	assertNoForegroundRuns(t, service)
}

func TestLifecycleCancellationDrainsUnclaimedSearchReservation(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "reserved-search-close.txt", []byte("needle\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := service.BeginSearchRequest(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- service.CloseFile(meta.FileID) }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not drain an unclaimed search reservation")
	}
	if canceled, err := service.CancelSearch(requestID); err != nil || canceled {
		t.Fatalf("cancel after lifecycle completion = %v, %v; want false, nil", canceled, err)
	}
	assertNoForegroundRuns(t, service)
}

func TestRefreshCancellationDrainsUnclaimedSearchReservation(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "reserved-search-refresh.txt", []byte("needle\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := service.BeginSearchRequest(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := service.RefreshFile(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.FileID != meta.FileID {
		t.Fatalf("refresh file id = %q, want %q", refreshed.FileID, meta.FileID)
	}
	if canceled, err := service.CancelSearch(requestID); err != nil || canceled {
		t.Fatalf("cancel after refresh completion = %v, %v; want false, nil", canceled, err)
	}
	assertNoForegroundRuns(t, service)
}

func TestShutdownDrainsSearchRequestMapAndRejectsNewReservations(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "reserved-search-shutdown.txt", []byte("needle\n"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := service.BeginSearchRequest(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}

	service.shutdown()
	service.searchRequestMu.Lock()
	retained := len(service.searchRequests)
	service.searchRequestMu.Unlock()
	if retained != 0 {
		t.Fatalf("shutdown retained %d search requests", retained)
	}
	if canceled, err := service.CancelSearch(requestID); err != nil || canceled {
		t.Fatalf("cancel after shutdown = %v, %v; want false, nil", canceled, err)
	}
	if _, err := service.BeginSearchRequest(meta.FileID); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("post-shutdown reservation error = %v, want ErrServiceStopped", err)
	}
}
