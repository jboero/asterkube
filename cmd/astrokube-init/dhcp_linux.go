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

// DHCP-first networking, the cloud-init / nomadinit way: before anything else
// needs the network, the init brings the primary interface up by DHCP so it has
// an IP, default route, and DNS up front. Asterinas has no AF_PACKET, but its
// UDP stack supports SO_BROADCAST, so this is a plain-UDP DHCPv4 client (RFC
// 2131) that sets the DHCP broadcast flag — the server then broadcasts its
// replies, which a socket bound to 0.0.0.0:68 receives before the interface has
// an address. The lease is applied with the existing netlink helpers, and DNS
// (option 6) is written to /etc/resolv.conf.

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

const (
	dhcpServerPort = 67
	dhcpClientPort = 68

	bootpRequest = 1
	bootpReply   = 2
	htypeEth     = 1

	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpAck      = 5

	optSubnetMask  = 1
	optRouter      = 3
	optDNS         = 6
	optRequestedIP = 50
	optLeaseTime   = 51
	optMsgType     = 53
	optServerID    = 54
	optParamList   = 55
	optEnd         = 255

	dhcpFlagBroadcast = 0x8000
)

var dhcpMagicCookie = [4]byte{0x63, 0x82, 0x53, 0x63}

// networkConfigured is set once DHCP (or a later static fallback) has put an
// address on the primary interface, so the static-config probes don't fight it.
var networkConfigured bool

type dhcpLease struct {
	ip       net.IP
	mask     net.IPMask
	gateway  net.IP
	dns      []net.IP
	serverID net.IP
	leaseSec uint32
}

// dhcpFirst runs DHCP on the primary interface and applies the result (address,
// default route, DNS). It is best-effort: on any failure (e.g. no DHCP server on
// a bare boot) it returns false and the caller falls back to static config.
func dhcpFirst(iface string) bool {
	fmt.Println()
	fmt.Printf("astrokube-init: -- DHCP: bringing up %s before anything else (cloud-init style) --\n", iface)

	nl, err := nlOpen()
	if err != nil {
		fmt.Printf("astrokube-init: DHCP SKIPPED (netlink: %v)\n", err)
		return false
	}
	idx, mac, err := nl.linkByName(iface)
	nl.close()
	if err != nil {
		fmt.Printf("astrokube-init: DHCP SKIPPED (no %s: %v)\n", iface, err)
		return false
	}
	if mac == nil {
		// Asterinas does not yet expose the NIC MAC via netlink IFLA_ADDRESS; use a
		// locally-administered fallback so slirp/lenient servers still lease. A real
		// cloud keys the lease on the true MAC, so this needs the kernel to surface
		// it (IFLA_ADDRESS) to be fully cloud-correct.
		mac = net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
		fmt.Printf("astrokube-init: DHCP: NIC MAC not exposed by kernel; using fallback %s\n", mac)
	}

	xid := uint32(time.Now().UnixNano())
	lease, err := dhcpAcquire(mac, xid, 4*time.Second)
	if err != nil {
		fmt.Printf("astrokube-init: DHCP failed (%v) — falling back to static config\n", err)
		return false
	}

	prefix, _ := lease.mask.Size()
	nl2, err := nlOpen()
	if err != nil {
		fmt.Printf("astrokube-init: DHCP got a lease but netlink reopen failed: %v\n", err)
		return false
	}
	defer nl2.close()
	if err := nl2.addAddrV4(idx, lease.ip, prefix); err != nil {
		fmt.Printf("astrokube-init: DHCP: assign %s/%d failed: %v\n", lease.ip, prefix, err)
		return false
	}
	if lease.gateway != nil && !lease.gateway.IsUnspecified() {
		if err := nl2.addDefaultRouteV4(lease.gateway, idx); err != nil {
			fmt.Printf("astrokube-init: DHCP: default route via %s failed: %v\n", lease.gateway, err)
		}
	}
	writeResolvConf(lease.dns)

	dnsStr := "(none)"
	if len(lease.dns) > 0 {
		var b []string
		for _, d := range lease.dns {
			b = append(b, d.String())
		}
		dnsStr = strings.Join(b, ",")
	}
	fmt.Printf("astrokube-init: DHCP OK — %s/%d via %s, dns=%s, lease=%ds (from %s) ✓\n",
		lease.ip, prefix, lease.gateway, dnsStr, lease.leaseSec, lease.serverID)
	networkConfigured = true
	return true
}

