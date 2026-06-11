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
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// containerEnv marks a re-exec of this binary as a pod's container entrypoint.
const containerEnv = "ASTROKUBE_CONTAINER"

// podHostnameEnv passes a pod's hostname to its container.
const podHostnameEnv = "ASTROKUBE_POD_HOSTNAME"

// manifestDir is where the node agent looks for static pod manifests, mirroring
// the kubelet's --pod-manifest-path (static pod) mode.
const manifestDir = "/etc/astrokube/pods"

// Environment variables passing a veth pod's addresses to its container.
const (
	podIPEnv     = "ASTROKUBE_POD_IP"
	hostIPEnv    = "ASTROKUBE_HOST_IP"
	vethPrefxEnv = "ASTROKUBE_VETH_PREFIX"
)

// peerIPEnv passes a bridged pod the address of its peer pod on the same
// bridge, enabling the pod↔pod connectivity tests.
const peerIPEnv = "ASTROKUBE_PEER_IP"

// vethUDPPort is the port used by the cross-namespace connectivity probe.
const vethUDPPort = "9999"

// bridgeUDPPort is the port used by the pod↔pod-over-bridge connectivity probe.
const bridgeUDPPort = "9998"

// podResources is a minimal subset of a pod's resource limits.
type podResources struct {
	MemoryMaxMiB  int `json:"memoryMaxMiB"`
	CPUMaxPercent int `json:"cpuMaxPercent"`
}

// podNetwork requests a veth pair wiring the pod to the host, à la CNI. When
// Bridge is set, the pod's veth host half is enslaved to that bridge instead of
// being a standalone host interface, and the gateway address lives on the
// bridge itself — so pods sharing a bridge reach each other through the hub.
type podNetwork struct {
	PodIP     string `json:"podIP"`
	HostIP    string `json:"hostIP"`
	Prefix    int    `json:"prefix"`
	Bridge    string `json:"bridge"`
	GatewayIP string `json:"gatewayIP"`
	PeerIP    string `json:"peerIP"`
}

// bridged reports whether the pod asks to be attached to a bridge.
func (s podSpec) bridged() bool {
	return s.Network != nil && s.Network.Bridge != ""
}

// podSpec is a minimal static pod manifest: enough to launch one container with
// resource limits, in the spirit of a Kubernetes PodSpec.
type podSpec struct {
	Name      string       `json:"name"`
	Hostname  string       `json:"hostname"`
	Command   []string     `json:"command"`
	Env       []string     `json:"env"`
	Resources podResources `json:"resources"`
	Network   *podNetwork  `json:"network,omitempty"`
}

// runNodeAgent is the kubelet-as-init's node loop: it reads static pod manifests
// and runs each one as an isolated, resource-limited container. This is the
// astrokube end-to-end milestone — the node actually running pods using the
// PID/UTS/IPC/NET namespaces and cgroup limits implemented in the kernel.
func runNodeAgent() {
	fmt.Println()
	fmt.Println("astrokube-init: node agent starting (kubelet static-pod mode)")
	specs := loadPodManifests(manifestDir)
	if len(specs) == 0 {
		fmt.Printf("  no pod manifests found in %s\n", manifestDir)
		return
	}
	// Ordinary pods run sequentially as before; bridged pods are gathered and
	// run together, since proving pod↔pod connectivity needs them overlapping.
	var bridged []podSpec
	for _, spec := range specs {
		if spec.bridged() {
			bridged = append(bridged, spec)
			continue
		}
		runPod(spec)
	}
	if len(bridged) > 0 {
		runBridgedPods(bridged)
	}
	fmt.Println("astrokube-init: node agent: all pods completed")
}

// loadPodManifests reads and parses every *.json manifest in dir, sorted by name.
func loadPodManifests(dir string) []podSpec {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	specs := make([]podSpec, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var spec podSpec
		if err := json.Unmarshal(data, &spec); err != nil {
			fmt.Printf("  skipping %s: %v\n", name, err)
			continue
		}
		specs = append(specs, spec)
	}
	return specs
}

// podHandle is a started pod container plus the host-side ends of its sync
// machinery, handed from startPodContainer to the host-side network setup and
// the bounded wait.
type podHandle struct {
	spec         podSpec
	cmd          *exec.Cmd
	cgroup       string
	vethReadyR   *os.File // host reads (container signals ready)
	vethCreatedW *os.File // host writes (signal container the link exists)
}

