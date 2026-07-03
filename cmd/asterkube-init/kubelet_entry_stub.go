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

	kubelettypes "k8s.io/kubelet/pkg/types"
)

// runAsKubelet (standalone build in the k8s.io/kubelet module) is a placeholder:
// this module cannot import the full kubelet command. The COMBINED zero-C build
// (cmd/asterkube-kubelet in the kubernetes tree) replaces this file with
// kubelet_entry_real.go, which runs the real upstream kubelet, CGO-free.
//
// build-zeroc-kubelet.sh omits this file and supplies the real one instead.
func runAsKubelet() {
	fmt.Println("asterkube-init: not running as PID 1; deferring to the normal kubelet entry point.")
	fmt.Printf("asterkube-init: (kubelet pod-name label key is %q)\n", kubelettypes.KubernetesPodNameLabel)
}
