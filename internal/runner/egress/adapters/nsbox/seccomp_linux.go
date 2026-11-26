//go:build linux

package nsbox

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Socket families that namespaces do not confine (AF_VSOCK reaches the WSL2/Hyper-V host whatever the network
// namespace; Bluetooth, CAN and others are not filtered by nftables) are refused with a seccomp filter installed
// on every thread of the helper and inherited by the agent: only Unix, IPv4, IPv6 and netlink sockets (all scoped
// to the sandbox's namespaces) may be created. io_uring is refused too (it can open sockets without the socket
// syscall), and so is any other syscall ABI (x32, 32-bit compat).
var allowedFamilies = []uint32{unix.AF_UNIX, unix.AF_INET, unix.AF_INET6, unix.AF_NETLINK}

type seccompABI struct {
	arch               uint32
	socket, socketpair uint32
}

func abi() (seccompABI, error) {
	switch runtime.GOARCH {
	case "amd64":
		return seccompABI{arch: unix.AUDIT_ARCH_X86_64, socket: 41, socketpair: 53}, nil
	case "arm64":
		return seccompABI{arch: unix.AUDIT_ARCH_AARCH64, socket: 198, socketpair: 199}, nil
	}
	return seccompABI{}, fmt.Errorf("no seccomp filter for %s", runtime.GOARCH)
}

const (
	sysIOUringSetup = 425
	x32Bit          = 0x40000000 // __X32_SYSCALL_BIT
	// Offsets in struct seccomp_data.
	offNR   = 0
	offArch = 4
	offArg0 = 16 // low 32 bits on little-endian amd64/arm64
)

func stmt(code uint16, k uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: k} }

func jump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

// filter builds the BPF program.
func filter(a seccompABI) []unix.SockFilter {
	const (
		ld   = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq  = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jset = unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K
		ret  = unix.BPF_RET | unix.BPF_K
	)
	allow := uint32(unix.SECCOMP_RET_ALLOW)
	errno := func(e unix.Errno) uint32 { return unix.SECCOMP_RET_ERRNO | uint32(e) } //nolint:gosec // G115: small errno

	n := uint8(len(allowedFamilies)) //nolint:gosec // G115: four families
	p := []unix.SockFilter{
		stmt(ld, offArch),
		jump(jeq, a.arch, 1, 0),
		stmt(ret, unix.SECCOMP_RET_KILL_PROCESS), // another ABI
		stmt(ld, offNR),
		jump(jset, x32Bit, 0, 1),
		stmt(ret, errno(unix.ENOSYS)), // x32 syscalls
		jump(jeq, sysIOUringSetup, 0, 1),
		stmt(ret, errno(unix.ENOSYS)),
		jump(jeq, a.socket, 2, 0),
		jump(jeq, a.socketpair, 1, 0),
		stmt(ret, allow),
		stmt(ld, offArg0),
	}
	for i, f := range allowedFamilies {
		// On a match jump to the final "allow" after the remaining checks and the refusal.
		p = append(p, jump(jeq, f, n-uint8(i), 0))
	}
	return append(p, stmt(ret, errno(unix.EAFNOSUPPORT)), stmt(ret, allow))
}

// confineSockets installs the filter on every thread of this process (TSYNC); children inherit it.
func confineSockets() error {
	a, err := abi()
	if err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	f := filter(a)
	prog := unix.SockFprog{Len: uint16(len(f)), Filter: &f[0]} //nolint:gosec // G115: a short fixed program
	_, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC,
		uintptr(unsafe.Pointer(&prog))) //nolint:gosec // G103: seccomp(2) takes a pointer to the program
	runtime.KeepAlive(f)
	if e != 0 {
		return fmt.Errorf("installing the seccomp filter: %w", e)
	}
	return nil
}

// vsockBlocked tries to open a vsock socket (the WSL2/Hyper-V host channel): it must be refused.
func vsockBlocked() bool {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err == nil {
		_ = unix.Close(fd)
		return false
	}
	return err != nil
}