// dhcpAcquire performs DISCOVER -> OFFER -> REQUEST -> ACK over UDP broadcast.
func dhcpAcquire(mac net.HardwareAddr, xid uint32, timeout time.Duration) (*dhcpLease, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	defer syscall.Close(fd)
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: dhcpClientPort}); err != nil {
		return nil, fmt.Errorf("bind :68: %w", err)
	}
	tv := syscall.NsecToTimeval(int64(timeout))
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	bcast := &syscall.SockaddrInet4{Port: dhcpServerPort, Addr: [4]byte{255, 255, 255, 255}}

	// DISCOVER (retry a few times in case the first frame is lost during link-up).
	var offer *dhcpLease
	for try := 0; try < 3 && offer == nil; try++ {
		if err := syscall.Sendto(fd, buildDHCP(dhcpDiscover, mac, xid, nil, nil), 0, bcast); err != nil {
			return nil, fmt.Errorf("send DISCOVER: %w", err)
		}
		offer, _ = recvDHCP(fd, xid, dhcpOffer)
	}
	if offer == nil {
		return nil, fmt.Errorf("no DHCPOFFER")
	}

	// REQUEST the offered address.
	if err := syscall.Sendto(fd, buildDHCP(dhcpRequest, mac, xid, offer.ip, offer.serverID), 0, bcast); err != nil {
		return nil, fmt.Errorf("send REQUEST: %w", err)
	}
	ack, err := recvDHCP(fd, xid, dhcpAck)
	if err != nil {
		return nil, fmt.Errorf("no DHCPACK: %w", err)
	}
	// The OFFER usually carries the full parameter set; merge anything the ACK omits.
	if ack.mask == nil {
		ack.mask = offer.mask
	}
	if ack.gateway == nil {
		ack.gateway = offer.gateway
	}
	if len(ack.dns) == 0 {
		ack.dns = offer.dns
	}
	if ack.mask == nil {
		ack.mask = net.CIDRMask(24, 32) // sane default if the server omitted it
	}
	return ack, nil
}

// buildDHCP assembles a BOOTP/DHCP request of the given message type.
func buildDHCP(msgType byte, mac net.HardwareAddr, xid uint32, reqIP, serverID net.IP) []byte {
	p := make([]byte, 240) // 236 BOOTP header + 4 magic cookie
	p[0] = bootpRequest
	p[1] = htypeEth
	p[2] = 6 // hlen
	binary.BigEndian.PutUint32(p[4:8], xid)
	binary.BigEndian.PutUint16(p[10:12], dhcpFlagBroadcast)
	copy(p[28:34], mac) // chaddr
	copy(p[236:240], dhcpMagicCookie[:])

	opts := []byte{optMsgType, 1, msgType}
	if reqIP != nil {
		if v4 := reqIP.To4(); v4 != nil {
			opts = append(opts, optRequestedIP, 4)
			opts = append(opts, v4...)
		}
	}
	if serverID != nil {
		if v4 := serverID.To4(); v4 != nil {
			opts = append(opts, optServerID, 4)
			opts = append(opts, v4...)
		}
	}
	opts = append(opts, optParamList, 5, optSubnetMask, optRouter, optDNS, optLeaseTime, optServerID)
	opts = append(opts, optEnd)
	return append(p, opts...)
}

// recvDHCP reads replies until one matches xid and the wanted message type, or
// the socket times out.
func recvDHCP(fd int, xid uint32, want byte) (*dhcpLease, error) {
	buf := make([]byte, 1500)
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return nil, err // typically EAGAIN on timeout
		}
		if n < 240 {
			continue
		}
		pkt := buf[:n]
		if pkt[0] != bootpReply || binary.BigEndian.Uint32(pkt[4:8]) != xid {
			continue
		}
		lease := &dhcpLease{ip: net.IP(append([]byte(nil), pkt[16:20]...))} // yiaddr
		mt := parseOptions(pkt[240:], lease)
		if mt == want {
			return lease, nil
		}
	}
	return nil, fmt.Errorf("timed out waiting for DHCP message type %d", want)
}

// parseOptions walks the DHCP option TLVs, filling lease, and returns the message
// type (option 53).
func parseOptions(opts []byte, lease *dhcpLease) byte {
	var msgType byte
	for i := 0; i < len(opts); {
		code := opts[i]
		if code == optEnd {
			break
		}
		if code == 0 { // pad
			i++
			continue
		}
		if i+1 >= len(opts) {
			break
		}
		l := int(opts[i+1])
		if i+2+l > len(opts) {
			break
		}
		v := opts[i+2 : i+2+l]
		switch code {
		case optMsgType:
			if l == 1 {
				msgType = v[0]
			}
		case optSubnetMask:
			if l == 4 {
				lease.mask = net.IPMask(append([]byte(nil), v...))
			}
		case optRouter:
			if l >= 4 {
				lease.gateway = net.IP(append([]byte(nil), v[:4]...))
			}
		case optDNS:
			for j := 0; j+4 <= l; j += 4 {
				lease.dns = append(lease.dns, net.IP(append([]byte(nil), v[j:j+4]...)))
			}
		case optServerID:
			if l == 4 {
				lease.serverID = net.IP(append([]byte(nil), v...))
			}
		case optLeaseTime:
			if l == 4 {
				lease.leaseSec = binary.BigEndian.Uint32(v)
			}
		}
		i += 2 + l
	}
	return msgType
}

// writeResolvConf writes /etc/resolv.conf from the DHCP-provided DNS servers.
func writeResolvConf(dns []net.IP) {
	if len(dns) == 0 {
		return
	}
	var b strings.Builder
	for _, d := range dns {
		fmt.Fprintf(&b, "nameserver %s\n", d)
	}
	_ = os.WriteFile("/etc/resolv.conf", []byte(b.String()), 0o644)
}
