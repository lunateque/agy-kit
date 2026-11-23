//go:build windows

package auth

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

var (
	advapi32       = syscall.NewLazyDLL("advapi32.dll")
	procCredReadW  = advapi32.NewProc("CredReadW")
	procCredWriteW = advapi32.NewProc("CredWriteW")
	procCredFree   = advapi32.NewProc("CredFree")
)

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
	maxCredBlobSize         = 1 << 20 // 1 MiB safety bound
)

type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// utf16PtrToString safely converts a NUL-terminated UTF-16 pointer without
// over-reading past the NUL terminator into unmapped memory.
func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	const maxUnits = 4096
	units := make([]uint16, 0, 64)
	for i := 0; i < maxUnits; i++ {
		u := *(*uint16)(unsafe.Add(unsafe.Pointer(p), uintptr(i)*unsafe.Sizeof(uint16(0))))
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	return syscall.UTF16ToString(units)
}

func readSysCredential(target string) (*RawCredential, error) {
	targetPtr, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return nil, err
	}

	var pCred *credentialW
	r1, _, errSys := procCredReadW.Call(
		uintptr(unsafe.Pointer(targetPtr)),
		uintptr(credTypeGeneric),
		0,
		uintptr(unsafe.Pointer(&pCred)),
	)
	if r1 == 0 {
		return nil, fmt.Errorf("CredReadW(%s) failed: %w", target, errSys)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(pCred)))

	if pCred == nil {
		return nil, errors.New("CredReadW returned nil credential")
	}
	if pCred.CredentialBlobSize > maxCredBlobSize {
		return nil, fmt.Errorf("credential blob size %d exceeds maximum %d", pCred.CredentialBlobSize, maxCredBlobSize)
	}

	raw := &RawCredential{
		Target:  target,
		Type:    pCred.Type,
		Persist: pCred.Persist,
		User:    utf16PtrToString(pCred.UserName),
	}

	if pCred.CredentialBlobSize > 0 && pCred.CredentialBlob != nil {
		blob := make([]byte, pCred.CredentialBlobSize)
		src := unsafe.Slice(pCred.CredentialBlob, pCred.CredentialBlobSize)
		copy(blob, src)
		raw.Blob = blob
	}

	return raw, nil
}

func writeSysCredential(raw *RawCredential) error {
	if raw == nil {
		return errors.New("nil credential")
	}
	if len(raw.Blob) > maxCredBlobSize {
		return fmt.Errorf("credential blob size %d exceeds maximum %d", len(raw.Blob), maxCredBlobSize)
	}
	targetPtr, err := syscall.UTF16PtrFromString(raw.Target)
	if err != nil {
		return err
	}

	var userPtr *uint16
	if raw.User != "" {
		userPtr, err = syscall.UTF16PtrFromString(raw.User)
		if err != nil {
			return err
		}
	}

	persist := raw.Persist
	if persist == 0 {
		persist = credPersistLocalMachine
	}

	credType := raw.Type
	if credType == 0 {
		credType = credTypeGeneric
	}

	c := credentialW{
		Flags:              0,
		Type:               credType,
		TargetName:         targetPtr,
		Comment:            nil,
		CredentialBlobSize: uint32(len(raw.Blob)),
		Persist:            persist,
		AttributeCount:     0,
		Attributes:         0,
		TargetAlias:        nil,
		UserName:           userPtr,
	}

	if len(raw.Blob) > 0 {
		c.CredentialBlob = &raw.Blob[0]
	}

	r1, _, errSys := procCredWriteW.Call(uintptr(unsafe.Pointer(&c)), 0)
	if r1 == 0 {
		return fmt.Errorf("CredWriteW(%s) failed: %w", raw.Target, errSys)
	}

	return nil
}
