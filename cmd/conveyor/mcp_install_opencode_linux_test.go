package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() { openCodeFixture = openCodeLinuxFixture }

// openCodeLinuxFixture is the client side of the namespace tests.
//
//	client <directory> <mode>      starts a stopped descendant, then exits or waits
//	descendant <directory>         holds a file lock in a new session, then stops
//	owner <directory> <binary>     runs a check that waits, for the crash test
//	inspect <directory> <socket>   reports what a client can see and reach
func openCodeLinuxFixture(args []string) int {
	if len(args) < 2 {
		return 125
	}
	directory := args[1]
	fail := func(err error) int {
		_ = os.WriteFile(filepath.Join(directory, "fixture-error"), []byte(err.Error()), 0o600)
		return 124
	}
	switch args[0] {
	case "descendant":
		lock, err := os.OpenFile(filepath.Join(directory, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
		if err == nil {
			err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		}
		if err != nil {
			return fail(err)
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
		// A continued descendant keeps the lock until it is killed.
		select {}
	case "client":
		command := exec.Command("/proc/self/exe", openCodeFixtureArg, "descendant", directory)
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := command.Start(); err != nil {
			return fail(err)
		}
		// The kernel reports the stop, so no sleep orders the two processes.
		var status syscall.WaitStatus
		if _, err := syscall.Wait4(command.Process.Pid, &status, syscall.WUNTRACED, nil); err != nil || !status.Stopped() {
			return fail(fmt.Errorf("descendant status %v: %v", status, err))
		}
		ready, err := os.OpenFile(filepath.Join(directory, "ready"), os.O_WRONLY, 0)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(ready, "stopped")
		ready.Close()
		switch args[2] {
		case "success":
			fmt.Println("resolved-secret")
			return 0
		case "failure":
			return 7
		case "flood":
			for {
				fmt.Println("resolved-secret")
			}
		}
		select {}
	case "owner":
		openCodeContain = openCodeContained
		_, _ = openCodeRun{binary: args[2], args: []string{"--version"}, dir: directory, env: os.Environ(), limit: openCodeVersionLimit}.execute(context.Background())
		return 0
	case "inspect":
		status, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return fail(err)
		}
		for _, line := range strings.Split(string(status), "\n") {
			for _, field := range []string{"CapInh:", "CapPrm:", "CapEff:", "CapAmb:", "NoNewPrivs:", "Uid:", "Gid:"} {
				if strings.HasPrefix(line, field) {
					fmt.Println(strings.Join(strings.Fields(line), " "))
				}
			}
		}
		links, _ := filepath.Glob("/proc/self/fd/*")
		for _, link := range links {
			// The read of the directory and the runtime poller are its own.
			if target, err := os.Readlink(link); err == nil && !strings.HasPrefix(target, "anon_inode:") && !strings.HasPrefix(target, "/proc/") {
				fmt.Println("fd", filepath.Base(link), strings.SplitN(target, ":", 2)[0])
			}
		}
		fmt.Println("pid", os.Getpid())
		interfaces, err := net.Interfaces()
		if err != nil {
			return fail(err)
		}
		for _, item := range interfaces {
			fmt.Println("interface", item.Name, item.Flags&net.FlagUp != 0)
		}
		if connection, err := net.Dial("tcp", args[2]); err == nil {
			connection.Close()
			fmt.Println("host reached")
		}
		// The port of the host is free here, as it is for a private server.
		if listener, err := net.Listen("tcp", args[2]); err != nil {
			fmt.Println("private listen failed")
		} else {
			if connection, err := net.Dial("tcp", args[2]); err != nil {
				fmt.Println("private loopback failed")
			} else {
				connection.Close()
			}
			listener.Close()
		}
		return 0
	}
	return 125
}

// requireOpenCodeNamespaces skips a test on a host that denies the check its
// namespaces. CONVEYOR_TEST_OPENCODE_NAMESPACES=1 turns the skip into a
// failure, so that a recorded run on a capable host cannot pass by skipping.
func requireOpenCodeNamespaces(t *testing.T) {
	t.Helper()
	useOpenCodeContainment(t)
	run := openCodeRun{binary: openCodeValidationBinary(t, "exit 0"), args: []string{"--version"}, dir: t.TempDir(), env: os.Environ(), limit: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := run.execute(ctx)
	if errors.Is(err, errOpenCodeContainment) && os.Getenv("CONVEYOR_TEST_OPENCODE_NAMESPACES") != "1" {
		t.Skip("this host denies user, PID, or network namespaces; no namespace evidence")
	}
	if err != nil {
		t.Fatal(err)
	}
}

func openCodeFixtureBinary(t *testing.T, args ...string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	line := "exec " + shellQuoteMCP(executable) + " " + openCodeFixtureArg
	for _, arg := range args {
		line += " " + shellQuoteMCP(arg)
	}
	return openCodeValidationBinary(t, line)
}

// openCodeFixtureDirectory holds the lock and the ready pipe of one fixture.
func openCodeFixtureDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(directory, "ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	return directory
}

// awaitOpenCodeDescendant returns a channel that closes when the fixture
// reports its stopped descendant. The read opens the pipe without blocking,
// and the poller then waits for the writer.
func awaitOpenCodeDescendant(t *testing.T, directory string) <-chan struct{} {
	t.Helper()
	// Read and write keeps the pipe open, so the read waits for the report.
	ready, err := os.OpenFile(filepath.Join(directory, "ready"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		if line, _ := bufio.NewReader(ready).ReadString('\n'); line == "stopped\n" {
			close(stopped)
		}
	}()
	t.Cleanup(func() { ready.Close() })
	return stopped
}

// openCodeLockFree reports whether no process holds the descendant's lock.
// The kernel drops the lock when the descendant ends.
func openCodeLockFree(t *testing.T, directory string, operation int) bool {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(directory, "lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("the descendant created no lock: %v", err)
	}
	defer lock.Close()
	return syscall.Flock(int(lock.Fd()), operation) == nil
}

// TestOpenCodeContainmentEndsStoppedDescendant proves that no process of the
// check remains when the runner returns. The descendant starts a new session,
// inherits the output pipes, and is stopped, so a group signal, a closed
// pipe, and a termination signal all miss it. An unrelated process survives.
func TestOpenCodeContainmentEndsStoppedDescendant(t *testing.T) {
	requireOpenCodeNamespaces(t)
	unrelated := exec.Command("/bin/sh", "-c", "read -r ignored")
	stdin, err := unrelated.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[string]error{"success": nil, "failure": nil, "wait": errOpenCodeTimeout, "flood": errOpenCodeOutputSize} {
		t.Run(mode, func(t *testing.T) {
			directory := openCodeFixtureDirectory(t)
			stopped := awaitOpenCodeDescendant(t, directory)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if mode == "wait" {
				// The deadline passes after the descendant stopped.
				go func() { <-stopped; cancel() }()
			}
			run := openCodeRun{binary: openCodeFixtureBinary(t, "client", directory, mode), args: []string{"--version"}, dir: directory, env: os.Environ(), limit: openCodeVersionLimit}
			result, err := run.execute(ctx)
			if !openCodeLockFree(t, directory, syscall.LOCK_EX|syscall.LOCK_NB) {
				t.Fatal("a descendant outlived the check")
			}
			select {
			case <-stopped:
			case <-time.After(10 * time.Second):
				t.Fatal("the descendant did not stop before the check ended")
			}
			if !errors.Is(err, want) || (want != nil && strings.Contains(err.Error(), "resolved-secret")) {
				t.Fatalf("err=%v, want %v", err, want)
			}
			if result.failed != (mode == "failure") || (mode == "success") != (string(result.stdout) == "resolved-secret\n") {
				t.Fatalf("result=%+v", result)
			}
			if message, err := os.ReadFile(filepath.Join(directory, "fixture-error")); err == nil {
				t.Fatalf("fixture: %s", message)
			}
		})
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("an unrelated process ended: %v", err)
	}
	stdin.Close()
	_ = unrelated.Wait()
}

// TestOpenCodeContainmentEndsWithInstaller kills the installer during a
// check. The kernel then ends the namespace.
func TestOpenCodeContainmentEndsWithInstaller(t *testing.T) {
	requireOpenCodeNamespaces(t)
	directory := openCodeFixtureDirectory(t)
	stopped := awaitOpenCodeDescendant(t, directory)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owner := exec.Command(executable, openCodeFixtureArg, "owner", directory, openCodeFixtureBinary(t, "client", directory, "wait"))
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() { _ = owner.Wait(); close(ended) }()
	select {
	case <-stopped:
	case <-ended:
		t.Fatal("the installer fixture ended early")
	case <-time.After(60 * time.Second):
		t.Fatal("the descendant did not stop")
	}
	if openCodeLockFree(t, directory, syscall.LOCK_EX|syscall.LOCK_NB) {
		t.Fatal("the descendant held no lock")
	}
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-ended
	free := make(chan bool, 1)
	go func() { free <- openCodeLockFree(t, directory, syscall.LOCK_EX) }()
	select {
	case ok := <-free:
		if !ok {
			t.Fatal("the lock was not read")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the client outlived the installer")
	}
}

// TestOpenCodeContainmentIsolatesClient proves what the client holds: no
// capability, no inherited descriptor, no host network, and a private
// loopback on which the port of a host service is free.
func TestOpenCodeContainmentIsolatesClient(t *testing.T) {
	requireOpenCodeNamespaces(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if connection, err := listener.Accept(); err == nil {
			connection.Close()
			accepted <- struct{}{}
		}
	}()
	// A descriptor without close-on-exec, as a parent process can leave one.
	var leaked [2]int
	if err := syscall.Pipe(leaked[:]); err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(leaked[0])
	defer syscall.Close(leaked[1])
	directory := t.TempDir()
	run := openCodeRun{binary: openCodeFixtureBinary(t, "inspect", directory, listener.Addr().String()), args: []string{"--version"}, dir: directory, env: os.Environ(), limit: openCodeVersionLimit}
	result, err := run.execute(context.Background())
	if err != nil || result.failed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	uid, gid := os.Geteuid(), os.Getegid()
	none := "0000000000000000"
	want := fmt.Sprintf("Uid: %d %d %d %d\nGid: %d %d %d %d\n", uid, uid, uid, uid, gid, gid, gid, gid) +
		"CapInh: " + none + "\nCapPrm: " + none + "\nCapEff: " + none + "\nCapAmb: " + none + "\nNoNewPrivs: 1\n" +
		"fd 0 /dev/null\nfd 1 pipe\nfd 2 pipe\npid 1\ninterface lo true\n"
	if string(result.stdout) != want {
		t.Fatalf("client sees:\n%s\nwant:\n%s", result.stdout, want)
	}
	select {
	case <-accepted:
		t.Fatal("the client reached a host port")
	default:
	}
}

// TestOpenCodeContainmentSetupRefusal denies the setup in the ways that a
// host can and proves that no client starts. Each case is deterministic with
// and without namespace access.
func TestOpenCodeContainmentSetupRefusal(t *testing.T) {
	previous := openCodeContain
	t.Cleanup(func() { openCodeContain = previous })
	for label, deny := range map[string]func(*exec.Cmd){
		// The helper is not a namespace init.
		"no namespaces": func(cmd *exec.Cmd) { cmd.SysProcAttr = nil },
		// The loopback is the host's own, and the helper cannot change it.
		"no network namespace": func(cmd *exec.Cmd) { cmd.SysProcAttr.Cloneflags &^= syscall.CLONE_NEWNET },
		// A host that limits unprivileged namespaces refuses the ioctl.
		"no network capability": func(cmd *exec.Cmd) {
			cmd.SysProcAttr.AmbientCaps = nil
			if os.Geteuid() == 0 {
				cmd.SysProcAttr.Cloneflags &^= syscall.CLONE_NEWNET
			}
		},
		"unmapped user": func(cmd *exec.Cmd) {
			cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: 0, Size: 1 << 20}}
			cmd.SysProcAttr.Cloneflags &^= syscall.CLONE_NEWNET
		},
		"no helper":      func(cmd *exec.Cmd) { cmd.Path = "/nonexistent/conveyor" },
		"no status pipe": func(cmd *exec.Cmd) { cmd.ExtraFiles = nil },
		"other command":  func(cmd *exec.Cmd) { cmd.Args = append(cmd.Args, "extra") },
	} {
		t.Run(label, func(t *testing.T) {
			openCodeContain = func(binary string, args []string, status *os.File) (*exec.Cmd, error) {
				cmd, err := openCodeContained(binary, args, status)
				if err == nil {
					deny(cmd)
				}
				return cmd, err
			}
			binary := openCodeValidationBinary(t, `echo "$*" >> "$ARGUMENT_LOG"; echo 'opencode v2.4.1'`)
			checkOpenCodeRefusal(t, mcpAuthOAuth, binary, errOpenCodeContainment, "")
		})
	}
}

// TestOpenCodeInitRefusesDirectUse proves that a person who starts the
// internal command outside the check starts no client.
func TestOpenCodeInitRefusesDirectUse(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "started")
	binary := openCodeValidationBinary(t, "echo started > "+shellQuoteMCP(marker))
	for _, args := range [][]string{nil, {binary}, {binary, "--version"}, {binary, "serve"}} {
		output, err := exec.Command(executable, append([]string{openCodeInitArg}, args...)...).CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != openCodeInitRefusalExitCode || !strings.Contains(string(output), "only inside the OpenCode check") {
			t.Fatalf("args=%v output=%s err=%v", args, output, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a client started: %v", err)
	}
}

// TestOpenCodeContainedInstall runs the whole transaction in the production
// containment with stubs for both majors.
func TestOpenCodeContainedInstall(t *testing.T) {
	requireOpenCodeNamespaces(t)
	id := testMCPOAuthIdentity(t, "https://factory.example", "")
	entry := `"` + id.name + `":{"type":"remote","url":"` + id.endpoint + `"}`
	for label, c := range map[string]struct{ binary, command string }{
		"major 1": {openCodeModeBinary(t, mcpAuthOAuth, ""), openCodeConfigCommand},
		"major 2": {openCodeV2Binary(t, openCodeV2Rows(entry)), openCodeConfigCommandV2},
	} {
		t.Run(label, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "opencode.json")
			result, err := reconcileNamedMCPRegistration(filepath.Dir(path), mcpInstallTarget{tool: "opencode", path: path}, id, false, true, func(string) (string, error) { return c.binary, nil })
			if err != nil || result.validation != "validated with "+c.command+" (not a native connection check)" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
	t.Run("server start", func(t *testing.T) {
		binary := openCodeV2Binary(t, `echo 'Error: Server process exited with code 1 resolved-secret' >&2; exit 1`)
		checkOpenCodeRefusal(t, mcpAuthOAuth, binary, errOpenCodeServerStart, "--version\ndebug config --standalone\n")
	})
	t.Run("missing executable", func(t *testing.T) {
		checkOpenCodeRefusal(t, mcpAuthOAuth, filepath.Join(t.TempDir(), "absent"), errOpenCodeLaunch, "")
	})
}
