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
	"sync"
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

// serviceVIP / serviceVPort exercise the fixed ClusterIP this build uses:
// 10.96.0.10:80, in the classic kube-proxy ClusterIP range. The same VIP:port
// is programmed for both protocols (the NAT engine keys rules on
// vip+vport+proto, so the UDP and TCP rules coexist), to two distinct backend
// ports: BackendPort for UDP and BackendPort+1 for TCP.
const (
	serviceVIP      = "10.96.0.10"
	serviceVPort    = 80
	serviceProtoUDP = 17 // IPPROTO_UDP
	serviceProtoTCP = 6  // IPPROTO_TCP
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
		serviceClientTCP()
	default:
		fmt.Printf("svc: unknown role %q (FAILED)\n", role)
	}
}

// udpEndpointPorts returns the two UDP backend endpoints this pod serves:
// BackendPort and BackendPort+2 (BackendPort+1 is reserved for TCP). Two
// distinct endpoints let the test prove the NAT engine load-balances a VIP
// across a backend set, the way kube-proxy spreads a Service over its endpoints.
func udpEndpointPorts() []int {
	base := backendListenPort()
	return []int{base, base + 2}
}

// serviceBackend is the backend pod's half. It serves the VIP across several
// endpoints concurrently: two UDP ports (a load-balanced Service backend set)
// and one TCP port. Every listener is bound up front so no client packet is
// refused, and all are served in parallel so a slow protocol never starves
// another. The destination rewrite has already happened in the bridge, so
// traffic arrives addressed to us; the VIP source the client used is restored
// on the reply path by the bridge.
func serviceBackend(podIP string) {
	var wg sync.WaitGroup

	// TCP endpoint on BackendPort+1.
	tcpAddr := &net.TCPAddr{IP: net.ParseIP(podIP), Port: backendListenPort() + 1}
	if ln, lerr := net.ListenTCP("tcp", tcpAddr); lerr != nil {
		fmt.Printf("svc-backend: FAILED to listen tcp %s: %v\n", tcpAddr, lerr)
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer ln.Close()
			serviceBackendTCP(ln)
		}()
	}

	// UDP load-balanced endpoints, each counting how many datagrams it served.
	ports := udpEndpointPorts()
	served := make([]int, len(ports))
	for i, port := range ports {
		wg.Add(1)
		go func(i, port int) {
			defer wg.Done()
			served[i] = serviceBackendUDPEndpoint(podIP, port)
		}(i, port)
	}
	wg.Wait()

	// Load-balancing proof: every UDP endpoint must have served at least one
	// datagram, i.e. the engine spread the client's flows across the set.
	hit := 0
	for i, n := range served {
		fmt.Printf("svc-backend: udp endpoint :%d served %d\n", ports[i], n)
		if n > 0 {
			hit++
		}
	}
	if hit == len(ports) {
		fmt.Printf("svc-lb: OK all %d udp endpoints hit (load balanced)\n", len(ports))
	} else {
		fmt.Printf("svc-lb: FAILED only %d/%d udp endpoints hit\n", hit, len(ports))
	}
}

// serviceBackendUDPEndpoint serves one UDP backend endpoint: it binds the pod's
// own address (the kernel's wildcard listener has a known bug) on the given
// port and echoes a reply to every datagram until the flow goes idle. It
// returns how many datagrams it served. A short per-read deadline lets it exit
// promptly once the client has stopped sending.
func serviceBackendUDPEndpoint(podIP string, port int) int {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(podIP), Port: port})
	if err != nil {
		fmt.Printf("svc-backend: FAILED to bind udp %s:%d: %v\n", podIP, port, err)
		return 0
	}
	defer conn.Close()

	served := 0
	buf := make([]byte, 256)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
		n, from, rerr := conn.ReadFromUDP(buf)
		if rerr != nil {
			return served // deadline: the client has stopped sending.
		}
		if _, werr := conn.WriteToUDP([]byte("svc-reply-from-backend"), from); werr != nil {
			fmt.Printf("svc-backend: udp :%d FAILED to reply to %s: %v\n", port, from, werr)
			return served
		}
		served++
		fmt.Printf("svc-backend: udp :%d served %q to %s\n", port, string(buf[:n]), from)
	}
}

