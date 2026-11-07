// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package limatype

// BhyveOpts is the typed form of `vmOpts.bhyve`.
//
// Networking selects slirp or tap-on-bridge. On FreeBSD >= 15.1 "auto" uses
// slirp with the "open" NAT option; on older releases it uses the bridge.
// Bridge, Network, Gateway, and Address configure the bridge mode only.
type BhyveOpts struct {
	// Firmware overrides the auto-detected boot ROM path.
	Firmware string `yaml:"firmware,omitempty" json:"firmware,omitempty"`
	// Networking is "auto", "slirp", or "bridge" (default: auto).
	Networking string `yaml:"networking,omitempty" json:"networking,omitempty"`
	// Bridge is the host bridge the tap is attached to (default: vm-public).
	Bridge string `yaml:"bridge,omitempty" json:"bridge,omitempty"`
	// Network is the bridge subnet in CIDR notation (default: 172.16.0.0/24).
	Network string `yaml:"network,omitempty" json:"network,omitempty"`
	// Gateway is the host address on the bridge (default: first host in Network).
	Gateway string `yaml:"gateway,omitempty" json:"gateway,omitempty"`
	// Address is the static guest address (default: derived from the instance
	// directory within the first eight usable hosts of Network).
	Address string `yaml:"address,omitempty" json:"address,omitempty"`
}
