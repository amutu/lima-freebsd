// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

//go:build freebsd

package osutil

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
)

// TestWarnIfICMPUnavailableLogsOnce verifies that the FreeBSD hint is emitted,
// names the affected traffic, and is not repeated on every VM start.
func TestWarnIfICMPUnavailableLogsOnce(t *testing.T) {
	withFakeProbe(t, func(typ int) error {
		if typ == unix.SOCK_DGRAM {
			return unix.EPROTONOSUPPORT
		}
		return unix.EPERM
	})
	// Reset the once-guard so that other tests cannot have consumed it.
	warnICMPOnce = sync.Once{}

	var buf bytes.Buffer
	origOut := logrus.StandardLogger().Out
	origLevel := logrus.GetLevel()
	logrus.SetOutput(&buf)
	logrus.SetLevel(logrus.WarnLevel)
	t.Cleanup(func() {
		logrus.SetOutput(origOut)
		logrus.SetLevel(origLevel)
	})

	WarnIfICMPUnavailable()
	WarnIfICMPUnavailable()

	out := buf.String()
	assert.Equal(t, strings.Count(out, "Guest-to-internet ICMP"), 1,
		"the warning must be emitted exactly once, got: %s", out)
	// It must not imply that the whole network is broken.
	assert.Assert(t, strings.Contains(out, "TCP, UDP and DNS are NOT affected"), out)
	assert.Assert(t, strings.Contains(out, "nc -z"), "the hint must suggest a TCP-based check: %s", out)
	assert.Assert(t, strings.Contains(out, "ping <gateway>"), out)
}

// TestWarnIfICMPUnavailableSilentWhenAvailable verifies that no warning is
// emitted when the host can forward ICMP.
func TestWarnIfICMPUnavailableSilentWhenAvailable(t *testing.T) {
	withFakeProbe(t, func(int) error { return nil })
	warnICMPOnce = sync.Once{}

	var buf bytes.Buffer
	origOut := logrus.StandardLogger().Out
	logrus.SetOutput(&buf)
	t.Cleanup(func() { logrus.SetOutput(origOut) })

	WarnIfICMPUnavailable()
	assert.Equal(t, buf.Len(), 0, "no output expected, got: %s", buf.String())
}
