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

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// probeChildEnv marks a re-exec of this binary as a throwaway child used to test
// namespace creation. A child launched with CLONE_NEWPID becomes PID 1 in its
// new namespace, so main() must check this *before* the normal PID-1 dispatch to
// avoid recursing into the init logic.
const probeChildEnv = "ASTROKUBE_PROBE_CHILD"

// probeBindPortEnv, when set on a probe child, asks it to bind that UDP port on
// 127.0.0.1 and report the outcome. Used by the network-namespace isolation test.
const probeBindPortEnv = "ASTROKUBE_BIND_PORT"

// probeMemCgroupEnv / probeMemMBEnv, when set on a probe child, ask it to join
// the given cgroup and then allocate that many MiB. Used by the cgroup
// memory.max enforcement test.
const (
	probeMemCgroupEnv = "ASTROKUBE_MEM_CGROUP"
	probeMemMBEnv     = "ASTROKUBE_MEM_MB"
)

// probeCPUCgroupEnv / probeCPUMsEnv, when set on a probe child, ask it to join
// the given cgroup ("none" to skip) and busy-loop for that many milliseconds,
// reporting the amount of work done. Used by the cpu.max enforcement test.
const (
	probeCPUCgroupEnv = "ASTROKUBE_CPU_CGROUP"
	probeCPUMsEnv     = "ASTROKUBE_CPU_MS"
)

// namespaceKinds are the Linux namespace types the kubelet relies on to isolate
// pods and containers. CLONE_NEWCGROUP is defined locally because it is not
// exported by the syscall package.
const cloneNewCgroup = 0x02000000

var namespaceKinds = []struct {
	name string
	flag uintptr
}{
	{"mount (CLONE_NEWNS)", syscall.CLONE_NEWNS},
	{"uts (CLONE_NEWUTS)", syscall.CLONE_NEWUTS},
	{"ipc (CLONE_NEWIPC)", syscall.CLONE_NEWIPC},
	{"pid (CLONE_NEWPID)", syscall.CLONE_NEWPID},
	{"net (CLONE_NEWNET)", syscall.CLONE_NEWNET},
	{"user (CLONE_NEWUSER)", syscall.CLONE_NEWUSER},
	{"cgroup (CLONE_NEWCGROUP)", cloneNewCgroup},
}

// probeKernel reports the kernel features a Kubernetes node needs: the supported
// filesystems, the cgroup v2 controller set, and which namespaces can actually
// be created. It is meant as a scoping tool — it changes nothing that survives
// the subsequent power-off.
func probeKernel() {
	fmt.Println()
	fmt.Println("astrokube-init: kernel capability probe (scoping Kubernetes support)")
	probeProcFile("supported filesystems", "/proc/filesystems")
	probeCgroupV2()
	probeNamespaces()
	fmt.Printf("  loopback round-trip (init ns): %s\n", loopbackRoundTrip())
	fmt.Printf("  icmp echo 127.0.0.1 (lo):      %s\n", icmpPingTest("127.0.0.1"))
	fmt.Printf("  icmp echo 10.0.2.15 (eth0):    %s\n", icmpPingTest("10.0.2.15"))
	fmt.Printf("  icmp echo 10.0.2.2 (gateway):  %s\n", icmpPingTest("10.0.2.2"))
	fmt.Printf("  net ns loopback isolation: %s\n", netnsIsolationTest())
	fmt.Printf("  cgroup memory.max enforcement: %s\n", memEnforcementTest())
	fmt.Printf("  cgroup cpu.max enforcement: %s\n", cpuEnforcementTest())
}

