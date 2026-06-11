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

// A tiny, dependency-free RTNETLINK (NETLINK_ROUTE) client.
//
// This is what a real CNI plugin uses to wire a pod's network namespace to the
// host: create a veth pair, place one end in the pod's netns, and assign
// addresses. It speaks the same wire protocol as iproute2 / vishvananda's
// netlink library, exercising the kernel's `RTM_NEWLINK`/`RTM_NEWADDR` support
// directly — there is no astrokube-specific shim involved.

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// Netlink attribute type IDs we use. These match the Linux UAPI; we define them
// locally rather than relying on the (incomplete) std `syscall` constant set.
const (
	iflaIfname    = 3  // IFLA_IFNAME
	iflaMaster    = 10 // IFLA_MASTER
	iflaLinkInfo  = 18 // IFLA_LINKINFO
	iflaNetNsPid  = 19 // IFLA_NET_NS_PID
	iflaInfoKind  = 1  // IFLA_INFO_KIND   (nested under IFLA_LINKINFO)
	iflaInfoData  = 2  // IFLA_INFO_DATA   (nested under IFLA_LINKINFO)
	vethInfoPeer  = 1  // VETH_INFO_PEER   (nested under IFLA_INFO_DATA)
	ifaLocal      = 2  // IFA_LOCAL
	ifaAddress    = 1  // IFA_ADDRESS
	nlmsgHdrLen   = 16 // sizeof(struct nlmsghdr)
	ifInfomsgLen  = 16 // sizeof(struct ifinfomsg)
	ifAddrmsgLen  = 8  // sizeof(struct ifaddrmsg)
	ifNameMaxSize = 15 // IFNAMSIZ - 1 (room for the NUL)

	rtmsgLen        = 12  // sizeof(struct rtmsg)
	rtaDst          = 1   // RTA_DST
	rtaOif          = 4   // RTA_OIF
	rtaGateway      = 5   // RTA_GATEWAY
	rtnUnicast      = 1   // RTN_UNICAST
	rtTableMain     = 254 // RT_TABLE_MAIN
	rtProtBoot      = 3   // RTPROT_BOOT
	rtScopeUniverse = 0   // RT_SCOPE_UNIVERSE
)

// host byte order, used for all netlink framing (addresses stay network order).
var nlOrder = binary.LittleEndian

// nlAlign rounds n up to the 4-byte netlink alignment.
func nlAlign(n int) int { return (n + 3) &^ 3 }

// nlAttr encodes one netlink attribute (rtattr TLV) with trailing padding.
func nlAttr(typ uint16, data []byte) []byte {
	l := 4 + len(data)
	b := make([]byte, nlAlign(l))
	nlOrder.PutUint16(b[0:2], uint16(l))
	nlOrder.PutUint16(b[2:4], typ)
	copy(b[4:], data)
	return b
}

// nlAttrStr encodes a NUL-terminated string attribute.
func nlAttrStr(typ uint16, s string) []byte {
	return nlAttr(typ, append([]byte(s), 0))
}

// nlAttrU32 encodes a 32-bit attribute in host byte order.
func nlAttrU32(typ uint16, v uint32) []byte {
	d := make([]byte, 4)
	nlOrder.PutUint32(d, v)
	return nlAttr(typ, d)
}

// concat joins already-encoded attribute byte slices.
func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// nlSocket is a bound NETLINK_ROUTE socket. All operations resolve against the
// caller's network namespace, exactly as Linux netlink does.
type nlSocket struct {
	fd  int
	seq uint32
	pid uint32
}

func nlOpen() (*nlSocket, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	s := &nlSocket{fd: fd}
	if lsa, err := syscall.Getsockname(fd); err == nil {
		if nl, ok := lsa.(*syscall.SockaddrNetlink); ok {
			s.pid = nl.Pid
		}
	}
	return s, nil
}

func (s *nlSocket) close() { syscall.Close(s.fd) }

// send frames and transmits one netlink message, returning its sequence number.
func (s *nlSocket) send(msgType, flags uint16, body []byte) (uint32, error) {
	s.seq++
	total := nlmsgHdrLen + len(body)
	buf := make([]byte, total)
	nlOrder.PutUint32(buf[0:4], uint32(total))
	nlOrder.PutUint16(buf[4:6], msgType)
	nlOrder.PutUint16(buf[6:8], flags)
	nlOrder.PutUint32(buf[8:12], s.seq)
	nlOrder.PutUint32(buf[12:16], s.pid)
	copy(buf[16:], body)
	return s.seq, syscall.Sendto(s.fd, buf, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK})
}

// recvAck waits for the kernel's acknowledgment of request seq (an NLMSG_ERROR
// segment with code 0 on success, or a negative errno on failure).
func (s *nlSocket) recvAck(seq uint32) error {
	for {
		buf := make([]byte, 8192)
		n, _, err := syscall.Recvfrom(s.fd, buf, 0)
		if err != nil {
			return err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Header.Seq != seq {
				continue
			}
			switch m.Header.Type {
			case syscall.NLMSG_ERROR:
				errno := int32(nlOrder.Uint32(m.Data[0:4]))
				if errno == 0 {
					return nil
				}
				return fmt.Errorf("netlink: %w", syscall.Errno(-errno))
			case syscall.NLMSG_DONE:
				return nil
			}
		}
	}
}

