// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

//go:build freebsd

package osutil

import (
	"bufio"
	"os"
	"strings"
)

// DNSAddresses returns the nameservers from /etc/resolv.conf. bhyve guests use
// these directly through the vm-public NAT, so they must be the host's real
// resolvers rather than the host-agent DNS proxy.
func DNSAddresses() ([]string, error) {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var addresses []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			addresses = append(addresses, fields[1])
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return addresses, nil
}

func ProxySettings() (map[string]string, error) {
	return make(map[string]string), nil
}
