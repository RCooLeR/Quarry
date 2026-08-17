//go:build windows

package winacl

import (
	"errors"
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	// ErrUnavailable means the destination filesystem cannot enforce persistent
	// Windows ACLs. Callers must fail before creating a sensitive file.
	ErrUnavailable       = errors.New("private Windows file ACLs are unavailable on the destination filesystem")
	getVolumeInformation = windows.GetVolumeInformationByHandle
)

// Descriptor returns a protected DACL granting full control only to the
// effective user and LocalSystem. The descriptor is supplied during creation;
// callers must never create with inherited access and tighten it afterward.
func Descriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("query effective Windows user for private file: %w", err)
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, errors.New("query effective Windows user for private file: invalid user SID")
	}
	userSID := user.User.Sid.String()
	if userSID == "" {
		return nil, errors.New("query effective Windows user for private file: empty user SID")
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + userSID + ")(A;;FA;;;SY)")
	if err != nil {
		return nil, fmt.Errorf("build private Windows file DACL: %w", err)
	}
	return descriptor, nil
}

// RequireVolume rejects filesystems such as FAT/exFAT that cannot persist an
// ACL supplied at object creation.
func RequireVolume(handle windows.Handle) error {
	var flags uint32
	if err := getVolumeInformation(handle, nil, 0, nil, nil, &flags, nil, 0); err != nil {
		return fmt.Errorf("inspect Windows filesystem ACL support: %w", err)
	}
	if flags&windows.FILE_PERSISTENT_ACLS == 0 {
		return ErrUnavailable
	}
	return nil
}

// RequirePath checks the destination's parent volume before any child object
// is created. path may use an extended-length Windows spelling.
func RequirePath(path string) error {
	dir, _ := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		name,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return fmt.Errorf("inspect Windows destination filesystem ACL support: %w", err)
	}
	defer windows.CloseHandle(handle)
	return RequireVolume(handle)
}

// OpenFile supplies Quarry's protected DACL to the Win32 create/open call.
// Windows applies it only when disposition creates a new object; an existing
// OPEN_ALWAYS object retains its current owner and DACL.
func OpenFile(path string, access, share, disposition, attributes uint32) (windows.Handle, error) {
	if err := RequirePath(path); err != nil {
		return windows.InvalidHandle, err
	}
	descriptor, err := Descriptor()
	if err != nil {
		return windows.InvalidHandle, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	security := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	return windows.CreateFile(name, access, share, security, disposition, attributes, 0)
}