// cpuEnforcementTest runs the same CPU-bound loop twice for equal wall-clock
// time: once unrestricted, and once in a cgroup throttled to 10% of a CPU via
// cpu.max. If cpu.max is enforced, the throttled run does roughly a tenth of the
// work; if not, the two runs do similar amounts.
func cpuEnforcementTest() string {
	const (
		cgroup  = "/sys/fs/cgroup/astrokube.cputest"
		burnMs  = 1000
		quota   = "10000 100000" // 10ms per 100ms = 10% of one CPU
		percent = 10
	)
	// cpu.max exists only if the parent delegates +cpu.
	if err := os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+cpu"), 0); err != nil {
		return "inconclusive (delegate +cpu: " + err.Error() + ")"
	}
	if err := os.Mkdir(cgroup, 0o755); err != nil {
		return "inconclusive (mkdir: " + err.Error() + ")"
	}
	defer os.Remove(cgroup)
	if err := os.WriteFile(cgroup+"/cpu.max", []byte(quota), 0); err != nil {
		return "inconclusive (set cpu.max: " + err.Error() + ")"
	}

	base := runBurn("none", burnMs)
	throttled := runBurn(cgroup, burnMs)
	if base <= 0 || throttled < 0 {
		return "inconclusive (burn failed)"
	}

	ratioPct := throttled * 100 / base
	if ratioPct < 35 {
		return fmt.Sprintf("ENFORCED — throttled to 10%%, did %d%% of the work of an unthrottled run (%d vs %d)",
			ratioPct, throttled, base)
	}
	return fmt.Sprintf("NOT enforced — throttled run did %d%% of unthrottled work (%d vs %d)", ratioPct, throttled, base)
}

// runBurn launches a child that joins cgroup (or "none") and busy-loops for ms,
// returning the amount of work it reported (-1 on failure).
func runBurn(cgroup string, ms int) int64 {
	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(),
		probeChildEnv+"=1",
		probeCPUCgroupEnv+"="+cgroup,
		fmt.Sprintf("%s=%d", probeCPUMsEnv, ms),
	)
	out, err := combinedOutputTimeout(cmd, 10*time.Second)
	if err != nil {
		return -1
	}
	var work int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &work); err != nil {
		return -1
	}
	return work
}

// cpuBurn is the child side of the cpu.max test: it optionally joins a cgroup,
// then busy-loops for the requested wall-clock time and reports work done.
func cpuBurn(cgroup, msStr string) string {
	if cgroup != "none" {
		pid := fmt.Sprintf("%d", os.Getpid())
		if err := os.WriteFile(cgroup+"/cgroup.procs", []byte(pid), 0); err != nil {
			return "-1"
		}
	}
	ms := 1000
	fmt.Sscanf(msStr, "%d", &ms)

	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	var work, acc uint64
	for time.Now().Before(deadline) {
		// A chunk of pure computation between wall-clock checks so the loop is
		// CPU-bound rather than syscall-bound.
		for i := 0; i < 200000; i++ {
			acc = acc*1664525 + 1013904223 + uint64(i)
		}
		work++
	}
	// Use acc so the loop is not optimized away.
	return fmt.Sprintf("%d", work|(acc&0))
}

// memEnforcementTest creates a cgroup with a small memory.max, then launches a
// child that joins it and tries to allocate far more than the limit. If
// memory.max is enforced, the kernel kills the child before it can report
// success; if not, the child allocates freely and survives.
func memEnforcementTest() string {
	const (
		cgroup   = "/sys/fs/cgroup/astrokube.memtest"
		limitMiB = 32
		allocMiB = 128
	)
	if err := os.Mkdir(cgroup, 0o755); err != nil {
		return "inconclusive (mkdir: " + err.Error() + ")"
	}
	defer os.Remove(cgroup)
	// memory.max exists only if the parent delegated +memory (done earlier).
	if err := os.WriteFile(cgroup+"/memory.max", []byte(fmt.Sprintf("%d", limitMiB<<20)), 0); err != nil {
		return "inconclusive (set memory.max: " + err.Error() + ")"
	}

	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(),
		probeChildEnv+"=1",
		probeMemCgroupEnv+"="+cgroup,
		fmt.Sprintf("%s=%d", probeMemMBEnv, allocMiB),
	)
	out, err := combinedOutputTimeout(cmd, 10*time.Second)
	report := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Sprintf("ENFORCED — child killed allocating %dMiB under a %dMiB limit (%s)",
			allocMiB, limitMiB, strings.TrimSpace(err.Error()))
	}
	return fmt.Sprintf("NOT enforced — child survived: %q (limit %dMiB)", report, limitMiB)
}

// memHog joins the given cgroup and then allocates and touches mb MiB. It is the
// child side of the memory.max enforcement test.
func memHog(cgroup, mb string) string {
	// Move ourselves into the cgroup so its memory.max applies.
	pid := fmt.Sprintf("%d", os.Getpid())
	if err := os.WriteFile(cgroup+"/cgroup.procs", []byte(pid), 0); err != nil {
		return "join failed: " + err.Error()
	}

	n := 128
	fmt.Sscanf(mb, "%d", &n)
	// Allocate and touch every page so the memory becomes resident.
	buf := make([]byte, n<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 0xa5
	}
	return fmt.Sprintf("survived allocating %dMiB", n)
}

