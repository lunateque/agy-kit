//go:build windows

package auth

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// readAutoCompactOverride checks the current process env first, then falls back
// to the Windows HKCU\Environment registry value so long-lived parent processes
// (like herdr started before SetEnvironmentVariable) still propagate the setting.
func readAutoCompactOverride() string {
	if v := strings.TrimSpace(os.Getenv("AGY_CLI_AUTO_COMPACT_OVERRIDE")); v != "" {
		return v
	}
	subkey, err := syscall.UTF16PtrFromString("Environment")
	if err != nil {
		return ""
	}
	var h syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, subkey, 0, syscall.KEY_READ, &h); err != nil {
		return ""
	}
	defer syscall.RegCloseKey(h)

	valName, err := syscall.UTF16PtrFromString("AGY_CLI_AUTO_COMPACT_OVERRIDE")
	if err != nil {
		return ""
	}
	var typ uint32
	var bufLen uint32
	if err := syscall.RegQueryValueEx(h, valName, nil, &typ, nil, &bufLen); err != nil || bufLen == 0 || bufLen > 1024 {
		return ""
	}
	buf := make([]uint16, bufLen/2+1)
	if err := syscall.RegQueryValueEx(h, valName, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &bufLen); err != nil {
		return ""
	}
	return strings.TrimSpace(syscall.UTF16ToString(buf))
}
