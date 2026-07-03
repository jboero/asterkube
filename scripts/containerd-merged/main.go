//go:build linux

/*
Copyright The containerd Authors.

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

// Command astrokube-containerd is a busybox-style multi-call binary that folds
// the containerd daemon and the ctr CLI — the same Go module
// (github.com/containerd/containerd/v2) — into ONE static, CGO-free binary, so
// the share carries a single copy of the Go runtime and the entire containerd
// library instead of two near-identical ~30-65 MB copies. It dispatches on
// argv[0]:
//
//	containerd   -> the daemon       (cmd/containerd)
//	ctr          -> the client CLI   (cmd/ctr)
//
// On the virtio-fs share, `ctr` is a hard link to this one inode.
//
// The runc v2 shim (containerd-shim-runc-v2) is deliberately NOT folded in. The
// shim initializes EVERY plugin in the global registry with no plugin config
// (pkg/shim/shim.go calls registry.Graph with a no-op disable filter), so if the
// daemon's builtin plugins were linked into the same binary the shim would try
// to initialize daemon-only plugins — e.g. imageverifier/bindir — and panic on
// their absent config. containerd ships the shim as its own binary for exactly
// this reason; we keep it separate too (built by build-containerd-merged.sh).
//
// The blank import below is the same daemon plugin registration upstream
// cmd/containerd/main.go relies on; ctr is a gRPC client and never initializes
// the registry, so the registered-but-unused plugins are harmless in ctr mode.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	containerdcmd "github.com/containerd/containerd/v2/cmd/containerd/command"
	ctrapp "github.com/containerd/containerd/v2/cmd/ctr/app"

	_ "github.com/containerd/containerd/v2/cmd/containerd/builtins"
)

func main() {
	switch filepath.Base(os.Args[0]) {
	case "ctr":
		runCtr()
	default:
		// "containerd" (or any other name): act as the daemon.
		runContainerd()
	}
}

// runContainerd mirrors upstream cmd/containerd/main.go.
func runContainerd() {
	app := containerdcmd.App()
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "containerd: %s\n", err)
		os.Exit(1)
	}
}

// runCtr mirrors upstream cmd/ctr/main.go (pluginCmds there is empty).
func runCtr() {
	app := ctrapp.New()
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "ctr: %s\n", err)
		os.Exit(1)
	}
}
