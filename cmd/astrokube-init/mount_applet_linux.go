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
	"syscall"
)

// The kubelet sets up pod volumes (the projected ServiceAccount-token tmpfs,
// emptyDir, bind mounts for configmaps/secrets/subpaths, and mount-propagation
// changes) by shelling out to the util-linux `mount`/`umount` commands. Asterinas
// ships no C and no util-linux, so this binary doubles as those tools: a symlink
// /usr/bin/{mount,umount} -> this binary makes the kubelet's exec("mount", ...)
// land here, and we translate the CLI into the mount(2)/umount2(2) syscalls.
// This is a pragmatic subset (enough for the kubelet's volume manager), not a
// full util-linux reimplementation.

// Linux mount flag constants (stable kernel ABI), defined locally so the applet
// is self-contained.
const (
	msRdonly      = 1 << 0
	msNosuid      = 1 << 1
	msNodev       = 1 << 2
	msNoexec      = 1 << 3
	msSynchronous = 1 << 4
	msRemount     = 1 << 5
	msDirsync     = 1 << 7
	msNoatime     = 1 << 10
	msNodiratime  = 1 << 11
	msBind        = 1 << 12
	msMove        = 1 << 13
	msRec         = 1 << 14
	msSilent      = 1 << 15
	msUnbindable  = 1 << 17
	msPrivate     = 1 << 18
	msSlave       = 1 << 19
	msShared      = 1 << 20
	msRelatime    = 1 << 21
	msStrictatime = 1 << 24

	mntForce  = 1 << 0
	mntDetach = 1 << 1
)

// runMount implements a subset of util-linux mount(8) on top of mount(2).
func runMount(args []string) {
	var fsType, source, target string
	var flags uintptr
	var data []string
	var positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-t" || a == "--types":
			if i++; i < len(args) {
				fsType = args[i]
			}
		case strings.HasPrefix(a, "-t"):
			fsType = a[2:]
		case a == "-o" || a == "--options":
			if i++; i < len(args) {
				applyMountOpts(args[i], &flags, &data)
			}
		case strings.HasPrefix(a, "-o"):
			applyMountOpts(a[2:], &flags, &data)
		case a == "-r" || a == "--read-only":
			flags |= msRdonly
		case a == "-w" || a == "--rw":
			flags &^= msRdonly
		case a == "-B" || a == "--bind":
			flags |= msBind
		case a == "-R" || a == "--rbind":
			flags |= msBind | msRec
		case a == "-M" || a == "--move":
			flags |= msMove
		case a == "--make-shared":
			flags |= msShared
		case a == "--make-rshared":
			flags |= msShared | msRec
		case a == "--make-private":
			flags |= msPrivate
		case a == "--make-rprivate":
			flags |= msPrivate | msRec
		case a == "--make-slave":
			flags |= msSlave
		case a == "--make-rslave":
			flags |= msSlave | msRec
		case a == "--make-unbindable":
			flags |= msUnbindable
		case a == "-n" || a == "--no-mtab" || a == "-v" || a == "--verbose" ||
			a == "-F" || a == "-s" || a == "--no-canonicalize":
			// No /etc/mtab and no canonicalization to honor; ignore.
		case strings.HasPrefix(a, "-"):
			// Unknown option: ignore conservatively rather than fail the mount.
		default:
			positional = append(positional, a)
		}
	}

	switch len(positional) {
	case 0:
		// `mount` with no target: nothing to do (we don't list mounts).
		return
	case 1:
		// remount / propagation change on an existing mountpoint.
		target = positional[0]
	default:
		source = positional[0]
		target = positional[1]
	}

	if err := syscall.Mount(source, target, fsType, flags, strings.Join(data, ",")); err != nil {
		fmt.Fprintf(os.Stderr, "mount: mounting %s on %s (type %s): %v\n", source, target, fsType, err)
		os.Exit(32) // util-linux mount's "mount failure" exit code
	}
}

// applyMountOpts translates a comma-separated -o option list into mount flags
// and filesystem-specific data (e.g. size=, mode=).
func applyMountOpts(opts string, flags *uintptr, data *[]string) {
	for _, o := range strings.Split(opts, ",") {
		switch o {
		case "", "defaults", "loop", "rw":
			if o == "rw" {
				*flags &^= msRdonly
			}
		case "bind":
			*flags |= msBind
		case "rbind":
			*flags |= msBind | msRec
		case "remount":
			*flags |= msRemount
		case "move":
			*flags |= msMove
		case "ro":
			*flags |= msRdonly
		case "nosuid":
			*flags |= msNosuid
		case "suid":
			*flags &^= msNosuid
		case "nodev":
			*flags |= msNodev
		case "dev":
			*flags &^= msNodev
		case "noexec":
			*flags |= msNoexec
		case "exec":
			*flags &^= msNoexec
		case "sync":
			*flags |= msSynchronous
		case "dirsync":
			*flags |= msDirsync
		case "noatime":
			*flags |= msNoatime
		case "nodiratime":
			*flags |= msNodiratime
		case "relatime":
			*flags |= msRelatime
		case "strictatime":
			*flags |= msStrictatime
		case "shared":
			*flags |= msShared
		case "rshared":
			*flags |= msShared | msRec
		case "private":
			*flags |= msPrivate
		case "rprivate":
			*flags |= msPrivate | msRec
		case "slave":
			*flags |= msSlave
		case "rslave":
			*flags |= msSlave | msRec
		case "silent":
			*flags |= msSilent
		default:
			// Filesystem-specific (size=, mode=, uid=, gid=, ...): pass through.
			*data = append(*data, o)
		}
	}
}

// runUmount implements a subset of util-linux umount(8) on top of umount2(2).
func runUmount(args []string) {
	var targets []string
	var flags int
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-l" || a == "--lazy":
			flags |= mntDetach
		case a == "-f" || a == "--force":
			flags |= mntForce
		case a == "-n" || a == "-v" || a == "-c" || a == "-d" || a == "-r" ||
			a == "--no-mtab" || a == "--verbose":
			// ignore
		case strings.HasPrefix(a, "-"):
			// Unknown option: ignore.
		default:
			targets = append(targets, a)
		}
	}
	for _, t := range targets {
		if err := syscall.Unmount(t, flags); err != nil {
			fmt.Fprintf(os.Stderr, "umount: %s: %v\n", t, err)
			os.Exit(32)
		}
	}
}

// installMountApplet symlinks /usr/bin/{mount,umount} (and /bin equivalents) to
// this binary so the kubelet's exec("mount", ...) resolves here. Must run after
// /proc is mounted (os.Executable reads /proc/self/exe).
func installMountApplet() {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "/usr/bin/kubelet" // the init binary's known location
	}
	for _, name := range []string{
		"/usr/bin/mount", "/usr/bin/umount", "/bin/mount", "/bin/umount",
	} {
		_ = os.Remove(name)
		if err := os.Symlink(self, name); err != nil {
			// /bin may not exist; that's fine as long as /usr/bin works.
			continue
		}
	}
}
