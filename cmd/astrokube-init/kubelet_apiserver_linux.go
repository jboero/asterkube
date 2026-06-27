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
	"os"
	"os/exec"
	"strings"
	"time"
)

// nodeName is the node identity the kubelet registers under; it must match the
// CN=system:node:<nodeName> in the staged kubeconfig's client certificate.
const nodeName = "astrokube"

// serveWindow is how long the node stays up after registering, so the control
// plane can mark it Ready and the scheduler can place a pod on it (and the host
// can observe). The window ends early once a scheduled container starts.
const serveWindow = 180 * time.Second

// apiserverPhase runs the REAL kubelet connected to a real Kubernetes apiserver
// (Layer 3a of the cluster-join path). Unlike the standalone phase, this kubelet
// authenticates to the apiserver with a CA-signed node client certificate
// (identity system:node:<node>, staged in the kubeconfig on the virtio-fs share)
// and registers a Node object. Reaching the apiserver uses the slirp path proven
// earlier (guest -> 10.0.2.2:<hostport> over TLS).
//
// The milestone here is REGISTRATION: the Asterinas node becomes visible to the
// real control plane (`kubectl get nodes`). It will be NotReady until a CNI is
// wired (Layer 3b), which is expected. This is a scouting harness: if the kubelet
// cannot register, it dumps the log so the first gap is visible.
func apiserverPhase(kubeletBin, sock, kubeconfig string, env []string) {
	fmt.Println("-- phase 5: kubelet joins a real apiserver (node registration) --")
	if _, err := os.Stat(kubeletBin); err != nil {
		fmt.Printf("apiserver: SKIPPED (no kubelet on the share: %v)\n", err)
		return
	}
	if _, err := os.Stat(kubeconfig); err != nil {
		fmt.Printf("apiserver: SKIPPED (no kubeconfig on the share at %s: %v)\n", kubeconfig, err)
		fmt.Println("apiserver: stage one with astrokube/make-node-kubeconfig.sh to enable this phase")
		return
	}

	const kubeletRoot = "/ext2/kubelet-apiserver"
	if err := os.MkdirAll(kubeletRoot, 0o755); err != nil {
		fmt.Printf("apiserver: FAILED mkdir %s: %v\n", kubeletRoot, err)
		return
	}

	const kubeletLog = "/run/kubelet-apiserver.log"
	logFile, err := os.Create(kubeletLog)
	if err != nil {
		fmt.Printf("apiserver: FAILED to create log: %v\n", err)
		return
	}
	defer logFile.Close()

	args := []string{
		"--kubeconfig=" + kubeconfig,
		"--container-runtime-endpoint=unix://" + sock,
		"--image-service-endpoint=unix://" + sock,
		"--root-dir=" + kubeletRoot,
		"--hostname-override=" + nodeName,
		// The node's address as seen by the apiserver (the slirp guest IP).
		"--node-ip=10.0.2.15",
		"--register-node=true",
		// Keep our experimental node out of the scheduler's default pool until
		// networking (Layer 3b) is ready, so cluster workloads don't land on a
		// NotReady node. DaemonSets with broad tolerations may still target it.
		"--register-with-taints=astrokube.io/experimental=true:NoSchedule",
		// Minimal cgroup management, matching the standalone phase.
		"--cgroup-driver=cgroupfs",
		"--cgroups-per-qos=false",
		"--enforce-node-allocatable=",
		"--cgroup-root=/",
		"--runtime-cgroups=/",
		"--kubelet-cgroups=/",
		"--fail-swap-on=false",
		"--resolv-conf=/etc/resolv.conf",
		// Register/heartbeat faster so the node appears promptly within the probe.
		"--node-status-update-frequency=4s",
		"--v=2",
	}
	k := exec.Command(kubeletBin, args...)
	k.Env = env
	k.Stdout = logFile
	k.Stderr = logFile
	if err := k.Start(); err != nil {
		fmt.Printf("apiserver: FAILED to start kubelet: %v\n", err)
		return
	}
	defer func() {
		_ = k.Process.Kill()
		_, _ = k.Process.Wait()
	}()
	fmt.Printf("apiserver: kubelet started; connecting to the apiserver and registering node %q...\n", nodeName)

	// Watch the kubelet log for the registration signal (or an early fatal).
	deadline := time.Now().Add(75 * time.Second)
	registered := false
	for time.Now().Before(deadline) {
		if k.ProcessState != nil && k.ProcessState.Exited() {
			fmt.Println("apiserver: kubelet exited early")
			break
		}
		log := readFileString(kubeletLog)
		if strings.Contains(log, "Successfully registered node") ||
			strings.Contains(log, "Successfully registered Node") {
			registered = true
			break
		}
		// Surface the most common first gaps explicitly.
		if hit := firstMatch(log, []string{
			"connection refused", "x509", "Unauthorized", "certificate signed by unknown authority",
			"no route to host", "i/o timeout", "tls: ",
		}); hit != "" {
			fmt.Printf("apiserver: connectivity/auth gap detected: %q\n", hit)
			// keep waiting in case it recovers (e.g. transient during startup)
		}
		time.Sleep(3 * time.Second)
	}

	if !registered {
		fmt.Println("apiserver: node NOT registered within 75s; kubelet log summary:")
		summarizeKubeletLog(kubeletLog)
		return
	}
	fmt.Printf("apiserver: PHASE 5 PASSED — the Asterinas node registered with the real apiserver as %q\n", nodeName)
	fmt.Println("apiserver: now serving as a live cluster node; the control plane can mark it Ready and schedule pods onto it.")

	// Stay up as a live node so the control plane marks us Ready and the
	// scheduler can place pods here, and so the host can observe via kubectl.
	// Report node-readiness and any apiserver-scheduled pod lifecycle as it
	// happens (deduplicated). Exit the window early once a scheduled pod has
	// started a container.
	serveDeadline := time.Now().Add(serveWindow)
	reported := make(map[string]bool)
	for time.Now().Before(serveDeadline) {
		if k.ProcessState != nil && k.ProcessState.Exited() {
			fmt.Println("apiserver: kubelet exited during the serve window")
			break
		}
		for _, ln := range splitLines(readFileString(kubeletLog)) {
			if firstMatch(ln, []string{
				"status is now: NodeReady",
				"source=\"api\"",
				"Started container",
				"Created container",
				"RunPodSandbox",
				"FailedMount",
				"failed to",
			}) == "" {
				continue
			}
			if reported[ln] {
				continue
			}
			reported[ln] = true
			msg := ln
			if i := strings.Index(msg, "] "); i > 0 && i+2 < len(msg) {
				msg = msg[i+2:]
			}
			fmt.Printf("apiserver(kubelet)| %s\n", msg)
		}
		time.Sleep(5 * time.Second)
	}
	fmt.Println("apiserver: serve window complete; powering off (the node will go NotReady once this VM stops).")
}

