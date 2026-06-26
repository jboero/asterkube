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

// kubeletPhase runs the REAL kubelet in standalone mode (no apiserver): it reads
// a static pod manifest and creates the pod through the CRI, against the same
// containerd. This is Layer 2 of the cluster-join path, de-risked offline the
// same way ctr/crictl were. The kubelet exercises a large amount of kernel
// surface (cgroup hierarchy, cAdvisor machine info, /proc + /sys reads), so this
// is primarily a scouting harness: if the pod does not appear via CRI, it dumps
// the kubelet log so the first gap is visible.
func kubeletPhase(kubeletBin, crictlBin, sock string, env []string) {
	fmt.Println("-- phase 4: kubelet standalone (static pod via CRI) --")
	if _, err := os.Stat(kubeletBin); err != nil {
		fmt.Printf("kubelet: SKIPPED (no kubelet on the share: %v)\n", err)
		return
	}

	for _, d := range []string{"/etc/kubernetes/manifests", "/run/kubelet"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fmt.Printf("kubelet: FAILED mkdir %s: %v\n", d, err)
			return
		}
	}

	// Node files the kubelet expects but the minimal node lacks. /dev/kmsg backs
	// the kubelet's kernel-log OOM watcher; Asterinas has no kmsg device, so
	// point it at /dev/null (open succeeds, reads hit EOF and the watcher
	// harmlessly exits). /etc/machine-id is the node's stable identity.
	_ = os.Symlink("/dev/null", "/dev/kmsg")
	_ = os.WriteFile("/etc/machine-id", []byte("0a57e1b1a5f34c0e9b00000000000001\n"), 0o444)
	if err := os.WriteFile("/etc/kubernetes/manifests/hello.yaml", []byte(staticPodManifest), 0o644); err != nil {
		fmt.Printf("kubelet: FAILED to write static pod: %v\n", err)
		return
	}

	const kubeletLog = "/run/kubelet.log"
	logFile, err := os.Create(kubeletLog)
	if err != nil {
		fmt.Printf("kubelet: FAILED to create log: %v\n", err)
		return
	}
	defer logFile.Close()

	args := []string{
		"--container-runtime-endpoint=unix://" + sock,
		"--image-service-endpoint=unix://" + sock,
		"--pod-manifest-path=/etc/kubernetes/manifests",
		"--root-dir=/run/kubelet",
		// Keep cgroup management minimal — don't build the QoS hierarchy or
		// enforce allocatable, which exercise more of the cgroup surface.
		"--cgroup-driver=cgroupfs",
		"--cgroups-per-qos=false",
		"--enforce-node-allocatable=",
		"--cgroup-root=/",
		"--runtime-cgroups=/",
		"--kubelet-cgroups=/",
		"--fail-swap-on=false",
		"--hostname-override=astrokube",
		"--resolv-conf=/etc/resolv.conf",
		"--v=2",
	}
	k := exec.Command(kubeletBin, args...)
	k.Env = env
	k.Stdout = logFile
	k.Stderr = logFile
	if err := k.Start(); err != nil {
		fmt.Printf("kubelet: FAILED to start: %v\n", err)
		return
	}
	defer func() {
		_ = k.Process.Kill()
		_, _ = k.Process.Wait()
	}()
	fmt.Println("kubelet: started in standalone mode; waiting for the static pod via CRI...")

	criBase := []string{"--runtime-endpoint", "unix://" + sock}
	deadline := time.Now().Add(45 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		if k.ProcessState != nil && k.ProcessState.Exited() {
			fmt.Println("kubelet: process exited early")
			break
		}
		out, _ := runWithTimeoutEnv(10*time.Second, env, crictlBin,
			append(append([]string{}, criBase...), "pods", "--name", "hello")...)
		if strings.Contains(out, "hello") {
			found = true
			for _, line := range splitLines(out) {
				if line != "" {
					fmt.Printf("kubelet(crictl pods)| %s\n", line)
				}
			}
			break
		}
		time.Sleep(3 * time.Second)
	}

	if !found {
		fmt.Println("kubelet: static pod NOT observed via CRI within 45s; kubelet log tail:")
		dumpTail(kubeletLog, 80)
		return
	}
	fmt.Println("kubelet: PHASE 4 PASSED — the real kubelet created a static pod via the CRI")
}

// staticPodManifest is a host-network static pod that runs the side-loaded hello
// image (already present in the CRI image store, so no pull).
const staticPodManifest = `apiVersion: v1
kind: Pod
metadata:
  name: hello
  namespace: default
spec:
  hostNetwork: true
  restartPolicy: Never
  containers:
  - name: hello
    image: docker.io/astrokube/hello:latest
    imagePullPolicy: Never
    command: ["/hello"]
`
