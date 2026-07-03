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
	"encoding/binary"
	"fmt"
	"syscall"
)

// probeNetfilter exercises the kernel's NETLINK_NETFILTER surface directly,
// the way `nft`/`iptables-nft` do: open the socket, ask for the ruleset
// generation (NFT_MSG_GETGEN), and dump the tables (NFT_MSG_GETTABLE). This is
// a fast in-guest check of the new kernel family, decoupled from the cluster.
func probeNetfilter() {
	const netlinkNetfilter = 12
	fmt.Println("-- netfilter: NETLINK_NETFILTER socket probe --")

	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, netlinkNetfilter)
	if err != nil {
		fmt.Printf("netfilter: socket(AF_NETLINK, SOCK_RAW, NETLINK_NETFILTER) FAILED: %v\n", err)
		return
	}
	defer syscall.Close(fd)
	fmt.Println("netfilter: socket(AF_NETLINK, SOCK_RAW, NETLINK_NETFILTER) OK")

	// Don't block forever if the kernel never replies.
	tv := syscall.Timeval{Sec: 3}
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}

	const (
		nfnlSubsysNftables = 10
		nftMsgGetTable     = 1
		nftMsgGetGen       = 16
		nlmFRequest        = 0x01
		nlmFAck            = 0x04
		nlmFDump           = 0x300
	)
	// nfgenmsg: family=AF_UNSPEC, version=0, res_id=0.
	nfgen := []byte{0, 0, 0, 0}

	// NFT_MSG_GETGEN -> expect NFT_MSG_NEWGEN.
	getgen := nlMsg((nfnlSubsysNftables<<8)|nftMsgGetGen, nlmFRequest|nlmFAck, 1, nfgen)
	if err := syscall.Sendto(fd, getgen, 0, sa); err != nil {
		fmt.Printf("netfilter: send GETGEN failed: %v\n", err)
		return
	}
	reportNlReply(fd, "GETGEN")

	// NFT_MSG_GETTABLE dump -> expect (empty) + NLMSG_DONE.
	gettable := nlMsg((nfnlSubsysNftables<<8)|nftMsgGetTable, nlmFRequest|nlmFDump, 2, nfgen)
	if err := syscall.Sendto(fd, gettable, 0, sa); err != nil {
		fmt.Printf("netfilter: send GETTABLE failed: %v\n", err)
		return
	}
	reportNlReply(fd, "GETTABLE-dump")

	fmt.Println("netfilter: PROBE DONE — the NETLINK_NETFILTER family answers nft-style requests")
}

// nlMsg builds a netlink message: a 16-byte nlmsghdr followed by the payload.
func nlMsg(msgType, flags uint16, seq uint32, payload []byte) []byte {
	l := 16 + len(payload)
	b := make([]byte, l)
	binary.LittleEndian.PutUint32(b[0:], uint32(l))
	binary.LittleEndian.PutUint16(b[4:], msgType)
	binary.LittleEndian.PutUint16(b[6:], flags)
	binary.LittleEndian.PutUint32(b[8:], seq)
	binary.LittleEndian.PutUint32(b[12:], 0) // pid (kernel fills in)
	copy(b[16:], payload)
	return b
}

// reportNlReply reads one datagram and prints the type of each nlmsg in it.
// Types of interest: NLMSG_ERROR=2 (ack), NLMSG_DONE=3, NFT_MSG_NEWGEN=(10<<8)|15=2575.
func reportNlReply(fd int, what string) {
	buf := make([]byte, 8192)
	n, _, err := syscall.Recvfrom(fd, buf, 0)
	if err != nil {
		fmt.Printf("netfilter: %s no reply: %v\n", what, err)
		return
	}
	for off := 0; off+16 <= n; {
		l := binary.LittleEndian.Uint32(buf[off:])
		t := binary.LittleEndian.Uint16(buf[off+4:])
		name := nlTypeName(t)
		fmt.Printf("netfilter: %s reply: type=%d %s (subsys=%d msg=%d) len=%d\n",
			what, t, name, t>>8, t&0xff, l)
		if l < 16 {
			break
		}
		off += int((l + 3) &^ 3)
	}
}

func nlTypeName(t uint16) string {
	switch {
	case t == 2:
		return "NLMSG_ERROR/ack"
	case t == 3:
		return "NLMSG_DONE"
	case t == (10<<8)|15:
		return "NFT_MSG_NEWGEN"
	default:
		return ""
	}
}
