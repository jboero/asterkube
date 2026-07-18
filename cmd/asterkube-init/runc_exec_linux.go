/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

// `runc exec`: run a process inside an already-running container — what
// `kubectl exec` drives (apiserver -> kubelet -> containerd -> shim -> here).
//
// Asterinas has no setns(CLONE_NEWNS) and no setns(CLONE_NEWPID), so we can't
// join the container's mount/pid namespaces directly. We approximate the mount
// namespace by chroot into the container's rootfs (still visible from the host
// mount namespace, since the container's pivot_root only affects its own ns),
// and join net/ipc/uts/cgroup via setns — enough for an interactive shell.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// execProcess is the subset of the OCI Process spec the shim writes to the
// file named by `runc exec --process <file>`.
type execProcess struct {
	Terminal bool     `json:"terminal"`
	Cwd      string   `json:"cwd"`
	Env      []string `json:"env"`
	Args     []string `json:"args"`
	User     struct {
		UID uint32 `json:"uid"`
		GID uint32 `json:"gid"`
	} `json:"user"`
}

const (
	ioctlTIOCSPTLCK = 0x40045431 // unlockpt
	ioctlTIOCGPTN   = 0x80045430 // ptsname (pty number)
)

func cmdExec(g runtimeGlobals, args []string) error {
	var processPath, consoleSocket, pidFile, id string
	var detach, tty bool
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--process" || a == "-p":
			i++
			processPath = args[i]
		case strings.HasPrefix(a, "--process="):
			processPath = strings.TrimPrefix(a, "--process=")
		case a == "--console-socket":
			i++
			consoleSocket = args[i]
		case strings.HasPrefix(a, "--console-socket="):
			consoleSocket = strings.TrimPrefix(a, "--console-socket=")
		case a == "--pid-file":
			i++
			pidFile = args[i]
		case strings.HasPrefix(a, "--pid-file="):
			pidFile = strings.TrimPrefix(a, "--pid-file=")
		case a == "-d" || a == "--detach":
			detach = true
		case a == "-t" || a == "--tty":
			tty = true
		case strings.HasPrefix(a, "-"):
			// Unknown flag; if it looks like it takes a value, skip it.
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		default:
			id = a
		}
	}
	if id == "" {
		return fmt.Errorf("exec: no container id")
	}
	st, err := readState(g, id)
	if err != nil {
		return fmt.Errorf("exec: read state %s: %w", id, err)
	}
	if st.InitPid <= 0 || !pidAlive(st.InitPid) {
		return fmt.Errorf("exec: container %s is not running", id)
	}

	var p execProcess
	if processPath != "" {
		data, err := os.ReadFile(processPath)
		if err != nil {
			return fmt.Errorf("exec: read process spec: %w", err)
		}
		if err := json.Unmarshal(data, &p); err != nil {
			return fmt.Errorf("exec: parse process spec: %w", err)
		}
	}
	if len(p.Args) == 0 {
		return fmt.Errorf("exec: no command in process spec")
	}
	if p.Cwd == "" {
		p.Cwd = "/"
	}
	if tty {
		p.Terminal = true
	}

	// Resolve the command against the container's PATH, inside its rootfs, so we
	// pass execve an absolute in-container path (execve runs post-chroot and does
	// no PATH lookup).
	argv0, err := resolveInRootfs(st.Rootfs, p.Args[0], p.Env)
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}

	// Join the container namespaces we can (net/ipc/uts/cgroup). The mount ns is
	// handled by chroot below; pid-ns setns is unsupported by the kernel.
	for _, ns := range []struct {
		name string
		flag uintptr
	}{
		{"net", syscall.CLONE_NEWNET}, {"ipc", syscall.CLONE_NEWIPC},
		{"uts", syscall.CLONE_NEWUTS}, {"cgroup", syscall.CLONE_NEWCGROUP},
	} {
		fd, err := syscall.Open(fmt.Sprintf("/proc/%d/ns/%s", st.InitPid, ns.name), syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		syscall.Syscall(sysSetns, uintptr(fd), ns.flag, 0)
		syscall.Close(fd)
	}

	// Name resolution: the container's /etc/resolv.conf is a bind-mount in its
	// mount namespace, which we can't join, so the chrooted exec would have no
	// resolver. Mirror the container's *actual* resolv.conf — read through
	// /proc/<init>/root (the container's mount view) — into the rootfs the exec
	// chroots to, so the exec session resolves names exactly like the container
	// does. Fall back to the node's resolv.conf. Best-effort.
	for _, src := range []string{
		fmt.Sprintf("/proc/%d/root/etc/resolv.conf", st.InitPid),
		"/etc/resolv.conf",
	} {
		data, err := os.ReadFile(src)
		if err != nil || len(data) == 0 {
			continue
		}
		// containerd may leave an empty directory as the bind-mount target for the
		// container's resolv.conf; the chrooted exec can't write a file over it, so
		// replace it.
		target := st.Rootfs + "/etc/resolv.conf"
		if fi, e := os.Lstat(target); e == nil && fi.IsDir() {
			_ = os.Remove(target)
		}
		_ = os.WriteFile(target, data, 0o644)
		break
	}

	attr := &syscall.SysProcAttr{Chroot: st.Rootfs}
	if p.User.UID != 0 || p.User.GID != 0 {
		attr.Credential = &syscall.Credential{Uid: p.User.UID, Gid: p.User.GID}
	}

	env := p.Env
	hasPath := false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			hasPath = true
			break
		}
	}
	if !hasPath {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}

	var master, slave *os.File
	files := []*os.File{os.Stdin, os.Stdout, os.Stderr}
	if p.Terminal && consoleSocket != "" {
		m, slavePath, err := openPTY()
		if err != nil {
			return fmt.Errorf("exec: openpty: %w", err)
		}
		master = m
		slave, err = os.OpenFile(slavePath, os.O_RDWR, 0)
		if err != nil {
			master.Close()
			return fmt.Errorf("exec: open %s: %w", slavePath, err)
		}
		files = []*os.File{slave, slave, slave}
		attr.Setsid = true
		attr.Setctty = true
		attr.Ctty = 0
	}

	proc, err := os.StartProcess(argv0, p.Args, &os.ProcAttr{
		Dir:   p.Cwd,
		Env:   env,
		Files: files,
		Sys:   attr,
	})
	if err != nil {
		if master != nil {
			master.Close()
			slave.Close()
		}
		return fmt.Errorf("exec: start %v: %w", p.Args, err)
	}

	if pidFile != "" {
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(proc.Pid)), 0o644)
	}

	if master != nil {
		slave.Close() // the child holds the slave; the shim gets the master
		if err := sendTerminalFD(consoleSocket, int(master.Fd())); err != nil {
			master.Close()
			return fmt.Errorf("exec: send console fd: %w", err)
		}
		master.Close()
		return nil // detached: the shim owns the pty and reaps the process
	}

	if detach {
		return nil
	}
	state, _ := proc.Wait()
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.ExitStatus() != 0 {
		os.Exit(ws.ExitStatus())
	}
	return nil
}

