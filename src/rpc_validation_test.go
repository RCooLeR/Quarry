package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGeneratedRPCIDValidationIsExactAndBounded(t *testing.T) {
	t.Parallel()

	validFileIDs := []string{"f1", "f9", "f10", "f9223372036854775807"}
	for _, value := range validFileIDs {
		if err := validateRPCFileID(value); err != nil {
			t.Errorf("validateRPCFileID(%q) = %v", value, err)
		}
	}
	validJobIDs := []string{"job1", "job9", "job10", "job9223372036854775807"}
	for _, value := range validJobIDs {
		if err := validateRPCJobID(value); err != nil {
			t.Errorf("validateRPCJobID(%q) = %v", value, err)
		}
	}

	invalidFileIDs := []string{
		"", "f", "f0", "f01", "F1", " f1", "f1 ", "file1", "f-1", "f+1",
		"f1.0", "f\u0661", "f9223372036854775808", "f" + strings.Repeat("9", maxRPCOpaqueSequenceDigits+1),
	}
	for _, value := range invalidFileIDs {
		if err := validateRPCFileID(value); !errors.Is(err, ErrInvalidFileID) {
			t.Errorf("validateRPCFileID(%q) = %v, want ErrInvalidFileID", value, err)
		}
	}
	invalidJobIDs := []string{
		"", "job", "job0", "job01", "Job1", " job1", "job1 ", "job-1", "job+1",
		"job1.0", "job\u0661", "job9223372036854775808", "job" + strings.Repeat("9", maxRPCOpaqueSequenceDigits+1),
	}
	for _, value := range invalidJobIDs {
		if err := validateRPCJobID(value); !errors.Is(err, ErrInvalidJobID) {
			t.Errorf("validateRPCJobID(%q) = %v, want ErrInvalidJobID", value, err)
		}
	}
}

func TestRPCByteLimitsUseStaticErrorsWithoutReflectingInput(t *testing.T) {
	t.Parallel()

	if err := validateRPCPath(strings.Repeat("p", maxRPCPathBytes)); err != nil {
		t.Fatalf("path at limit: %v", err)
	}
	oversizedPath := strings.Repeat("p", maxRPCPathBytes+1)
	if err := validateRPCPath(oversizedPath); !errors.Is(err, ErrRPCPathTooLong) || strings.Contains(err.Error(), oversizedPath) {
		t.Fatalf("oversized path error = %v", err)
	}
	if err := validateRPCEnum(strings.Repeat("m", maxRPCEnumBytes)); err != nil {
		t.Fatalf("enum at limit: %v", err)
	}
	oversizedEnum := strings.Repeat("m", maxRPCEnumBytes+1)
	if err := validateRPCEnum(oversizedEnum); !errors.Is(err, ErrRPCValueTooLong) || strings.Contains(err.Error(), oversizedEnum) {
		t.Fatalf("oversized enum error = %v", err)
	}
}

