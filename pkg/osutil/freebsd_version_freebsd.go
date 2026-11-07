// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

//go:build freebsd

package osutil

import (
	"os/exec"
	"strings"
	"sync"
)

var freebsdVersion = sync.OnceValues(func() (string, error) {
	for _, args := range [][]string{{"-u"}, {"-k"}} {
		out, err := exec.Command("freebsd-version", args...).Output()
		if err == nil {
			if v := strings.TrimSpace(string(out)); v != "" {
				return v, nil
			}
		}
	}
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
})

// FreeBSDVersion returns the installed FreeBSD userland version, falling back
// to the kernel version and then to `uname -r`.
func FreeBSDVersion() (string, error) {
	return freebsdVersion()
}