// runPod creates the pod's cgroup with its resource limits, launches its
// container in fresh PID/UTS/IPC/NET/mount namespaces, moves it into the cgroup
// before it runs (via a sync pipe, as a container runtime does), and reports the
// container's isolated view.
func runPod(spec podSpec) {
	var netEnv []string
	if spec.Network != nil {
		netEnv = []string{
			podIPEnv + "=" + spec.Network.PodIP,
			hostIPEnv + "=" + spec.Network.HostIP,
			fmt.Sprintf("%s=%d", vethPrefxEnv, spec.Network.Prefix),
		}
	}
	h := startPodContainer(spec, netEnv)
	if h == nil {
		return
	}

	// For a veth pod, the node agent (running in the host/initial network
	// namespace) creates the veth pair via RTNETLINK and places the pod end in
	// the container's netns — exactly what a CNI plugin does. It then tells the
	// container (fd 5) to assign its address, and once the container signals it
	// is listening (fd 4), sends it a datagram over the veth to prove
	// cross-namespace connectivity.
	if spec.Network != nil {
		if err := hostSetupVeth(spec, h.cmd.Process.Pid); err != nil {
			fmt.Printf("  pod[%s] host: veth setup failed: %v\n", spec.Name, err)
		}
		h.signalVethCreated()
	}

	waitPod(spec, h)
}

// startPodContainer creates the pod's cgroup with its resource limits, launches
// the container in fresh PID/UTS/IPC/NET/mount namespaces, moves it into the
// cgroup before it runs (via the fd 3 sync pipe), and releases it. netEnv is
// appended to the container's environment; when spec.Network is set the veth
// sync pipes (fd 4/5) are created too. On failure it prints the reason and
// returns nil.
func startPodContainer(spec podSpec, netEnv []string) *podHandle {
	fmt.Printf("astrokube-init: starting pod %q (mem=%dMiB cpu=%d%%, namespaces=pid,uts,ipc,net,mnt)\n",
		spec.Name, spec.Resources.MemoryMaxMiB, spec.Resources.CPUMaxPercent)

	cgroup := "/sys/fs/cgroup/" + spec.Name
	if err := os.Mkdir(cgroup, 0o755); err != nil && !os.IsExist(err) {
		fmt.Printf("  pod %q: cannot create cgroup: %v\n", spec.Name, err)
		return nil
	}
	if spec.Resources.MemoryMaxMiB > 0 {
		_ = os.WriteFile(cgroup+"/memory.max",
			[]byte(strconv.Itoa(spec.Resources.MemoryMaxMiB<<20)), 0)
	}
	if spec.Resources.CPUMaxPercent > 0 {
		quota := spec.Resources.CPUMaxPercent * 1000 // of a 100000us period
		_ = os.WriteFile(cgroup+"/cpu.max", []byte(fmt.Sprintf("%d 100000", quota)), 0)
	}

	// A sync pipe lets us move the container into its cgroup before it runs any
	// of its own code — the same ordering a real runtime (runc) uses.
	syncR, syncW, err := os.Pipe()
	if err != nil {
		fmt.Printf("  pod %q: pipe failed: %v\n", spec.Name, err)
		os.Remove(cgroup)
		return nil
	}

	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Env = append(append([]string{}, spec.Env...), podHostnameEnv+"="+spec.Hostname)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWIPC | syscall.CLONE_NEWNET,
	}
	extraFiles := []*os.File{syncR} // fd 3: cgroup sync

	// For a veth pod, set up two more pipes:
	//   fd 4 (veth-ready):   container → host, "my address is assigned, I'm listening"
	//   fd 5 (veth-created): host → container, "I created your eth0; assign its address"
	// and pass the pod's address via env.
	var vethReadyR *os.File   // host reads (container signals ready)
	var vethCreatedW *os.File // host writes (signal container the link exists)
	if spec.Network != nil {
		r4, w4, perr := os.Pipe()
		if perr != nil {
			fmt.Printf("  pod %q: veth pipe failed: %v\n", spec.Name, perr)
			syncR.Close()
			syncW.Close()
			os.Remove(cgroup)
			return nil
		}
		r5, w5, perr := os.Pipe()
		if perr != nil {
			fmt.Printf("  pod %q: veth pipe failed: %v\n", spec.Name, perr)
			syncR.Close()
			syncW.Close()
			r4.Close()
			w4.Close()
			os.Remove(cgroup)
			return nil
		}
		vethReadyR = r4
		vethCreatedW = w5
		extraFiles = append(extraFiles, w4) // fd 4: veth-ready  (child writes)
		extraFiles = append(extraFiles, r5) // fd 5: veth-created (child reads)
	}
	cmd.Env = append(cmd.Env, netEnv...)
	cmd.ExtraFiles = extraFiles
	prefix := &prefixWriter{prefix: "  pod[" + spec.Name + "] "}
	cmd.Stdout = prefix
	cmd.Stderr = prefix

	if err := cmd.Start(); err != nil {
		fmt.Printf("  pod %q: failed to start container: %v\n", spec.Name, err)
		syncR.Close()
		syncW.Close()
		os.Remove(cgroup)
		return nil
	}
	syncR.Close() // only the child needs the read end
	for _, f := range extraFiles[1:] {
		f.Close() // close our copies of the child's write ends
	}

	// Move the container into its cgroup using its host-namespace PID, then
	// release it to run.
	if err := os.WriteFile(cgroup+"/cgroup.procs", []byte(strconv.Itoa(cmd.Process.Pid)), 0); err != nil {
		fmt.Printf("  pod[%s] WARN cgroup.procs write: %v\n", spec.Name, err)
	}
	_, _ = syncW.Write([]byte{1})
	syncW.Close()

	return &podHandle{spec: spec, cmd: cmd, cgroup: cgroup, vethReadyR: vethReadyR, vethCreatedW: vethCreatedW}
}

