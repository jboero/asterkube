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

// Config-drive cluster join: bind a GENERIC, self-contained asterkube node image
// to a specific cluster by attaching a small ext2 "join bundle" disk at boot —
// the cloud-init/Ignition model. The bundle is produced host-side by
// asterkube-join.sh and carries a PRE-ISSUED kubeconfig: a node client cert
// minted through the cluster's CSR API (CA key never leaves the cluster), the
// real cluster CA, node settings, and optional hosts/resolv.conf pins.
//
// Why this and not the boot-args token path: delivering an already-issued client
// cert means the kubelet registers with `--kubeconfig` DIRECTLY and never runs an
// in-VM TLS-bootstrap CSR. That CSR exchange is unreliable over the demo NIC on
// the Asterinas TCP/HTTP2 stack (it "rejects the request for an unknown reason"),
// which is what kept the released image from reaching a joined+Ready node. With a
// pre-issued cert there is no CSR to fail.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// joinBundleMount is where we mount the join-bundle disk (read-only).
	joinBundleMount = "/mnt/join"
	// joinBundleMarker is the sentinel asterkube-join.sh writes so the node can
	// recognize the config disk among any other attached volumes.
	joinBundleMarker = ".asterkube-join"
)

// clusterJoined is set true once either join path has registered the node, so the
// persist banner can honestly say the node joined a cluster.
var clusterJoined bool

// findJoinBundle locates the cluster join bundle disk, if one is attached. The
// disk may land on any virtio-blk node depending on attach order, so we look for
// the marker file rather than trust a fixed device name: first at spots the
// default fstab may already have mounted, then by probing raw devices read-only.
func findJoinBundle() (string, bool) {
	// 1. Already mounted? The default /etc/fstab mounts /dev/vda at /ext2.
	for _, dir := range []string{joinBundleMount, "/ext2"} {
		if isJoinBundle(dir) {
			return dir, true
		}
	}
	// 2. Probe raw virtio-blk devices as read-only ext2.
	_ = os.MkdirAll(joinBundleMount, 0o755)
	for _, dev := range []string{"/dev/vda", "/dev/vdb", "/dev/vdc", "/dev/vdd", "/dev/vde"} {
		if !exists(dev) {
			continue
		}
		if err := syscall.Mount(dev, joinBundleMount, "ext2", syscall.MS_RDONLY, ""); err != nil {
			continue
		}
		if isJoinBundle(joinBundleMount) {
			fmt.Printf("asterkube-init: cluster join bundle found on %s\n", dev)
			return joinBundleMount, true
		}
		_ = syscall.Unmount(joinBundleMount, 0)
	}
	return "", false
}

// isJoinBundle reports whether dir holds a join bundle (marker + kubeconfig).
func isJoinBundle(dir string) bool {
	return exists(filepath.Join(dir, joinBundleMarker)) && exists(filepath.Join(dir, "kubeconfig"))
}

// prepareKubeletNodeFiles creates the node files the real kubelet requires but a
// minimal Asterinas node lacks. /dev/kmsg backs the kubelet's kernel-log OOM
// watcher — WITHOUT it the kubelet aborts at startup ("failed to create kubelet:
// open /dev/kmsg: no such file or directory"); pointing it at /dev/null lets the
// open succeed and the watcher harmlessly exit. /etc/machine-id is the node's
// stable identity. Both are idempotent (errors ignored if already present).
func prepareKubeletNodeFiles() {
	_ = os.Symlink("/dev/null", "/dev/kmsg")
	_ = os.WriteFile("/etc/machine-id", []byte("0a57e1b1a5f34c0e9b00000000000001\n"), 0o444)

	// The kubelet's user-namespace manager reads the node's user database at
	// startup to build uid/gid mappings; on a minimal node these files are absent
	// and the kubelet aborts ("create user namespace manager: kubelet mappings:
	// open /etc/passwd: no such file or directory"). Provide a minimal, static
	// user database plus the subordinate-id ranges userns pods map through.
	writeIfAbsent := func(path, content string, mode os.FileMode) {
		if !exists(path) {
			_ = os.WriteFile(path, []byte(content), mode)
		}
	}
	writeIfAbsent("/etc/passwd", "root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/:/sbin/nologin\n", 0o644)
	writeIfAbsent("/etc/group", "root:x:0:\nnobody:x:65534:\n", 0o644)
	writeIfAbsent("/etc/subuid", "root:100000:65536\n", 0o644)
	writeIfAbsent("/etc/subgid", "root:100000:65536\n", 0o644)
}

// parseEnvFile reads a simple KEY=VALUE file (node.env), ignoring blanks and
// comments, into a map.
func parseEnvFile(path string) map[string]string {
	m := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		if i := strings.IndexByte(ln, '='); i > 0 {
			m[strings.TrimSpace(ln[:i])] = strings.TrimSpace(ln[i+1:])
		}
	}
	return m
}

