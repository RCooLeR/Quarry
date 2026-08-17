//go:build !windows && !linux

package fileio

import (
	"context"
	"os"
)

type atomicOutputPlatformState struct{}

func openAtomicOutputTemp(string, os.FileMode) (*os.File, string, atomicOutputPlatformState, error) {
	return nil, "", atomicOutputPlatformState{}, ErrSecureAtomicOutputUnavailable
}

func publishAtomicOutput(context.Context, *AtomicOutput) error {
	return ErrSecureAtomicOutputUnavailable
}

func cleanupAtomicOutput(*AtomicOutput) error { return nil }
