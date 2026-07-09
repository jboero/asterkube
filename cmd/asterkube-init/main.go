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

// Command asterkube-init is an experimental init (PID 1) front-end for the
// kubelet. The idea ("asterkube") is to boot a Kubernetes node directly on the
// Asterinas Rust kernel with no C and no libc: a Rust kernel and a Go init that
// is an extension of the kubelet itself.
//
// When this binary is executed as PID 1 (for example, symlinked to /sbin/init
// and selected with the kernel command line `init=/sbin/init`), it skips all of
// the usual kubelet flag parsing and instead behaves as the system init: it
// sets up the basic filesystem environment and then, in this first milestone,
// reports what it did and powers the machine off.
//
// When it is *not* PID 1, it is a stand-in for the normal kubelet entry point
// and simply explains that it would defer to the real kubelet command.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// runAsKubelet runs the ordinary kubelet path (not PID 1, not an applet). It is
// supplied by one of two build-time variants so the SAME main() works for both:
//   - kubelet_entry_stub.go: the lightweight standalone build in this module
//     (k8s.io/kubelet) — a placeholder, since this module cannot import the full
//     kubelet command.
//   - kubelet_entry_real.go: the combined zero-C build from the kubernetes tree
//     (cmd/asterkube-kubelet) — runs the REAL upstream kubelet, CGO-free.
// The file that defines it is selected by which tree the binary is built in.