// netnsIsolationTest checks whether a new network namespace has its own loopback
// stack, separate from the initial namespace. The parent (in the initial
// namespace) binds a fixed loopback port and holds it, then a child in a new
// network namespace tries to bind the *same* port. If the child succeeds, the
// namespaces have independent loopback stacks (isolated); if it gets
// "address in use", they share a single global stack (not isolated).
func netnsIsolationTest() string {
	const port = "54321"
	held, err := net.ListenPacket("udp", "127.0.0.1:"+port)
	if err != nil {
		return "inconclusive (parent bind failed: " + err.Error() + ")"
	}
	defer held.Close()

	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(), probeChildEnv+"=1", probeBindPortEnv+"="+port)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
	out, err := combinedOutputTimeout(cmd, 10*time.Second)
	report := strings.TrimSpace(string(out))
	if err != nil {
		return "child failed (" + strings.TrimSpace(err.Error()+" "+report) + ")"
	}
	switch {
	case strings.HasPrefix(report, "bound"):
		return "ISOLATED — new netns bound 127.0.0.1:" + port + " while initial ns held it"
	case strings.Contains(report, "in use"):
		return "shared (new netns saw the initial ns's bind: " + report + ")"
	default:
		return report
	}
}

// combinedOutputTimeout runs cmd and returns its combined output, but never
// blocks longer than d — if the child hangs (a rare namespace-teardown race), it
// is killed so the probe can continue rather than wedging the whole node.
func combinedOutputTimeout(cmd *exec.Cmd, d time.Duration) ([]byte, error) {
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return []byte(buf.String()), err
	case <-time.After(d):
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return []byte(buf.String()), fmt.Errorf("timed out")
	}
}

// tryBindUDP attempts to bind addr and reports the outcome as a short token.
func tryBindUDP(addr string) string {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return "error: " + err.Error()
	}
	conn.Close()
	return "bound " + addr
}

// loopbackRoundTrip binds a UDP socket on 127.0.0.1, sends a datagram to itself,
// and reads it back. It exercises the loopback interface and the socket stack.
func loopbackRoundTrip() string {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return "FAILED to bind (" + err.Error() + ")"
	}
	defer conn.Close()

	payload := []byte("astrokube-ping")
	if _, err := conn.WriteTo(payload, conn.LocalAddr()); err != nil {
		return "FAILED to send (" + err.Error() + ")"
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len(payload))
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		return "FAILED to receive (" + err.Error() + ")"
	}
	if string(buf[:n]) != string(payload) {
		return "FAILED: payload mismatch"
	}
	return "ok (" + conn.LocalAddr().String() + ")"
}

func probeProcFile(label, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("  %s: unavailable (%v)\n", label, err)
		return
	}
	fields := strings.Fields(strings.ReplaceAll(string(data), "\n", " "))
	fmt.Printf("  %s: %s\n", label, strings.Join(fields, " "))
}

// probeCgroupV2 reports the controllers the kernel advertises and tries to
// create a child cgroup and delegate controllers into it — the exact operations
// the kubelet performs to build its cgroup hierarchy.
func probeCgroupV2() {
	const root = "/sys/fs/cgroup"
	controllers, err := os.ReadFile(root + "/cgroup.controllers")
	if err != nil {
		fmt.Printf("  cgroup v2: not mounted or no cgroup.controllers (%v)\n", err)
		return
	}
	fmt.Printf("  cgroup v2 controllers: %s\n", strings.TrimSpace(string(controllers)))

	if subtree, err := os.ReadFile(root + "/cgroup.subtree_control"); err == nil {
		fmt.Printf("  cgroup v2 subtree_control: %q\n", strings.TrimSpace(string(subtree)))
	}

	// Try the kubelet-style operation: make a child cgroup and enable a
	// controller in the parent's subtree_control so the child inherits it.
	child := root + "/astrokube.probe"
	if err := os.Mkdir(child, 0o755); err != nil {
		fmt.Printf("  cgroup v2 mkdir child: FAILED (%v)\n", err)
		return
	}
	fmt.Printf("  cgroup v2 mkdir child: ok (%s)\n", child)
	if err := os.WriteFile(root+"/cgroup.subtree_control", []byte("+memory"), 0); err != nil {
		fmt.Printf("  cgroup v2 enable +memory in subtree_control: FAILED (%v)\n", err)
	} else {
		fmt.Printf("  cgroup v2 enable +memory in subtree_control: ok\n")
	}
	if entries, err := os.ReadDir(child); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		fmt.Printf("  cgroup v2 child interface files: %s\n", strings.Join(names, " "))
	}

	// The kubelet writes memory.max to cap a pod's memory. Exercise that exact
	// path: write a limit and read it back.
	const limit = "104857600" // 100 MiB
	if err := os.WriteFile(child+"/memory.max", []byte(limit), 0); err != nil {
		fmt.Printf("  cgroup v2 write child memory.max: FAILED (%v)\n", err)
	} else {
		got, _ := os.ReadFile(child + "/memory.max")
		fmt.Printf("  cgroup v2 write child memory.max=%s, read back %q: ok\n", limit, strings.TrimSpace(string(got)))
	}

	_ = os.Remove(child)
}