// signalVethCreated tells the container (fd 5) that its eth0 exists, so it can
// assign its address, and hands the ready pipe (fd 4) to the host-side ping.
func (h *podHandle) signalVethCreated() {
	if h.vethCreatedW != nil {
		_, _ = h.vethCreatedW.Write([]byte{1})
		h.vethCreatedW.Close()
	}
	if h.vethReadyR != nil {
		go hostSideVethPing(h.spec, h.vethReadyR)
	}
}

// waitPod waits for the pod's container, but never lets a single pod wedge the
// node: it is bounded by a watchdog, and the pod's cgroup is removed afterward.
func waitPod(spec podSpec, h *podHandle) {
	defer os.Remove(h.cgroup)
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-time.After(15 * time.Second):
		_ = h.cmd.Process.Kill()
		// Don't block the node indefinitely if the kill doesn't reap promptly.
		select {
		case waitErr = <-done:
		case <-time.After(2 * time.Second):
			waitErr = fmt.Errorf("timed out after 15s (kill did not reap)")
		}
	}

	if waitErr != nil {
		fmt.Printf("astrokube-init: pod %q exited with error: %v\n", spec.Name, waitErr)
	} else {
		fmt.Printf("astrokube-init: pod %q completed successfully\n", spec.Name)
	}
}

// prefixWriter writes each newline-terminated chunk to stdout with a fixed
// prefix, so a pod's container output is streamed live and clearly attributed.
type prefixWriter struct {
	prefix string
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		fmt.Println(w.prefix + line)
	}
	return len(p), nil
}

// runContainer is the entrypoint executed inside a pod's namespaces. It waits to
// be placed in its cgroup, isolates its hostname, and reports the view it sees,
// proving the namespace and cgroup isolation are live.
func runContainer() {
	// Wait until the node agent has moved us into our cgroup (fd 3 is the sync
	// pipe's read end).
	if sync := os.NewFile(3, "sync"); sync != nil {
		var buf [1]byte
		_, _ = sync.Read(buf[:])
		sync.Close()
	}

	if hostname := os.Getenv(podHostnameEnv); hostname != "" {
		if err := syscall.Sethostname([]byte(hostname)); err != nil {
			fmt.Printf("sethostname failed: %v\n", err)
		}
	}
	hostname, _ := os.Hostname()

	fmt.Printf("container started: pid=%d hostname=%q\n", os.Getpid(), hostname)
	fmt.Printf("isolated loopback: %s\n", loopbackRoundTrip())

	// A veth pod proves cross-namespace networking; an ordinary pod just runs a
	// bounded workload.
	if podIP := os.Getenv(podIPEnv); podIP != "" {
		fmt.Printf("veth networking: %s\n", containerVethTest(podIP))
	} else {
		fmt.Printf("workload: %s\n", containerWorkload())
	}
	fmt.Println("container exiting cleanly")
}

