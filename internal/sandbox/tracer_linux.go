package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Access is one successful open or rename observed in a traced process.
type Access struct {
	Path    string // absolute, cleaned
	Write   bool   // opened for writing, created, or renamed into place
	Dir     bool   // opened with O_DIRECTORY
	Missing bool   // a read-only open that failed with ENOENT
}

// Process is one traced process (or thread).
type Process struct {
	Pid      int
	Parent   int      // 0 for the root
	Argv     []string // command line of the first exec
	Accesses []Access
}

// Result is everything observed while the command ran.
type Result struct {
	ExitCode  int
	Root      int
	Processes map[int]*Process
}

// Children returns the pids whose parent is pid.
func (r *Result) Children(pid int) []int {
	var out []int
	for p, proc := range r.Processes {
		if proc.Parent == pid {
			out = append(out, p)
		}
	}
	return out
}

// Subtree returns pid and all its descendants.
func (r *Result) Subtree(pid int) []*Process {
	kids := map[int][]int{}
	for p, proc := range r.Processes {
		kids[proc.Parent] = append(kids[proc.Parent], p)
	}
	var out []*Process
	stack := []int{pid}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if proc, ok := r.Processes[p]; ok {
			out = append(out, proc)
		}
		stack = append(stack, kids[p]...)
	}
	return out
}

type pending struct {
	paths []string // candidate paths resolved at syscall entry
	write bool
	dir   bool
}

// Run executes argv in dir under the tracer and returns what it accessed.
// The calling binary must call Shim at startup.
func Run(dir string, argv []string, stdout, stderr *os.File) (*Result, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	// All ptrace requests must come from the thread that started the tracee.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	cmd := exec.Command(self, argv...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), shimEnv+"=1")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &unix.SysProcAttr{Ptrace: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	root := cmd.Process.Pid
	res := &Result{Root: root, Processes: map[int]*Process{root: {Pid: root}}}

	// The child stops with SIGTRAP when it execs the shim.
	var ws unix.WaitStatus
	if _, err := unix.Wait4(root, &ws, 0, nil); err != nil {
		return nil, err
	}
	opts := unix.PTRACE_O_TRACESECCOMP | unix.PTRACE_O_TRACEFORK | unix.PTRACE_O_TRACEVFORK |
		unix.PTRACE_O_TRACECLONE | unix.PTRACE_O_TRACEEXEC | unix.PTRACE_O_TRACESYSGOOD | unix.PTRACE_O_EXITKILL
	if err := unix.PtraceSetOptions(root, opts); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("ptrace setoptions: %w", err)
	}
	if err := unix.PtraceCont(root, 0); err != nil {
		return nil, err
	}

	live := map[int]bool{root: true}
	started := map[int]bool{root: true} // has had its initial stop
	inflight := map[int]*pending{}
	proc := func(pid int) *Process {
		p, ok := res.Processes[pid]
		if !ok {
			p = &Process{Pid: pid}
			res.Processes[pid] = p
		}
		return p
	}

	for len(live) > 0 {
		pid, err := unix.Wait4(-1, &ws, unix.WALL, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ECHILD) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch {
		case ws.Exited(), ws.Signaled():
			delete(live, pid)
			delete(inflight, pid)
			if pid == root {
				if ws.Exited() {
					res.ExitCode = ws.ExitStatus()
				} else {
					res.ExitCode = 128 + int(ws.Signal())
				}
			}
			continue
		case !ws.Stopped():
			continue
		}
		live[pid] = true
		sig := ws.StopSignal()
		cause := ws.TrapCause()
		deliver := 0
		if !started[pid] {
			// A new tracee starts with SIGSTOP, possibly before its parent's
			// fork event is reported. Swallow it.
			started[pid] = true
			proc(pid)
			if sig == unix.SIGSTOP {
				unix.PtraceCont(pid, 0)
				continue
			}
		}

		switch {
		case sig == unix.SIGTRAP && cause == unix.PTRACE_EVENT_SECCOMP:
			if p := syscallEntry(pid); p != nil {
				inflight[pid] = p
				// Resume to the syscall-exit stop to learn whether it succeeded.
				unix.PtraceSyscall(pid, 0)
				continue
			}
		case sig == unix.SIGTRAP|0x80: // syscall stop (PTRACE_O_TRACESYSGOOD)
			if p := inflight[pid]; p != nil {
				delete(inflight, pid)
				if r, err := getRegs(pid); err == nil {
					// Successful calls are accesses. A read that failed with
					// ENOENT is kept too: a step probing for a generated file
					// before it exists is evidence of a missing dependency.
					ok := r.ret() >= 0
					missing := r.ret() == -int64(unix.ENOENT) && !p.write
					if ok || missing {
						pr := proc(pid)
						for _, path := range p.paths {
							pr.Accesses = append(pr.Accesses, Access{Path: path, Write: p.write, Dir: p.dir, Missing: missing})
						}
					}
				}
			}
		case sig == unix.SIGTRAP && (cause == unix.PTRACE_EVENT_FORK || cause == unix.PTRACE_EVENT_VFORK || cause == unix.PTRACE_EVENT_CLONE):
			if msg, err := unix.PtraceGetEventMsg(pid); err == nil {
				child := int(msg)
				proc(child).Parent = pid
				live[child] = true
			}
		case sig == unix.SIGTRAP && cause == unix.PTRACE_EVENT_EXEC:
			// A non-leader thread that execs takes over the leader's pid;
			// the event message is its former tid.
			if msg, err := unix.PtraceGetEventMsg(pid); err == nil && int(msg) != pid {
				delete(live, int(msg))
			}
			if pr := proc(pid); pr.Argv == nil {
				pr.Argv = readCmdline(pid)
			}
		case sig == unix.SIGTRAP && cause == 0:
			// plain SIGTRAP from an exec without PTRACE_O_TRACEEXEC: swallow
		default:
			deliver = int(sig)
		}
		if inflight[pid] != nil {
			// A signal arrived between syscall entry and exit; keep
			// stopping at the exit so the result is not lost.
			unix.PtraceSyscall(pid, deliver)
			continue
		}
		unix.PtraceCont(pid, deliver)
	}
	return res, nil
}

