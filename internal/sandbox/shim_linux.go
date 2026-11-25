// Package sandbox runs a command under ptrace with a seccomp filter and
// records every file each process opens or renames into place.
//
// The seccomp filter sends only the file-opening system calls to the tracer
// (SECCOMP_RET_TRACE); everything else runs at full speed. The filter is
// installed by a small shim: the tracer starts its own binary with
// KILN_SANDBOX_SHIM=1, and Shim (called from main's init) installs the filter
// and execs the real command, which inherits it along with all its children.
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const shimEnv = "KILN_SANDBOX_SHIM"

const (
	bpfLD  = 0x00
	bpfW   = 0x00
	bpfABS = 0x20
	bpfJMP = 0x05
	bpfJEQ = 0x10
	bpfK   = 0x00
	bpfRET = 0x06

	seccompRetAllow = 0x7fff0000
	seccompRetTrace = 0x7ff00000
	// offsets in struct seccomp_data
	offNr   = 0
	offArch = 4
)

func stmt(code uint16, k uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: k} }
func jeq(k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: bpfJMP | bpfJEQ | bpfK, Jt: jt, Jf: jf, K: k}
}

// filter traps the traced system calls and allows everything else. A process
// running another architecture's ABI (not expected) is allowed untraced.
func filter() []unix.SockFilter {
	prog := []unix.SockFilter{
		stmt(bpfLD|bpfW|bpfABS, offArch),
		jeq(auditArch, 1, 0),
		stmt(bpfRET|bpfK, seccompRetAllow),
		stmt(bpfLD|bpfW|bpfABS, offNr),
	}
	n := len(tracedSyscalls)
	for i, nr := range tracedSyscalls {
		// on match jump to the RET TRACE after the remaining checks
		prog = append(prog, jeq(uint32(nr), uint8(n-i), 0))
	}
	return append(prog, stmt(bpfRET|bpfK, seccompRetAllow), stmt(bpfRET|bpfK, seccompRetTrace))
}

// Shim installs the seccomp filter and execs the wrapped command when the
// process was started by the tracer. Call it first thing in main (or init).
func Shim() {
	if os.Getenv(shimEnv) != "1" {
		return
	}
	runtime.LockOSThread()
	os.Unsetenv(shimEnv)
	args := os.Args[1:]
	if len(args) == 0 {
		shimFail("no command")
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		shimFail("%v", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		shimFail("no_new_privs: %v", err)
	}
	f := filter()
	prog := unix.SockFprog{Len: uint16(len(f)), Filter: &f[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, 1 /* SECCOMP_SET_MODE_FILTER */, 0, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		shimFail("seccomp: %v", errno)
	}
	err = syscall.Exec(path, args, os.Environ())
	shimFail("exec %s: %v", path, err)
}

func shimFail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "kiln sandbox shim: "+format+"\n", args...)
	os.Exit(127)
}
