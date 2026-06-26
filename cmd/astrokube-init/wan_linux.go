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
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"time"
)

// probeOutboundTCP is the gating experiment for joining a real cluster: a
// Kubernetes node must open TCP+TLS to a kube-apiserver, so the very first
// question is whether eth0 can carry outbound TCP at all. ICMP is not a useful
// signal here — QEMU's user-mode (slirp) networking does not forward ping even
// on Linux, while TCP works fine.
//
// It brings eth0 up with slirp's default guest address, adds a default route,
// and opens a TCP connection to the host (reachable at 10.0.2.2 under slirp),
// then does a tiny HTTP exchange to prove bidirectional data flow.
func probeOutboundTCP() {
	fmt.Println()
	fmt.Println("-- wan: outbound TCP over eth0 (slirp user-net) --")

	nl, err := nlOpen()
	if err != nil {
		fmt.Printf("wan: FAILED to open netlink: %v\n", err)
		return
	}
	defer nl.close()

	idx, err := nl.linkIndexByName("eth0")
	if err != nil {
		fmt.Printf("wan: SKIPPED (no eth0 interface: %v)\n", err)
		return
	}
	if err := nl.addAddrV4(idx, net.IPv4(10, 0, 2, 15), 24); err != nil {
		fmt.Printf("wan: FAILED to assign 10.0.2.15/24 to eth0: %v\n", err)
		return
	}
	if err := nl.addDefaultRouteV4(net.IPv4(10, 0, 2, 2), idx); err != nil {
		fmt.Printf("wan: FAILED to add default route via 10.0.2.2: %v\n", err)
		return
	}
	fmt.Println("wan: eth0 configured 10.0.2.15/24, default via 10.0.2.2")

	// Under slirp the host is reachable from the guest at 10.0.2.2. The host
	// runs a throwaway HTTP server on :18080 for this probe.
	const target = "10.0.2.2:18080"
	conn, err := net.DialTimeout("tcp", target, 8*time.Second)
	if err != nil {
		fmt.Printf("wan: FAILED to TCP-connect to %s: %v\n", target, err)
		return
	}
	defer conn.Close()
	fmt.Printf("wan: TCP CONNECTED to %s (handshake OK)\n", target)

	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	if _, err := conn.Write([]byte("GET / HTTP/1.0\r\nHost: astrokube\r\n\r\n")); err != nil {
		fmt.Printf("wan: connected but write failed: %v\n", err)
		return
	}
	buf, _ := io.ReadAll(io.LimitReader(conn, 512))
	if len(buf) == 0 {
		fmt.Println("wan: connected + wrote, but no response read (one-way only?)")
		return
	}
	for _, line := range splitLines(string(buf)) {
		if line != "" {
			fmt.Printf("wan< %s\n", line)
		}
	}
	fmt.Println("wan: OUTBOUND TCP WORKS")

	// eth0 is configured; now the real gate — TLS, since a kube-apiserver is
	// HTTPS. This exercises Go's crypto/tls record layer + handshake (which
	// needs getrandom and a working clock) over Asterinas's TCP stack.
	probeOutboundTLS()
}

// probeOutboundTLS opens a TLS connection to the host's HTTPS server (self-signed,
// so certificate verification is skipped — we are testing the transport/handshake,
// not trust) and does a tiny HTTPS exchange.
func probeOutboundTLS() {
	const target = "10.0.2.2:18443"
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", target, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		fmt.Printf("wan: FAILED TLS handshake to %s: %v\n", target, err)
		return
	}
	defer conn.Close()
	state := conn.ConnectionState()
	fmt.Printf("wan: TLS HANDSHAKE OK to %s (version=0x%04x cipher=0x%04x)\n",
		target, state.Version, state.CipherSuite)

	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	if _, err := conn.Write([]byte("GET / HTTP/1.0\r\nHost: astrokube\r\n\r\n")); err != nil {
		fmt.Printf("wan: TLS connected but write failed: %v\n", err)
		return
	}
	buf, _ := io.ReadAll(io.LimitReader(conn, 256))
	for _, line := range splitLines(string(buf)) {
		if line != "" {
			fmt.Printf("wan(tls)< %s\n", line)
		}
	}
	fmt.Println("wan: OUTBOUND TLS WORKS — HTTPS apiserver connectivity is viable over slirp")
}
