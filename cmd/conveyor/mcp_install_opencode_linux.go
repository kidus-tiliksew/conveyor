package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"
)

const (
	openCodeCapNetAdmin         = 12
	openCodePrSetPdeathsig      = 1
	openCodePrSetNoNewPrivs     = 38
	openCodePrCapAmbient        = 47
	openCodePrCapAmbientClear   = 4
	openCodeCapabilityVersion3  = 0x20080522
	openCodeInitRefusalExitCode = 125
)

// openCodeContained starts this executable again as the init of new user,
// PID, and network namespaces. The helper raises the private loopback and
// then becomes the client, so the client is the namespace init. When that
// init ends, the kernel kills each process in the namespace, including one
// that started a new session. The client cannot reach a host port, so it can
// neither contact nor displace the OpenCode service of the user. Only the
// caller's own UID and GID are mapped. The new session has no terminal, so
// no client output reaches it (component-cli-onboarding invariant 11).
func openCodeContained(binary string, args []string, status *os.File) (*exec.Cmd, error) {
	uid, gid := os.Geteuid(), os.Getegid()
	return &exec.Cmd{
		Path:       "/proc/self/exe",
		Args:       append([]string{"conveyor", openCodeInitArg, binary}, args...),
		ExtraFiles: []*os.File{status},
		SysProcAttr: &syscall.SysProcAttr{
			Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNET,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
			AmbientCaps: []uintptr{openCodeCapNetAdmin},
			Setsid:      true,
		},
	}, nil
}

// runOpenCodeInit is the namespace helper. It refuses unless it is a
// namespace init with a private loopback and one of the check's commands. It
// returns only when it did not become the client.
func runOpenCodeInit(args []string) int {
	runtime.LockOSThread()
	if err := prepareOpenCodeInit(args); err != nil {
		fmt.Fprintln(os.Stderr, "error: this internal command runs only inside the OpenCode check")
		return openCodeInitRefusalExitCode
	}
	// The write fails when the installer is gone, and then no client starts.
	if _, err := syscall.Write(openCodeStatusFD, []byte{openCodeReady}); err != nil {
		return openCodeInitRefusalExitCode
	}
	_ = syscall.Exec(args[0], args, os.Environ())
	_, _ = syscall.Write(openCodeStatusFD, []byte{openCodeExecFailed})
	return openCodeInitRefusalExitCode
}

func prepareOpenCodeInit(args []string) error {
	if os.Getpid() != 1 || len(args) < 2 {
		return syscall.EINVAL
	}
	command := fmt.Sprintf("%q", args[1:])
	if command != `["--version"]` && command != `["debug" "config"]` && command != `["debug" "config" "--standalone"]` {
		return syscall.EINVAL
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(openCodeStatusFD, &stat); err != nil {
		return err
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		return syscall.EINVAL
	}
	// Pass only the standard streams. The status pipe and each descriptor
	// that the installer inherited close on exec.
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if fd, parseErr := strconv.Atoi(entry.Name()); parseErr == nil && fd > 2 {
			syscall.CloseOnExec(fd)
		}
	}
	if err := raiseOpenCodeLoopback(); err != nil {
		return err
	}
	return dropOpenCodePrivileges()
}

// raiseOpenCodeLoopback sets the loopback interface up. A new network
// namespace starts with it down, so an interface that is already up is not
// private and refuses. The socket closes before the client starts.
func raiseOpenCodeLoopback() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	request := struct {
		name  [syscall.IFNAMSIZ]byte
		flags uint16
		_     [22]byte
	}{}
	copy(request.name[:], "lo")
	ioctl := func(operation uintptr) error {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), operation, uintptr(unsafe.Pointer(&request))); errno != 0 {
			return errno
		}
		return nil
	}
	if err := ioctl(syscall.SIOCGIFFLAGS); err != nil {
		return err
	}
	if request.flags&syscall.IFF_UP != 0 {
		return syscall.EEXIST
	}
	request.flags |= syscall.IFF_UP
	if err := ioctl(syscall.SIOCSIFFLAGS); err != nil {
		return err
	}
	if err := ioctl(syscall.SIOCGIFFLAGS); err != nil {
		return err
	}
	if request.flags&syscall.IFF_UP == 0 {
		return syscall.ENETDOWN
	}
	return nil
}

// dropOpenCodePrivileges removes the namespace capabilities before the client
// starts and ties the namespace to the installer, so that an installer that
// is killed leaves no client.
func dropOpenCodePrivileges() error {
	prctl := func(option, first, second uintptr) error {
		if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, option, first, second, 0, 0, 0); errno != 0 {
			return errno
		}
		return nil
	}
	if err := prctl(openCodePrCapAmbient, openCodePrCapAmbientClear, 0); err != nil {
		return err
	}
	header := struct {
		version uint32
		pid     int32
	}{version: openCodeCapabilityVersion3}
	var data [2]struct{ effective, permitted, inheritable uint32 }
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data)), 0); errno != 0 {
		return errno
	}
	if err := prctl(openCodePrSetNoNewPrivs, 1, 0); err != nil {
		return err
	}
	return prctl(openCodePrSetPdeathsig, uintptr(syscall.SIGKILL), 0)
}