// serviceBackendTCP accepts one connection on the already-bound listener, reads
// the client's hello and writes a reply. That the connection was accepted at
// all means the client's SYN was DNAT'd to us and our SYN-ACK was reverse-NAT'd
// back to the VIP (otherwise the client's stack would have dropped it).
func serviceBackendTCP(ln *net.TCPListener) {
	_ = ln.SetDeadline(time.Now().Add(12 * time.Second))
	conn, err := ln.Accept()
	if err != nil {
		fmt.Printf("svc-backend: no tcp connect (FAILED): %v\n", err)
		return
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	buf := make([]byte, 256)
	n, rerr := conn.Read(buf)
	if rerr != nil {
		fmt.Printf("svc-backend: tcp read FAILED: %v\n", rerr)
		return
	}
	if _, err := conn.Write([]byte("svc-tcp-reply-from-backend")); err != nil {
		fmt.Printf("svc-backend: tcp reply FAILED: %v\n", err)
		return
	}
	fmt.Printf("svc-backend: served tcp %q to %s\n", string(buf[:n]), conn.RemoteAddr())
}

// serviceClient is the client pod's UDP half: it sends several datagrams to the
// VIP, each over a *fresh* connection so each gets a new ephemeral source port.
// Distinct source ports hash to different backends in the kernel, so the batch
// exercises the whole backend set (the backend logs confirm the spread). Every
// reply must come back FROM the VIP — a reply from a backend's real address
// would mean reverse-NAT failed. The first datagram retries to absorb the
// backend still binding.
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

	const sends = 8
	ok := 0
	var lastErr string
	for i := 0; i < sends; i++ {
		// The first datagram may race the backend binding; give it a few tries.
		tries := 1
		if i == 0 {
			tries = 5
		}
		for t := 0; t < tries; t++ {
			result, err := serviceClientSendOnce(target, vipIP, i)
			if err == reverseNATFailed {
				fmt.Printf("svc-client: FAILED reverse-NAT (reply not from VIP)\n")
				return
			}
			if err == nil {
				ok++
				_ = result
				break
			}
			lastErr = err.Error()
			if t < tries-1 {
				time.Sleep(1 * time.Second)
			}
		}
	}

	if ok == sends {
		fmt.Printf("svc-client: OK %d/%d udp replies from VIP %s\n", ok, sends, vip)
	} else {
		fmt.Printf("svc-client: PARTIAL %d/%d udp replies from VIP (last err: %s)\n", ok, sends, lastErr)
	}
}

// reverseNATFailed marks the one unrecoverable outcome: a reply arrived from a
// backend's real address instead of the VIP, so the reverse rewrite is broken.
var reverseNATFailed = fmt.Errorf("reverse-NAT failed")

// serviceClientSendOnce sends one datagram to the VIP over a fresh connection
// and reads the reply, verifying its source is the VIP. It returns the reply
// payload on success, reverseNATFailed if the reply came from a non-VIP source,
// or another error if the send/read failed.
func serviceClientSendOnce(target *net.UDPAddr, vipIP net.IP, seq int) (string, error) {
	conn, err := net.DialUDP("udp", nil, target)
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	if _, werr := conn.Write([]byte(fmt.Sprintf("svc-hello-%d", seq))); werr != nil {
		return "", fmt.Errorf("write: %w", werr)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, from, rerr := conn.ReadFromUDP(buf)
	if rerr != nil {
		return "", fmt.Errorf("read: %w", rerr)
	}
	if !from.IP.Equal(vipIP) {
		return "", reverseNATFailed
	}
	return string(buf[:n]), nil
}

// serviceClientTCP is the client pod's TCP half: dial the VIP and exchange a
// message. A completed TCP handshake is itself the proof of reverse-NAT — the
// client's stack only accepts a SYN-ACK whose source is exactly the VIP:port it
// sent the SYN to, so if the backend's SYN-ACK source were not rewritten back
// to the VIP the connection would never establish. It retries to absorb the
// backend still binding.
func serviceClientTCP() {
	vip := os.Getenv(svcVIPEnv)
	vport := serviceVPort
	if p, err := strconv.Atoi(os.Getenv(svcVPortEnv)); err == nil && p > 0 {
		vport = p
	}
	vipIP := net.ParseIP(vip)
	if vipIP == nil {
		fmt.Printf("svc-client: FAILED bad VIP %q (tcp)\n", vip)
		return
	}
	target := &net.TCPAddr{IP: vipIP, Port: vport}

	var lastErr string
	for attempt := 1; attempt <= 6; attempt++ {
		conn, err := net.DialTCP("tcp", nil, target)
		if err != nil {
			lastErr = "dial: " + err.Error()
		} else {
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			if _, werr := conn.Write([]byte("svc-tcp-hello")); werr != nil {
				lastErr = "write: " + werr.Error()
			} else {
				buf := make([]byte, 256)
				n, rerr := conn.Read(buf)
				if rerr != nil {
					lastErr = "read: " + rerr.Error()
				} else {
					// RemoteAddr is the VIP we dialed; the established
					// connection already proves the SYN-ACK came from it.
					fmt.Printf("svc-client: OK tcp reply %q from %s\n", string(buf[:n]), conn.RemoteAddr())
					conn.Close()
					return
				}
			}
			conn.Close()
		}
		if attempt < 6 {
			time.Sleep(1 * time.Second)
		}
	}
	fmt.Printf("svc-client: FAILED tcp no reply from VIP after 6 attempts: %s\n", lastErr)
}

// backendListenPort returns the backend listen port from the env, defaulting to
// 9097 if unset or unparseable.
func backendListenPort() int {
	if p, err := strconv.Atoi(os.Getenv(svcBackendPortEnv)); err == nil && p > 0 {
		return p
	}
	return 9097
}
