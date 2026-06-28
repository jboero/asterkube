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

import "os"

// pureGoMode reports whether this is a zero-C image: one with no C runtime
// (glibc/musl) present. In that image the dynamically-linked upstream
// containerd/runc/kubelet cannot execute, so the init runs only its own
// static, CGO-free Go code (mounts, probes, and the pure-Go node agent that
// launches containers via clone() + cgroups directly).
//
// Detection is by content, not a flag: if libc is absent the image is, by
// construction, pure Go.
func pureGoMode() bool {
	for _, libc := range []string{"/lib64/libc.so.6", "/lib/libc.so.6", "/usr/lib/libc.so.6"} {
		if _, err := os.Stat(libc); err == nil {
			return false
		}
	}
	return true
}
