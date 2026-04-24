//go:build linux

// Package proctitle sets the process name visible to ps, top, and btop.
//
// On Linux we use prctl(PR_SET_NAME) for the kernel-level comm (visible in
// /proc/<pid>/comm and in top's first column), and overwrite argv[0] for ps
// and btop's cmdline column. No cgo; uses syscall.
package proctitle

import (
	"os"
	"syscall"
	"unsafe"
)

const prSetName = 15 // from <linux/prctl.h>

// Set updates the process title. Best-effort; failures return an error but
// don't abort the program.
func Set(name string) error {
	if len(name) == 0 {
		return nil
	}
	// PR_SET_NAME takes a 16-byte buffer.
	buf := make([]byte, 16)
	copy(buf, name)
	buf[15] = 0
	_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetName, uintptr(unsafe.Pointer(&buf[0])), 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	overwriteArgv0(name)
	return nil
}

// overwriteArgv0 replaces the first argv slot in the process's memory.
// Makes the daemon show as "agent-router" in btop/htop/ps instead of the
// full go binary path.
func overwriteArgv0(name string) {
	if len(os.Args) == 0 {
		return
	}
	argv0 := os.Args[0]
	// Take the address of argv0's backing memory. This is the same memory
	// that ps reads via /proc/<pid>/cmdline.
	hdr := (*[1 << 20]byte)(unsafe.Pointer(unsafe.StringData(argv0)))
	max := len(argv0)
	n := len(name)
	if n > max {
		n = max
	}
	for i := 0; i < n; i++ {
		hdr[i] = name[i]
	}
	for i := n; i < max; i++ {
		hdr[i] = 0
	}
}
