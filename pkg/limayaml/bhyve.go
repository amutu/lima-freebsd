// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package limayaml

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"runtime"
	"strconv"
	"strings"

	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/osutil"
)

const (
	// DefaultBhyveBridge is the vm-bhyve bridge used when vmOpts.bhyve.bridge
	// is not set.
	DefaultBhyveBridge = "vm-public"
	// DefaultBhyveNetwork is the subnet used when vmOpts.bhyve.network is not
	// set.
	DefaultBhyveNetwork = "172.16.0.0/24"
	// BhyveSlirpNetwork, BhyveSlirpGateway, and BhyveSlirpAddress are the fixed
	// libslirp network parameters used by FreeBSD >= 15.1 slirp "open" mode.
	BhyveSlirpNetwork = "10.0.2.0/24"
	BhyveSlirpGateway = "10.0.2.2"
	BhyveSlirpAddress = "10.0.2.15"
	// Bhyve networking modes.
	BhyveModeAuto   = "auto"
	BhyveModeSlirp  = "slirp"
	BhyveModeBridge = "bridge"
	// The derived guest address is chosen from bhyveIPFirst..bhyveIPLast,
	// which stays outside the vm-bhyve dnsmasq pool for the default network.
	defaultBhyveIPFirst = 2
	defaultBhyveIPLast  = 9
)

// BhyveNetworkConfig is the resolved slirp or tap-on-bridge network
// configuration.
type BhyveNetworkConfig struct {
	Mode      string
	Bridge    string
	Network   *net.IPNet
	Gateway   net.IP
	Address   net.IP
	PrefixLen int
}

// BhyveOptsFrom returns the typed `vmOpts.bhyve` configuration.
func BhyveOptsFrom(y *limatype.LimaYAML) (limatype.BhyveOpts, error) {
	var opts limatype.BhyveOpts
	if y == nil {
		return opts, nil
	}
	raw, ok := y.VMOpts[limatype.BHYVE]
	if !ok || raw == nil {
		return opts, nil
	}
	if err := Convert(raw, &opts, "vmOpts.bhyve"); err != nil {
		return opts, err
	}
	return opts, nil
}

// ResolveBhyveNetwork resolves the tap-on-bridge network configuration for the
// instance. Empty vmOpts.bhyve fields fall back to the documented defaults.
func ResolveBhyveNetwork(instDir string, y *limatype.LimaYAML) (BhyveNetworkConfig, error) {
	var cfg BhyveNetworkConfig
	opts, err := BhyveOptsFrom(y)
	if err != nil {
		return cfg, err
	}

	mode := opts.Networking
	if mode == "" {
		mode = BhyveModeAuto
	}
	switch mode {
	case BhyveModeAuto:
		if bhyveSlirpOpenSupported() {
			mode = BhyveModeSlirp
		} else {
			mode = BhyveModeBridge
		}
	case BhyveModeSlirp:
		if !bhyveSlirpOpenSupported() {
			return cfg, errors.New("field `vmOpts.bhyve.networking` must not be \"slirp\" before FreeBSD 15.1 (slirp \"open\" is unavailable); use \"bridge\" or \"auto\"")
		}
	case BhyveModeBridge:
	default:
		return cfg, fmt.Errorf("field `vmOpts.bhyve.networking` must be %q, %q, or %q, got %q",
			BhyveModeAuto, BhyveModeSlirp, BhyveModeBridge, mode)
	}
	cfg.Mode = mode

	if mode == BhyveModeSlirp {
		_, ipnet, err := net.ParseCIDR(BhyveSlirpNetwork)
		if err != nil {
			return cfg, err
		}
		cfg.Network = ipnet
		cfg.Gateway = net.ParseIP(BhyveSlirpGateway).To4()
		cfg.Address = net.ParseIP(BhyveSlirpAddress).To4()
		cfg.PrefixLen = 24
		return cfg, nil
	}

	cfg.Bridge = opts.Bridge
	if cfg.Bridge == "" {
		cfg.Bridge = DefaultBhyveBridge
	}

	network := opts.Network
	if network == "" {
		network = DefaultBhyveNetwork
	}
	_, ipnet, err := net.ParseCIDR(network)
	if err != nil {
		return cfg, fmt.Errorf("field `vmOpts.bhyve.network` must be a CIDR, got %#q: %w", network, err)
	}
	if ipnet.IP.To4() == nil {
		return cfg, fmt.Errorf("field `vmOpts.bhyve.network` must be IPv4, got %#q", network)
	}
	ones, bits := ipnet.Mask.Size()
	if bits-ones < 2 {
		return cfg, fmt.Errorf("field `vmOpts.bhyve.network` must have at least 4 addresses, got %#q", network)
	}
	cfg.Network = ipnet
	cfg.PrefixLen = ones

	if opts.Gateway != "" {
		cfg.Gateway = net.ParseIP(opts.Gateway).To4()
		if cfg.Gateway == nil {
			return cfg, fmt.Errorf("field `vmOpts.bhyve.gateway` must be an IPv4 address, got %#q", opts.Gateway)
		}
	} else {
		cfg.Gateway = addIPv4(ipnet.IP.To4(), 1)
	}
	if !ipnet.Contains(cfg.Gateway) || cfg.Gateway.Equal(ipnet.IP) || cfg.Gateway.Equal(lastIPv4(ipnet)) {
		return cfg, fmt.Errorf("field `vmOpts.bhyve.gateway` must be a usable address in %#q, got %#q",
			network, cfg.Gateway)
	}

	if opts.Address != "" {
		cfg.Address = net.ParseIP(opts.Address).To4()
		if cfg.Address == nil {
			return cfg, fmt.Errorf("field `vmOpts.bhyve.address` must be an IPv4 address, got %#q", opts.Address)
		}
		if !ipnet.Contains(cfg.Address) || cfg.Address.Equal(ipnet.IP) || cfg.Address.Equal(lastIPv4(ipnet)) {
			return cfg, fmt.Errorf("field `vmOpts.bhyve.address` must be a usable address in %#q, got %#q",
				network, cfg.Address)
		}
		if cfg.Address.Equal(cfg.Gateway) {
			return cfg, fmt.Errorf("field `vmOpts.bhyve.address` must differ from the gateway %#q", cfg.Gateway)
		}
	} else {
		cfg.Address = deriveBhyveAddress(ipnet, cfg.Gateway, instDir)
	}

	return cfg, nil
}

