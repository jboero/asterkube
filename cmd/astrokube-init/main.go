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

// Command astrokube-init is an experimental init (PID 1) front-end for the
// kubelet. The idea ("astrokube") is to boot a Kubernetes node directly on the
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

	// Importing a kubelet package keeps this binary genuinely part of the
	// kubelet module rather than a free-standing init, matching the goal of
	// making init an extension of the kubelet.
	kubelettypes "k8s.io/kubelet/pkg/types"
)

func main() {
	// Multi-call applet: when invoked as `mount`/`umount` (via a PATH symlink),
	// behave as that tool. The kubelet shells out to these to set up pod volumes
	// and Asterinas ships no util-linux, so this one static binary stands in.
	switch filepath.Base(os.Args[0]) {
	case "mount":
		runMount(os.Args[1:])
		return
	case "umount":
		runUmount(os.Args[1:])
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

// runAsKubelet is the placeholder for the ordinary kubelet path taken when this
// process is not PID 1. The real implementation will exec/serve the kubelet.
func runAsKubelet() {
	fmt.Println("astrokube-init: not running as PID 1; deferring to the normal kubelet entry point.")
	fmt.Printf("astrokube-init: (kubelet pod-name label key is %q)\n", kubelettypes.KubernetesPodNameLabel)
}

// runAsInit performs the PID 1 responsibilities. For this milestone that means
// mounting the standard virtual filesystems a Kubernetes node relies on and
// then halting.
func runAsInit() {
	banner()

	results := mountAll(defaultMounts())
	report(results)

	// Provide `mount`/`umount` (this binary, multi-call) on PATH before any
	// component that shells out to them — the kubelet's volume manager does, to
	// set up pod volumes like the projected ServiceAccount-token tmpfs.
	installMountApplet()

	probeKernel()

	// Exercise the new NETLINK_NETFILTER kernel surface (what nft/iptables use).
	probeNetfilter()

	// Exercise the container-runtime substrate (overlayfs snapshot, runc,
	// containerd) before the network pod tests.
	runContainerRuntimeTests()

	// Act as the node agent: run the static pods, exercising the kernel's
	// namespace and cgroup support end-to-end.
	runNodeAgent()

	// Gating experiment for joining a real cluster: can eth0 carry outbound TCP
	// to reach a kube-apiserver?
	probeOutboundTCP()

	fmt.Println()
	fmt.Println("astrokube-init: node work complete. Powering off.")
	powerOff()
}

func banner() {
	const line = "============================================================"
	fmt.Println()
	fmt.Println(line)
	fmt.Println(" astrokube-init: running as PID 1 on the Asterinas kernel")
	fmt.Println(" Rust kernel + Go init, no C, no libc.")
	fmt.Println(line)
	fmt.Println()
}