// resolveInRootfs finds cmd within the container's rootfs and returns its path
// as seen AFTER chroot (i.e. without the rootfs prefix), so it can be execve'd
// in the chrooted child.
func resolveInRootfs(rootfs, cmd string, env []string) (string, error) {
	if strings.Contains(cmd, "/") {
		abs := cmd
		if !strings.HasPrefix(abs, "/") {
			abs = "/" + abs
		}
		if _, err := os.Stat(rootfs + abs); err == nil {
			return abs, nil
		}
		return "", fmt.Errorf("%q not found in container", cmd)
	}
	pathEnv := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			pathEnv = strings.TrimPrefix(e, "PATH=")
			break
		}
	}
	for _, dir := range strings.Split(pathEnv, ":") {
		if dir == "" {
			continue
		}
		cand := strings.TrimRight(dir, "/") + "/" + cmd
		if fi, err := os.Stat(rootfs + cand); err == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	return "", fmt.Errorf("%q not found in container PATH", cmd)
}

// openPTY allocates a pseudo-terminal via /dev/ptmx and returns the master file
// and the slave's path (/dev/pts/N).
func openPTY() (*os.File, string, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), ioctlTIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		m.Close()
		return nil, "", fmt.Errorf("unlockpt: %v", e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), ioctlTIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		m.Close()
		return nil, "", fmt.Errorf("ptsname: %v", e)
	}
	return m, fmt.Sprintf("/dev/pts/%d", n), nil
}

// sendTerminalFD passes the pty master fd to the shim over the console socket
// (SCM_RIGHTS), the runc terminal protocol.
func sendTerminalFD(socketPath string, fd int) error {
	c, err := net.Dial("unix", socketPath)
	if err != nil {
		return err
	}
	defer c.Close()
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("console socket is not a unix socket")
	}
	_, _, err = uc.WriteMsgUnix([]byte("\n"), syscall.UnixRights(fd), nil)
	return err
}
