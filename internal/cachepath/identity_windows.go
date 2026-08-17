//go:build windows

package cachepath

// Preserve the resolved spelling. Windows supports case-sensitive directories,
// so unconditional case folding can map two distinct files to one cache slot.
// filepath.EvalSymlinks already converges aliases when the filesystem can
// resolve them; an unresolved spelling is safer in its own slot.
func normalizeSourceIdentity(path string) string { return path }