// probeNamespaces lists the init's own namespaces and then tests whether each
// namespace type can be created, by launching a short-lived child of this binary
// with the corresponding clone flag.
func probeNamespaces() {
	if entries, err := os.ReadDir("/proc/self/ns"); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		fmt.Printf("  namespaces present for PID 1: %s\n", strings.Join(names, " "))
	} else {
		fmt.Printf("  /proc/self/ns: unavailable (%v)\n", err)
	}

	fmt.Println("  namespace creation (clone) support:")
	for _, ns := range namespaceKinds {
		fmt.Printf("    %-26s %s\n", ns.name, testNamespace(ns.flag))
	}
}

// testNamespace re-execs this binary as a throwaway child requesting a single
// new namespace, and reports whether the kernel honored the request.
func testNamespace(flag uintptr) string {
	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(), probeChildEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flag}
	out, err := combinedOutputTimeout(cmd, 10*time.Second)
	if err != nil {
		return "UNSUPPORTED (" + strings.TrimSpace(strings.ReplaceAll(err.Error()+" "+string(out), "\n", " ")) + ")"
	}
	report := strings.TrimSpace(string(out))
	if report != "" {
		return "ok (child " + report + ")"
	}
	return "ok"
}

// icmpChecksum computes the RFC 1071 internet checksum over data.
func icmpChecksum(data []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(data); i += 2 {
		sum += uint32(data[i])<<8 | uint32(data[i+1])
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// icmpPingTest sends an ICMP echo request to dst over a raw ICMP socket (the
// classic ping pattern) and waits up to ~5s for a matching echo reply. The
// reply arrives with an IP header prefix, as raw sockets do on Linux.
func icmpPingTest(dst string) string {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, 1 /* IPPROTO_ICMP */)
	if err != nil {
		return "FAILED socket: " + err.Error()
	}
	defer syscall.Close(fd)
	_ = syscall.SetNonblock(fd, true)

	const ident, seq = 0xBEEF, 1
	payload := []byte("astrokube-ping")
	pkt := make([]byte, 8+len(payload))
	pkt[0] = 8 // echo request
	pkt[4], pkt[5] = byte(ident>>8), byte(ident&0xff)
	pkt[6], pkt[7] = byte(seq>>8), byte(seq&0xff)
	copy(pkt[8:], payload)
	csum := icmpChecksum(pkt)
	pkt[2], pkt[3] = byte(csum>>8), byte(csum&0xff)

	var sa syscall.SockaddrInet4
	copy(sa.Addr[:], net.ParseIP(dst).To4())
	if err := syscall.Sendto(fd, pkt, 0, &sa); err != nil {
		return "FAILED sendto: " + err.Error()
	}

	buf := make([]byte, 1500)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return "FAILED recvfrom: " + err.Error()
		}
		if n < 20+8 {
			continue
		}
		ihl := int(buf[0]&0x0f) * 4
		icmp := buf[ihl:n]
		if len(icmp) < 8 || icmp[0] != 0 /* echo reply */ {
			continue
		}
		gotIdent := int(icmp[4])<<8 | int(icmp[5])
		if gotIdent != ident {
			continue
		}
		ttl := buf[8]
		return fmt.Sprintf("ok (reply from %s, ttl=%d, %d bytes)", dst, ttl, len(icmp))
	}
	return "FAILED: no echo reply within 5s"
}