func main() {
	// The pure-Go OCI runtime re-execs this binary as the container init inside
	// the new namespaces; this must come before everything else.
	if len(os.Args) >= 5 && os.Args[1] == "__runc_init" {
		os.Exit(runcInit(os.Args[2], os.Args[3], os.Args[4]))
	}

	// Multi-call applet: when invoked as `mount`/`umount` (via a PATH symlink),
	// behave as that tool. The kubelet shells out to these to set up pod volumes
	// and Asterinas ships no util-linux, so this one static binary stands in.
	// Invoked as `runc`/`asterkube-runc`, it is the pure-Go, CGO-free OCI runtime
	// that containerd's shim drives — so the node runs containers with zero C.
	switch filepath.Base(os.Args[0]) {
	case "runc", "asterkube-runc":
		os.Exit(runOCIRuntime(os.Args[1:]))
	case "mount":
		runMount(os.Args[1:])
		return
	case "umount":
		runUmount(os.Args[1:])
		return
	case "shebang-probe":
		// Used by probeShebang: print our argv so the caller can verify the
		// kernel passed the script's path (argv[1]) to the interpreter.
		fmt.Printf("shebang-probe argv: %v\n", os.Args)
		return
	}
	// A seccomp enforcement probe child (re-exec'd by runSeccompProbe). It
	// installs a real BPF filter and attempts the targeted syscall to prove the
	// kernel enforces it. Must come before the PID-1 / kubelet dispatch.
	if mode := os.Getenv(seccompProbeEnv); mode != "" {
		seccompProbeChild(mode)
		return
	}
	// An astromac MAC probe role (re-exec'd by runMacProbe). "main" runs the
	// scenario; "peer" is the second tenant it signals. Before PID-1 dispatch.
	switch os.Getenv(macProbeEnv) {
	case "main":
		macProbeMain()
		return
	case "peer":
		macProbePeer()
		return
	}
	// astromac file-access MAC probe roles.
	switch os.Getenv(fileMacProbeEnv) {
	case "main":
		fileMacProbeMain()
		return
	case "peer":
		fileMacProbePeer()
		return
	}
	// astromac network MAC probe roles.
	switch os.Getenv(socketMacProbeEnv) {
	case "main":
		socketMacProbeMain()
		return
	case "peer":
		socketMacProbePeer()
		return
	}
	// user-namespace probe child (launched with CLONE_NEWUSER).
	if os.Getenv(usernsProbeEnv) == "child" {
		usernsProbeChild()
		return
	}
	// rootless capability-boundary probe roles.
	switch os.Getenv(rootlessProbeEnv) {
	case "unpriv":
		rootlessProbeUnpriv()
		return
	case "userns":
		rootlessProbeUserns()
		return
	}
	// rootless id-mapping probe roles.
	switch os.Getenv(usermapProbeEnv) {
	case "unpriv":
		usermapProbeUnpriv()
		return
	case "child":
		usermapProbeChild()
		return
	}
	// volume-hardening probe: a setuid binary re-exec'd to report its euid.
	if os.Getenv(suidReportEnv) != "" {
		fmt.Printf("euid=%d\n", os.Geteuid())
		return
	}
	// pod-sandbox netns probe roles.
	switch os.Getenv(sandboxProbeEnv) {
	case "hold":
		sandboxProbeHold()
		return
	case "join":
		sandboxProbeJoin()
		return
	}
	// A pod's container is this binary re-exec'd inside fresh namespaces; with
	// CLONE_NEWPID it sees getpid()==1, so this guard must come first to keep it
	// from recursing into the init logic.
	if os.Getenv(containerEnv) != "" {
		runContainer()
		return
	}
	// When runc (or containerd) execs this binary as the container payload, it is
	// PID 1 inside the container's PID namespace — this guard keeps it from
	// recursing into the init logic. It just proves it is alive inside the
	// container and exits.
	if os.Getenv(runcPayloadEnv) != "" {
		host, _ := os.Hostname()
		fmt.Printf("runc-payload: hello from inside the container — pid=%d hostname=%q\n", os.Getpid(), host)
		return
	}
	// A namespace-probe child re-execs this binary; when launched with
	// CLONE_NEWPID it sees getpid()==1, so this guard must come first to keep it
	// from recursing into the init logic.
	if os.Getenv(probeChildEnv) != "" {
		// When asked, try to bind a specific loopback port; this is used to test
		// whether a new network namespace has an isolated loopback stack.
		if port := os.Getenv(probeBindPortEnv); port != "" {
			fmt.Print(tryBindUDP("127.0.0.1:" + port))
			return
		}
		// When asked, join a cgroup and allocate memory; used to test cgroup
		// memory.max enforcement. If the limit is enforced, the kernel kills this
		// child before it can report success.
		if cg := os.Getenv(probeMemCgroupEnv); cg != "" {
			fmt.Print(memHog(cg, os.Getenv(probeMemMBEnv)))
			return
		}
		// When asked, join a cgroup and busy-loop; used to test cgroup cpu.max
		// throttling by comparing work done with and without a CPU quota.
		if cg := os.Getenv(probeCPUCgroupEnv); cg != "" {
			fmt.Print(cpuBurn(cg, os.Getenv(probeCPUMsEnv)))
			return
		}
		// Report the PID this child sees (1 in a new PID namespace) and whether
		// loopback works (proves a new network namespace has its own usable lo).
		fmt.Printf("getpid=%d lo=%s\n", os.Getpid(), loopbackRoundTrip())
		return
	}
	if os.Getpid() != 1 {
		runAsKubelet()
		return
	}
	runAsInit()
}

