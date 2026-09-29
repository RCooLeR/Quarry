//go:build windows

package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsLogCreationIsPrivateAndExistingACLIsPreserved(t *testing.T) {
	dir := permissiveWindowsLogDirectory(t)
	newPath := filepath.Join(dir, "new.log")
	newLog, err := openLogForAppend(newPath)
	if err != nil {
		t.Fatal(err)
	}
	assertProtectedWindowsLogDACL(t, newPath)
	if _, err := newLog.WriteString("new entry\n"); err != nil {
		_ = newLog.Close()
		t.Fatal(err)
	}
	if err := newLog.Close(); err != nil {
		t.Fatal(err)
	}
	assertProtectedWindowsLogDACL(t, newPath)

	existingPath := filepath.Join(dir, "existing.log")
	if err := os.WriteFile(existingPath, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, control := windowsLogDACL(t, existingPath)
	if control&windows.SE_DACL_PROTECTED != 0 || !strings.Contains(before, ";;;WD)") {
		t.Fatalf("existing log fixture is not permissive inherited access: %s", before)
	}
	existing, err := openLogForAppend(existingPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := existing.Close(); err != nil {
		t.Fatal(err)
	}
	after, _ := windowsLogDACL(t, existingPath)
	if after != before {
		t.Fatalf("OPEN_ALWAYS rewrote existing log ACL\nbefore: %s\nafter:  %s", before, after)
	}
}

func permissiveWindowsLogDirectory(t *testing.T) string {
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

func assertProtectedWindowsLogDACL(t *testing.T, path string) {
	t.Helper()
	sddl, control := windowsLogDACL(t, path)
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("new log DACL is not protected: %s", sddl)
	}
	if strings.Contains(sddl, ";;;WD)") {
		t.Fatalf("new log grants Everyone access: %s", sddl)
	}
	if !strings.Contains(sddl, ";;;SY)") {
		t.Fatalf("new log does not grant LocalSystem access: %s", sddl)
	}
}

func windowsLogDACL(t *testing.T, path string) (string, windows.SECURITY_DESCRIPTOR_CONTROL) {
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
