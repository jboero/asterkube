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
)

// This file is the astermac multi-tenant ADAPTER: it turns a pod's spec (its
// Tenant field — where a real launcher would map a namespace / seLinuxOptions /
// tenant annotation) into actual kernel security labels, and demonstrates that
// two real pods of different tenants are isolated by the kernel MAC.

// ipToBE converts a dotted-quad IPv4 string to u32::from_be_bytes(octets), the
// key the kernel's astermac IP-label table and connect hook use.
func ipToBE(s string) uint32 {
	ip := net.ParseIP(s).To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

// applyTenantLabels labels each tenant pod's IP with its tenant and switches the
// astermac MAC to enforcing — done before any pod starts, so a cross-tenant
// connect can never race ahead of its peer's label. Unlabeled (tenant 0) pods,
// i.e. every ordinary pod, are unaffected.
func applyTenantLabels(specs []podSpec) {
	labeled := false
	for _, s := range specs {
		if s.Tenant == 0 || s.Network == nil || s.Network.PodIP == "" {
			continue
		}
		if err := labelIP(uintptr(ipToBE(s.Network.PodIP)), uintptr(s.Tenant)); err != nil {
			fmt.Printf("astermac: FAILED to label pod %q IP %s: %v\n", s.Name, s.Network.PodIP, err)
			continue
		}
		fmt.Printf("astermac: labeled pod %q IP %s -> tenant %d\n", s.Name, s.Network.PodIP, s.Tenant)
		labeled = true
	}
	if labeled {
		if err := prctlMacMode(macModeEnforcing); err != nil {
			fmt.Printf("astermac: FAILED to set enforcing: %v\n", err)
		} else {
			fmt.Println("astermac: enforcing ON for tenant-labeled pods (unlabeled pods unaffected)")
		}
	}
}

// cleanupTenantLabels restores the permissive default and removes the demo
// labels after the tenant pods finish, leaving the node in its k8s-safe state.
func cleanupTenantLabels(specs []podSpec) {
	for _, s := range specs {
		if s.Tenant != 0 && s.Network != nil && s.Network.PodIP != "" {
			_ = labelIP(uintptr(ipToBE(s.Network.PodIP)), 0)
		}
	}
	_ = prctlMacMode(macModePermissive)
}

// containerSetTenant runs at the very start of a container: if the node agent
// assigned this pod a tenant, the pod labels itself (it holds CAP_SYS_ADMIN in
// its namespaces). All of the pod's subsequent operations are then mediated by
// astermac against that tenant.
func containerSetTenant() {
	t := os.Getenv(podTenantEnv)
	if t == "" {
		return
	}
	n, err := strconv.Atoi(t)
	if err != nil || n <= 0 {
		return
	}
	if err := prctlSetTenant(uintptr(n)); err != nil {
		fmt.Printf("astermac: pod failed to set tenant %d: %v\n", n, err)
		return
	}
	fmt.Printf("astermac: pod labeled tenant %d\n", n)
}

// tenantConnectProbe attempts a TCP connect from this (tenant-labeled) pod to a
// peer pod's address. Under enforcing MAC a cross-tenant connect is denied with
// EPERM before the connection is even attempted.
func tenantConnectProbe(target string) string {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Sprintf("socket failed: %v", err)
	}
	defer syscall.Close(fd)
	ip := ipToBE(target)
	sa := &syscall.SockaddrInet4{Port: 9} // discard port; the MAC check precedes connect
	sa.Addr = [4]byte{byte((ip >> 24) & 0xff), byte((ip >> 16) & 0xff), byte((ip >> 8) & 0xff), byte(ip & 0xff)}
	err = syscall.Connect(fd, sa)
	if err == syscall.EPERM {
		return fmt.Sprintf("connect %s -> DENIED by astermac (EPERM): cross-tenant isolated ✓", target)
	}
	return fmt.Sprintf("connect %s -> NOT isolated (err=%v)", target, err)
}

// buildTenantDemoSpecs builds two real pods on a shared bridge with different
// astermac tenants. tn1 (tenant 1) tries to reach tn2 (tenant 2) — which the
// kernel must deny, proving multi-tenant isolation between actual pods (not just
// the synthetic probes), driven entirely by the pods' spec via the adapter.
func buildTenantDemoSpecs() []podSpec {
	const (
		bridge = "tnbr"
		gw     = "10.244.7.1"
	)
	mk := func(name string, tenant uint32, ip, probe string) podSpec {
		return podSpec{
			Name:      name,
			Hostname:  name,
			Command:   []string{"/usr/bin/kubelet"},
			Env:       []string{containerEnv + "=1"},
			Resources: podResources{MemoryMaxMiB: 128, CPUMaxPercent: 50},
			Tenant:    tenant,
			Network: &podNetwork{
				Bridge:        bridge,
				GatewayIP:     gw,
				Prefix:        24,
				PodIP:         ip,
				TenantProbeIP: probe,
			},
		}
	}
	return []podSpec{
		mk("tn1", 1, "10.244.7.2", "10.244.7.3"),
		mk("tn2", 2, "10.244.7.3", ""),
	}
}