// syscallEntry decodes a trapped syscall and resolves its path arguments.
func syscallEntry(pid int) *pending {
	r, err := getRegs(pid)
	if err != nil {
		return nil
	}
	at := func(dirfd uint64, addr uint64) string {
		p, err := readString(pid, uintptr(addr))
		if err != nil || p == "" {
			return ""
		}
		return resolve(pid, int32(dirfd), p)
	}
	writeFlags := func(flags uint64) bool {
		return flags&(unix.O_WRONLY|unix.O_RDWR|unix.O_CREAT|unix.O_TRUNC) != 0
	}
	p := &pending{}
	switch nr := r.nr(); nr {
	case unix.SYS_OPENAT:
		p.paths = []string{at(r.arg(0), r.arg(1))}
		p.write, p.dir = writeFlags(r.arg(2)), r.arg(2)&unix.O_DIRECTORY != 0
	case unix.SYS_OPENAT2:
		var how [8]byte
		if n, _ := unix.ProcessVMReadv(pid, []unix.Iovec{{Base: &how[0], Len: 8}},
			[]unix.RemoteIovec{{Base: uintptr(r.arg(2)), Len: 8}}, 0); n == 8 {
			flags := uint64(how[0]) | uint64(how[1])<<8 | uint64(how[2])<<16 | uint64(how[3])<<24
			p.write, p.dir = writeFlags(flags), flags&unix.O_DIRECTORY != 0
		}
		p.paths = []string{at(r.arg(0), r.arg(1))}
	case unix.SYS_RENAMEAT, unix.SYS_RENAMEAT2:
		p.paths, p.write = []string{at(r.arg(2), r.arg(3))}, true
	default:
		if q := legacySyscall(pid, nr, r, at, writeFlags); q != nil {
			p = q
		} else {
			return nil
		}
	}
	if len(p.paths) == 0 || p.paths[0] == "" {
		return nil
	}
	return p
}

const atFDCWD = -100

// resolve turns a (dirfd, path) pair into an absolute path using /proc.
func resolve(pid int, dirfd int32, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	var base string
	var err error
	if dirfd == atFDCWD {
		base, err = os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	} else {
		base, err = os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", pid, dirfd))
	}
	if err != nil {
		return ""
	}
	return filepath.Join(base, p)
}

// readString reads a NUL-terminated string from the tracee.
func readString(pid int, addr uintptr) (string, error) {
	var out []byte
	buf := make([]byte, 256)
	for len(out) < unix.PathMax {
		// Do not cross a page boundary in one read: the next page may be unmapped.
		n := len(buf)
		if rem := 4096 - int(addr%4096); rem < n {
			n = rem
		}
		got, err := unix.ProcessVMReadv(pid, []unix.Iovec{{Base: &buf[0], Len: uint64(n)}},
			[]unix.RemoteIovec{{Base: addr, Len: n}}, 0)
		if err != nil {
			return "", err
		}
		if i := bytes.IndexByte(buf[:got], 0); i >= 0 {
			return string(append(out, buf[:i]...)), nil
		}
		out = append(out, buf[:got]...)
		addr += uintptr(got)
	}
	return "", errors.New("path too long")
}

func readCmdline(pid int) []string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}