// joinFromBundle binds this node to the cluster described by the join bundle in
// dir: it applies the bundle's host/DNS pins, installs CNI, brings up the static
// containerd, and launches the real kubelet with the PRE-ISSUED kubeconfig so it
// registers directly — no in-VM CSR.
func joinFromBundle(dir string) {
	kubeconfig := filepath.Join(dir, "kubeconfig")
	settings := parseEnvFile(filepath.Join(dir, "node.env"))
	node := settings["NODE_NAME"]
	if node == "" {
		node = nodeName
	}
	nodeIP := settings["NODE_IP"]
	if nodeIP == "" {
		nodeIP = "10.0.2.15"
	}

	fmt.Println()
	fmt.Println("asterkube-init: ===== cluster join (host-issued kubeconfig) =====")
	fmt.Printf("asterkube-init: node=%q, pre-issued node cert on the config disk (no in-VM CSR)\n", node)

	// 1. eth0 up (idempotent with DHCP-first).
	configureEth0BestEffort()

	// 2. Apply the bundle's cluster hostname pin + resolver, if present. Different
	//    clusters have different apiserver hostnames/DNS, so the node reads them
	//    from the bundle rather than guessing.
	installClusterHosts(filepath.Join(dir, "hosts"))
	if rc := filepath.Join(dir, "resolv.conf"); exists(rc) {
		if data, err := os.ReadFile(rc); err == nil {
			if err := os.WriteFile("/etc/resolv.conf", data, 0o644); err == nil {
				fmt.Println("asterkube-init: /etc/resolv.conf set from the join bundle")
			}
		}
	}

	// 3. Install the CNI (ptp+portmap conflist + static plugins) BEFORE containerd
	//    starts so its CRI reports NetworkReady — the prerequisite for Ready, not
	//    just Registered.
	setupCNI("/usr/share/asterkube/cni")

	// 4. Bring up the baked-in static containerd (persistent).
	linkRunc()
	env := append(os.Environ(), "PATH=/usr/bin:/bin", "XDG_RUNTIME_DIR=/run")
	sock := "/run/containerd/containerd.sock"
	cd, ok := startStaticContainerd("/run/containerd/root", "/run/containerd/state", sock, "/run/containerd/containerd.log", env)
	if !ok {
		fmt.Println("asterkube-init: FAILED join (containerd did not come up)")
		return
	}

	// 5. Launch the REAL kubelet with the pre-issued kubeconfig. No
	//    --bootstrap-kubeconfig, so there is no CSR exchange inside the VM.
	root := "/var/lib/kubelet"
	_ = os.MkdirAll(root+"/pki", 0o755)
	prepareKubeletNodeFiles()
	logPath := "/run/kubelet-join.log"
	logf, err := os.Create(logPath)
	if err != nil {
		fmt.Printf("asterkube-init: FAILED to create kubelet log: %v\n", err)
		return
	}
	args := []string{
		"--kubeconfig=" + kubeconfig,
		"--container-runtime-endpoint=unix://" + sock,
		"--image-service-endpoint=unix://" + sock,
		"--root-dir=" + root,
		"--cert-dir=" + root + "/pki",
		"--hostname-override=" + node,
		"--node-ip=" + nodeIP,
		"--register-node=true",
		"--node-labels=" + asterkubeNodeLabels, // kernel identity; kubernetes.io/os stays "linux"
		"--cgroup-driver=cgroupfs", "--cgroups-per-qos=false", "--enforce-node-allocatable=",
		"--cgroup-root=/", "--runtime-cgroups=/", "--kubelet-cgroups=/",
		"--fail-swap-on=false",
		"--resolv-conf=/etc/resolv.conf",
		"--node-status-update-frequency=4s",
		"--v=2",
	}
	if t := strings.TrimSpace(settings["TAINTS"]); t != "" {
		args = append(args, "--register-with-taints="+t)
	}
	if d := strings.TrimSpace(settings["CLUSTER_DNS"]); d != "" {
		args = append(args, "--cluster-dns="+d)
	}
	k := exec.Command("/usr/bin/kubelet", args...)
	k.Env = env
	k.Stdout = logf
	k.Stderr = logf
	if err := k.Start(); err != nil {
		fmt.Printf("asterkube-init: FAILED to start kubelet: %v\n", err)
		return
	}
	fmt.Printf("asterkube-init: kubelet registering node %q with its issued cert...\n", node)

	// 6. Watch for registration (or the first connectivity/auth gap).
	registered := false
	deadline := time.Now().Add(75 * time.Second)
	for time.Now().Before(deadline) {
		if k.ProcessState != nil && k.ProcessState.Exited() {
			fmt.Println("asterkube-init: kubelet exited early during join")
			break
		}
		log := readFileString(logPath)
		if strings.Contains(log, "Successfully registered node") || strings.Contains(log, "Successfully registered Node") {
			registered = true
			break
		}
		if hit := firstMatch(log, []string{"Unauthorized", "x509", "certificate signed by unknown", "connection refused", "i/o timeout", "tls:"}); hit != "" {
			fmt.Printf("asterkube-init: join gap detected: %q (continuing to watch)\n", hit)
		}
		time.Sleep(3 * time.Second)
	}
	if !registered {
		fmt.Println("asterkube-init: node did NOT register within 75s; kubelet log summary:")
		summarizeKubeletLog(logPath)
		_ = k.Process.Kill()
		_ = cd.Process.Kill()
		return
	}
	clusterJoined = true
	fmt.Printf("asterkube-init: CLUSTER JOIN PASSED — node %q registered with its issued cert ✓\n", node)
	fmt.Println("asterkube-init: CNI installed (ptp+portmap); the node reports Ready once")
	fmt.Println("asterkube-init: containerd's CRI confirms NetworkReady — check `kubectl get nodes`.")

	// 7. Keep containerd + kubelet running so the node persists (serveForever).
	keepAlive(cd)
	keepAlive(k)
}
