//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// errAddrInUse is the errno of a bind to an address that another socket
// holds. Windows reports WSAEADDRINUSE (10048); syscall.EADDRINUSE is only
// an invented value on Windows and never matches a real bind error.
const errAddrInUse = windows.WSAEADDRINUSE

// detachSysProcAttr detaches the restarted daemon from the console so it
// survives the upgrade process exiting.
// 0x00000008 = DETACHED_PROCESS, 0x00000200 = CREATE_NEW_PROCESS_GROUP.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200}
}

// stillActive is what GetExitCodeProcess reports for a running process
// (STILL_ACTIVE, 0x103). x/sys/windows does not define it.
const stillActive = 259

// processAlive reports whether pid is a running process.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

// watchOwner blocks on the parent process handle and triggers shutdown when
// it exits — Windows' equivalent of the Unix kill(pid, 0) poll.
func watchOwner(pid int, shutdown func()) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Can't watch (already gone or no access): assume gone — a child-mode
		// daemon without a live owner must not linger.
		fmt.Fprintf(os.Stderr,
			"memstated: cannot watch owner pid %d (%v) — shutting down\n", pid, err)
		shutdown()
		return
	}
	defer windows.CloseHandle(h)
	_, _ = windows.WaitForSingleObject(h, windows.INFINITE)
	fmt.Fprintf(os.Stderr,
		"memstated: owner pid %d vanished — shutting down\n", pid)
	shutdown()
}
