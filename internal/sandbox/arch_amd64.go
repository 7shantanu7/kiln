//go:build linux && amd64

package sandbox

import "golang.org/x/sys/unix"

const auditArch = 0xC000003E // AUDIT_ARCH_X86_64

var tracedSyscalls = []int{
	unix.SYS_OPEN, unix.SYS_CREAT, unix.SYS_OPENAT, unix.SYS_OPENAT2,
	unix.SYS_RENAME, unix.SYS_RENAMEAT, unix.SYS_RENAMEAT2,
}

type regs struct{ r unix.PtraceRegs }

func getRegs(pid int) (*regs, error) {
	var r regs
	err := unix.PtraceGetRegs(pid, &r.r)
	return &r, err
}

func (r *regs) nr() int { return int(r.r.Orig_rax) }
func (r *regs) arg(i int) uint64 {
	switch i {
	case 0:
		return r.r.Rdi
	case 1:
		return r.r.Rsi
	case 2:
		return r.r.Rdx
	case 3:
		return r.r.R10
	case 4:
		return r.r.R8
	default:
		return r.r.R9
	}
}
func (r *regs) ret() int64 { return int64(r.r.Rax) }

// legacySyscall decodes open, creat and rename, which take paths relative to
// the working directory.
func legacySyscall(pid int, nr int, r *regs, at func(uint64, uint64) string, writeFlags func(uint64) bool) *pending {
	cwd := uint64(0xffffffffffffff9c) // AT_FDCWD as an unsigned register value
	switch nr {
	case unix.SYS_OPEN:
		return &pending{paths: []string{at(cwd, r.arg(0))}, write: writeFlags(r.arg(1)), dir: r.arg(1)&unix.O_DIRECTORY != 0}
	case unix.SYS_CREAT:
		return &pending{paths: []string{at(cwd, r.arg(0))}, write: true}
	case unix.SYS_RENAME:
		return &pending{paths: []string{at(cwd, r.arg(1))}, write: true}
	}
	return nil
}
