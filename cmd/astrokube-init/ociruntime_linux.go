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

// A pure-Go OCI runtime — a libc-free, CGO_ENABLED=0 replacement for runc.
//
// runc is the only piece of the container stack that fundamentally needs C (its
// libcontainer/nsenter `nsexec` is cgo). This package implements the subset of
// the runc CLI that containerd's shim (io.containerd.runc.v2) drives —
// create/start/state/kill/delete/version — entirely in Go, so a node image can
// run real OCI containers with zero C. Namespace creation uses Go's native
// clone()-with-flags (SysProcAttr.Cloneflags) for fresh namespaces, the same
// mechanism the node agent already uses; the create/start handshake uses the
// OCI exec.fifo so the container is fully set up at `create` and only execs the
// user process at `start`.
//
// Scope: containers whose namespaces are created fresh (e.g. `ctr run`, and pod
// sandboxes). Joining an existing PID namespace via setns (shareProcessNamespace,
// `exec` into a running container) is intentionally out of scope here.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---- minimal OCI runtime-spec subset (config.json) ----

type ociSpec struct {
	OCIVersion string      `json:"ociVersion"`
	Process    *ociProcess `json:"process"`
	Root       *ociRoot    `json:"root"`
	Hostname   string      `json:"hostname"`
	Mounts     []ociMount  `json:"mounts"`
	Linux      *ociLinux   `json:"linux"`
}
type ociProcess struct {
	Terminal bool     `json:"terminal"`
	Cwd      string   `json:"cwd"`
	Env      []string `json:"env"`
	Args     []string `json:"args"`
}
type ociRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}
type ociMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options"`
}
type ociLinux struct {
	Namespaces  []ociNamespace `json:"namespaces"`
	Resources   *ociResources  `json:"resources"`
	CgroupsPath string         `json:"cgroupsPath"`
}
type ociNamespace struct {
	Type string `json:"type"`
	Path string `json:"path"`
}
type ociResources struct {
	Memory *struct {
		Limit *int64 `json:"limit"`
	} `json:"memory"`
	CPU *struct {
		Quota  *int64  `json:"quota"`
		Period *uint64 `json:"period"`
	} `json:"cpu"`
}

// ---- our on-disk container state (<root>/<id>/state.json) ----

type containerState struct {
	OCIVersion string `json:"ociVersion"`
	ID         string `json:"id"`
	InitPid    int    `json:"pid"`
	Status     string `json:"status"` // created | running | stopped
	Bundle     string `json:"bundle"`
	Rootfs     string `json:"rootfs"`
	Created    string `json:"created"`
	CgroupPath string `json:"cgroupPath"`
}

// runtimeGlobals are the flags containerd's shim passes before the subcommand.
type runtimeGlobals struct {
	root string
	log  string
}

const defaultRuntimeRoot = "/run/astrokube-runc"

// oPath is O_PATH on linux/amd64 (Go's syscall package omits it). It opens a
// reference to a path without blocking or needing read/write access, so the
// container init can pin the exec.fifo before pivot_root moves it out of view.
const oPath = 0x200000

// runOCIRuntime is the CLI entry point used when this binary is invoked as
// `runc`. It returns the process exit code.
func runOCIRuntime(args []string) int {
	g := runtimeGlobals{root: defaultRuntimeRoot}
	// Parse global flags up to the subcommand.
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--root":
			i++
			g.root = args[i]
		case strings.HasPrefix(a, "--root="):
			g.root = strings.TrimPrefix(a, "--root=")
		case a == "--log":
			i++
			g.log = args[i]
		case strings.HasPrefix(a, "--log="):
			g.log = strings.TrimPrefix(a, "--log=")
		case a == "--log-format", a == "--systemd-cgroup", a == "--debug", a == "--rootless":
			// boolean/handled flags we accept and ignore
		case a == "--log-format=text", a == "--log-format=json":
		case strings.HasPrefix(a, "--"):
			// unknown global flag, possibly with a value; skip a trailing value
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && isGlobalValueFlag(a) {
				i++
			}
		default:
			goto dispatch
		}
		i++
	}