func TestInvalidRPCInputsFailBeforeNilServiceDereference(t *testing.T) {
	t.Parallel()

	var service *FileService
	invalidFileID := "f" + strings.Repeat("9", maxRPCOpaqueSequenceDigits+1)
	invalidJobID := "job" + strings.Repeat("9", maxRPCOpaqueSequenceDigits+1)
	oversizedPath := strings.Repeat("p", maxRPCPathBytes+1)

	if _, _, err := service.acquireReadFile(invalidFileID); !errors.Is(err, ErrInvalidFileID) {
		t.Fatalf("acquireReadFile error = %v, want ErrInvalidFileID", err)
	}
	if _, err := service.FileSize(invalidFileID); !errors.Is(err, ErrInvalidFileID) {
		t.Fatalf("FileSize error = %v, want ErrInvalidFileID", err)
	}
	if _, _, err := service.acquireExclusiveFile(invalidFileID); !errors.Is(err, ErrInvalidFileID) {
		t.Fatalf("acquireExclusiveFile error = %v, want ErrInvalidFileID", err)
	}
	if _, _, err := service.beginForegroundRun(context.Background(), invalidFileID); !errors.Is(err, ErrInvalidFileID) {
		t.Fatalf("beginForegroundRun error = %v, want ErrInvalidFileID", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := service.beginForegroundRun(canceled, invalidFileID); !errors.Is(err, ErrInvalidFileID) {
		t.Fatalf("beginForegroundRun with canceled parent error = %v, want ErrInvalidFileID first", err)
	}
	if err := service.CloseFile(invalidFileID); !errors.Is(err, ErrInvalidFileID) {
		t.Fatalf("CloseFile error = %v, want ErrInvalidFileID", err)
	}
	if _, err := service.RefreshFile(invalidFileID); !errors.Is(err, ErrInvalidFileID) {
		t.Fatalf("RefreshFile error = %v, want ErrInvalidFileID", err)
	}
	if err := service.CancelJob(invalidJobID); !errors.Is(err, ErrInvalidJobID) {
		t.Fatalf("CancelJob error = %v, want ErrInvalidJobID", err)
	}
	if _, err := service.OpenFile(oversizedPath); !errors.Is(err, ErrRPCPathTooLong) {
		t.Fatalf("OpenFile error = %v, want ErrRPCPathTooLong", err)
	}
	if _, err := service.InspectInPlaceRecovery(oversizedPath); !errors.Is(err, ErrRPCPathTooLong) {
		t.Fatalf("InspectInPlaceRecovery error = %v, want ErrRPCPathTooLong", err)
	}
	if _, err := runServiceJob(service, jobSpec{FileID: invalidFileID}, func(context.Context, func(int64, int64, string)) (int, error) {
		t.Fatal("invalid source-bound job callback ran")
		return 0, nil
	}); !errors.Is(err, ErrInvalidFileID) {
		t.Fatalf("runServiceJob error = %v, want ErrInvalidFileID", err)
	}
	if _, err := runServiceJob(service, jobSpec{Kind: strings.Repeat("k", maxRPCEnumBytes+1)}, func(context.Context, func(int64, int64, string)) (int, error) {
		t.Fatal("oversized job-kind callback ran")
		return 0, nil
	}); !errors.Is(err, ErrRPCValueTooLong) {
		t.Fatalf("runServiceJob oversized kind error = %v, want ErrRPCValueTooLong", err)
	}
}

func TestInvalidCancelJobDoesNotAllocateManager(t *testing.T) {
	t.Parallel()

	service := NewFileService()
	if service.jobMgr != nil {
		t.Fatal("new service unexpectedly has a job manager")
	}
	if err := service.CancelJob("job01"); !errors.Is(err, ErrInvalidJobID) {
		t.Fatalf("CancelJob error = %v, want ErrInvalidJobID", err)
	}
	if service.jobMgr != nil {
		t.Fatal("invalid cancellation allocated the job manager")
	}
}

func TestInvalidSourceBoundJobCreatesNoJobAndEmitsNoEvent(t *testing.T) {
	service := NewFileService()
	mu, events := installJobEventRecorder(t, service)
	called := false
	invalidFileID := " f1"

	_, err := runServiceJob(service, jobSpec{Title: "must not start", FileID: invalidFileID}, func(context.Context, func(int64, int64, string)) (int, error) {
		called = true
		return 0, nil
	})
	if !errors.Is(err, ErrInvalidFileID) {
		t.Fatalf("runServiceJob error = %v, want ErrInvalidFileID", err)
	}
	if called {
		t.Fatal("invalid source-bound job callback ran")
	}
	manager := service.jobs()
	manager.mu.Lock()
	active := manager.active
	sequence := manager.seq
	manager.mu.Unlock()
	if active != nil || sequence != 0 {
		t.Fatalf("invalid job changed manager state: active=%v sequence=%d", active, sequence)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*events) != 0 {
		t.Fatalf("invalid job emitted events: %#v", *events)
	}
}

func TestInvalidFileIDDoesNotOpenSaveDialog(t *testing.T) {
	service := NewFileService()
	previous := saveCopySaveDialog
	dialogCalls := 0
	saveCopySaveDialog = func(string, string) (string, error) {
		dialogCalls++
		return "unused", nil
	}
	t.Cleanup(func() { saveCopySaveDialog = previous })

	if _, err := service.SaveCopyViaDialog("f01"); !errors.Is(err, ErrInvalidFileID) {
		t.Fatalf("SaveCopyViaDialog error = %v, want ErrInvalidFileID", err)
	}
	if dialogCalls != 0 {
		t.Fatalf("invalid file ID opened save dialog %d times", dialogCalls)
	}
}

func TestDisabledSQLPresetAndRegexReplaceRemainFailClosedBeforeDialog(t *testing.T) {
	service := NewFileService()
	huge := strings.Repeat("x", maxRPCPathBytes+1)
	if _, err := service.SqlReplaceViaDialog(huge, huge, huge, true, true, true); !errors.Is(err, ErrSQLRegexReplaceUnsupported) {
		t.Fatalf("SqlReplaceViaDialog error = %v, want ErrSQLRegexReplaceUnsupported", err)
	}
	if _, err := service.SqlApplyPresetViaDialog(huge, huge, huge, huge, huge, huge); !errors.Is(err, ErrSQLCleanupPresetsDisabled) {
		t.Fatalf("SqlApplyPresetViaDialog error = %v, want ErrSQLCleanupPresetsDisabled", err)
	}
}