// createVethPeerNetns issues `RTM_NEWLINK` to create a veth pair named `name`
// (placed in the caller's netns) with its peer named `peerName` placed in the
// network namespace of process `peerPid` (via VETH_INFO_PEER + IFLA_NET_NS_PID).
//
// Wire equivalent of: `ip link add <name> type veth peer name <peer> netns <pid>`.
func (s *nlSocket) createVethPeerNetns(name, peerName string, peerPid int) error {
	// Peer specification: an (empty) ifinfomsg, the peer name, and its target netns.
	peerBody := concat(
		make([]byte, ifInfomsgLen),
		nlAttrStr(iflaIfname, peerName),
		nlAttrU32(iflaNetNsPid, uint32(peerPid)),
	)
	linkInfo := nlAttr(iflaLinkInfo, concat(
		nlAttrStr(iflaInfoKind, "veth"),
		nlAttr(iflaInfoData, nlAttr(vethInfoPeer, peerBody)),
	))

	body := concat(
		make([]byte, ifInfomsgLen), // ifinfomsg (AF_UNSPEC, no flags)
		nlAttrStr(iflaIfname, name),
		linkInfo,
	)

	flags := uint16(syscall.NLM_F_REQUEST | syscall.NLM_F_ACK |
		syscall.NLM_F_CREATE | syscall.NLM_F_EXCL)
	seq, err := s.send(syscall.RTM_NEWLINK, flags, body)
	if err != nil {
		return err
	}
	return s.recvAck(seq)
}

// createBridge issues `RTM_NEWLINK` to create a bridge interface named `name`
// in the caller's netns. No address is assigned here; the caller gives the
// bridge its gateway address via RTM_NEWADDR, exactly as `ip` would.
//
// Wire equivalent of: `ip link add <name> type bridge`.
func (s *nlSocket) createBridge(name string) error {
	body := concat(
		make([]byte, ifInfomsgLen), // ifinfomsg (AF_UNSPEC, no flags)
		nlAttrStr(iflaIfname, name),
		nlAttr(iflaLinkInfo, nlAttrStr(iflaInfoKind, "bridge")),
	)

	flags := uint16(syscall.NLM_F_REQUEST | syscall.NLM_F_ACK |
		syscall.NLM_F_CREATE | syscall.NLM_F_EXCL)
	seq, err := s.send(syscall.RTM_NEWLINK, flags, body)
	if err != nil {
		return err
	}
	return s.recvAck(seq)
}

// createVethOnBridge is createVethPeerNetns with the host half enslaved to a
// bridge at creation time: a top-level IFLA_MASTER carries the bridge ifindex,
// so the kernel makes the host end a bridge port directly — it never appears
// as a standalone host interface, so callers must not look it up afterwards.
// The peer is placed in the network namespace of process `peerPid` as usual.
//
// Wire equivalent of: `ip link add <hostName> type veth peer name <peer>
// netns <pid>` with the host end enslaved in the same RTM_NEWLINK.
func (s *nlSocket) createVethOnBridge(hostName, peerName string, peerPid, masterIndex int) error {
	// Peer specification: an (empty) ifinfomsg, the peer name, and its target netns.
	peerBody := concat(
		make([]byte, ifInfomsgLen),
		nlAttrStr(iflaIfname, peerName),
		nlAttrU32(iflaNetNsPid, uint32(peerPid)),
	)
	linkInfo := nlAttr(iflaLinkInfo, concat(
		nlAttrStr(iflaInfoKind, "veth"),
		nlAttr(iflaInfoData, nlAttr(vethInfoPeer, peerBody)),
	))

	body := concat(
		make([]byte, ifInfomsgLen), // ifinfomsg (AF_UNSPEC, no flags)
		nlAttrStr(iflaIfname, hostName),
		linkInfo,
		nlAttrU32(iflaMaster, uint32(masterIndex)),
	)

	flags := uint16(syscall.NLM_F_REQUEST | syscall.NLM_F_ACK |
		syscall.NLM_F_CREATE | syscall.NLM_F_EXCL)
	seq, err := s.send(syscall.RTM_NEWLINK, flags, body)
	if err != nil {
		return err
	}
	return s.recvAck(seq)
}