dispatch:
	if i >= len(args) {
		runcLog(g, "no subcommand")
		return 1
	}
	cmd, rest := args[i], args[i+1:]
	if err := os.MkdirAll(g.root, 0o700); err != nil {
		runcLog(g, "mkdir root: %v", err)
	}
	var err error
	switch cmd {
	case "--version", "version":
		printRuncVersion()
		return 0
	case "features":
		// Returning an error makes containerd fall back to built-in defaults.
		return 1
	case "create":
		err = cmdCreate(g, rest)
	case "start":
		err = cmdStart(g, rest)
	case "state":
		err = cmdState(g, rest)
	case "kill":
		err = cmdKill(g, rest)
	case "delete":
		err = cmdDelete(g, rest)
	default:
		runcLog(g, "unsupported runc command %q", cmd)
		return 1
	}
	if err != nil {
		runcLog(g, "%s: %v", cmd, err)
		return 1
	}
	return 0
}

func isGlobalValueFlag(a string) bool {
	switch a {
	case "--log", "--root", "--log-format":
		return true
	}
	return false
}

func printRuncVersion() {
	fmt.Println("runc version 1.4.3-astrokube (pure-Go, CGO-free)")
	fmt.Println("spec: 1.2.0")
	fmt.Println("go: pure-go")
	fmt.Println("libseccomp: none")
}

// runcLog writes a diagnostic both to stderr and to the shim's --log file (so a
// failure is visible to containerd, which reads that log).
func runcLog(g runtimeGlobals, format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	fmt.Fprintf(os.Stderr, "astrokube-runc: %s\n", msg)
	if g.log != "" {
		if f, err := os.OpenFile(g.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			rec := map[string]string{"level": "error", "msg": msg, "time": time.Now().Format(time.RFC3339)}
			b, _ := json.Marshal(rec)
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
}

// ---- create ----

func cmdCreate(g runtimeGlobals, args []string) error {
	var bundle, pidFile, id string
	i := 0
	for i < len(args) {
		switch a := args[i]; {
		case a == "--bundle", a == "-b":
			i++
			bundle = args[i]
		case strings.HasPrefix(a, "--bundle="):
			bundle = strings.TrimPrefix(a, "--bundle=")
		case a == "--pid-file":
			i++
			pidFile = args[i]
		case strings.HasPrefix(a, "--pid-file="):
			pidFile = strings.TrimPrefix(a, "--pid-file=")
		case a == "--console-socket":
			i++ // terminal not supported; skip the value
		case a == "--no-pivot", a == "--no-new-keyring", a == "--preserve-fds":
			if a == "--preserve-fds" {
				i++
			}
		case !strings.HasPrefix(a, "-"):
			id = a
		}
		i++
	}
	if id == "" {
		return fmt.Errorf("no container id")
	}
	if bundle == "" {
		bundle, _ = os.Getwd()
	}

	spec, err := readSpec(bundle)
	if err != nil {
		return err
	}
	rootfs := spec.Root.Path
	if !filepath.IsAbs(rootfs) {
		rootfs = filepath.Join(bundle, rootfs)
	}

	stateDir := filepath.Join(g.root, id)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	fifo := filepath.Join(stateDir, "exec.fifo")
	_ = os.Remove(fifo)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		return fmt.Errorf("mkfifo: %w", err)
	}

	// Fork the container init into fresh namespaces. It sets the container up
	// and blocks on the fifo until `start`.
	self := "/proc/self/exe"
	attr := &syscall.SysProcAttr{Cloneflags: cloneFlagsFor(spec)}
	files := []*os.File{os.Stdin, os.Stdout, os.Stderr}
	procAttr := &os.ProcAttr{
		Files: files,
		Sys:   attr,
	}
	proc, err := os.StartProcess(self, []string{"runc", "__runc_init", g.root, id, bundle}, procAttr)
	if err != nil {
		return fmt.Errorf("fork container init: %w", err)
	}
	pid := proc.Pid

	// Apply the cgroup now (the init is blocked, so it cannot escape its limits).
	cgPath := applyCgroup(spec, id, pid)

	st := containerState{
		OCIVersion: orDefault(spec.OCIVersion, "1.2.0"),
		ID:         id,
		InitPid:    pid,
		Status:     "created",
		Bundle:     bundle,
		Rootfs:     rootfs,
		Created:    time.Now().Format(time.RFC3339Nano),
		CgroupPath: cgPath,
	}
	if err := writeState(g, st); err != nil {
		return err
	}
	if pidFile != "" {
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o644); err != nil {
			return fmt.Errorf("write pid-file: %w", err)
		}
	}
	// Detach: do NOT wait — the init reparents to the shim (the subreaper).
	_ = proc.Release()
	return nil
}

