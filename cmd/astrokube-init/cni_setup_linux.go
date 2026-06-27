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

// podCIDR is the pod subnet the controller-manager assigned to this node. The
// CNI IPAM hands out pod addresses from it. (Assigned deterministically as the
// Nth /24 of the cluster pod CIDR; update if the node is recreated.)
const podCIDR = "10.244.4.0/24"

// setupCNI installs a minimal CNI configuration so containerd's CRI reports
// NetworkReady — the prerequisite for the node to become Ready. It must run
// BEFORE the containerd daemon starts so the config is present when the CRI
// initializes its CNI (rather than relying on a runtime fsnotify reload).
//
// containerd uses the default CNI directories (we launch it without a custom
// config): conf_dir=/etc/cni/net.d, bin_dir=/opt/cni/bin. The config mirrors
// kind's own (ptp + portmap, host-local IPAM); the static plugin binaries are
// copied from the virtio-fs share for actual pod networking later. NetworkReady
// itself only needs a parseable conflist, so this degrades gracefully if the
// plugin binaries are absent.
func setupCNI(cniSrcDir string) {
	for _, d := range []string{"/opt/cni/bin", "/etc/cni/net.d", "/run/cni-ipam-state"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fmt.Printf("cni: FAILED mkdir %s: %v\n", d, err)
			return
		}
	}

	if entries, err := os.ReadDir(cniSrcDir); err == nil {
		n := 0
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if err := copyExecutable(filepath.Join(cniSrcDir, e.Name()),
				filepath.Join("/opt/cni/bin", e.Name())); err != nil {
				fmt.Printf("cni: WARN copy %s: %v\n", e.Name(), err)
				continue
			}
			n++
		}
		fmt.Printf("cni: installed %d plugin binaries from %s into /opt/cni/bin\n", n, cniSrcDir)
	} else {
		fmt.Printf("cni: no plugins on the share (%s): %v; config-only readiness\n", cniSrcDir, err)
	}

	if err := os.WriteFile("/etc/cni/net.d/10-astrokube.conflist", []byte(cniConflist), 0o644); err != nil {
		fmt.Printf("cni: FAILED to write conflist: %v\n", err)
		return
	}
	fmt.Printf("cni: wrote /etc/cni/net.d/10-astrokube.conflist (ptp + portmap, %s)\n", podCIDR)
}

// copyExecutable copies src to dst and marks it executable.
func copyExecutable(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		return err
	}
	return os.Chmod(dst, 0o755)
}

// cniConflist is a kind-style ptp+portmap network with host-local IPAM over the
// node's pod CIDR.
const cniConflist = `{
  "cniVersion": "0.3.1",
  "name": "astrokube",
  "plugins": [
    {
      "type": "ptp",
      "ipMasq": true,
      "ipam": {
        "type": "host-local",
        "dataDir": "/run/cni-ipam-state",
        "routes": [{ "dst": "0.0.0.0/0" }],
        "ranges": [[{ "subnet": "` + podCIDR + `" }]]
      },
      "mtu": 1500
    },
    {
      "type": "portmap",
      "capabilities": { "portMappings": true }
    }
  ]
}
`
