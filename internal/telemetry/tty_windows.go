//go:build windows

package telemetry

import (
	"syscall"
	"unsafe"
)

type coord struct {
	X int16
	Y int16
}

type smallRect struct {
	Left   int16
	Top    int16
	Right  int16
	Bottom int16
}

type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleScreenInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

func detectTTYWidth() int {
	// Open CONOUT$ directly so width detection works even when stdout is piped by agy.
	p, err := syscall.UTF16PtrFromString("CONOUT$")
	if err != nil {
		return 0
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil || h == syscall.InvalidHandle {
		return 0
	}
	defer syscall.CloseHandle(h)

	var info consoleScreenBufferInfo
	r, _, _ := procGetConsoleScreenInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 0
	}
	if w := int(info.Window.Right-info.Window.Left) + 1; w > 0 {
		return w
	}
	return int(info.Size.X)
}