func cloneFlagsFor(spec *ociSpec) uintptr {
	var f uintptr
	if spec.Linux == nil {
		return syscall.CLONE_NEWNS
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Path != "" {
			// Joining an existing namespace (setns) is out of scope; skip it so
			// at least the create-fresh namespaces still apply.
			continue
		}
		switch ns.Type {
		case "pid":
			f |= syscall.CLONE_NEWPID
		case "mount":
			f |= syscall.CLONE_NEWNS
		case "ipc":
			f |= syscall.CLONE_NEWIPC
		case "uts":
			f |= syscall.CLONE_NEWUTS
		case "network":
			f |= syscall.CLONE_NEWNET
		case "cgroup":
			f |= syscall.CLONE_NEWCGROUP
		case "user":
			f |= syscall.CLONE_NEWUSER
		}
	}
	if f&syscall.CLONE_NEWNS == 0 {
		f |= syscall.CLONE_NEWNS // we always need our own mount ns to pivot_root
	}
	return f
}

// ---- the container init (runs in the new namespaces, blocks until start) ----

func runcInit(root, id, bundle string) int {
	spec, err := readSpec(bundle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "astrokube-runc init: spec: %v\n", err)
		return 1
	}
	rootfs := spec.Root.Path
	if !filepath.IsAbs(rootfs) {
		rootfs = filepath.Join(bundle, rootfs)
	}
	fifo := filepath.Join(root, id, "exec.fifo")

	// Reference the fifo by an O_PATH fd now, before pivot_root moves it out of
	// view; we re-open it (blocking O_WRONLY) through /proc/self/fd afterwards.
	fifoFd, err := syscall.Open(fifo, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "astrokube-runc init: open fifo: %v\n", err)
		return 1
	}

	if spec.Hostname != "" {
		_ = syscall.Sethostname([]byte(spec.Hostname))
	}
	if err := setupRootfs(rootfs, spec); err != nil {
		fmt.Fprintf(os.Stderr, "astrokube-runc init: rootfs: %v\n", err)
		return 1
	}

	// Block until `start` opens the read end of the fifo.
	wf, err := os.OpenFile("/proc/self/fd/"+strconv.Itoa(fifoFd), os.O_WRONLY, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "astrokube-runc init: reopen fifo: %v\n", err)
		return 1
	}
	if _, err := wf.Write([]byte{0}); err != nil {
		fmt.Fprintf(os.Stderr, "astrokube-runc init: signal fifo: %v\n", err)
		return 1
	}
	wf.Close()
	syscall.Close(fifoFd)

	// Exec the user process, replacing this Go runtime in place (pid preserved).
	if spec.Process == nil || len(spec.Process.Args) == 0 {
		fmt.Fprintf(os.Stderr, "astrokube-runc init: empty process args\n")
		return 1
	}
	if spec.Process.Cwd != "" {
		_ = syscall.Chdir(spec.Process.Cwd)
	}
	argv0 := spec.Process.Args[0]
	path := resolveInRootPath(argv0, spec.Process.Env)
	if err := syscall.Exec(path, spec.Process.Args, spec.Process.Env); err != nil {
		fmt.Fprintf(os.Stderr, "astrokube-runc init: exec %s: %v\n", path, err)
		return 1
	}
	return 0 // unreachable
}