// addAddrV4 issues `RTM_NEWADDR` to assign an IPv4 address to interface ifindex
// in the caller's netns. Wire equivalent of: `ip addr add <ip>/<prefix> dev …`.
func (s *nlSocket) addAddrV4(ifindex int, ip net.IP, prefix int) error {
	v4 := ip.To4()
	if v4 == nil {
		return fmt.Errorf("not an IPv4 address: %v", ip)
	}

	// struct ifaddrmsg { family; prefixlen; flags; scope; index }
	hdr := make([]byte, ifAddrmsgLen)
	hdr[0] = syscall.AF_INET
	hdr[1] = byte(prefix)
	nlOrder.PutUint32(hdr[4:8], uint32(ifindex))

	// The address bytes are kept in network order (as in net.IP.To4()).
	body := concat(
		hdr,
		nlAttr(ifaLocal, []byte(v4)),
		nlAttr(ifaAddress, []byte(v4)),
	)

	flags := uint16(syscall.NLM_F_REQUEST | syscall.NLM_F_ACK |
		syscall.NLM_F_CREATE | syscall.NLM_F_REPLACE)
	seq, err := s.send(syscall.RTM_NEWADDR, flags, body)
	if err != nil {
		return err
	}
	return s.recvAck(seq)
}

// addDefaultRouteV4 issues `RTM_NEWROUTE` to install `default via gw dev ifindex`
// in the caller's netns. Wire equivalent of: `ip route add default via <gw>`.
func (s *nlSocket) addDefaultRouteV4(gw net.IP, ifindex int) error {
	v4 := gw.To4()
	if v4 == nil {
		return fmt.Errorf("not an IPv4 gateway: %v", gw)
	}

	// struct rtmsg { family; dst_len; src_len; tos; table; protocol; scope; type; flags }
	hdr := make([]byte, rtmsgLen)
	hdr[0] = syscall.AF_INET // family
	hdr[1] = 0               // dst_len = 0 -> default route
	hdr[4] = rtTableMain     // table
	hdr[5] = rtProtBoot      // protocol
	hdr[6] = rtScopeUniverse // scope
	hdr[7] = rtnUnicast      // type

	body := concat(
		hdr,
		nlAttr(rtaGateway, []byte(v4)),
		nlAttrU32(rtaOif, uint32(ifindex)),
	)

	flags := uint16(syscall.NLM_F_REQUEST | syscall.NLM_F_ACK | syscall.NLM_F_CREATE)
	seq, err := s.send(syscall.RTM_NEWROUTE, flags, body)
	if err != nil {
		return err
	}
	return s.recvAck(seq)
}

// defaultRouteGateway issues an `RTM_GETROUTE` dump and returns the gateway of
// the default route (`dst_len == 0`) in the caller's netns, or nil if none.
func (s *nlSocket) defaultRouteGateway() (net.IP, error) {
	seq, err := s.send(syscall.RTM_GETROUTE,
		syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP, make([]byte, rtmsgLen))
	if err != nil {
		return nil, err
	}

	for {
		buf := make([]byte, 16384)
		n, _, err := syscall.Recvfrom(s.fd, buf, 0)
		if err != nil {
			return nil, err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if m.Header.Seq != seq {
				continue
			}
			switch m.Header.Type {
			case syscall.NLMSG_DONE:
				return nil, nil
			case syscall.NLMSG_ERROR:
				errno := int32(nlOrder.Uint32(m.Data[0:4]))
				return nil, fmt.Errorf("GETROUTE dump: %w", syscall.Errno(-errno))
			case syscall.RTM_NEWROUTE:
				if len(m.Data) < rtmsgLen || m.Data[1] != 0 {
					continue // not the default route (dst_len != 0)
				}
				attrs, perr := syscall.ParseNetlinkRouteAttr(&m)
				if perr != nil {
					continue
				}
				for _, a := range attrs {
					if a.Attr.Type == rtaGateway && len(a.Value) == 4 {
						return net.IP(a.Value).To4(), nil
					}
				}
			}
		}
	}
}

// linkIndexByName issues an `RTM_GETLINK` dump and returns the index of the
// interface named `name` in the caller's netns.
func (s *nlSocket) linkIndexByName(name string) (int, error) {
	seq, err := s.send(syscall.RTM_GETLINK,
		syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP, make([]byte, ifInfomsgLen))
	if err != nil {
		return 0, err
	}

	for {
		buf := make([]byte, 16384)
		n, _, err := syscall.Recvfrom(s.fd, buf, 0)
		if err != nil {
			return 0, err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return 0, err
		}
		for _, m := range msgs {
			if m.Header.Seq != seq {
				continue
			}
			switch m.Header.Type {
			case syscall.NLMSG_DONE:
				return 0, fmt.Errorf("interface %q not found", name)
			case syscall.NLMSG_ERROR:
				errno := int32(nlOrder.Uint32(m.Data[0:4]))
				return 0, fmt.Errorf("GETLINK dump: %w", syscall.Errno(-errno))
			case syscall.RTM_NEWLINK:
				ifi := (*syscall.IfInfomsg)(unsafe.Pointer(&m.Data[0]))
				attrs, perr := syscall.ParseNetlinkRouteAttr(&m)
				if perr != nil {
					continue
				}
				for _, a := range attrs {
					if a.Attr.Type == iflaIfname && trimNul(a.Value) == name {
						return int(ifi.Index), nil
					}
				}
			}
		}
	}
}

// trimNul returns b without a trailing NUL byte, if present.
func trimNul(b []byte) string {
	if n := len(b); n > 0 && b[n-1] == 0 {
		return string(b[:n-1])
	}
	return string(b)
}
