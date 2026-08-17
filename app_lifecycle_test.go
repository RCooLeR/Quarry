package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAppLifecycleNativeCloseIsFailClosedAndDeduplicated(t *testing.T) {
	lifecycle := newAppLifecycle()
	var notifications atomic.Int32
	lifecycle.bind(func() {}, func() { notifications.Add(1) })

	if lifecycle.interceptClose() {
		t.Fatal("first native close was allowed without frontend approval")
	}
	if lifecycle.interceptClose() {
		t.Fatal("duplicate native close was allowed while a decision was pending")
	}
	if got := notifications.Load(); got != 1 {
		t.Fatalf("duplicate close emitted %d requests, want 1", got)
	}

	if err := lifecycle.CancelClose(); err != nil {
		t.Fatalf("CancelClose: %v", err)
	}
	if lifecycle.interceptClose() {
		t.Fatal("close after cancellation was allowed without a new approval")
	}
	if got := notifications.Load(); got != 2 {
		t.Fatalf("new close after cancellation emitted %d total requests, want 2", got)
	}
}

func TestAppLifecycleApprovalArmsExactlyOneShutdown(t *testing.T) {
	lifecycle := newAppLifecycle()
	var quits atomic.Int32
	lifecycle.bind(func() { quits.Add(1) }, func() {})

	if err := lifecycle.ApproveClose(); !errors.Is(err, errNoCloseRequest) {
		t.Fatalf("ApproveClose without request error = %v, want %v", err, errNoCloseRequest)
	}
	if lifecycle.interceptClose() {
		t.Fatal("unapproved close was allowed")
	}
	if err := lifecycle.ApproveClose(); err != nil {
		t.Fatalf("ApproveClose: %v", err)
	}
	if got := quits.Load(); got != 1 {
		t.Fatalf("quit calls = %d, want 1", got)
	}
	if err := lifecycle.ApproveClose(); !errors.Is(err, errCloseInProgress) {
		t.Fatalf("duplicate ApproveClose error = %v, want %v", err, errCloseInProgress)
	}
	if !lifecycle.interceptClose() {
		t.Fatal("armed close was rejected")
	}
	if err := lifecycle.CancelClose(); !errors.Is(err, errCloseInProgress) {
		t.Fatalf("CancelClose after approval error = %v, want %v", err, errCloseInProgress)
	}
	if !lifecycle.interceptClose() {
		t.Fatal("shutdown-in-progress close was rejected")
	}
}

func TestAppLifecycleApplicationQuitUsesSameGuard(t *testing.T) {
	lifecycle := newAppLifecycle()
	var notifications atomic.Int32
	var quits atomic.Int32
	lifecycle.bind(func() { quits.Add(1) }, func() { notifications.Add(1) })

	if lifecycle.interceptClose() {
		t.Fatal("application quit was allowed without frontend approval")
	}
	if got := notifications.Load(); got != 1 {
		t.Fatalf("notifications = %d, want 1", got)
	}
	if err := lifecycle.ApproveClose(); err != nil {
		t.Fatalf("ApproveClose: %v", err)
	}
	if got := quits.Load(); got != 1 {
		t.Fatalf("quit calls = %d, want 1", got)
	}
	if !lifecycle.interceptClose() {
		t.Fatal("approved application quit was rejected")
	}
}

func TestAppLifecycleConcurrentApprovalCannotBypassOneShotGuard(t *testing.T) {
	lifecycle := newAppLifecycle()
	var quits atomic.Int32
	lifecycle.bind(func() { quits.Add(1) }, func() {})
	if lifecycle.interceptClose() {
		t.Fatal("unapproved close was allowed")
	}

	const callers = 16
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lifecycle.ApproveClose() == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := successes.Load(); got != 1 {
		t.Fatalf("successful approvals = %d, want 1", got)
	}
	if got := quits.Load(); got != 1 {
		t.Fatalf("quit calls = %d, want 1", got)
	}
	if !lifecycle.interceptClose() {
		t.Fatal("the single approved close was rejected")
	}
}

func TestAppLifecycleRunsServiceShutdownOnlyAfterApprovedClose(t *testing.T) {
	lifecycle := newAppLifecycle()
	var shutdowns atomic.Int32
	lifecycle.bind(func() {}, func() {})
	lifecycle.bindShutdown(func() { shutdowns.Add(1) })

	if lifecycle.interceptClose() {
		t.Fatal("unapproved close was allowed")
	}
	if got := shutdowns.Load(); got != 0 {
		t.Fatalf("shutdown ran before approval: %d", got)
	}
	if err := lifecycle.CancelClose(); err != nil {
		t.Fatal(err)
	}
	if got := shutdowns.Load(); got != 0 {
		t.Fatalf("shutdown ran for a canceled close: %d", got)
	}

	if lifecycle.interceptClose() {
		t.Fatal("second unapproved close was allowed")
	}
	if err := lifecycle.ApproveClose(); err != nil {
		t.Fatal(err)
	}
	if !lifecycle.interceptClose() {
		t.Fatal("approved close was rejected")
	}
	if !lifecycle.interceptClose() {
		t.Fatal("in-progress close was rejected")
	}
	if got := shutdowns.Load(); got != 1 {
		t.Fatalf("shutdown calls = %d, want exactly 1", got)
	}
}
