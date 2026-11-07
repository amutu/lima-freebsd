// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package osutil

import (
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
)

// withFakeProbe replaces the ICMP socket probe for the duration of a test.
func withFakeProbe(t *testing.T, fn func(typ int) error) {
	t.Helper()
	orig := icmpSocketProbe
	t.Cleanup(func() { icmpSocketProbe = orig })
	icmpSocketProbe = fn
}

// TestCheckICMPForwardingUnprivilegedDatagram covers hosts that allow the
// unprivileged ping socket (macOS, Linux with ping_group_range).
func TestCheckICMPForwardingUnprivilegedDatagram(t *testing.T) {
	var tried []int
	withFakeProbe(t, func(typ int) error {
		tried = append(tried, typ)
		return nil
	})

	assert.NilError(t, CheckICMPForwarding())
	// The datagram form is tried first and success short-circuits.
	assert.DeepEqual(t, tried, []int{unix.SOCK_DGRAM})
}

// TestCheckICMPForwardingRawOnly covers a privileged process: no datagram form,
// but a raw ICMP socket is permitted.
func TestCheckICMPForwardingRawOnly(t *testing.T) {
	var tried []int
	withFakeProbe(t, func(typ int) error {
		tried = append(tried, typ)
		if typ == unix.SOCK_DGRAM {
			return unix.EPROTONOSUPPORT
		}
		return nil
	})

	assert.NilError(t, CheckICMPForwarding())
	assert.DeepEqual(t, tried, []int{unix.SOCK_DGRAM, unix.SOCK_RAW})
}

// TestCheckICMPForwardingUnavailable is the FreeBSD case: the datagram form does
// not exist and raw sockets are root-only.
func TestCheckICMPForwardingUnavailable(t *testing.T) {
	withFakeProbe(t, func(typ int) error {
		if typ == unix.SOCK_DGRAM {
			return unix.EPROTONOSUPPORT
		}
		return unix.EPERM
	})

	err := CheckICMPForwarding()
	assert.Assert(t, err != nil, "expected an error when no ICMP socket is available")
	// The message must name both attempts so that operators can tell
	// "the kernel has no such socket type" from "you are not root".
	assert.ErrorContains(t, err, "unprivileged ICMP")
	assert.ErrorContains(t, err, "SOCK_DGRAM")
	assert.ErrorContains(t, err, "SOCK_RAW")
}

// TestCheckICMPForwardingUnexpectedError keeps unrelated failure modes
// distinguishable from the known FreeBSD one.
func TestCheckICMPForwardingUnexpectedError(t *testing.T) {
	withFakeProbe(t, func(typ int) error {
		if typ == unix.SOCK_DGRAM {
			return unix.EMFILE
		}
		return unix.ENOMEM
	})

	err := CheckICMPForwarding()
	assert.Assert(t, err != nil)
	assert.ErrorContains(t, err, "no usable ICMP socket")
	assert.Assert(t, !strings.Contains(err.Error(), "unprivileged ICMP"),
		"an unexpected errno must not be reported as the known limitation: %v", err)
}
