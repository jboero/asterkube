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
	"net"
	"os"
	"syscall"
	"time"
)

// Masquerade (source NAT): a pod on a pod-subnet bridge reaches a host on an
// "uplink" network through the kernel's inter-bridge router. The node marks the
// uplink bridge so the router rewrites the pod's source address to the uplink's
// own address on the way out — exactly what a node does when masquerading a pod
// to its eth0 so the outside world replies to the node, not to an unroutable pod
// IP. The reply is reverse-NATed back to the pod by the kernel. The uplink here
// stands in for eth0; the SNAT/conntrack mechanism is identical.
const (
	// prAstrokubeMasq is the prctl option that marks a bridge as a masquerade
	// uplink (arg2 = the bridge's interface index).
	prAstrokubeMasq = 0x4b55424d

	// masqPort is the UDP port the masquerade server listens on.
	masqPort = 9200
)

// Environment variables passing the masquerade role and (for the client) the
// uplink server address to a bridged pod's container.
const (
	masqRoleEnv   = "ASTROKUBE_MASQ_ROLE"
	masqTargetEnv = "ASTROKUBE_MASQ_TARGET"
	// masqUplinkGwEnv passes the uplink gateway address the server expects to
	// see as the (masqueraded) source.
	masqUplinkGwEnv = "ASTROKUBE_MASQ_UPLINK_GW"
)

// addMasqUplink marks the bridge with interface index idx as a masquerade
// uplink via prctl. It returns the syscall errno when nonzero, nil on success.
func addMasqUplink(idx int) error {
	_, _, errno := syscall.Syscall6(prSysPrctl, prAstrokubeMasq, uintptr(idx), 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// containerMasqTest runs inside a bridged pod, gated on ASTROKUBE_MASQ_ROLE.
func containerMasqTest(podIP string) {
	switch os.Getenv(masqRoleEnv) {
	case "server":
		masqServer(podIP)
	case "client":
		masqClient()
	default:
		fmt.Printf("masq: unknown role %q (FAILED)\n", os.Getenv(masqRoleEnv))
	}
}

// masqServer is the uplink-side host: it binds its address and, on receiving a
// datagram, checks the source — which must be the uplink gateway (the pod's
// address was masqueraded away) — then replies. Binding the pod's own address
// explicitly avoids the kernel's wildcard-listener bug.
func masqServer(podIP string) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(podIP), Port: masqPort})
	if err != nil {
		fmt.Printf("masq-server: FAILED to bind %s:%d: %v\n", podIP, masqPort, err)
		return
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	buf := make([]byte, 256)
	n, from, err := conn.ReadFromUDP(buf)
	if err != nil {
		fmt.Printf("masq-server: no request (FAILED): %v\n", err)
		return
	}
	if _, err := conn.WriteToUDP([]byte("masq-reply"), from); err != nil {
		fmt.Printf("masq-server: FAILED to reply to %s: %v\n", from, err)
		return
	}

	// The source must be the uplink gateway, not the client's pod IP.
	wantGw := os.Getenv(masqUplinkGwEnv)
	if wantGw != "" && from.IP.Equal(net.ParseIP(wantGw)) {
		fmt.Printf("masq-server: OK saw masqueraded source %s for %q\n", from.IP, string(buf[:n]))
	} else {
		fmt.Printf("masq-server: FAILED source %s not masqueraded (want %s)\n", from.IP, wantGw)
	}
}

// masqClient is the pod-subnet-side client: it sends to the uplink server and
// verifies it gets a reply back (proving the kernel reverse-NATed the reply to
// the pod). It retries to absorb the server still binding.
func masqClient() {
	target := os.Getenv(masqTargetEnv)
	targetIP := net.ParseIP(target)
	if targetIP == nil {
		fmt.Printf("masq-client: FAILED bad target %q\n", target)
		return
	}
	dst := &net.UDPAddr{IP: targetIP, Port: masqPort}

	var lastErr string
	for attempt := 1; attempt <= 8; attempt++ {
		conn, err := net.DialUDP("udp", nil, dst)
		if err != nil {
			lastErr = "dial: " + err.Error()
		} else {
			if _, werr := conn.Write([]byte("masq-hello")); werr != nil {
				lastErr = "write: " + werr.Error()
			} else {
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 256)
				n, from, rerr := conn.ReadFromUDP(buf)
				if rerr != nil {
					lastErr = "read: " + rerr.Error()
				} else {
					conn.Close()
					fmt.Printf("masq-client: OK reply %q from %s\n", string(buf[:n]), from)
					return
				}
			}
			conn.Close()
		}
		if attempt < 8 {
			time.Sleep(1 * time.Second)
		}
	}
	fmt.Printf("masq-client: FAILED no reply from %s after 8 attempts: %s\n", target, lastErr)
}
