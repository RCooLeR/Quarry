//go:build windows

package document

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsIndexCacheTempAndNewLockArePrivateAtCreation(t *testing.T) {
	dir := permissiveWindowsIndexCacheDirectory(t)
	finalName := strings.Repeat("a", sha256.Size*2) + indexCacheExtension
	temp, tempPath, err := openIndexCacheTemp(dir, finalName)
	if err != nil {
		t.Fatal(err)
	}
	assertProtectedWindowsIndexCacheDACL(t, tempPath)
	if err := temp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tempPath); err != nil {
		t.Fatal(err)
	}

	release, err := acquireIndexCacheDirectoryLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, ".quarry-index.lock")
	assertProtectedWindowsIndexCacheDACL(t, lockPath)
	if err := release(); err != nil {
		t.Fatal(err)
	}
	assertProtectedWindowsIndexCacheDACL(t, lockPath)
}

func TestWindowsExistingIndexCacheLockKeepsItsACL(t *testing.T) {
	dir := permissiveWindowsIndexCacheDirectory(t)
	lockPath := filepath.Join(dir, ".quarry-index.lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before, control := windowsIndexCacheDACL(t, lockPath)
	if control&windows.SE_DACL_PROTECTED != 0 || !strings.Contains(before, ";;;WD)") {
		t.Fatalf("existing lock fixture is not permissive inherited access: %s", before)
	}
	release, err := acquireIndexCacheDirectoryLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	after, _ := windowsIndexCacheDACL(t, lockPath)
	if after != before {
		t.Fatalf("OPEN_ALWAYS rewrote existing lock ACL\nbefore: %s\nafter:  %s", before, after)
	}
}

func permissiveWindowsIndexCacheDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	descriptor, err := windows.SecurityDescriptorFromString("D:(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertProtectedWindowsIndexCacheDACL(t *testing.T, path string) {
	t.Helper()
	sddl, control := windowsIndexCacheDACL(t, path)
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("index-cache DACL is not protected: %s", sddl)
	}
	if strings.Contains(sddl, ";;;WD)") {
		t.Fatalf("index-cache object grants Everyone access: %s", sddl)
	}
	if !strings.Contains(sddl, ";;;SY)") {
		t.Fatalf("index-cache object does not grant LocalSystem access: %s", sddl)
	}
}

func windowsIndexCacheDACL(t *testing.T, path string) (string, windows.SECURITY_DESCRIPTOR_CONTROL) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	return descriptor.String(), control
}
