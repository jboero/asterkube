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

// Boot-args cluster join: bind a GENERIC astrokube node image to a specific
// Kubernetes cluster purely from the kernel command line — no config drive, no
// cloud-init datasource. Asterinas delivers each `key=value` token on the kernel
// cmdline to the init process as an environment variable (the same mechanism that
// gives it PATH/USER), so the cluster coordinates arrive as ASTROKUBE_* env vars.
//
// Set them in OSDK.toml `kcmd_args` (baked) or, for a direct kernel boot, in
// `-append`:
//
//	ASTROKUBE_APISERVER=https://10.0.2.2:6443
//	ASTROKUBE_TOKEN=abcdef.0123456789abcdef        # kubeadm bootstrap token
//	ASTROKUBE_CA_HASH=sha256:1b2c...               # kubeadm-style CA pubkey hash
//	ASTROKUBE_NODE_NAME=web-1                       # optional (default: astrokube)
//	ASTROKUBE_CLUSTER_DNS=10.96.0.10               # optional
//	ASTROKUBE_TAINTS=key=val:NoSchedule            # optional
//	ASTROKUBE_NODE_IP=10.0.2.15                    # optional (default: eth0 slirp IP)
//	ASTROKUBE_INSECURE=1                            # skip apiserver TLS verify (dev/slirp)
//
// The node then does a kubeadm-style TLS bootstrap: it builds a bootstrap
// kubeconfig from the token, and the real kubelet uses it to submit a CSR, get a
// node client cert auto-issued, and register the Node — credentials never have to
// be pre-baked into the image.

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	envApiserver  = "ASTROKUBE_APISERVER"
	envToken      = "ASTROKUBE_TOKEN"
	envCAHash     = "ASTROKUBE_CA_HASH"
	envNodeName   = "ASTROKUBE_NODE_NAME"
	envClusterDNS = "ASTROKUBE_CLUSTER_DNS"
	envTaints     = "ASTROKUBE_TAINTS"
	envNodeIP     = "ASTROKUBE_NODE_IP"
	envInsecure   = "ASTROKUBE_INSECURE"
)

// bootArgsConfigured reports whether the kernel cmdline asked this node to join a
// cluster (ASTROKUBE_APISERVER is set).
func bootArgsConfigured() bool {
	return strings.TrimSpace(os.Getenv(envApiserver)) != ""
}

