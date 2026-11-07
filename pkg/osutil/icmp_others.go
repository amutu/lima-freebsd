// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !freebsd

package osutil

// WarnIfICMPUnavailable is a no-op outside FreeBSD: macOS allows unprivileged
// ICMP sockets, and on Linux the limitation is pre-existing upstream behaviour.
func WarnIfICMPUnavailable() {}
