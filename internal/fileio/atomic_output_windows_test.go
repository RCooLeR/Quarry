//go:build windows

package fileio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestAtomicOutputWindowsUsesProtectedPrivateDACLAtCreationAndPublication(t *testing.T) {
	dir := permissiveWindowsOutputDirectory(t)
	finalPath := filepath.Join(dir, "result.txt")
	out, err := OpenAtomicOutput(finalPath, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Cleanup()

	assertPrivateWindowsOutputDACL(t, out.TempPath())
	if _, err := out.Write([]byte("private bytes")); err != nil {
		t.Fatal(err)
	}
	if err := out.Commit(); err != nil {
		t.Fatal(err)
	}
	assertPrivateWindowsOutputDACL(t, finalPath)
}

func TestWindowsFileOutputPrimitivesUseProtectedPrivateDACL(t *testing.T) {
	dir := permissiveWindowsOutputDirectory(t)
	exclusivePath := filepath.Join(dir, "exclusive.txt")
	exclusive, err := OpenExclusiveOutput(exclusivePath, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer exclusive.Cleanup()
	assertPrivateWindowsOutputDACL(t, exclusivePath)
	if _, err := exclusive.Write([]byte("exclusive")); err != nil {
		t.Fatal(err)
	}
	if err := exclusive.Close(); err != nil {
		t.Fatal(err)
	}
	assertPrivateWindowsOutputDACL(t, exclusivePath)

	atomicPath := filepath.Join(dir, "small-atomic.txt")
	if _, err := WriteFileAtomic(atomicPath, []byte("atomic"), AtomicWriteOptions{Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	assertPrivateWindowsOutputDACL(t, atomicPath)
}

func TestWriteFileAtomicWindowsOverwriteTempAndJournalArePrivateAtCreation(t *testing.T) {
	dir := permissiveWindowsOutputDirectory(t)
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	originalAfterJournal := afterAtomicOverwriteJournal
	observedJournal := false
	afterAtomicOverwriteJournal = func() {
		journal, present, err := loadAtomicWriteJournal(path)
		if err != nil || !present {
			t.Fatalf("load live overwrite journal: present=%v err=%v", present, err)
		}
		assertPrivateWindowsOutputDACL(t, journal.TempPath)
		assertPrivateWindowsOutputDACL(t, path+atomicWriteJournalSuffix)
		observedJournal = true
	}
	t.Cleanup(func() { afterAtomicOverwriteJournal = originalAfterJournal })

	if _, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if !observedJournal {
		t.Fatal("overwrite did not expose the journaled pre-publication phase")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("destination = %q, %v; want new generation", got, err)
	}
	assertPrivateWindowsOutputDACL(t, path)
}

func permissiveWindowsOutputDirectory(t *testing.T) string {
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

	// Prove that an ordinary child really does inherit the permissive parent;
	// otherwise a private-output assertion would not exercise the policy gap.
	ordinary := filepath.Join(dir, "ordinary-inherited.txt")
	if err := os.WriteFile(ordinary, []byte("ordinary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !windowsDACLContainsSID(t, ordinary, "S-1-1-0") {
		t.Fatal("permissive parent fixture did not give an ordinary child an Everyone ACE")
	}
	return dir
}

func assertPrivateWindowsOutputDACL(t *testing.T, path string) {
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
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL for %q is not protected: %s", path, descriptor.String())
	}

	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		user.User.Sid.String(): false,
		"S-1-5-18":             false,
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil {
		t.Fatalf("DACL for %q is nil", path)
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatal(err)
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("DACL for %q contains a non-allow ACE: %s", path, descriptor.String())
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if _, ok := want[sid]; !ok {
			t.Fatalf("DACL for %q grants unexpected SID %s: %s", path, sid, descriptor.String())
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			t.Fatalf("DACL for %q contains inherited ACE for %s: %s", path, sid, descriptor.String())
		}
		want[sid] = true
	}
	for sid, present := range want {
		if !present {
			t.Fatalf("DACL for %q does not grant required SID %s: %s", path, sid, descriptor.String())
		}
	}
	runtime.KeepAlive(descriptor)
}

func windowsDACLContainsSID(t *testing.T, path, wantSID string) bool {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatal(err)
		}
		if ace != nil && (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String() == wantSID {
			runtime.KeepAlive(descriptor)
			return true
		}
	}
	runtime.KeepAlive(descriptor)
	return false
}

func TestAtomicOutputWindowsPublishesOwnedHandleAfterTempPathSubstitution(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "result.txt")
	out, err := OpenAtomicOutput(finalPath, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Cleanup()
	if _, err := out.Write([]byte("owned bytes")); err != nil {
		t.Fatal(err)
	}
	tempPath := out.TempPath()

	// Even when delete sharing permits the temporary name to be moved, commit
	// must publish the retained object and must not touch a substitute later
	// created under the stale temporary pathname.
	movedTemp := tempPath + ".moved"
	if err := os.Rename(tempPath, movedTemp); err != nil {
		t.Fatalf("move retained temp pathname: %v", err)
	}
	if err := os.WriteFile(tempPath, []byte("substitute"), 0o600); err != nil {
		t.Fatalf("create temp-path substitute: %v", err)
	}

	if err := out.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(finalPath); err != nil || string(got) != "owned bytes" {
		t.Fatalf("final = %q, err %v", got, err)
	}
	if got, err := os.ReadFile(tempPath); err != nil || string(got) != "substitute" {
		t.Fatalf("temp-path substitute = %q, err %v", got, err)
	}
	if _, err := os.Stat(movedTemp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned object retained moved temp name %q: %v", movedTemp, err)
	}
	if err := os.Remove(tempPath); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicOutputWindowsReportsPostPublicationDurabilityState(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "result.txt")
	out, err := OpenAtomicOutput(finalPath, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Cleanup()
	if _, err := out.Write([]byte("complete output")); err != nil {
		t.Fatal(err)
	}

	originalSyncDir := syncAtomicOutputDirectory
	syncAtomicOutputDirectory = func(windows.Handle) error { return fmt.Errorf("simulated directory sync failure") }
	t.Cleanup(func() { syncAtomicOutputDirectory = originalSyncDir })
	err = out.Commit()
	var publication *PublicationError
	if !errors.As(err, &publication) {
		t.Fatalf("error = %v, want PublicationError", err)
	}
	if publication.Durable || publication.FinalPath != finalPath {
		t.Fatalf("publication = %+v", publication)
	}
	if got, readErr := os.ReadFile(finalPath); readErr != nil || string(got) != "complete output" {
		t.Fatalf("published final = %q, err %v", got, readErr)
	}
}

func TestAtomicOutputWindowsPathDriftDeletesOnlyPublishedHandle(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "result.txt")
	movedOwnedPath := filepath.Join(dir, "owned-moved.txt")
	out, err := OpenAtomicOutput(finalPath, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Cleanup()
	if _, err := out.Write([]byte("owned bytes")); err != nil {
		t.Fatal(err)
	}

	originalHook := afterAtomicOutputPublish
	afterAtomicOutputPublish = func() {
		if err := os.Rename(finalPath, movedOwnedPath); err != nil {
			t.Fatalf("move published handle: %v", err)
		}
		if err := os.WriteFile(finalPath, []byte("replacement sentinel"), 0o600); err != nil {
			t.Fatalf("create replacement sentinel: %v", err)
		}
	}
	t.Cleanup(func() { afterAtomicOutputPublish = originalHook })

	err = out.Commit()
	if !errors.Is(err, ErrOutputPathDrift) {
		t.Fatalf("commit error = %v, want ErrOutputPathDrift", err)
	}
	var drift *OutputPathDriftError
	if !errors.As(err, &drift) || !drift.RolledBack {
		t.Fatalf("drift error = %+v, want confirmed rollback", drift)
	}
	if got, err := os.ReadFile(finalPath); err != nil || string(got) != "replacement sentinel" {
		t.Fatalf("replacement sentinel = %q, err %v", got, err)
	}
	if _, err := os.Stat(movedOwnedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned drifted output remains: %v", err)
	}
}

func TestExactChildPathRejectsWindowsDriveRelativeParent(t *testing.T) {
	for _, dir := range []string{`C:`, `C:relative`} {
		if _, err := ExactChildPath(dir, "output.txt"); !errors.Is(err, ErrInvalidExactPath) {
			t.Fatalf("ExactChildPath(%q) error = %v, want ErrInvalidExactPath", dir, err)
		}
	}
}

func TestAtomicOutputWindowsUnconfirmedDriftIsPublicationError(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "result.txt")
	movedOwnedPath := filepath.Join(dir, "owned-moved.txt")
	out, err := OpenAtomicOutput(finalPath, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Cleanup()
	if _, err := out.Write([]byte("owned bytes")); err != nil {
		t.Fatal(err)
	}

	originalHook := afterAtomicOutputPublish
	afterAtomicOutputPublish = func() {
		if err := os.Rename(finalPath, movedOwnedPath); err != nil {
			t.Fatalf("move published handle: %v", err)
		}
		if err := os.WriteFile(finalPath, []byte("replacement sentinel"), 0o600); err != nil {
			t.Fatalf("create replacement sentinel: %v", err)
		}
	}
	originalSync := syncAtomicOutputDirectory
	syncAtomicOutputDirectory = func(windows.Handle) error { return errors.New("simulated rollback sync failure") }
	t.Cleanup(func() {
		afterAtomicOutputPublish = originalHook
		syncAtomicOutputDirectory = originalSync
	})

	err = out.Commit()
	if !errors.Is(err, ErrOutputPathDrift) {
		t.Fatalf("commit error = %v, want ErrOutputPathDrift", err)
	}
	var publication *PublicationError
	if !errors.As(err, &publication) || !publication.LocationUncertain {
		t.Fatalf("publication error = %+v, want uncertain location", publication)
	}
	var drift *OutputPathDriftError
	if !errors.As(err, &drift) || drift.RolledBack {
		t.Fatalf("drift error = %+v, want unconfirmed rollback", drift)
	}
	if got, err := os.ReadFile(finalPath); err != nil || string(got) != "replacement sentinel" {
		t.Fatalf("replacement sentinel = %q, err %v", got, err)
	}
	if _, err := os.Stat(movedOwnedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned drifted output remains despite handle deletion: %v", err)
	}
}
