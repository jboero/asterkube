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
	"bufio"
	"fmt"
	"os"
	"strings"
)

// fstabPath is the standard table of extra filesystems to mount at boot. A
// documented default ships in the image (astrokube/fstab.default); operators can
// override it (it is a plain file in the rootfs, or could be supplied via a
// config drive / boot arg).
const fstabPath = "/etc/fstab"

// fstabPseudoOpts are fstab option words that are NOT kernel mount flags or
// fs-specific data — they steer the init, not the mount() call, so they are
// stripped before the option string is handed to applyMountOpts. ("noauto" and
// "nofail" are handled explicitly in parseFstab.)
var fstabPseudoOpts = map[string]bool{
	"auto": true, "nofail": true, "_netdev": true,
	"user": true, "users": true, "nouser": true, "owner": true, "group": true,
}

// parseFstab reads an /etc/fstab-style table into mountSpecs. Each non-comment
// line is:
//
//	<device|source>  <mountpoint>  <fstype>  <options>  [dump]  [pass]
//
// Comments ('#') and blank lines are ignored. An entry with "noauto" is defined
// but not mounted at boot. fstab pseudo-options (nofail, _netdev, user, …) are
// consumed here; everything else (ro, nosuid, nodev, noexec, size=…, …) is parsed
// by the shared applyMountOpts so behavior matches the `mount` applet. Block
// devices (source starts with /dev/) are best-effort: missing devices are
// skipped, not failed — the practical effect of "nofail", and the init never
// aborts on a single failed mount anyway.
func parseFstab(path string) []mountSpec {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var specs []mountSpec
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue // need at least device, mountpoint, fstype
		}
		source, target, fstype := fields[0], fields[1], fields[2]
		opts := "defaults"
		if len(fields) >= 4 {
			opts = fields[3]
		}

		noauto := false
		kept := make([]string, 0, 4)
		for _, o := range strings.Split(opts, ",") {
			switch {
			case o == "noauto":
				noauto = true
			case fstabPseudoOpts[o] || strings.HasPrefix(o, "comment="):
				// consumed by the init, not passed to mount()
			default:
				kept = append(kept, o)
			}
		}
		if noauto {
			continue
		}

		var flags uintptr
		var data []string
		applyMountOpts(strings.Join(kept, ","), &flags, &data)
		specs = append(specs, mountSpec{
			source: source,
			target: target,
			fstype: fstype,
			flags:  flags,
			data:   strings.Join(data, ","),
			device: strings.HasPrefix(source, "/dev/"),
		})
	}
	return specs
}

// mountFstab mounts every auto entry in /etc/fstab and reports the outcome. It
// runs after the essential pseudo-filesystems are up (so entries may reference
// /dev, and the rootfs holding /etc/fstab is in place).
func mountFstab() {
	specs := parseFstab(fstabPath)
	if len(specs) == 0 {
		return
	}
	fmt.Printf("astrokube-init: mounting %s entries\n", fstabPath)
	for _, r := range mountAll(specs) {
		switch {
		case r.skipped:
			fmt.Printf("  [skip] %-20s (%s): %s\n", r.spec.target, r.spec.fstype, r.note)
		case r.err != nil:
			fmt.Printf("  [fail] %-20s (%s): %v\n", r.spec.target, r.spec.fstype, r.err)
		default:
			fmt.Printf("  [ ok ] %-20s (%s)\n", r.spec.target, r.spec.fstype)
		}
	}
}