// hostSetupVeth runs in the host (initial) network namespace. It creates the
// veth pair via RTNETLINK — placing one end ("eth0") in the pod's network
// namespace (identified by peerPid) — and assigns the host-side address. This is
// the host half of what a CNI plugin does; the pod assigns its own address.
func hostSetupVeth(spec podSpec, peerPid int) error {
	nl, err := nlOpen()
	if err != nil {
		return fmt.Errorf("open netlink: %w", err)
	}
	defer nl.close()

	hostName := "veth-" + spec.Name
	if len(hostName) > ifNameMaxSize {
		hostName = hostName[:ifNameMaxSize]
	}

	// Create the pair: host end (hostName) stays here; peer "eth0" is placed in
	// the pod's netns. Equivalent to `ip link add hostName type veth peer name
	// eth0 netns <peerPid>`.
	if err := nl.createVethPeerNetns(hostName, "eth0", peerPid); err != nil {
		return fmt.Errorf("create veth pair: %w", err)
	}

	idx, err := nl.linkIndexByName(hostName)
	if err != nil {
		return fmt.Errorf("find %s: %w", hostName, err)
	}
	if err := nl.addAddrV4(idx, net.ParseIP(spec.Network.HostIP), spec.Network.Prefix); err != nil {
		return fmt.Errorf("assign %s: %w", spec.Network.HostIP, err)
	}

	fmt.Printf("  pod[%s] host: created veth %s (%s/%d), peer eth0 in pod netns pid=%d via RTNETLINK\n",
		spec.Name, hostName, spec.Network.HostIP, spec.Network.Prefix, peerPid)
	return nil
}

// runBridgedPods proves kernel bridging: it creates each requested bridge with
// its gateway address, then runs all bridged pods concurrently — they must
// overlap so the pod↔pod tests can reach each other through the bridge hub.
func runBridgedPods(specs []podSpec) {
	nl, err := nlOpen()
	if err != nil {
		fmt.Printf("bridge: FAILED to open netlink: %v\n", err)
		return
	}
	defer nl.close()

	// Create each distinct bridge via RTM_NEWLINK and put the gateway address on
	// the bridge itself (RTM_NEWADDR) — the bridge is the pods' default gateway.
	bridgeIdx := make(map[string]int)
	for _, spec := range specs {
		name := spec.Network.Bridge
		if _, done := bridgeIdx[name]; done {
			continue
		}
		if err := nl.createBridge(name); err != nil {
			fmt.Printf("bridge: FAILED to create %s: %v\n", name, err)
			continue
		}
		idx, err := nl.linkIndexByName(name)
		if err != nil {
			fmt.Printf("bridge: FAILED to find %s after create: %v\n", name, err)
			continue
		}
		if err := nl.addAddrV4(idx, net.ParseIP(spec.Network.GatewayIP), spec.Network.Prefix); err != nil {
			fmt.Printf("bridge: FAILED to assign %s to %s: %v\n", spec.Network.GatewayIP, name, err)
			continue
		}
		bridgeIdx[name] = idx
		fmt.Printf("bridge: created %s idx=%d gw=%s/%d\n", name, idx, spec.Network.GatewayIP, spec.Network.Prefix)
	}

	// Start every bridged pod concurrently; each goroutine carries the usual
	// 15s-per-pod watchdog, so the whole group stays bounded.
	var wg sync.WaitGroup
	for _, spec := range specs {
		idx, ok := bridgeIdx[spec.Network.Bridge]
		if !ok {
			fmt.Printf("astrokube-init: pod %q skipped: bridge %q FAILED\n", spec.Name, spec.Network.Bridge)
			continue
		}
		wg.Add(1)
		go func(spec podSpec, idx int) {
			defer wg.Done()
			runBridgedPod(spec, idx)
		}(spec, idx)
	}
	wg.Wait()
}

// runBridgedPod launches one bridged pod and wires its veth host half onto the
// bridge. The container's default-route logic is reused unchanged by passing
// the bridge's gateway address as the pod's "host IP".
func runBridgedPod(spec podSpec, bridgeIdx int) {
	netEnv := []string{
		podIPEnv + "=" + spec.Network.PodIP,
		hostIPEnv + "=" + spec.Network.GatewayIP, // the gateway lives on the bridge
		fmt.Sprintf("%s=%d", vethPrefxEnv, spec.Network.Prefix),
	}
	if spec.Network.PeerIP != "" {
		netEnv = append(netEnv, peerIPEnv+"="+spec.Network.PeerIP)
	}
	h := startPodContainer(spec, netEnv)
	if h == nil {
		return
	}

	if err := hostSetupVethOnBridge(spec, h.cmd.Process.Pid, bridgeIdx); err != nil {
		fmt.Printf("  pod[%s] host: bridged veth setup FAILED: %v\n", spec.Name, err)
	}
	h.signalVethCreated()

	waitPod(spec, h)
}

