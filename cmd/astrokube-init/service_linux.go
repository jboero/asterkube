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
	"strconv"
	"syscall"
	"time"
)

// Service DNAT: a kube-proxy-style ClusterIP. The node programs a single rule
// into the kernel bridge (via prctl, having CAP_NET_ADMIN as PID 1); thereafter
// a bridged pod that sends a UDP datagram to the VIP has its destination
// rewritten to a backend pod, and the backend's reply has its source rewritten
// back to the VIP — so the client sees a reply from the ClusterIP, never the
// backend. This mirrors what kube-proxy does with conntrack-backed DNAT.
const (
	// prSysPrctl is the amd64 prctl syscall number.
	prSysPrctl = 157
	// prAstrokubeDNAT is the prctl option that programs a Service DNAT rule.
	prAstrokubeDNAT = 0x4b554244
)

// Environment variables passing the Service VIP, its port, the backend port and
// this pod's role ("client" or "backend") to a bridged pod's container.
const (
	svcVIPEnv         = "ASTROKUBE_SVC_VIP"
	svcVPortEnv       = "ASTROKUBE_SVC_VPORT"
	svcBackendPortEnv = "ASTROKUBE_SVC_BACKEND_PORT"
	svcRoleEnv        = "ASTROKUBE_SVC_ROLE"
)

// serviceVIP / serviceVPort / serviceProto are the fixed ClusterIP this build
// exercises: 10.96.0.10:80 over UDP, in the classic kube-proxy ClusterIP range.
const (
	serviceVIP   = "10.96.0.10"
	serviceVPort = 80
	serviceProto = 17 // IPPROTO_UDP (6 would be TCP)
)

// ip4ToU32 parses a dotted-quad IPv4 address into a packed big-endian uint32:
// a.b.c.d => (a<<24)|(b<<16)|(c<<8)|d. The bool reports whether s was a valid
// IPv4 address.
func ip4ToU32(s string) (uint32, bool) {
	ip := net.ParseIP(s)
	if ip == nil {
		return 0, false
	}
	v4 := ip.To4()
	if v4 == nil {
		return 0, false
	}
	return uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3]), true
}

// addServiceDNAT programs one Service DNAT rule into the kernel via prctl: a
// packet to vip:vport is rewritten to backend:bport (and the reverse reply
// rewritten back to the VIP). proto is 17 for UDP, 6 for TCP. It returns the
// errno from the syscall when nonzero, nil on success.
func addServiceDNAT(vip string, vport int, backend string, bport int, proto int) error {
	vipU32, ok := ip4ToU32(vip)
	if !ok {
		return fmt.Errorf("bad VIP %q", vip)
	}
	backendU32, ok := ip4ToU32(backend)
	if !ok {
		return fmt.Errorf("bad backend %q", backend)
	}
	ports := uintptr(uint32(vport)<<16 | (uint32(bport) & 0xffff))
	_, _, errno := syscall.Syscall6(prSysPrctl, prAstrokubeDNAT,
		uintptr(vipU32), uintptr(backendU32), ports, uintptr(proto), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// containerServiceTest runs inside a bridged pod, after the pod↔pod peer tests,
// and exercises the Service DNAT path. It is gated by the caller on
// ASTROKUBE_SVC_ROLE being set. Everything is bounded — a missing/late peer
// produces a FAILED line, never a hang (the 15s pod watchdog backstops).
func containerServiceTest(podIP string) {
	role := os.Getenv(svcRoleEnv)
	switch role {
	case "backend":
		serviceBackend(podIP)
	case "client":
		serviceClient()
	default:
		fmt.Printf("svc: unknown role %q (FAILED)\n", role)
	}
}

// serviceBackend is the backend pod's half: bind the pod's own address (the
// kernel's wildcard listener has a known bug, so we bind PodIP explicitly) on
// the backend port and, on receiving a datagram, reply to its sender. The
// destination rewrite has already happened in the bridge, so the datagram
// arrives addressed to us; the source the client used (the VIP) is restored on
// the way back by the bridge.
func serviceBackend(podIP string) {
	port := backendListenPort()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(podIP), Port: port})
	if err != nil {
		fmt.Printf("svc-backend: FAILED to bind %s:%d: %v\n", podIP, port, err)
		return
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(12 * time.Second))
	buf := make([]byte, 256)
	n, from, err := conn.ReadFromUDP(buf)
	if err != nil {
		fmt.Printf("svc-backend: no request (FAILED): %v\n", err)
		return
	}
	if _, err := conn.WriteToUDP([]byte("svc-reply-from-backend"), from); err != nil {
		fmt.Printf("svc-backend: FAILED to reply to %s: %v\n", from, err)
		return
	}
	fmt.Printf("svc-backend: served %q to %s\n", string(buf[:n]), from)
}

// serviceClient is the client pod's half: send to the VIP and verify the reply
// comes back FROM the VIP (proving reverse-NAT), not from the backend IP. UDP
// gives no delivery signal and the backend may still be binding, so it retries
// up to 6 times, 1s apart, with a 2s read deadline each attempt.
func serviceClient() {
	vip := os.Getenv(svcVIPEnv)
	vport := serviceVPort
	if p, err := strconv.Atoi(os.Getenv(svcVPortEnv)); err == nil && p > 0 {
		vport = p
	}
	vipIP := net.ParseIP(vip)
	if vipIP == nil {
		fmt.Printf("svc-client: FAILED bad VIP %q\n", vip)
		return
	}
	target := &net.UDPAddr{IP: vipIP, Port: vport}

	var lastErr string
	for attempt := 1; attempt <= 6; attempt++ {
		conn, err := net.DialUDP("udp", nil, target)
		if err != nil {
			lastErr = "dial: " + err.Error()
		} else {
			if _, werr := conn.Write([]byte("svc-hello")); werr != nil {
				lastErr = "write: " + werr.Error()
			} else {
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 256)
				n, from, rerr := conn.ReadFromUDP(buf)
				if rerr != nil {
					lastErr = "read: " + rerr.Error()
				} else if !from.IP.Equal(vipIP) {
					// A reply that arrives from the backend's real address means
					// the bridge did not rewrite the source — reverse-NAT failed.
					conn.Close()
					fmt.Printf("svc-client: FAILED reverse-NAT (reply from backend %s not VIP)\n", from.IP)
					return
				} else {
					conn.Close()
					fmt.Printf("svc-client: OK reply %q from %s\n", string(buf[:n]), from)
					return
				}
			}
			conn.Close()
		}
		if attempt < 6 {
			time.Sleep(1 * time.Second)
		}
	}
	fmt.Printf("svc-client: FAILED no reply from VIP after 6 attempts: %s\n", lastErr)
}

// backendListenPort returns the backend listen port from the env, defaulting to
// 9097 if unset or unparseable.
func backendListenPort() int {
	if p, err := strconv.Atoi(os.Getenv(svcBackendPortEnv)); err == nil && p > 0 {
		return p
	}
	return 9097
}
