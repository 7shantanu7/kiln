//go:build linux && arm64

package sandbox

import "golang.org/x/sys/unix"

const auditArch = 0xC00000B7 // AUDIT_ARCH_AARCH64

// arm64 has no open/creat/rename: only the *at variants exist.
var tracedSyscalls = []int{unix.SYS_OPENAT, unix.SYS_OPENAT2, unix.SYS_RENAMEAT, unix.SYS_RENAMEAT2}

type regs struct{ r unix.PtraceRegsArm64 }

func getRegs(pid int) (*regs, error) {
	var r regs
	// arm64 has no PTRACE_GETREGS; read the general registers with GETREGSET.
	err := unix.PtraceGetRegSetArm64(pid, 1 /* NT_PRSTATUS */, &r.r)
	return &r, err
}

func (r *regs) nr() int          { return int(r.r.Regs[8]) }
func (r *regs) arg(i int) uint64 { return r.r.Regs[i] }
func (r *regs) ret() int64       { return int64(r.r.Regs[0]) }

// legacySyscall decodes syscalls that exist only on older ABIs; arm64 has none.
func legacySyscall(int, int, *regs, func(uint64, uint64) string, func(uint64) bool) *pending {
	return nil
}
