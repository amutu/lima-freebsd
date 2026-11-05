// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package osutil

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// icmpSocketProbe reports whether an ICMP socket of the given type can be
// created by the current (unprivileged) user. It creates the socket and closes
// it again; only the capability matters.
//
// It is a variable so that tests can substitute it without touching real file
// descriptors.
var icmpSocketProbe = func(typ int) error {
	fd, err := unix.Socket(unix.AF_INET, typ, unix.IPPROTO_ICMP)
	if err != nil {
		return err
	}
	_ = unix.Close(fd)
	return nil
}

// CheckICMPForwarding reports whether QEMU's user-mode networking can forward
// guest ICMP traffic (i.e. whether `ping <internet-host>` works inside the VM).
//
// QEMU builds its user-mode network on a userspace NAT stack (libslirp, or
// gvisor-tap-vsock for "user-v2"), and to emit a real ICMP echo request it must
// open an ICMP socket on the host. Two kinds are acceptable:
//
//   - SOCK_DGRAM + IPPROTO_ICMP: the "unprivileged ping socket" available on
//     macOS and (with net.ipv4.ping_group_range) on Linux.
//   - SOCK_RAW + IPPROTO_ICMP: requires root.
//
// On FreeBSD neither is available to an unprivileged user: raw sockets are
// root-only and the datagram form is not implemented at all
// (`Protocol not supported`). The NAT stack therefore cannot forward ICMP and
// silently drops it, while TCP, UDP and DNS keep working normally.
//
// The returned error describes the limitation; nil means ICMP forwarding works.
func CheckICMPForwarding() error {
	dgramErr := icmpSocketProbe(unix.SOCK_DGRAM)
	if dgramErr == nil {
		return nil
	}
	rawErr := icmpSocketProbe(unix.SOCK_RAW)
	if rawErr == nil {
		return nil
	}
	if errors.Is(dgramErr, unix.EPROTONOSUPPORT) && errors.Is(rawErr, unix.EPERM) {
		return fmt.Errorf("the host does not provide unprivileged ICMP sockets "+
			"(SOCK_DGRAM/IPPROTO_ICMP: %v; SOCK_RAW/IPPROTO_ICMP: %v)", dgramErr, rawErr)
	}
	return fmt.Errorf("no usable ICMP socket (SOCK_DGRAM: %v; SOCK_RAW: %v)", dgramErr, rawErr)
}
