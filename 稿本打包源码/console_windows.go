//go:build windows

package main

import (
	"bufio"
	"os"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procSetOutputCP  = kernel32.NewProc("SetConsoleOutputCP")
	procSetCP        = kernel32.NewProc("SetConsoleCP")
	procReadConsoleW = kernel32.NewProc("ReadConsoleW")
	procGetMode      = kernel32.NewProc("GetConsoleMode")
	procSetTitle     = kernel32.NewProc("SetConsoleTitleW")
)

func setupConsole() {
	procSetOutputCP.Call(65001)
	procSetCP.Call(65001)
	if t, err := syscall.UTF16PtrFromString("稿本打包"); err == nil {
		procSetTitle.Call(uintptr(unsafe.Pointer(t)))
	}
}

var fallback = bufio.NewReader(os.Stdin)

// readLine reads a line as UTF-16 from a real console so Chinese paths survive; falls back to stdin when redirected.
func readLine() string {
	h := syscall.Handle(os.Stdin.Fd())
	var mode uint32
	if r, _, _ := procGetMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		s, _ := fallback.ReadString('\n')
		return strings.TrimRight(s, "\r\n")
	}
	var out []uint16
	buf := make([]uint16, 1024)
	for {
		var n uint32
		r, _, _ := procReadConsoleW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&n)), 0)
		if r == 0 || n == 0 {
			break
		}
		out = append(out, buf[:n]...)
		if n > 0 && buf[n-1] == '\n' {
			break
		}
	}
	return strings.TrimRight(string(utf16.Decode(out)), "\r\n")
}
