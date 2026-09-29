//go:build !windows

package cachepath

func normalizeSourceIdentity(path string) string { return path }