// summarizeKubeletLog prints the parts of a (possibly large, goroutine-dump-
// heavy) kubelet log that explain a failure: any crash header (panic / Go fatal
// error / signal-triggered dump) with a following window, every line matching a
// connectivity/auth/registration keyword, and the final lines as a fallback.
// This survives the log being mostly goroutine stacks, where a plain tail misses
// the cause entirely.
func summarizeKubeletLog(path string) {
	lines := splitLines(readFileString(path))
	if len(lines) == 0 {
		fmt.Println("  (kubelet log empty or unreadable)")
		return
	}

	// 1) Crash header + window. Go writes "panic:" / "fatal error:" / a signal
	// banner ("SIGQUIT: quit"), then the crashing goroutine "[running]:".
	crashMarkers := []string{"panic:", "fatal error:", "SIGQUIT", "SIGABRT", "SIGSEGV", "[running]:"}
	for i, ln := range lines {
		if firstMatch(ln, crashMarkers) != "" {
			fmt.Printf("  --- crash header at line %d ---\n", i+1)
			end := i + 25
			if end > len(lines) {
				end = len(lines)
			}
			for _, c := range lines[i:end] {
				fmt.Printf("  log| %s\n", c)
			}
			fmt.Println("  --- end crash header ---")
			break
		}
	}

	// 2) Every interesting non-stack line (connectivity, auth, registration).
	keywords := []string{
		"connection refused", "x509", "Unauthorized", "Forbidden", "forbidden",
		"certificate", "tls:", "TLS", "no route to host", "i/o timeout", "EOF",
		"register", "Register", "node \"", "Failed", "failed to", "Unable",
		"apiserver", "controller", "lease",
	}
	printed := 0
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "log| ") {
			ln = strings.TrimSpace(ln)[len("log| "):]
		}
		// Skip goroutine-stack noise.
		if strings.HasPrefix(ln, "\t") || strings.Contains(ln, "goroutine ") ||
			strings.Contains(ln, "runtime.") || strings.Contains(ln, "+0x") {
			continue
		}
		if firstMatch(ln, keywords) != "" {
			fmt.Printf("  kubelet| %s\n", ln)
			if printed++; printed >= 60 {
				fmt.Println("  ... (more matching lines truncated)")
				break
			}
		}
	}
	if printed == 0 {
		fmt.Println("  (no connectivity/auth/registration lines matched; raw tail:)")
		dumpTail(path, 40)
	}
}

// firstMatch returns the first needle found in haystack, or "".
func firstMatch(haystack string, needles []string) string {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return n
		}
	}
	return ""
}

// readFileString reads a file, returning "" on error.
func readFileString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
