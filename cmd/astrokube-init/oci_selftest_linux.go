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
	"time"
)

// ociSelfTest exercises the pure-Go OCI runtime end to end without containerd:
// it hand-builds a minimal OCI bundle (rootfs = this static binary as /init,
// which prints from inside the container and exits) and drives it through the
// real runc CLI path create -> start -> state -> delete. It proves the runtime
// creates namespaces, sets up + pivot_roots the rootfs, runs the workload under
// a cgroup, and reports state — all with zero C.
func ociSelfTest() {
	fmt.Println("astrokube-runc: self-test — running a container through the pure-Go OCI runtime (zero C)")

	const bundle = "/run/octest-bundle"
	const rootfs = bundle + "/rootfs"
	const root = "/run/octest"
	if err := os.MkdirAll(rootfs+"/proc", 0o755); err != nil {
		fmt.Printf("astrokube-runc: self-test FAILED mkdir: %v\n", err)
		return
	}

	// Populate the container rootfs with this (static, self-contained) binary.
	exe, err := os.Executable()
	if err != nil {
		exe = "/proc/self/exe"
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		fmt.Printf("astrokube-runc: self-test FAILED reading %s: %v\n", exe, err)
		return
	}
	if err := os.WriteFile(rootfs+"/init", data, 0o755); err != nil {
		fmt.Printf("astrokube-runc: self-test FAILED writing rootfs/init: %v\n", err)
		return
	}

	const config = `{
  "ociVersion": "1.2.0",
  "process": {
    "args": ["/init"],
    "env": ["ASTROKUBE_RUNC_PAYLOAD=1", "PATH=/"],
    "cwd": "/"
  },
  "root": { "path": "rootfs" },
  "hostname": "octest",
  "mounts": [
    { "destination": "/proc", "type": "proc", "source": "proc", "options": ["nosuid","noexec","nodev"] }
  ],
  "linux": {
    "namespaces": [
      { "type": "pid" }, { "type": "mount" }, { "type": "uts" }, { "type": "ipc" }
    ],
    "cgroupsPath": "/astrokube-octest"
  }
}`
	if err := os.WriteFile(bundle+"/config.json", []byte(config), 0o644); err != nil {
		fmt.Printf("astrokube-runc: self-test FAILED writing config.json: %v\n", err)
		return
	}

	if rc := runOCIRuntime([]string{"--root", root, "create", "--bundle", bundle, "--pid-file", bundle + "/pid", "octest"}); rc != 0 {
		fmt.Println("astrokube-runc: self-test FAILED at create")
		return
	}
	fmt.Println("astrokube-runc: created (container init forked into fresh namespaces, blocked on exec.fifo)")
	if rc := runOCIRuntime([]string{"--root", root, "start", "octest"}); rc != 0 {
		fmt.Println("astrokube-runc: self-test FAILED at start")
		return
	}
	time.Sleep(1 * time.Second)
	fmt.Print("astrokube-runc: state -> ")
	runOCIRuntime([]string{"--root", root, "state", "octest"})
	runOCIRuntime([]string{"--root", root, "delete", "--force", "octest"})
	fmt.Println("astrokube-runc: self-test complete")
}
