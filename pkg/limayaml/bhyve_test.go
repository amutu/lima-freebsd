// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package limayaml

import (
	"testing"

	"gotest.tools/v3/assert"

	"github.com/lima-vm/lima/v2/pkg/limatype"
)

func withSlirpSupport(t *testing.T, supported bool) {
	t.Helper()
	old := bhyveSlirpOpenSupported
	bhyveSlirpOpenSupported = func() bool { return supported }
	t.Cleanup(func() { bhyveSlirpOpenSupported = old })
}

func TestBhyveNetworkConfigDefaults(t *testing.T) {
	withSlirpSupport(t, false)
	cfg, err := ResolveBhyveNetwork("/home/user/.lima/default", &limatype.LimaYAML{})
	assert.NilError(t, err)
	assert.Equal(t, cfg.Mode, BhyveModeBridge)
	assert.Equal(t, cfg.Bridge, DefaultBhyveBridge)
	assert.Equal(t, cfg.Network.String(), DefaultBhyveNetwork)
	assert.Equal(t, cfg.Gateway.String(), "172.16.0.1")
	assert.Equal(t, cfg.PrefixLen, 24)
	assert.Assert(t, cfg.Network.Contains(cfg.Address))
	assert.Assert(t, !cfg.Address.Equal(cfg.Gateway))
	if last := cfg.Address.To4()[3]; last < defaultBhyveIPFirst || last > defaultBhyveIPLast {
		t.Fatalf("derived address %q is outside the default range", cfg.Address)
	}

	again, err := ResolveBhyveNetwork("/home/user/.lima/default", &limatype.LimaYAML{})
	assert.NilError(t, err)
	assert.Equal(t, again.Address.String(), cfg.Address.String())
}

func TestBhyveNetworkConfigAutoSlirp(t *testing.T) {
	withSlirpSupport(t, true)
	cfg, err := ResolveBhyveNetwork("/home/user/.lima/default", &limatype.LimaYAML{})
	assert.NilError(t, err)
	assert.Equal(t, cfg.Mode, BhyveModeSlirp)
	assert.Equal(t, cfg.Network.String(), BhyveSlirpNetwork)
	assert.Equal(t, cfg.Gateway.String(), BhyveSlirpGateway)
	assert.Equal(t, cfg.Address.String(), BhyveSlirpAddress)
	assert.Equal(t, cfg.PrefixLen, 24)
}

func TestBhyveNetworkConfigExplicitSlirp(t *testing.T) {
	withSlirpSupport(t, true)
	y := &limatype.LimaYAML{VMOpts: limatype.VMOpts{limatype.BHYVE: map[string]any{
		"networking": "slirp",
	}}}
	cfg, err := ResolveBhyveNetwork("/home/user/.lima/default", y)
	assert.NilError(t, err)
	assert.Equal(t, cfg.Mode, BhyveModeSlirp)
	assert.Equal(t, cfg.Address.String(), BhyveSlirpAddress)
}

func TestBhyveNetworkConfigSlirpUnsupported(t *testing.T) {
	withSlirpSupport(t, false)
	y := &limatype.LimaYAML{VMOpts: limatype.VMOpts{limatype.BHYVE: map[string]any{
		"networking": "slirp",
	}}}
	_, err := ResolveBhyveNetwork("/home/user/.lima/default", y)
	assert.ErrorContains(t, err, "FreeBSD 15.1")
}

func TestBhyveNetworkConfigCustom(t *testing.T) {
	y := &limatype.LimaYAML{VMOpts: limatype.VMOpts{limatype.BHYVE: map[string]any{
		"networking": "bridge",
		"bridge":     "vm-lab",
		"network":    "10.10.0.0/16",
		"gateway":    "10.10.0.1",
		"address":    "10.10.0.42",
	}}}
	cfg, err := ResolveBhyveNetwork("/home/user/.lima/lab", y)
	assert.NilError(t, err)
	assert.Equal(t, cfg.Mode, BhyveModeBridge)
	assert.Equal(t, cfg.Bridge, "vm-lab")
	assert.Equal(t, cfg.Network.String(), "10.10.0.0/16")
	assert.Equal(t, cfg.Gateway.String(), "10.10.0.1")
	assert.Equal(t, cfg.Address.String(), "10.10.0.42")
	assert.Equal(t, cfg.PrefixLen, 16)
}

func TestBhyveNetworkConfigInvalid(t *testing.T) {
	cases := []struct {
		name string
		opts map[string]any
	}{
		{"bad networking", map[string]any{"networking": "bogus"}},
		{"bad network", map[string]any{"networking": "bridge", "network": "not-a-cidr"}},
		{"ipv6 network", map[string]any{"networking": "bridge", "network": "fd00::/64"}},
		{"gateway outside", map[string]any{"networking": "bridge", "network": "10.0.0.0/24", "gateway": "10.0.1.1"}},
		{"address outside", map[string]any{"networking": "bridge", "network": "10.0.0.0/24", "address": "10.0.1.1"}},
		{"address is gateway", map[string]any{"networking": "bridge", "network": "10.0.0.0/24", "gateway": "10.0.0.1", "address": "10.0.0.1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := &limatype.LimaYAML{VMOpts: limatype.VMOpts{limatype.BHYVE: tc.opts}}
			_, err := ResolveBhyveNetwork("/home/user/.lima/x", y)
			assert.Assert(t, err != nil)
		})
	}
}

func TestFreeBSDVersionAtLeast(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"15.0-RELEASE-p14", false},
		{"15.1-RELEASE", true},
		{"15.1-STABLE", true},
		{"16.0-CURRENT", true},
		{"14.3-RELEASE", false},
		{"garbage", false},
	}
	for _, tc := range cases {
		assert.Equal(t, freebsdVersionAtLeast(tc.version, 15, 1), tc.want, tc.version)
	}
}