// setupRootfs makes mounts private, applies the spec mounts, and pivot_roots
// into the container rootfs.
func setupRootfs(rootfs string, spec *ociSpec) error {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make-rprivate: %w", err)
	}
	// Bind the rootfs onto itself so it is a mount point pivot_root can use.
	if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind rootfs: %w", err)
	}
	for _, m := range spec.Mounts {
		dest := filepath.Join(rootfs, m.Destination)
		_ = os.MkdirAll(dest, 0o755)
		flags, data := mountOptions(m.Options)
		src := m.Source
		if src == "" {
			src = m.Type
		}
		if err := syscall.Mount(src, dest, m.Type, flags, data); err != nil {
			// Best-effort: a missing mount (e.g. cgroup, mqueue) should not stop
			// the container from running for this proof.
			fmt.Fprintf(os.Stderr, "astrokube-runc init: mount %s(%s)->%s: %v\n", m.Type, src, m.Destination, err)
		}
	}
	// pivot_root into rootfs.
	old := filepath.Join(rootfs, ".oldroot")
	_ = os.MkdirAll(old, 0o700)
	if err := syscall.PivotRoot(rootfs, old); err != nil {
		// Fall back to chroot if pivot_root is unavailable.
		if cerr := syscall.Chroot(rootfs); cerr != nil {
			return fmt.Errorf("pivot_root: %w (chroot: %v)", err, cerr)
		}
		return syscall.Chdir("/")
	}
	if err := syscall.Chdir("/"); err != nil {
		return err
	}
	_ = syscall.Unmount("/.oldroot", syscall.MNT_DETACH)
	_ = os.Remove("/.oldroot")
	return nil
}

func mountOptions(opts []string) (uintptr, string) {
	var flags uintptr
	var data []string
	for _, o := range opts {
		switch o {
		case "ro":
			flags |= syscall.MS_RDONLY
		case "nosuid":
			flags |= syscall.MS_NOSUID
		case "nodev":
			flags |= syscall.MS_NODEV
		case "noexec":
			flags |= syscall.MS_NOEXEC
		case "bind":
			flags |= syscall.MS_BIND
		case "rbind":
			flags |= syscall.MS_BIND | syscall.MS_REC
		case "relatime":
			flags |= syscall.MS_RELATIME
		case "strictatime":
			flags |= syscall.MS_STRICTATIME
		case "remount":
			flags |= syscall.MS_REMOUNT
		default:
			data = append(data, o)
		}
	}
	return flags, strings.Join(data, ",")
}

// resolveInRootPath finds the binary path inside the (already pivoted) rootfs.
func resolveInRootPath(argv0 string, env []string) string {
	if strings.Contains(argv0, "/") {
		return argv0
	}
	var pathEnv string
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			pathEnv = strings.TrimPrefix(e, "PATH=")
		}
	}
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	for _, dir := range strings.Split(pathEnv, ":") {
		cand := filepath.Join(dir, argv0)
		if st, err := os.Stat(cand); err == nil && st.Mode()&0o111 != 0 {
			return cand
		}
	}
	return argv0
}

// ---- start / state / kill / delete ----

func cmdStart(g runtimeGlobals, args []string) error {
	id := lastArg(args)
	st, err := readState(g, id)
	if err != nil {
		return err
	}
	fifo := filepath.Join(g.root, id, "exec.fifo")
	// Opening the read end unblocks the init's blocking O_WRONLY open.
	f, err := os.OpenFile(fifo, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open fifo: %w", err)
	}
	buf := make([]byte, 1)
	_, _ = f.Read(buf)
	f.Close()
	_ = os.Remove(fifo)
	st.Status = "running"
	return writeState(g, st)
}

func cmdState(g runtimeGlobals, args []string) error {
	id := lastArg(args)
	st, err := readState(g, id)
	if err != nil {
		return err
	}
	status := st.Status
	if st.InitPid > 0 && !pidAlive(st.InitPid) {
		status = "stopped"
	}
	out := map[string]interface{}{
		"ociVersion":  st.OCIVersion,
		"id":          st.ID,
		"pid":         st.InitPid,
		"status":      status,
		"bundle":      st.Bundle,
		"rootfs":      st.Rootfs,
		"created":     st.Created,
		"annotations": map[string]string{},
	}
	b, _ := json.Marshal(out)
	os.Stdout.Write(b)
	os.Stdout.Write([]byte("\n"))
	return nil
}