// hostSetupVethOnBridge runs in the host (initial) network namespace. It
// creates the pod's veth pair via RTNETLINK with the host half enslaved to the
// bridge at creation time (IFLA_MASTER) — the host half is a bridge port, not a
// standalone host interface, so there is nothing to look up or address here;
// the gateway address already lives on the bridge.
func hostSetupVethOnBridge(spec podSpec, peerPid, bridgeIdx int) error {
	nl, err := nlOpen()
	if err != nil {
		return fmt.Errorf("open netlink: %w", err)
	}
	defer nl.close()

	hostName := "veth-" + spec.Name
	if len(hostName) > ifNameMaxSize {
		hostName = hostName[:ifNameMaxSize]
	}

	if err := nl.createVethOnBridge(hostName, "eth0", peerPid, bridgeIdx); err != nil {
		return fmt.Errorf("create bridged veth pair: %w", err)
	}

	fmt.Printf("  pod[%s] host: created veth %s as port of bridge idx=%d, peer eth0 in pod netns pid=%d via RTNETLINK\n",
		spec.Name, hostName, bridgeIdx, peerPid)
	return nil
}

// containerVethTest runs inside the pod. It waits for the node agent to create
// its "eth0" end (fd 5), assigns the pod's address to it via RTNETLINK, then
// waits to receive the host's datagram over the veth — proving cross-namespace
// connectivity, all driven by real netlink rather than any astrokube-specific
// shim.
func containerVethTest(podIP string) string {
	prefix := 24
	fmt.Sscanf(os.Getenv(vethPrefxEnv), "%d", &prefix)

	// Wait until the node agent has created our eth0 in this netns.
	if created := os.NewFile(5, "veth-created"); created != nil {
		var b [1]byte
		_, _ = created.Read(b[:])
		created.Close()
	}

	// Assign our address to eth0 via RTNETLINK, in this pod's network namespace.
	nl, err := nlOpen()
	if err != nil {
		return "FAILED to open netlink: " + err.Error()
	}
	defer nl.close()
	idx, err := nl.linkIndexByName("eth0")
	if err != nil {
		return "FAILED to find eth0: " + err.Error()
	}
	if err := nl.addAddrV4(idx, net.ParseIP(podIP), prefix); err != nil {
		return "FAILED to assign " + podIP + ": " + err.Error()
	}

	// Program a default route via the host veth end (RTM_NEWROUTE), as a CNI
	// plugin does, then read it back via RTM_GETROUTE to confirm it landed.
	gw := os.Getenv(hostIPEnv)
	if gw != "" {
		if err := nl.addDefaultRouteV4(net.ParseIP(gw), idx); err != nil {
			fmt.Printf("route: FAILED to add default via %s: %v\n", gw, err)
		} else if got, err := nl.defaultRouteGateway(); err != nil {
			fmt.Printf("route: GETROUTE failed: %v\n", err)
		} else if got != nil && got.String() == gw {
			fmt.Printf("route: default via %s installed + read back via RTNETLINK\n", gw)
		} else {
			fmt.Printf("route: default route mismatch (wanted %s, got %v)\n", gw, got)
		}
	}

	// Ping the host's veth end across the namespaces: this exercises ICMP
	// echo end to end (raw socket send, veth delivery, the host interface's
	// echo responder, and the reply routed back to the pod).
	if gw != "" {
		fmt.Printf("veth icmp: %s\n", icmpPingTest(gw))
	}

	// A bridged pod also proves pod↔pod connectivity: its peer hangs off the
	// same bridge, so traffic between them flows through the bridge hub.
	if peer := os.Getenv(peerIPEnv); peer != "" {
		containerPeerTest(podIP, peer)
	}

	// Listen on our address before signaling readiness.
	conn, err := net.ListenPacket("udp", podIP+":"+vethUDPPort)
	if err != nil {
		return "FAILED to bind " + podIP + ": " + err.Error()
	}
	defer conn.Close()

	// Signal the node agent (fd 4) that our address is up and we are listening.
	if ready := os.NewFile(4, "veth-ready"); ready != nil {
		_, _ = ready.Write([]byte{1})
		ready.Close()
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 128)
	n, from, err := conn.ReadFrom(buf)
	if err != nil {
		return fmt.Sprintf("eth0 up (%s/%d) but no host datagram: %v", podIP, prefix, err)
	}
	return fmt.Sprintf("CROSS-NS OK — received %q from host %s over veth eth0 (pod %s, RTNETLINK)",
		string(buf[:n]), from, podIP)
}

