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
)

// probeShebang verifies the kernel passes a script's path (not the caller's
// bare argv[0]) to a #! interpreter. This is exactly what broke kube-proxy: its
// go-iptables execs "iptables" (a short argv[0]) which resolves to a #!/bin/sh
// wrapper; a buggy kernel hands the interpreter "iptables" instead of the
// script path, so it fails with "cannot open iptables: No such file".
//
// The guest has no /bin/sh, so this binary doubles as the interpreter (via the
// shebang-probe multi-call applet). We exec a script whose interpreter is that
// applet, using a short argv[0], and check the interpreter saw the script path.
func probeShebang() {
	fmt.Println("-- shebang: interpreter-path exec probe --")

	self, err := os.Executable()
	if err != nil {
		fmt.Printf("shebang: cannot find self: %v\n", err)
		return
	}
	if err := os.Symlink(self, "/usr/bin/shebang-probe"); err != nil && !os.IsExist(err) {
		fmt.Printf("shebang: cannot install interpreter symlink: %v\n", err)
		return
	}

	const script = "/tmp/sb-test.sh"
	if err := os.WriteFile(script, []byte("#!/usr/bin/shebang-probe\n"), 0o755); err != nil {
		fmt.Printf("shebang: cannot write script: %v\n", err)
		return
	}

	// Run the script with a deliberately short argv[0] (as a PATH-resolved exec
	// would), while the exec path is the full script path.
	cmd := exec.Command(script)
	cmd.Args = []string{"sb-short-argv0"}
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	fmt.Printf("shebang: interpreter saw -> %s\n", trimmed)
	if err != nil {
		fmt.Printf("shebang: exec error: %v\n", err)
	}

	if strings.Contains(trimmed, script) {
		fmt.Println("shebang: PASS — interpreter received the script path (#! scripts exec correctly)")
	} else {
		fmt.Println("shebang: FAIL — interpreter did not receive the script path")
	}
}
