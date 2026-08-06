//go:build windows

package main

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

const attachParentProcess = ^uint32(0)

var (
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	attachConsoleProc = kernel32.NewProc("AttachConsole")
	setOutputCPProc   = kernel32.NewProc("SetConsoleOutputCP")
)

func prepareConsole(args []string) {
	if len(args) == 0 {
		return
	}
	attached, attachErr := callConsoleProc(attachConsoleProc, uintptr(attachParentProcess))
	if !attached && attachErr != windows.ERROR_ACCESS_DENIED {
		return
	}
	_, _, _ = setOutputCPProc.Call(65001)
	bindConsoleStreams()
}

func callConsoleProc(procedure *windows.LazyProc, arguments ...uintptr) (bool, error) {
	result, _, callErr := procedure.Call(arguments...)
	if result != 0 {
		return true, nil
	}
	if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
		return false, errno
	}
	return false, syscall.EINVAL
}

func bindConsoleStreams() {
	if !usableStandardStream(os.Stdin) {
		if stdin, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
			os.Stdin = stdin
		}
	}
	if !usableStandardStream(os.Stdout) {
		if stdout, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			os.Stdout = stdout
		}
	}
	if !usableStandardStream(os.Stderr) {
		if stderr, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			os.Stderr = stderr
		}
	}
}

func usableStandardStream(stream *os.File) bool {
	if stream == nil {
		return false
	}
	_, err := stream.Stat()
	return err == nil
}
