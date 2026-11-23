//go:build windows

package auth

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32DLL        = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32DLL.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32DLL.NewProc("SetConsoleMode")
)

const (
	enableProcessedInput = 0x0001
	enableLineInput      = 0x0002
	enableEchoInput      = 0x0004
)

// readSingleKey reads one keypress from stdin in raw console mode (Windows).
// Returns: "up", "down", "enter", "esc", or the ASCII lowercase character.
func readSingleKey() (string, error) {
	h := os.Stdin.Fd()
	var oldMode uint32
	r, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&oldMode)))
	if r != 0 {
		newMode := (oldMode &^ (enableLineInput | enableEchoInput)) | enableProcessedInput
		_, _, _ = procSetConsoleMode.Call(h, uintptr(newMode))
		defer procSetConsoleMode.Call(h, uintptr(oldMode))
	}

	var buf [8]byte
	n, err := os.Stdin.Read(buf[:])
	if err != nil || n == 0 {
		return "", err
	}
	b := buf[0]
	if b == 0x1b {
		if n >= 3 && buf[1] == '[' {
			switch buf[2] {
			case 'A':
				return "up", nil
			case 'B':
				return "down", nil
			}
		}
		return "esc", nil
	}
	if b == '\r' || b == '\n' {
		return "enter", nil
	}
	if b == 0x03 {
		return "esc", nil
	}
	if b >= 'A' && b <= 'Z' {
		b += 'a' - 'A'
	}
	return string([]byte{b}), nil
}