// bootArgsJoin binds this node to the cluster named on the kernel cmdline: it
// brings up the static containerd, builds a bootstrap kubeconfig from the token,
// and launches the real kubelet to TLS-bootstrap and register the Node.
func bootArgsJoin() {
	apiserver := strings.TrimSpace(os.Getenv(envApiserver))
	token := strings.TrimSpace(os.Getenv(envToken))
	node := envOr(envNodeName, "astrokube")
	nodeIP := envOr(envNodeIP, "10.0.2.15")

	fmt.Println()
	fmt.Println("astrokube-init: ===== boot-args cluster join =====")
	fmt.Printf("astrokube-init: apiserver=%s node=%q (from kernel cmdline)\n", apiserver, node)
	if token == "" {
		fmt.Printf("astrokube-init: SKIPPED join (no %s on the kernel cmdline)\n", envToken)
		return
	}

	// 1. Ensure eth0 has its address + default route so the apiserver is reachable
	//    (idempotent: the wan probe may have configured it already).
	configureEth0BestEffort()

	// 2. Build the bootstrap kubeconfig from the token. Trust is established either
	//    by the kubeadm-style CA pubkey hash (secure) or skipped (dev/slirp, where
	//    the apiserver's slirp address isn't in its cert SANs).
	bootstrap := "/run/bootstrap.kubeconfig"
	if err := writeBootstrapKubeconfig(bootstrap, apiserver, token); err != nil {
		fmt.Printf("astrokube-init: FAILED to build bootstrap kubeconfig: %v\n", err)
		return
	}

	// 3. Bring up the baked-in static containerd (persistent: the node keeps it).
	linkRunc()
	env := append(os.Environ(), "PATH=/usr/bin:/bin", "XDG_RUNTIME_DIR=/run")
	sock := "/run/containerd/containerd.sock"
	cd, ok := startStaticContainerd("/run/containerd/root", "/run/containerd/state", sock, "/run/containerd/containerd.log", env)
	if !ok {
		fmt.Println("astrokube-init: FAILED join (containerd did not come up)")
		return
	}

	// 4. Launch the REAL kubelet to TLS-bootstrap with the token and register.
	root := "/var/lib/kubelet"
	_ = os.MkdirAll(root+"/pki", 0o755)
	logPath := "/run/kubelet-join.log"
	logf, err := os.Create(logPath)
	if err != nil {
		fmt.Printf("astrokube-init: FAILED to create kubelet log: %v\n", err)
		return
	}
	args := []string{
		"--bootstrap-kubeconfig=" + bootstrap,
		"--kubeconfig=" + root + "/kubelet.conf", // kubelet writes its issued config here
		"--cert-dir=" + root + "/pki",
		"--container-runtime-endpoint=unix://" + sock,
		"--image-service-endpoint=unix://" + sock,
		"--root-dir=" + root,
		"--hostname-override=" + node,
		"--node-ip=" + nodeIP,
		"--register-node=true",
		"--cgroup-driver=cgroupfs", "--cgroups-per-qos=false", "--enforce-node-allocatable=",
		"--cgroup-root=/", "--runtime-cgroups=/", "--kubelet-cgroups=/",
		"--fail-swap-on=false",
		"--resolv-conf=/etc/resolv.conf",
		"--node-status-update-frequency=4s",
		"--v=2",
	}
	if t := strings.TrimSpace(os.Getenv(envTaints)); t != "" {
		args = append(args, "--register-with-taints="+t)
	}
	if d := strings.TrimSpace(os.Getenv(envClusterDNS)); d != "" {
		args = append(args, "--cluster-dns="+d)
	}
	k := exec.Command("/usr/bin/kubelet", args...)
	k.Env = env
	k.Stdout = logf
	k.Stderr = logf
	if err := k.Start(); err != nil {
		fmt.Printf("astrokube-init: FAILED to start kubelet: %v\n", err)
		return
	}
	fmt.Printf("astrokube-init: kubelet TLS-bootstrapping with the token and registering node %q...\n", node)

	// 5. Watch for registration (or the first connectivity/auth gap).
	registered := false
	deadline := time.Now().Add(75 * time.Second)
	for time.Now().Before(deadline) {
		if k.ProcessState != nil && k.ProcessState.Exited() {
			fmt.Println("astrokube-init: kubelet exited early during join")
			break
		}
		log := readFileString(logPath)
		if strings.Contains(log, "Successfully registered node") || strings.Contains(log, "Successfully registered Node") {
			registered = true
			break
		}
		if hit := firstMatch(log, []string{"Unauthorized", "x509", "certificate signed by unknown", "connection refused", "i/o timeout", "tls:"}); hit != "" {
			fmt.Printf("astrokube-init: join gap detected: %q (continuing to watch)\n", hit)
		}
		time.Sleep(3 * time.Second)
	}
	if !registered {
		fmt.Println("astrokube-init: node did NOT register within 75s; kubelet log summary:")
		summarizeKubeletLog(logPath)
		_ = k.Process.Kill()
		_ = cd.Process.Kill()
		return
	}
	fmt.Printf("astrokube-init: BOOT-ARGS JOIN PASSED — node %q registered with the apiserver ✓\n", node)
	fmt.Println("astrokube-init: (Ready awaits a CNI; that is the next gap, not a join failure.)")

	// 6. Keep containerd + kubelet running so the node persists (serveForever).
	keepAlive(cd)
	keepAlive(k)
}