// containerPeerTest runs inside a bridged pod and proves two pods reach each
// other through the bridge: an ICMP echo to the peer pod (retried, since the
// peer may still be assigning its address), then a UDP datagram pod→pod with
// deterministic roles so exactly one side listens and the other sends.
// Everything is bounded — a missing peer produces FAILED lines, not a hang.
func containerPeerTest(podIP, peerIP string) {
	// ICMP first: up to 3 attempts, 2s apart, to ride out peer startup skew.
	var res string
	for attempt := 1; attempt <= 3; attempt++ {
		res = icmpPingTest(peerIP)
		if strings.HasPrefix(res, "ok") {
			break
		}
		if attempt < 3 {
			time.Sleep(2 * time.Second)
		}
	}
	fmt.Printf("peer icmp: %s\n", res)

	// Roles by address order: the lower address listens, the higher one sends.
	me, peer := net.ParseIP(podIP).To4(), net.ParseIP(peerIP).To4()
	if me == nil || peer == nil {
		fmt.Printf("pod-udp: FAILED to parse addresses %q / %q\n", podIP, peerIP)
		return
	}
	if bytes.Compare(me, peer) < 0 {
		podUDPListen(podIP)
	} else {
		podUDPSend(peerIP)
	}
}

// podUDPListen is the lower-addressed bridged pod's half of the pod↔pod UDP
// test: bind the pod address and wait (bounded) for the peer's datagram.
func podUDPListen(podIP string) {
	conn, err := net.ListenPacket("udp", podIP+":"+bridgeUDPPort)
	if err != nil {
		fmt.Printf("pod-udp: FAILED to bind %s: %v\n", podIP, err)
		return
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 128)
	n, from, err := conn.ReadFrom(buf)
	if err != nil {
		fmt.Printf("pod-udp: FAILED to receive on %s: %v\n", podIP, err)
		return
	}
	fmt.Printf("pod-udp: got %q from %s\n", string(buf[:n]), from)
}

// podUDPSend is the higher-addressed bridged pod's half of the pod↔pod UDP
// test. UDP gives no delivery signal, so it sends up to 5 datagrams 1s apart —
// covering the window before the peer has bound — and reports the outcome.
func podUDPSend(peerIP string) {
	addr := peerIP + ":" + bridgeUDPPort
	sent := false
	for attempt := 1; attempt <= 5; attempt++ {
		if conn, err := net.Dial("udp", addr); err == nil {
			if _, werr := conn.Write([]byte("bridged-hello")); werr == nil {
				sent = true
			}
			conn.Close()
		}
		if attempt < 5 {
			time.Sleep(1 * time.Second)
		}
	}
	if sent {
		fmt.Printf("pod-udp: sent to %s\n", addr)
		return
	}
	fmt.Printf("pod-udp: FAILED to send to %s after 5 attempts\n", addr)
}

// hostSideVethPing waits for the veth pod's container to create the veth and
// start listening, then sends it a datagram from the initial namespace. The
// kernel routes it over the veth into the pod's namespace.
func hostSideVethPing(spec podSpec, ready *os.File) {
	defer ready.Close()
	var buf [1]byte
	if _, err := ready.Read(buf[:]); err != nil {
		return // container never became ready (e.g. veth creation failed)
	}
	addr := spec.Network.PodIP + ":" + vethUDPPort
	conn, err := net.Dial("udp", addr)
	if err != nil {
		fmt.Printf("  pod[%s] host: dial %s failed: %v\n", spec.Name, addr, err)
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping-from-host-over-veth")); err != nil {
		fmt.Printf("  pod[%s] host: send failed: %v\n", spec.Name, err)
		return
	}
	fmt.Printf("  pod[%s] host: sent datagram to %s over veth\n", spec.Name, addr)
}

// containerWorkload does a small, bounded amount of memory and CPU work to prove
// the container runs (within its limits) rather than being killed or hung.
func containerWorkload() string {
	const allocMiB = 8
	buf := make([]byte, allocMiB<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 0xa5
	}
	var acc uint64
	for i := 0; i < 5_000_000; i++ {
		acc = acc*1664525 + 1013904223 + uint64(i)
	}
	return fmt.Sprintf("ok (allocated %dMiB, computed checksum %#02x)", allocMiB, acc&0xff)
}
