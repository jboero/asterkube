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
	"path/filepath"
)

// installCABundle places a system CA trust bundle (staged from the host on the
// virtio-fs share) at the paths Go's crypto/x509 consults on Linux. Without it,
// containerd cannot verify TLS to image registries and pulls fail with "x509:
// certificate signed by unknown authority" — which blocks the cluster's
// kube-proxy/kindnet DaemonSets from pulling their images onto the node.
//
// Must run before containerd starts pulling (Go builds the cert pool lazily on
// the first TLS dial, so installing it before the daemon launches is safe).
func installCABundle(src string) {
	data, err := os.ReadFile(src)
	if err != nil {
		fmt.Printf("ca: no CA bundle on the share (%s): %v; registry TLS will fail\n", src, err)
		return
	}
	// Go's crypto/x509 (root_linux.go) tries these in order and uses the first
	// that exists; write the common Debian and Fedora/RHEL locations.
	targets := []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	}
	n := 0
	for _, t := range targets {
		if err := os.MkdirAll(filepath.Dir(t), 0o755); err != nil {
			continue
		}
		if err := os.WriteFile(t, data, 0o644); err != nil {
			continue
		}
		n++
	}
	fmt.Printf("ca: installed host CA bundle (%d bytes) to %d trust path(s)\n", len(data), n)
}