// writeBootstrapKubeconfig writes a kubeadm-style bootstrap kubeconfig that
// authenticates with the bootstrap token. TLS trust comes from the CA pubkey hash
// (secure) unless ASTROKUBE_INSECURE is set (dev/slirp).
func writeBootstrapKubeconfig(path, apiserver, token string) error {
	insecure := strings.TrimSpace(os.Getenv(envInsecure)) != ""
	caHash := strings.TrimSpace(os.Getenv(envCAHash))

	var clusterFields string
	switch {
	case insecure:
		fmt.Println("astrokube-init: bootstrap trust: insecure-skip-tls-verify (dev/slirp)")
		clusterFields = "    insecure-skip-tls-verify: true"
	case caHash != "":
		caB64, err := fetchAndVerifyClusterCA(apiserver, caHash)
		if err != nil {
			return fmt.Errorf("CA discovery via %s: %w", envCAHash, err)
		}
		fmt.Printf("astrokube-init: bootstrap trust: cluster CA verified against %s ✓\n", envCAHash)
		clusterFields = "    certificate-authority-data: " + caB64
	default:
		return fmt.Errorf("no trust anchor: set %s (secure) or %s=1 (dev)", envCAHash, envInsecure)
	}

	cfg := "apiVersion: v1\nkind: Config\n" +
		"clusters:\n- name: c\n  cluster:\n    server: " + apiserver + "\n" + clusterFields + "\n" +
		"users:\n- name: tls-bootstrap-token-user\n  user:\n    token: " + token + "\n" +
		"contexts:\n- name: ctx\n  context:\n    cluster: c\n    user: tls-bootstrap-token-user\n" +
		"current-context: ctx\n"
	return os.WriteFile(path, []byte(cfg), 0o600)
}

// fetchAndVerifyClusterCA fetches the kube-public/cluster-info ConfigMap from the
// apiserver (over an unverified TLS connection), extracts the cluster CA it
// advertises, and verifies it matches the expected kubeadm CA pubkey hash
// (sha256 of the DER SubjectPublicKeyInfo). Returns the CA as base64 PEM. This is
// the kubeadm discovery model: trust the bootstrap channel only after the CA
// matches a hash the operator supplied out of band.
func fetchAndVerifyClusterCA(apiserver, caHash string) (string, error) {
	url := strings.TrimRight(apiserver, "/") + "/api/v1/namespaces/kube-public/configmaps/cluster-info"
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	m := regexp.MustCompile(`certificate-authority-data:\s*([A-Za-z0-9+/=]+)`).FindSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("cluster-info has no certificate-authority-data")
	}
	caB64 := string(m[1])
	caPEM, err := base64.StdEncoding.DecodeString(caB64)
	if err != nil {
		return "", fmt.Errorf("decode CA: %w", err)
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return "", fmt.Errorf("CA is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse CA: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(spki)
	got := "sha256:" + hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, caHash) {
		return "", fmt.Errorf("CA hash mismatch: cmdline %s, server %s", caHash, got)
	}
	return caB64, nil
}

// configureEth0BestEffort assigns eth0 the slirp address + default route, ignoring
// "already configured" errors (the wan probe may have done it).
func configureEth0BestEffort() {
	if networkConfigured {
		return // DHCP-first already brought the interface up
	}
	nl, err := nlOpen()
	if err != nil {
		return
	}
	defer nl.close()
	idx, err := nl.linkIndexByName("eth0")
	if err != nil {
		return
	}
	_ = nl.addAddrV4(idx, net.IPv4(10, 0, 2, 15), 24)
	_ = nl.addDefaultRouteV4(net.IPv4(10, 0, 2, 2), idx)
}

// startStaticContainerd starts the baked-in static containerd at the given paths
// and waits for its API socket. Returns the running daemon and true on success.
func startStaticContainerd(root, state, sock, logPath string, env []string) (*exec.Cmd, bool) {
	_ = os.MkdirAll(root, 0o755)
	_ = os.MkdirAll(state, 0o755)
	logf, err := os.Create(logPath)
	if err != nil {
		fmt.Printf("astrokube-init: FAILED to open containerd log: %v\n", err)
		return nil, false
	}
	cd := exec.Command(zeroCContainerd, "--root", root, "--state", state, "--address", sock, "--log-level", "info")
	cd.Env = env
	cd.Stdout = logf
	cd.Stderr = logf
	if err := cd.Start(); err != nil {
		fmt.Printf("astrokube-init: FAILED to start containerd: %v\n", err)
		return nil, false
	}
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			return cd, true
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("astrokube-init: containerd socket never appeared")
	dumpTail(logPath, 30)
	_ = cd.Process.Kill()
	return nil, false
}

// linkRunc points /usr/bin/runc at this multi-call binary (our pure-Go OCI runtime).
func linkRunc() {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "/usr/bin/kubelet"
	}
	_ = os.Remove("/usr/bin/runc")
	_ = os.Symlink(self, "/usr/bin/runc")
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
