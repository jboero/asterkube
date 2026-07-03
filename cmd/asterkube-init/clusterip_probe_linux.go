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
	"net"
	"time"
)

// apiserverClusterIP is the well-known ClusterIP of the kubernetes.default
// Service — the apiserver VIP. It is not a real host: it is only reachable if
// the kernel DNATs it (out eth0) to the control-plane endpoint that kube-proxy
// programmed into the NAT table.
const apiserverClusterIP = "10.96.0.1:443"

// probeNodeClusterIP proves that *node-originated* ClusterIP traffic is DNAT'd
// by the kernel on the eth0 path (not just pod traffic on the bridges). It dials
// the apiserver's ClusterIP and, on success, completes a TLS handshake — which
// can only happen if the kernel rewrote the VIP to the real apiserver endpoint
// and reverse-NAT'd the reply so this socket (bound to the VIP) accepted it.
//
// It retries because the NAT rule lands only once kube-proxy has synced its
// ruleset, which the node translates into the NAT table shortly after boot.
func probeNodeClusterIP() {
	var lastErr string
	for attempt := 1; attempt <= 12; attempt++ {
		conn, err := net.DialTimeout("tcp", apiserverClusterIP, 3*time.Second)
		if err != nil {
			lastErr = err.Error()
			time.Sleep(3 * time.Second)
			continue
		}
		// The TCP connect already proves the DNAT routed us to a live backend.
		// A TLS handshake confirms the backend is the apiserver itself.
		tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
		_ = tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
		herr := tlsConn.Handshake()
		_ = tlsConn.Close()
		if herr == nil {
			fmt.Printf("clusterip: OK node reached the apiserver via ClusterIP %s "+
				"— eth0-path DNAT works (TCP + TLS handshake to the VIP)\n", apiserverClusterIP)
		} else {
			fmt.Printf("clusterip: OK node TCP-connected to apiserver ClusterIP %s via "+
				"eth0 DNAT (TLS handshake: %v)\n", apiserverClusterIP, herr)
		}
		return
	}
	fmt.Printf("clusterip: node could not reach ClusterIP %s via eth0 DNAT after 12 "+
		"attempts: %s (is the apiserver Service rule programmed yet?)\n",
		apiserverClusterIP, lastErr)
}
