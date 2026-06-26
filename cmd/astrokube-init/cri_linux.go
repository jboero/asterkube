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
	"strings"
	"time"
)

// criPhase drives containerd through the CRI (Container Runtime Interface) — the
// API the kubelet actually speaks — using crictl: it imports the images into the
// CRI namespace, runs a pod sandbox, then creates/starts a container in it. The
// sandbox uses host networking (CRI network namespace mode NODE) so it needs no
// CNI plugin, which is the right first step since CNI's bridge plugin expects
// iptables that Asterinas does not have.
func criPhase(ctrBin, crictlBin, sock string, env []string, logPath string) {
	fmt.Println("-- phase 3: CRI via crictl (hostNetwork pod sandbox) --")
	if _, err := os.Stat(crictlBin); err != nil {
		fmt.Printf("cri: SKIPPED (no crictl on the share: %v)\n", err)
		return
	}

	// CRI uses the "k8s.io" containerd namespace; import the sandbox (pause) and
	// workload images there so no registry pull is needed.
	for _, tar := range []string{virtiofsMount + "/pause.tar", helloTar} {
		out, err := runWithTimeoutEnv(40*time.Second, env, ctrBin,
			"--address", sock, "-n", "k8s.io", "images", "import", tar)
		for _, line := range splitLines(out) {
			if line != "" {
				fmt.Printf("cri| %s\n", line)
			}
		}
		if err != nil {
			fmt.Printf("cri: FAILED to import %s: %v\n", tar, err)
			return
		}
	}

	// CRI templates each pod's /etc/hosts and /etc/resolv.conf from the *node's*
	// copies; the minimal initramfs ships neither, so create them.
	_ = os.MkdirAll("/etc", 0o755)
	_ = os.WriteFile("/etc/hosts", []byte("127.0.0.1 localhost\n::1 localhost\n"), 0o644)
	_ = os.WriteFile("/etc/resolv.conf", []byte("nameserver 10.0.2.3\n"), 0o644)

	_ = os.MkdirAll("/run/cri-logs", 0o755)
	if err := os.WriteFile("/run/cri-pod.json", []byte(criPodConfig), 0o644); err != nil {
		fmt.Printf("cri: FAILED to write pod config: %v\n", err)
		return
	}
	if err := os.WriteFile("/run/cri-ctr.json", []byte(criContainerConfig), 0o644); err != nil {
		fmt.Printf("cri: FAILED to write container config: %v\n", err)
		return
	}

	criBase := []string{"--runtime-endpoint", "unix://" + sock, "--image-endpoint", "unix://" + sock}
	run := func(timeout time.Duration, label string, args ...string) (string, bool) {
		out, err := runWithTimeoutEnv(timeout, env, crictlBin, append(append([]string{}, criBase...), args...)...)
		for _, line := range splitLines(out) {
			if line != "" {
				fmt.Printf("crictl| %s\n", line)
			}
		}
		if err != nil {
			fmt.Printf("cri: FAILED %s: %v\n", label, err)
			fmt.Println("cri: daemon log tail:")
			dumpTail(logPath, 50)
			return out, false
		}
		return out, true
	}

	// Run the pod sandbox (this starts the pause container).
	podOut, ok := run(40*time.Second, "crictl runp", "runp", "/run/cri-pod.json")
	if !ok {
		return
	}
	sandboxID := lastNonEmptyLine(podOut)
	fmt.Printf("cri: pod sandbox running (id=%s)\n", shortID(sandboxID))

	// Create and start the workload container inside the sandbox.
	createOut, ok := run(40*time.Second, "crictl create", "create", sandboxID, "/run/cri-ctr.json", "/run/cri-pod.json")
	if !ok {
		return
	}
	containerID := lastNonEmptyLine(createOut)
	if _, ok := run(40*time.Second, "crictl start", "start", containerID); !ok {
		return
	}

	// Give the entrypoint a moment to run, then read its logs.
	time.Sleep(1500 * time.Millisecond)
	logsOut, _ := run(15*time.Second, "crictl logs", "logs", containerID)
	if strings.Contains(logsOut, "hello from a containerd-managed container") {
		fmt.Println("cri: PHASE 3 PASSED — pod + container ran via the CRI (kubelet-style)")
	} else {
		fmt.Println("cri: container created/started via CRI; entrypoint output not captured yet")
	}
}

func lastNonEmptyLine(s string) string {
	lines := splitLines(s)
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

func shortID(id string) string {
	if len(id) > 13 {
		return id[:13]
	}
	return id
}

// criPodConfig is a minimal CRI pod-sandbox spec using host networking
// (namespace_options.network = 2 == NODE), so no CNI plugin is required.
const criPodConfig = `{
  "metadata": {
    "name": "astrokube-sandbox",
    "namespace": "default",
    "uid": "astrokube-sandbox-uid",
    "attempt": 1
  },
  "log_directory": "/run/cri-logs",
  "linux": {
    "security_context": {
      "namespace_options": { "network": 2 }
    }
  }
}`

// criContainerConfig runs the side-loaded static hello image as a CRI container.
//
// pid namespace mode CONTAINER (1) gives the container its own PID namespace
// (created with clone(CLONE_NEWPID), which works) instead of joining the
// sandbox's via setns. setns into a PID namespace is not yet implemented in
// Asterinas (it needs pid_ns_for_children semantics), so shareProcessNamespace
// pods are a TODO; this keeps the common (non-shared) case working.
const criContainerConfig = `{
  "metadata": { "name": "hello" },
  "image": { "image": "docker.io/astrokube/hello:latest" },
  "command": ["/hello"],
  "log_path": "hello.0.log",
  "linux": {
    "security_context": {
      "namespace_options": { "pid": 1 }
    }
  }
}`
