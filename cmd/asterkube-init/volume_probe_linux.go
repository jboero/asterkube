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
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// volume_probe addresses the rootless-container volume-mount escalation: a
// "nonroot" container with a writable host volume must not be able to gain root
// through it. The kernel now enforces the mount security flags container
// runtimes set on volumes:
//   - nosuid: a setuid-root binary on the volume does NOT escalate on exec;
//   - noexec: code dropped on the volume cannot be executed at all.
// This probe mounts a nosuid (and a noexec) tmpfs, places a setuid-root copy of
// this binary on it, and confirms an unprivileged exec is contained — with a
// control on a normal mount proving setuid still works where it's allowed.

const (
	suidReportEnv = "ASTERKUBE_SUID_REPORT" // child prints its euid

	suidMode = 0o4755 // setuid + rwxr-xr-x
)

// copyExe copies this binary to dst and makes it setuid-root (owner is root,
// since init runs as root).
func copyExe(dst string) error {
	src, err := os.Executable()
	if err != nil {
		src = "/proc/self/exe"
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	out.Close()
	return syscall.Chmod(dst, suidMode)
}

// execAsUnprivReportEuid execs `bin` as uid 1000 with the suid-report env and
// returns its reported euid (or -1 on failure / -2 if exec was denied).
func execAsUnprivReportEuid(bin string) (int, error) {
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), suidReportEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: unprivUID, Gid: unprivUID, NoSetGroups: true},
	}
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil && len(out) == 0 {
		return -2, err // exec itself failed (e.g. noexec EACCES)
	}
	var euid int
	if _, serr := fmt.Sscanf(s, "euid=%d", &euid); serr != nil {
		return -1, fmt.Errorf("bad child output %q (%v)", s, err)
	}
	return euid, nil
}

// runVolumeProbe (root, called from init) sets up nosuid/noexec mounts and a
// setuid-root binary, then verifies an unprivileged process cannot escalate.
func runVolumeProbe() {
	fmt.Println()
	fmt.Println("asterkube-init: ===== rootless volume-mount hardening probe =====")
	fmt.Println("asterkube-init: a setuid binary on a nosuid volume must NOT grant root")

	_ = os.MkdirAll("/run/suidvol", 0o755)
	_ = os.MkdirAll("/run/nosuidvol", 0o755)
	_ = os.MkdirAll("/run/noexecvol", 0o755)

	// Control: a normal (suid-allowed) tmpfs — setuid must escalate here.
	if err := syscall.Mount("tmpfs", "/run/suidvol", "tmpfs", 0, ""); err != nil {
		fmt.Printf("asterkube-init: volume probe setup failed (suid tmpfs): %v\n", err)
		fmt.Println("asterkube-init: ===== end volume-mount hardening probe =====")
		return
	}
	nosuidErr := syscall.Mount("tmpfs", "/run/nosuidvol", "tmpfs", syscall.MS_NOSUID, "")
	noexecErr := syscall.Mount("tmpfs", "/run/noexecvol", "tmpfs", syscall.MS_NOEXEC, "")
	if nosuidErr != nil || noexecErr != nil {
		fmt.Printf("asterkube-init: volume probe setup failed (nosuid=%v noexec=%v)\n", nosuidErr, noexecErr)
		fmt.Println("asterkube-init: ===== end volume-mount hardening probe =====")
		return
	}

	if err := copyExe("/run/suidvol/bin"); err != nil {
		fmt.Printf("asterkube-init: copy to suidvol failed: %v\n", err)
		return
	}
	if err := copyExe("/run/nosuidvol/bin"); err != nil {
		fmt.Printf("asterkube-init: copy to nosuidvol failed: %v\n", err)
		return
	}
	if err := copyExe("/run/noexecvol/bin"); err != nil {
		fmt.Printf("asterkube-init: copy to noexecvol failed: %v\n", err)
		return
	}

	// Control: setuid-root binary on a normal mount -> unprivileged exec escalates to 0.
	ctrlEuid, ctrlErr := execAsUnprivReportEuid("/run/suidvol/bin")
	fmt.Printf("  control (suid mount): unprivileged exec of setuid-root -> euid=%d (want 0) err=%v\n", ctrlEuid, ctrlErr)

	// nosuid: same setuid-root binary -> exec must NOT escalate (euid stays 1000).
	nosuidEuid, nosuidErr2 := execAsUnprivReportEuid("/run/nosuidvol/bin")
	fmt.Printf("  nosuid mount: unprivileged exec of setuid-root -> euid=%d (want %d) err=%v\n", nosuidEuid, unprivUID, nosuidErr2)

	// noexec: exec must be refused outright.
	noexecEuid, noexecErr2 := execAsUnprivReportEuid("/run/noexecvol/bin")
	noexecDenied := noexecEuid == -2
	fmt.Printf("  noexec mount: exec attempt -> denied=%v (want true) detail=%v\n", noexecDenied, noexecErr2)

	controlOK := ctrlEuid == 0
	nosuidOK := nosuidEuid == unprivUID
	if controlOK && nosuidOK && noexecDenied {
		fmt.Println("asterkube-init: volume hardening ENFORCED — nosuid blocks setuid escalation, noexec blocks exec ✓")
	} else {
		fmt.Println("asterkube-init: volume hardening INCOMPLETE — see above")
	}
	fmt.Println("asterkube-init: ===== end volume-mount hardening probe =====")
}