func cmdKill(g runtimeGlobals, args []string) error {
	var id, sig string
	all := false
	for _, a := range args {
		switch {
		case a == "--all", a == "-a":
			all = true
		case strings.HasPrefix(a, "-"):
		case id == "":
			id = a
		default:
			sig = a
		}
	}
	st, err := readState(g, id)
	if err != nil {
		return err
	}
	signum := parseSignal(sig)
	_ = all
	if st.InitPid > 0 {
		if err := syscall.Kill(st.InitPid, signum); err != nil && err != syscall.ESRCH {
			return err
		}
	}
	return nil
}

func cmdDelete(g runtimeGlobals, args []string) error {
	var id string
	force := false
	for _, a := range args {
		switch {
		case a == "--force", a == "-f":
			force = true
		case !strings.HasPrefix(a, "-"):
			id = a
		}
	}
	st, err := readState(g, id)
	if err != nil {
		// Already gone.
		return nil
	}
	if force && st.InitPid > 0 && pidAlive(st.InitPid) {
		_ = syscall.Kill(st.InitPid, syscall.SIGKILL)
	}
	if st.CgroupPath != "" {
		_ = os.Remove(st.CgroupPath)
	}
	return os.RemoveAll(filepath.Join(g.root, id))
}

// ---- helpers ----

func readSpec(bundle string) (*ociSpec, error) {
	b, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("read config.json: %w", err)
	}
	var s ociSpec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse config.json: %w", err)
	}
	if s.Root == nil {
		s.Root = &ociRoot{Path: "rootfs"}
	}
	return &s, nil
}

func writeState(g runtimeGlobals, st containerState) error {
	b, _ := json.Marshal(st)
	return os.WriteFile(filepath.Join(g.root, st.ID, "state.json"), b, 0o600)
}

func readState(g runtimeGlobals, id string) (containerState, error) {
	var st containerState
	b, err := os.ReadFile(filepath.Join(g.root, id, "state.json"))
	if err != nil {
		return st, fmt.Errorf("container %q not found", id)
	}
	return st, json.Unmarshal(b, &st)
}

func applyCgroup(spec *ociSpec, id string, pid int) string {
	if spec.Linux == nil {
		return ""
	}
	rel := spec.Linux.CgroupsPath
	if rel == "" {
		rel = "/astrokube/" + id
	}
	// Only cgroupfs-style paths are handled; a systemd "slice:prefix:name" is
	// reduced to a plain path.
	rel = strings.ReplaceAll(rel, ":", "/")
	cg := filepath.Join("/sys/fs/cgroup", rel)
	if err := os.MkdirAll(cg, 0o755); err != nil {
		return ""
	}
	if r := spec.Linux.Resources; r != nil {
		if r.Memory != nil && r.Memory.Limit != nil && *r.Memory.Limit > 0 {
			_ = os.WriteFile(filepath.Join(cg, "memory.max"), []byte(strconv.FormatInt(*r.Memory.Limit, 10)), 0o644)
		}
		if r.CPU != nil && r.CPU.Quota != nil && *r.CPU.Quota > 0 {
			period := uint64(100000)
			if r.CPU.Period != nil && *r.CPU.Period > 0 {
				period = *r.CPU.Period
			}
			_ = os.WriteFile(filepath.Join(cg, "cpu.max"), []byte(fmt.Sprintf("%d %d", *r.CPU.Quota, period)), 0o644)
		}
	}
	_ = os.WriteFile(filepath.Join(cg, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644)
	return cg
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func lastArg(args []string) string {
	for i := len(args) - 1; i >= 0; i-- {
		if !strings.HasPrefix(args[i], "-") {
			return args[i]
		}
	}
	return ""
}

func parseSignal(s string) syscall.Signal {
	s = strings.TrimPrefix(strings.ToUpper(s), "SIG")
	switch s {
	case "TERM", "15", "":
		return syscall.SIGTERM
	case "KILL", "9":
		return syscall.SIGKILL
	case "INT", "2":
		return syscall.SIGINT
	case "HUP", "1":
		return syscall.SIGHUP
	case "QUIT", "3":
		return syscall.SIGQUIT
	case "USR1", "10":
		return syscall.SIGUSR1
	}
	if n, err := strconv.Atoi(s); err == nil {
		return syscall.Signal(n)
	}
	return syscall.SIGTERM
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
