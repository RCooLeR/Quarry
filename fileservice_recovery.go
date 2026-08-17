package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/quarry/quarry-wails3/internal/inplace"
)

// InPlaceRecoveryState is a mutation-free service view of adjacent recovery
// evidence. Status is one of none, pending, committed, invalid,
// source_mismatch, drifted, unsafe_artifact, or inaccessible. The frontend can
// present this state without parsing an error string or loading artifact bytes.
type InPlaceRecoveryState struct {
	Detected          bool   `json:"detected"`
	SourcePath        string `json:"sourcePath"`
	SidecarPath       string `json:"sidecarPath"`
	ArtifactKind      string `json:"artifactKind"`
	ArtifactSize      int64  `json:"artifactSize"`
	Status            string `json:"status"`
	Phase             string `json:"phase"`
	SourceState       string `json:"sourceState"`
	EntryCount        int    `json:"entryCount"`
	PayloadBytes      int64  `json:"payloadBytes"`
	SourceMatches     bool   `json:"sourceMatches"`
	CanRollback       bool   `json:"canRollback"`
	CanClear          bool   `json:"canClear"`
	Detail            string `json:"detail"`
	RecommendedAction string `json:"recommendedAction"`
}

// InspectInPlaceRecovery exposes bounded, read-only recovery metadata for a
// user-selected source path. It never opens the source writable and never
// removes, rewrites, follows, or quarantines the adjacent artifact.
func (s *FileService) InspectInPlaceRecovery(path string) (InPlaceRecoveryState, error) {
	if err := validateRPCPath(path); err != nil {
		return InPlaceRecoveryState{}, err
	}
	if err := s.ensureServiceRunning(); err != nil {
		return InPlaceRecoveryState{}, err
	}
	return inspectInPlaceRecovery(path)
}

func inspectInPlaceRecovery(path string) (InPlaceRecoveryState, error) {
	if err := validateRPCPath(path); err != nil {
		return InPlaceRecoveryState{}, err
	}
	if path == "" {
		return InPlaceRecoveryState{}, errors.New("source path is required")
	}
	recoveryPath := sidecarPath(path)
	state := InPlaceRecoveryState{
		SourcePath:        path,
		SidecarPath:       recoveryPath,
		Status:            "none",
		RecommendedAction: "No recovery action is required.",
	}
	artifactInfo, err := os.Lstat(recoveryPath)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return InPlaceRecoveryState{}, fmt.Errorf("check in-place recovery data at %q: %w", recoveryPath, err)
	}
	state.Detected = true
	state.ArtifactSize = artifactInfo.Size()
	state.ArtifactKind = recoveryArtifactKind(artifactInfo.Mode())
	state.RecommendedAction = "Preserve the source and recovery artifact; inspect or restore through an explicit recovery workflow."
	if artifactInfo.Mode()&os.ModeSymlink != 0 || !artifactInfo.Mode().IsRegular() {
		state.Status = "unsafe_artifact"
		state.Detail = "The adjacent recovery path is not a regular, identity-held recovery file."
		return state, nil
	}

	inspection, inspectErr := inplace.Inspect(path, recoveryPath)
	if inspectErr != nil {
		state.Detail = inspectErr.Error()
		switch {
		case errors.Is(inspectErr, inplace.ErrInvalidSidecar):
			state.Status = "invalid"
		case errors.Is(inspectErr, inplace.ErrSourceMismatch):
			state.Status = "source_mismatch"
		case errors.Is(inspectErr, inplace.ErrUnsafeSource):
			state.Status = "source_mismatch"
		case errors.Is(inspectErr, inplace.ErrRecoveryDrift):
			state.Status = "drifted"
		case errors.Is(inspectErr, inplace.ErrUnsafeSidecar):
			state.Status = "unsafe_artifact"
		default:
			state.Status = "inaccessible"
		}
		return state, nil
	}
	state.ArtifactSize = inspection.ArtifactSize
	state.Phase = inspection.Phase
	state.Status = inspection.Phase
	state.SourceState = inspection.SourceState
	state.EntryCount = inspection.EntryCount
	state.PayloadBytes = inspection.PayloadBytes
	state.SourceMatches = inspection.SourceMatches
	state.CanRollback = inspection.CanRollback
	state.CanClear = inspection.CanClear
	if inspection.SourceState == "drifted" {
		state.Status = "drifted"
		state.RecommendedAction = "Preserve both files; the source no longer matches either recorded transaction state and requires manual recovery."
	} else if inspection.CanRollback {
		state.RecommendedAction = "Review the transaction and explicitly roll back through the recovery workflow before opening the source."
	} else if inspection.CanClear {
		state.RecommendedAction = "Verify the completed patch and explicitly clear the committed recovery record before opening the source."
	}
	return state, nil
}

func recoveryArtifactKind(mode os.FileMode) string {
	switch {
	case mode&os.ModeSymlink != 0:
		return "symlink"
	case mode.IsRegular():
		return "regular"
	case mode.IsDir():
		return "directory"
	default:
		return "other"
	}
}