func deriveBhyveAddress(ipnet *net.IPNet, gateway net.IP, instDir string) net.IP {
	ones, bits := ipnet.Mask.Size()
	hostBits := bits - ones
	maxHost := (uint64(1) << uint(hostBits)) - 2
	start, end := uint64(defaultBhyveIPFirst), uint64(defaultBhyveIPLast)
	if end > maxHost {
		end = maxHost
	}
	if start > end {
		start = end
	}
	count := end - start + 1

	h := fnv.New32a()
	_, _ = h.Write([]byte(instDir))
	offset := start + uint64(h.Sum32())%count

	base := binary.BigEndian.Uint32(ipnet.IP.To4())
	for i := uint64(0); i < count; i++ {
		candidate := make(net.IP, net.IPv4len)
		binary.BigEndian.PutUint32(candidate, base+uint32(offset))
		if !candidate.Equal(gateway) {
			return candidate
		}
		offset++
		if offset > end {
			offset = start
		}
	}
	return addIPv4(ipnet.IP.To4(), 1)
}

func addIPv4(base net.IP, n uint32) net.IP {
	ip := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(ip, binary.BigEndian.Uint32(base)+n)
	return ip
}

func lastIPv4(ipnet *net.IPNet) net.IP {
	base := binary.BigEndian.Uint32(ipnet.IP.To4())
	mask := binary.BigEndian.Uint32(ipnet.Mask)
	ip := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(ip, base|^mask)
	return ip
}

// bhyveSlirpOpenSupported is a variable so that tests can simulate old and new
// FreeBSD releases without booting them.
var bhyveSlirpOpenSupported = detectBhyveSlirpOpen

func detectBhyveSlirpOpen() bool {
	if runtime.GOOS != "freebsd" {
		return false
	}
	v, err := osutil.FreeBSDVersion()
	if err != nil {
		return false
	}
	return freebsdVersionAtLeast(v, 15, 1)
}

func freebsdVersionAtLeast(v string, wantMajor, wantMinor int) bool {
	major, minor, ok := parseMajorMinor(v)
	if !ok {
		return false
	}
	return major > wantMajor || (major == wantMajor && minor >= wantMinor)
}

func parseMajorMinor(v string) (int, int, bool) {
	parts := strings.SplitN(strings.TrimSpace(v), ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minorStr := parts[1]
	end := 0
	for end < len(minorStr) && minorStr[end] >= '0' && minorStr[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, 0, false
	}
	minor, err := strconv.Atoi(minorStr[:end])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}