// runAsInit performs the PID 1 responsibilities. For this milestone that means
// mounting the standard virtual filesystems a Kubernetes node relies on and
// then halting.
func runAsInit() {
	banner()

	results := mountAll(defaultMounts())
	report(results)

	// Mount the operator-configurable filesystems listed in /etc/fstab (extra
	// data volumes, partitions, tmpfs, virtio-fs shares). A documented default
	// ships in the image; missing/failed entries are skipped, never fatal.
	mountFstab()

	// Provide `mount`/`umount` (this binary, multi-call) on PATH before any
	// component that shells out to them — the kubelet's volume manager does, to
	// set up pod volumes like the projected ServiceAccount-token tmpfs.
	installMountApplet()

	// DHCP-first: bring up the primary interface (IP + default route + DNS) before
	// anything else needs the network — the cloud-init / nomadinit model. Best
	// effort: a bare boot with no DHCP server falls back to static config later.
	dhcpFirst("eth0")

	probeKernel()

	// Exercise the new NETLINK_NETFILTER kernel surface (what nft/iptables use).
	probeNetfilter()

	// Verify the shebang interpreter-path fix (what kube-proxy's iptables-wrapper
	// needs): exec a #! script with a short argv[0] and confirm the interpreter
	// receives the script's path, not the bare argv[0].
	probeShebang()

	// Exercise the container-runtime substrate (overlayfs snapshot, runc,
	// containerd) before the network pod tests — unless this is a zero-C image
	// with no libc, in which case the dynamically-linked upstream runc/containerd
	// cannot run; the pure-Go node agent below carries the container workload.
	if pureGoMode() {
		fmt.Println("asterkube-init: pure-Go image (no libc present) — skipping the")
		fmt.Println("asterkube-init: glibc-linked containerd/runc phases; the pure-Go node")
		fmt.Println("asterkube-init: agent runs containers with zero C below.")
		// Prove the pure-Go OCI runtime (our CGO-free runc replacement) works...
		ociSelfTest()
		// ...then prove static containerd drives it end to end, zero C.
		zeroCContainerdTest()
	} else {
		runContainerRuntimeTests()
	}

	// Prove the kernel ENFORCES seccomp BPF filters (errno + kill), the first
	// hardening milestone toward the multi-tenant security posture. Runs in
	// isolated child processes so the (unremovable, inherited) filter never
	// leaks into the node agent or container runtime.
	runSeccompProbe()

	// Prove the native astromac MAC mediates cross-tenant operations (the second
	// hardening milestone). Permissive by default; the probe drives enforcing
	// transiently in a child subtree and restores permissive.
	runMacProbe()
	// ...and that it covers cross-tenant FILE access on a shared filesystem.
	runFileMacProbe()
	// ...and cross-tenant network connects on a shared network.
	runSocketMacProbe()
	// User-namespace Stage 1: clone(CLONE_NEWUSER) creates a real namespace.
	runUsernsProbe()
	// Rootless: an unprivileged process gains capabilities inside its own user
	// namespace but stays powerless on the host (the privesc boundary).
	runRootlessProbe()
	// Rootless id mapping: an unprivileged process maps its uid/gid so the
	// container sees itself as root 0 (functional rootless).
	runUsermapProbe()
	// Volume hardening: a setuid binary on a nosuid volume can't grant root,
	// and a noexec volume can't run code (closes the rootless-volume escalation).
	runVolumeProbe()
	// Pod sandbox: a workload joins the sandbox's network namespace via setns
	// (multi-container pods share localhost) — the runtime now uses this.
	runSandboxNetnsProbe()

	// Act as the node agent: run the static pods, exercising the kernel's
	// namespace and cgroup support end-to-end.
	runNodeAgent()

	// Gating experiment for joining a real cluster: can eth0 carry outbound TCP
	// to reach a kube-apiserver?
	probeOutboundTCP()

	// Bind this generic image to a cluster, if asked. Preferred: a config-drive
	// "join bundle" (asterkube-join.sh) carrying a PRE-ISSUED kubeconfig — the
	// node registers with its issued cert, no in-VM CSR. Fallback: cluster
	// coordinates on the kernel cmdline (ASTERKUBE_APISERVER=…), which TLS-
	// bootstraps with a token. Either way the node registers and persists.
	if dir, ok := findJoinBundle(); ok {
		joinFromBundle(dir)
	} else if bootArgsConfigured() {
		bootArgsJoin()
	}

	// Keep the node running as a persistent, interactive member until an ACPI
	// shutdown arrives when EITHER it came up live (containerd + the apiserver
	// kubelet are running) OR ASTERKUBE_PERSIST is set — the latter guarantees a
	// downloadable sample image stays up for the user to explore, even if the
	// capability demos didn't leave a long-running process behind. Only a plain,
	// non-persistent demo boot powers off after the checks.
	fmt.Println()
	if nodeIsLive() || os.Getenv("ASTERKUBE_PERSIST") != "" {
		serveForever()
	} else {
		fmt.Println("asterkube-init: node did not come up live; powering off.")
		powerOff()
	}
}

func banner() {
	const line = "============================================================"
	fmt.Println()
	fmt.Println(line)
	fmt.Println(" asterkube-init: running as PID 1 on the Asterinas kernel")
	fmt.Println(" Rust kernel + Go init, no C, no libc.")
	fmt.Println(line)
	fmt.Println()
}
