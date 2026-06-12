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
)

// containerdBin / ctrBin are where containerd and its CLI are baked in.
const (
	containerdBin = "/usr/bin/containerd"
	ctrBin        = "/usr/bin/ctr"
)

// containerdPhase2 brings up the containerd daemon and drives it with ctr to run
// a container from a side-loaded image (no network pull). Filled in once Phase 1
// (runc) is solid; for now it reports readiness so the boot is informative.
func containerdPhase2() {
	fmt.Println("-- phase 2: containerd daemon --")
	if _, err := os.Stat(containerdBin); err != nil {
		fmt.Printf("containerd: SKIPPED (no %s baked into image)\n", containerdBin)
		return
	}
	// TODO(phase2): start containerd, ctr image import a baked OCI tarball, ctr run.
	fmt.Println("containerd: present; daemon bring-up pending Phase 1 sign-off")
}
