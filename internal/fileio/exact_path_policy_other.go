//go:build !windows

package fileio

func validatePlatformExactPath(_ string, _ string, _ bool) error { return nil }
