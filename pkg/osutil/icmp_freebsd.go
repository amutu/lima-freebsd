// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

//go:build freebsd

package osutil

import (
	"sync"

	"github.com/sirupsen/logrus"
)

var warnICMPOnce sync.Once

// WarnIfICMPUnavailable explains, once per process, that guest-to-internet ICMP
// will be silently dropped, so that "ping fails" is not mistaken for
// "the network is down".
func WarnIfICMPUnavailable() {
	err := CheckICMPForwarding()
	if err == nil {
		return
	}
	warnICMPOnce.Do(func() {
		logrus.Warnf("Guest-to-internet ICMP (ping) will not work on this host: %v. "+
			"Lima's user-mode networking (libslirp, or gvisor-tap-vsock for \"user-v2\") forwards ICMP "+
			"by emitting real ICMP packets from the host process, which FreeBSD only permits for root "+
			"(and it has no unprivileged ICMP datagram sockets). "+
			"TCP, UDP and DNS are NOT affected - verify connectivity with e.g. `nc -z <host> 443` "+
			"instead of ping. `ping <gateway>` still works because the gateway is answered "+
			"inside the userspace network stack.", err)
	})
}
